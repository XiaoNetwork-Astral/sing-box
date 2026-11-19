package naive

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

type fallbackRecordedRequest struct {
	method     string
	requestURI string
	host       string
	path       string
	rawQuery   string
	header     http.Header
}

type fallbackRecorder struct {
	access   sync.Mutex
	requests []fallbackRecordedRequest
}

func (r *fallbackRecorder) add(request *http.Request) {
	r.access.Lock()
	defer r.access.Unlock()
	r.requests = append(r.requests, fallbackRecordedRequest{
		method:     request.Method,
		requestURI: request.RequestURI,
		host:       request.Host,
		path:       request.URL.Path,
		rawQuery:   request.URL.RawQuery,
		header:     request.Header.Clone(),
	})
}

func (r *fallbackRecorder) all() []fallbackRecordedRequest {
	r.access.Lock()
	defer r.access.Unlock()
	return append([]fallbackRecordedRequest(nil), r.requests...)
}

func (r *fallbackRecorder) last() fallbackRecordedRequest {
	r.access.Lock()
	defer r.access.Unlock()
	return r.requests[len(r.requests)-1]
}

func newFallbackUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *fallbackRecorder) {
	t.Helper()
	recorder := &fallbackRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.add(request)
		if handler != nil {
			handler(writer, request)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

func newFallbackInbound(t *testing.T, fallback *option.NaiveFallbackOptions) *Inbound {
	t.Helper()
	inbound, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "test", option.NaiveInboundOptions{
		Users: []auth.User{
			{
				Username: "sekai",
				Password: "password",
			},
		},
		Network:  network.NetworkTCP,
		Fallback: fallback,
	})
	require.NoError(t, err)
	naiveInbound := inbound.(*Inbound)
	if naiveInbound.fallback != nil {
		t.Cleanup(func() {
			_ = naiveInbound.fallback.Close()
		})
	}
	return naiveInbound
}

func newFallbackRequest(method string, target string, remoteAddress string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = remoteAddress
	return request
}

func TestFallbackProxy(t *testing.T) {
	t.Run("path and headers", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL: upstream.URL + "/base?prefix=1",
			Headers: badoption.HTTPHeader{
				"X-Test":  {"server"},
				"X-Multi": {"one", "two"},
			},
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello?q=2", "203.0.113.5:1234")
		request.Header.Set("X-Test", "client")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "ok", response.Body.String())
		requests := recorder.all()
		require.Len(t, requests, 1)
		require.Equal(t, "/base/hello", requests[0].path)
		require.Equal(t, "prefix=1&q=2", requests[0].rawQuery)
		require.Equal(t, "original.example", requests[0].host)
		require.Equal(t, []string{"server"}, requests[0].header.Values("X-Test"))
		require.Equal(t, []string{"one", "two"}, requests[0].header.Values("X-Multi"))
	})

	t.Run("rewrite host", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:         upstream.URL + "/base",
			RewriteHost: true,
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, strings.TrimPrefix(upstream.URL, "http://"), recorder.last().host)
	})

	t.Run("host header", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:         upstream.URL + "/base",
			RewriteHost: true,
			Headers: badoption.HTTPHeader{
				"Host": {"override.example"},
			},
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, "override.example", recorder.last().host)
	})
}

