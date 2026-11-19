package naive

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
)

const maxFallbackRejectBody = 1 << 20

var fallbackHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

var fallbackForwardedHeaders = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Real-Ip",
	"Cf-Connecting-Ip",
	"True-Client-Ip",
}

type naiveFallback struct {
	url              *url.URL
	rejectURL        *url.URL
	rejectStatusCode int
	headers          badoption.HTTPHeader
	rewriteHost      bool
	enableXForwarded bool
	transport        *http.Transport
	client           *http.Client
	proxy            *httputil.ReverseProxy
}

type naiveConnContextKey struct{}

func newNaiveFallback(inbound *Inbound, options *option.NaiveFallbackOptions) (*naiveFallback, error) {
	if options.URL == "" {
		return nil, E.New("missing fallback.url")
	}
	fallbackURL, err := parseFallbackURL(options.URL)
	if err != nil {
		return nil, E.Cause(err, "invalid fallback.url")
	}
	var rejectURL *url.URL
	if options.RejectURL != "" {
		rejectURL, err = parseFallbackRejectURL(options.RejectURL)
		if err != nil {
			return nil, err
		}
	}
	rejectStatusCode := options.RejectStatusCode
	if rejectStatusCode == 0 {
		rejectStatusCode = http.StatusMethodNotAllowed
	}
	if rejectStatusCode < http.StatusBadRequest || rejectStatusCode > 599 {
		return nil, E.New("invalid fallback.reject_status_code: must be between 400 and 599")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	fallback := &naiveFallback{
		url:              fallbackURL,
		rejectURL:        rejectURL,
		rejectStatusCode: rejectStatusCode,
		headers:          options.Headers,
		rewriteHost:      options.RewriteHost,
		enableXForwarded: options.EnableXForwarded,
		transport:        transport,
	}
	fallback.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	fallback.proxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(fallback.url)
			inbound.rewriteFallbackRequest(fallback, request)
		},
		Transport: transport,
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			if E.IsClosedOrCanceled(err) {
				inbound.logger.DebugContext(request.Context(), "fallback upstream error: ", err)
			} else {
				inbound.logger.ErrorContext(request.Context(), "fallback upstream error: ", err)
			}
			writer.WriteHeader(http.StatusBadGateway)
		},
	}
	return fallback, nil
}

func parseFallbackURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, E.New("URL scheme must be http or https")
	}
	if parsed.Host == "" {
		return nil, E.New("URL missing host")
	}
	return parsed, nil
}

func parseFallbackRejectURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, E.Cause(err, "invalid fallback.reject_url")
	}
	if parsed.IsAbs() || parsed.Host != "" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, E.New("invalid fallback.reject_url: URL scheme must be http or https")
		}
		if parsed.Host == "" {
			return nil, E.New("invalid fallback.reject_url: URL missing host")
		}
		return parsed, nil
	}
	if !strings.HasPrefix(value, "/") {
		return nil, E.New("invalid fallback.reject_url: must be an absolute http or https URL or a path starting with /")
	}
	return parsed, nil
}

func (f *naiveFallback) Close() error {
	f.transport.CloseIdleConnections()
	return nil
}

func (n *Inbound) connContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, naiveConnContextKey{}, conn)
}

func (n *Inbound) rewriteFallbackRequest(fallback *naiveFallback, request *httputil.ProxyRequest) {
	clientIP, relayed := fallbackClientIP(request.In)
	n.applyFallbackForwarding(fallback, request.In.Header, request.Out.Header, request.In.Host, clientIP, relayed)
	fallback.applyHeaders(request.Out.Header)
	request.Out.Header.Del("Host")
	request.Out.Host = fallback.resolveHost(fallback.url, request.In.Host)
	n.logger.DebugContext(request.In.Context(), request.In.Method, " ", request.In.RequestURI, " ", clientIP)
}

func (n *Inbound) applyFallbackForwarding(fallback *naiveFallback, inHeader http.Header, outHeader http.Header, originalHost string, clientIP netip.Addr, relayed bool) {
	for _, name := range fallbackForwardedHeaders {
		outHeader.Del(name)
		if relayed {
			for _, value := range inHeader.Values(name) {
				outHeader.Add(name, value)
			}
		}
	}
	if !fallback.enableXForwarded {
		return
	}
	if clientIP.IsValid() {
		outHeader.Set("X-Forwarded-For", clientIP.String())
	} else {
		outHeader.Del("X-Forwarded-For")
	}
	var proto, host string
	if relayed {
		proto = inHeader.Get("X-Forwarded-Proto")
		host = inHeader.Get("X-Forwarded-Host")
	}
	if proto != "http" && proto != "https" {
		proto = "http"
		if n.tlsConfig != nil {
			proto = "https"
		}
	}
	outHeader.Set("X-Forwarded-Proto", proto)
	if host == "" {
		host = originalHost
	}
	outHeader.Set("X-Forwarded-Host", host)
}

func (f *naiveFallback) resolveHost(target *url.URL, defaultHost string) string {
	for name, values := range f.headers {
		if strings.EqualFold(name, "Host") {
			if len(values) > 0 {
				return values[0]
			}
			return ""
		}
	}
	if f.rewriteHost {
		return target.Host
	}
	return defaultHost
}

func (f *naiveFallback) applyHeaders(header http.Header) {
	for name, values := range f.headers {
		if strings.EqualFold(name, "Host") {
			continue
		}
		header.Del(name)
		for _, value := range values {
			header.Add(name, value)
		}
	}
}

