//go:build with_naive_outbound

package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

func startNaiveFallbackInstance(t *testing.T, networkList option.NetworkList, quic bool, fallback *option.NaiveFallbackOptions) string {
	t.Helper()
	caPem, certPem, keyPem := createSelfSignedCertificate(t, "example.org")
	caPemContent, err := os.ReadFile(caPem)
	require.NoError(t, err)
	startInstance(t, option.Options{
		Inbounds: []option.Inbound{
			{
				Type: C.TypeMixed,
				Tag:  "mixed-in",
				Options: &option.HTTPMixedInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     common.Ptr(badoption.Addr(netip.IPv4Unspecified())),
						ListenPort: clientPort,
					},
				},
			},
			{
				Type: C.TypeNaive,
				Tag:  "naive-in",
				Options: &option.NaiveInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:     common.Ptr(badoption.Addr(netip.IPv4Unspecified())),
						ListenPort: serverPort,
					},
					Users: []auth.User{
						{
							Username: "sekai",
							Password: "password",
						},
					},
					Network:  networkList,
					Fallback: fallback,
					InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
						TLS: &option.InboundTLSOptions{
							Enabled:         true,
							ServerName:      "example.org",
							CertificatePath: certPem,
							KeyPath:         keyPem,
						},
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: C.TypeDirect,
			},
			{
				Type: C.TypeNaive,
				Tag:  "naive-out",
				Options: &option.NaiveOutboundOptions{
					ServerOptions: option.ServerOptions{
						Server:     "127.0.0.1",
						ServerPort: serverPort,
					},
					Username: "sekai",
					Password: "password",
					QUIC:     quic,
					OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
						TLS: &option.OutboundTLSOptions{
							Enabled:     true,
							ServerName:  "example.org",
							Certificate: []string{string(caPemContent)},
						},
					},
				},
			},
		},
		Route: &option.RouteOptions{
			Rules: []option.Rule{
				{
					Type: C.RuleTypeDefault,
					DefaultOptions: option.DefaultRule{
						RawDefaultRule: option.RawDefaultRule{
							Inbound: []string{"mixed-in"},
						},
						RuleAction: option.RuleAction{
							Action: C.RuleActionTypeRoute,
							RouteOptions: option.RouteActionOptions{
								Outbound: "naive-out",
							},
						},
					},
				},
			},
		},
	})
	return caPem
}

func newNaiveFallbackUpstream(t *testing.T, connectSeen *atomic.Bool) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			connectSeen.Store(true)
		}
		writer.Header().Set("X-Fallback", "yes")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("fallback page"))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func testNaiveFallbackHTTP(t *testing.T, caPem string, connectSeen *atomic.Bool) {
	t.Helper()
	caPemContent, err := os.ReadFile(caPem)
	require.NoError(t, err)
	certPool := x509.NewCertPool()
	require.True(t, certPool.AppendCertsFromPEM(caPemContent))
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    certPool,
				ServerName: "example.org",
			},
		},
	}
	defer client.CloseIdleConnections()
	response, err := client.Get("https://127.0.0.1:" + strconv.Itoa(int(serverPort)) + "/hello?from=e2e")
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "fallback page", string(body))
	require.Equal(t, "yes", response.Header.Get("X-Fallback"))

	conn, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(serverPort)), &tls.Config{
		RootCAs:    certPool,
		ServerName: "example.org",
		NextProtos: []string{"http/1.1"},
	})
	require.NoError(t, err)
	defer conn.Close()
	authorization := base64.StdEncoding.EncodeToString([]byte("sekai:wrong"))
	_, err = conn.Write([]byte("CONNECT example.org:443 HTTP/1.1\r\nHost: example.org:443\r\nPadding: padding\r\nProxy-Authorization: Basic " + authorization + "\r\n\r\n"))
	require.NoError(t, err)
	rejectResponse, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	rejectBody, err := io.ReadAll(rejectResponse.Body)
	rejectResponse.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusMethodNotAllowed, rejectResponse.StatusCode)
	require.Empty(t, rejectBody)
	require.False(t, connectSeen.Load())
}

func TestNaiveFallback(t *testing.T) {
	var connectSeen atomic.Bool
	upstream := newNaiveFallbackUpstream(t, &connectSeen)
	caPem := startNaiveFallbackInstance(t, network.NetworkTCP, false, &option.NaiveFallbackOptions{
		URL: upstream.URL + "/site",
	})
	testTCP(t, clientPort, testPort)
	testNaiveFallbackHTTP(t, caPem, &connectSeen)
}

func TestNaiveFallbackQUIC(t *testing.T) {
	var connectSeen atomic.Bool
	upstream := newNaiveFallbackUpstream(t, &connectSeen)
	startNaiveFallbackInstance(t, network.NetworkUDP, true, &option.NaiveFallbackOptions{
		URL: upstream.URL + "/site",
	})
	testTCP(t, clientPort, testPort)
	require.False(t, connectSeen.Load())
}

func TestNaiveFallbackRejectURL(t *testing.T) {
	var connectSeen atomic.Bool
	var rejectHost atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			connectSeen.Store(true)
		}
		if request.URL.Path == "/site/reject" {
			rejectHost.Store(request.Host)
		}
		writer.Header().Set("X-Fallback", "yes")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("fallback page"))
	}))
	t.Cleanup(upstream.Close)
	caPem := startNaiveFallbackInstance(t, network.NetworkTCP, false, &option.NaiveFallbackOptions{
		URL:       upstream.URL + "/site",
		RejectURL: "/reject",
	})
	testTCP(t, clientPort, testPort)

	caPemContent, err := os.ReadFile(caPem)
	require.NoError(t, err)
	certPool := x509.NewCertPool()
	require.True(t, certPool.AppendCertsFromPEM(caPemContent))
	conn, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(serverPort)), &tls.Config{
		RootCAs:    certPool,
		ServerName: "example.org",
		NextProtos: []string{"http/1.1"},
	})
	require.NoError(t, err)
	defer conn.Close()
	authorization := base64.StdEncoding.EncodeToString([]byte("sekai:wrong"))
	_, err = conn.Write([]byte("CONNECT example.org:443 HTTP/1.1\r\nHost: example.org:443\r\nPadding: padding\r\nProxy-Authorization: Basic " + authorization + "\r\n\r\n"))
	require.NoError(t, err)
	rejectResponse, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	rejectBody, err := io.ReadAll(rejectResponse.Body)
	rejectResponse.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusMethodNotAllowed, rejectResponse.StatusCode)
	require.Equal(t, "fallback page", string(rejectBody))
	require.Equal(t, "yes", rejectResponse.Header.Get("X-Fallback"))
	require.Equal(t, "example.org", rejectHost.Load())
	require.False(t, connectSeen.Load())
}