func TestFallbackForwarding(t *testing.T) {
	t.Run("direct strips headers", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: upstream.URL})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
		request.Header.Set("Forwarded", "for=198.51.100.7")
		request.Header.Set("X-Forwarded-For", "198.51.100.7")
		request.Header.Set("X-Forwarded-Host", "forged.example")
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("X-Real-Ip", "198.51.100.7")
		request.Header.Set("Cf-Connecting-Ip", "198.51.100.7")
		request.Header.Set("True-Client-Ip", "198.51.100.7")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		header := recorder.last().header
		for _, name := range fallbackForwardedHeaders {
			require.Empty(t, header.Values(name), name)
		}
	})

	t.Run("direct sets forwarded for", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:              upstream.URL,
			EnableXForwarded: true,
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
		request.Header.Set("X-Forwarded-For", "198.51.100.7")
		request.Header.Set("X-Real-Ip", "198.51.100.7")
		request.Header.Set("Cf-Connecting-Ip", "198.51.100.7")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		header := recorder.last().header
		require.Equal(t, []string{"203.0.113.5"}, header.Values("X-Forwarded-For"))
		require.Equal(t, []string{"http"}, header.Values("X-Forwarded-Proto"))
		require.Equal(t, []string{"original.example"}, header.Values("X-Forwarded-Host"))
		require.Empty(t, header.Values("X-Real-Ip"))
		require.Empty(t, header.Values("Cf-Connecting-Ip"))
	})

	t.Run("relayed keeps headers", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: upstream.URL})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "127.0.0.1:1234")
		request.Header.Set("Forwarded", "for=198.51.100.7")
		request.Header.Set("X-Forwarded-For", "192.0.2.1, 198.51.100.7")
		request.Header.Set("X-Forwarded-Host", "relay.example")
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("X-Real-Ip", "198.51.100.7")
		request.Header.Set("Cf-Connecting-Ip", "198.51.100.7")
		request.Header.Set("True-Client-Ip", "198.51.100.7")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		header := recorder.last().header
		require.Equal(t, []string{"for=198.51.100.7"}, header.Values("Forwarded"))
		require.Equal(t, []string{"192.0.2.1, 198.51.100.7"}, header.Values("X-Forwarded-For"))
		require.Equal(t, []string{"relay.example"}, header.Values("X-Forwarded-Host"))
		require.Equal(t, []string{"https"}, header.Values("X-Forwarded-Proto"))
		require.Equal(t, []string{"198.51.100.7"}, header.Values("X-Real-Ip"))
		require.Equal(t, []string{"198.51.100.7"}, header.Values("Cf-Connecting-Ip"))
		require.Equal(t, []string{"198.51.100.7"}, header.Values("True-Client-Ip"))
	})

	t.Run("relayed cloudflare address", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:              upstream.URL,
			EnableXForwarded: true,
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "127.0.0.1:1234")
		request.Header.Set("Cf-Connecting-Ip", "198.51.100.7")
		request.Header.Set("X-Forwarded-For", "192.0.2.1, 10.0.0.2")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, []string{"198.51.100.7"}, recorder.last().header.Values("X-Forwarded-For"))
	})

	t.Run("relayed forwarded chain", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:              upstream.URL,
			EnableXForwarded: true,
		})
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "127.0.0.1:1234")
		request.Header.Set("X-Forwarded-For", "192.0.2.1, 198.51.100.7, 10.0.0.2")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		require.Equal(t, []string{"198.51.100.7"}, recorder.last().header.Values("X-Forwarded-For"))
	})
}

func TestFallbackReject(t *testing.T) {
	t.Run("missing padding", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: upstream.URL})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		require.Empty(t, response.Body.String())
		require.Empty(t, recorder.all())
	})

	t.Run("bad password", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: upstream.URL})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		request.Header.Set("Padding", "padding")
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		require.Empty(t, response.Body.String())
		require.Empty(t, recorder.all())
	})

	t.Run("custom status", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:              upstream.URL,
			RejectStatusCode: http.StatusTeapot,
		})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusTeapot, response.Code)
		require.Empty(t, response.Body.String())
		require.Empty(t, recorder.all())
	})
}

func TestFallbackRejectURL(t *testing.T) {
	t.Run("relative path", func(t *testing.T) {
		upstream, recorder := newFallbackUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("X-Reject", "yes")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte("landing page"))
		})
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:              upstream.URL + "/base?prefix=1",
			RejectURL:        "/landing?from=reject",
			RejectStatusCode: http.StatusForbidden,
			Headers: badoption.HTTPHeader{
				"X-Test": {"server"},
			},
			EnableXForwarded: true,
		})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		request.TLS = &tls.ConnectionState{ServerName: "sni.example"}
		request.Header.Set("Padding", "padding")
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code)
		require.Equal(t, "landing page", response.Body.String())
		require.Equal(t, "yes", response.Header().Get("X-Reject"))
		requests := recorder.all()
		require.Len(t, requests, 1)
		require.Equal(t, http.MethodGet, requests[0].method)
		require.Equal(t, "/base/landing", requests[0].path)
		require.Equal(t, "prefix=1&from=reject", requests[0].rawQuery)
		require.Equal(t, "sni.example", requests[0].host)
		require.Equal(t, []string{"server"}, requests[0].header.Values("X-Test"))
		require.Equal(t, []string{"203.0.113.5"}, requests[0].header.Values("X-Forwarded-For"))
		require.Equal(t, []string{"sni.example"}, requests[0].header.Values("X-Forwarded-Host"))
		require.Empty(t, requests[0].header.Values("Proxy-Authorization"))
		require.Empty(t, requests[0].header.Values("Padding"))
		require.Equal(t, "12", response.Header().Get("Content-Length"))
	})

	t.Run("absolute URL", func(t *testing.T) {
		upstream, _ := newFallbackUpstream(t, nil)
		rejectUpstream, rejectRecorder := newFallbackUpstream(t, nil)
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:         upstream.URL + "/base",
			RejectURL:   rejectUpstream.URL + "/absolute?from=reject",
			RewriteHost: true,
		})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		request.Header.Set("Padding", "padding")
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		requests := rejectRecorder.all()
		require.Len(t, requests, 1)
		require.Equal(t, "/absolute", requests[0].path)
		require.Equal(t, "from=reject", requests[0].rawQuery)
		require.Equal(t, strings.TrimPrefix(rejectUpstream.URL, "http://"), requests[0].host)
	})

	t.Run("unreachable", func(t *testing.T) {
		upstream, _ := newFallbackUpstream(t, nil)
		deadUpstream, _ := newFallbackUpstream(t, nil)
		deadURL := deadUpstream.URL
		deadUpstream.Close()
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
			URL:       upstream.URL,
			RejectURL: deadURL + "/unreachable",
		})
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		request.Header.Set("Padding", "padding")
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		require.Empty(t, response.Body.String())
	})
}