func (n *Inbound) rejectFallback(ctx context.Context, writer http.ResponseWriter, request *http.Request, reason error) {
	n.badRequest(ctx, request, reason)
	fallback := n.fallback
	if fallback.rejectURL == nil {
		writer.WriteHeader(fallback.rejectStatusCode)
		return
	}
	target := fallback.rejectTarget()
	header, body, err := n.fetchFallbackReject(ctx, fallback, request, target)
	if err != nil {
		n.logger.DebugContext(ctx, "fetch fallback reject response: ", err)
		writer.WriteHeader(fallback.rejectStatusCode)
		return
	}
	for name, values := range header {
		writer.Header().Del(name)
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(fallback.rejectStatusCode)
	_, _ = writer.Write(body)
}

func (f *naiveFallback) rejectTarget() *url.URL {
	if f.rejectURL.IsAbs() {
		target := *f.rejectURL
		return &target
	}
	outRequest := &http.Request{URL: &url.URL{
		Path:       f.rejectURL.Path,
		RawPath:    f.rejectURL.RawPath,
		RawQuery:   f.rejectURL.RawQuery,
		ForceQuery: f.rejectURL.ForceQuery,
	}}
	proxyRequest := &httputil.ProxyRequest{Out: outRequest}
	proxyRequest.SetURL(f.url)
	return outRequest.URL
}

func (n *Inbound) fetchFallbackReject(ctx context.Context, fallback *naiveFallback, clientRequest *http.Request, target *url.URL) (http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, C.TCPTimeout)
	defer cancel()
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	defaultHost := n.fallbackRejectDefaultHost(clientRequest)
	clientIP, relayed := fallbackClientIP(clientRequest)
	n.applyFallbackForwarding(fallback, clientRequest.Header, upstreamRequest.Header, defaultHost, clientIP, relayed)
	fallback.applyHeaders(upstreamRequest.Header)
	upstreamRequest.Host = fallback.resolveHost(target, defaultHost)
	response, err := fallback.client.Do(upstreamRequest)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFallbackRejectBody+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxFallbackRejectBody {
		return nil, nil, E.New("fallback reject response body too large")
	}
	header := response.Header.Clone()
	removeFallbackHopByHopHeaders(header)
	header.Del("Content-Length")
	return header, body, nil
}

func (n *Inbound) fallbackRejectDefaultHost(request *http.Request) string {
	serverName := fallbackServerName(request)
	if serverName != "" {
		return serverName
	}
	return n.fallback.url.Host
}

func fallbackServerName(request *http.Request) string {
	if request.TLS != nil && request.TLS.ServerName != "" {
		return request.TLS.ServerName
	}
	conn, loaded := request.Context().Value(naiveConnContextKey{}).(net.Conn)
	if !loaded {
		return ""
	}
	for range 8 {
		if conn == nil {
			return ""
		}
		if stateConn, isStateConn := conn.(interface{ ConnectionState() tls.ConnectionState }); isStateConn {
			return stateConn.ConnectionState().ServerName
		}
		upstreamConn, isUpstreamConn := conn.(interface{ Upstream() any })
		if !isUpstreamConn {
			return ""
		}
		upstream, isNetConn := upstreamConn.Upstream().(net.Conn)
		if !isNetConn {
			return ""
		}
		conn = upstream
	}
	return ""
}

func removeFallbackHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				header.Del(token)
			}
		}
	}
	for _, name := range fallbackHopHeaders {
		header.Del(name)
	}
}

func fallbackClientIP(request *http.Request) (netip.Addr, bool) {
	peer, peerValid := parseFallbackAddress(request.RemoteAddr)
	relayed := peerValid && (peer.IsLoopback() || peer.IsPrivate())
	if !relayed && peerValid {
		if localAddress, loaded := request.Context().Value(http.LocalAddrContextKey).(net.Addr); loaded {
			if local, localValid := parseFallbackAddress(localAddress.String()); localValid && local == peer {
				relayed = true
			}
		}
	}
	if !relayed {
		return peer, false
	}
	if cloudflareAddress, loaded := parseFallbackAddress(request.Header.Get("Cf-Connecting-Ip")); loaded {
		return cloudflareAddress, true
	}
	addresses := parseFallbackForwardedHeader(request.Header.Values("X-Forwarded-For"))
	for _, address := range slices.Backward(addresses) {
		if !address.IsLoopback() && !address.IsPrivate() {
			return address, true
		}
	}
	if len(addresses) > 0 {
		return addresses[0], true
	}
	return peer, true
}

func parseFallbackForwardedHeader(values []string) []netip.Addr {
	var addresses []netip.Addr
	for _, value := range values {
		for entry := range strings.SplitSeq(value, ",") {
			if address, loaded := parseFallbackAddress(entry); loaded {
				addresses = append(addresses, address)
			}
		}
	}
	return addresses
}

func parseFallbackAddress(value string) (netip.Addr, bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" {
		return netip.Addr{}, false
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap(), true
	}
	if addressPort, err := netip.ParseAddrPort(value); err == nil {
		return addressPort.Addr().Unmap(), true
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		host = strings.Trim(host, "[]")
		if address, err := netip.ParseAddr(host); err == nil {
			return address.Unmap(), true
		}
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		if address, err := netip.ParseAddr(value[1 : len(value)-1]); err == nil {
			return address.Unmap(), true
		}
	}
	return netip.Addr{}, false
}