func TestFallbackValidation(t *testing.T) {
	testCases := []struct {
		name     string
		fallback *option.NaiveFallbackOptions
		error    string
	}{
		{"missing url", &option.NaiveFallbackOptions{}, "missing fallback.url"},
		{"bad scheme", &option.NaiveFallbackOptions{URL: "ftp://example.com"}, "must be http or https"},
		{"missing host", &option.NaiveFallbackOptions{URL: "http:///path"}, "missing host"},
		{"status 200", &option.NaiveFallbackOptions{URL: "http://127.0.0.1:8080", RejectStatusCode: http.StatusOK}, "between 400 and 599"},
		{"status 600", &option.NaiveFallbackOptions{URL: "http://127.0.0.1:8080", RejectStatusCode: 600}, "between 400 and 599"},
		{"bad reject url", &option.NaiveFallbackOptions{URL: "http://127.0.0.1:8080", RejectURL: "relative"}, "invalid fallback.reject_url"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewInbound(context.Background(), nil, log.NewNOPFactory().Logger(), "test", option.NaiveInboundOptions{
				Users: []auth.User{
					{
						Username: "sekai",
						Password: "password",
					},
				},
				Network:  network.NetworkTCP,
				Fallback: testCase.fallback,
			})
			require.ErrorContains(t, err, testCase.error)
		})
	}

	t.Run("valid reject url", func(t *testing.T) {
		for _, rejectURL := range []string{"", "/path?query=1", "https://example.com/path"} {
			inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
				URL:       "http://127.0.0.1:8080",
				RejectURL: rejectURL,
			})
			require.NotNil(t, inbound.fallback)
		}
	})

	t.Run("default reject status", func(t *testing.T) {
		inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: "http://127.0.0.1:8080"})
		require.Equal(t, http.StatusMethodNotAllowed, inbound.fallback.rejectStatusCode)
	})
}

func TestNoFallback(t *testing.T) {
	inbound := newFallbackInbound(t, nil)
	t.Run("non connect", func(t *testing.T) {
		request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code)
	})
	t.Run("bad password", func(t *testing.T) {
		request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
		request.Header.Set("Padding", "padding")
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
		response := httptest.NewRecorder()
		inbound.ServeHTTP(response, request)
		require.Equal(t, http.StatusProxyAuthRequired, response.Code)
	})
}

func TestFallbackUpstreamError(t *testing.T) {
	upstream, _ := newFallbackUpstream(t, nil)
	upstream.Close()
	inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{URL: upstream.URL})
	request := newFallbackRequest(http.MethodGet, "http://original.example/hello", "203.0.113.5:1234")
	response := httptest.NewRecorder()
	inbound.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Empty(t, response.Body.String())
}

func TestFallbackRejectBodyLimit(t *testing.T) {
	upstream, _ := newFallbackUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", maxFallbackRejectBody+1)), maxFallbackRejectBody+1)
	})
	inbound := newFallbackInbound(t, &option.NaiveFallbackOptions{
		URL:       upstream.URL,
		RejectURL: "/reject",
	})
	request := newFallbackRequest(http.MethodConnect, "http://connect.example:443", "203.0.113.5:1234")
	request.Header.Set("Padding", "padding")
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("sekai:wrong")))
	response := httptest.NewRecorder()
	inbound.ServeHTTP(response, request)
	require.Equal(t, http.StatusMethodNotAllowed, response.Code)
	require.Empty(t, response.Body.String())
}
