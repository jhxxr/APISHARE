package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func normalizedUserHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func publicUserIP(ip net.IP) bool {
	if allowLoopbackUserUpstreamForTest && ip.IsLoopback() {
		return true
	}
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// Include shared carrier networks and "this network", which IsPrivate excludes.
		return v4[0] != 0 && !(v4[0] == 100 && v4[1]&0xc0 == 64)
	}
	return true
}

func validatePublicUserHost(host string) error {
	host = normalizedUserHost(host)
	if host == "" || strings.Contains(host, "%") {
		return errors.New("接口或代理地址缺少有效主机名")
	}
	if allowLoopbackUserUpstreamForTest && host == "localhost" {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return errors.New("禁止使用本机或内网域名作为接口或代理地址")
	}
	if ip := net.ParseIP(host); ip != nil && !publicUserIP(ip) {
		return errors.New("禁止使用本机、局域网或链路本地 IP 作为接口或代理地址")
	}
	return nil
}

type userHostLookup func(context.Context, string) ([]net.IPAddr, error)

// Validate every answer, not just the first. The chosen numeric IP is then used
// on the wire, so neither a second DNS lookup nor a proxy can re-resolve the name.
func resolvePublicUserHost(ctx context.Context, host string, lookup userHostLookup) (net.IP, error) {
	if err := validatePublicUserHost(host); err != nil {
		return nil, err
	}
	host = normalizedUserHost(host)
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	answers, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("无法解析接口或代理主机名: %w", err)
	}
	if len(answers) == 0 {
		return nil, errors.New("接口或代理主机名没有可用地址")
	}
	var chosen net.IP
	for _, answer := range answers {
		if answer.Zone != "" || !publicUserIP(answer.IP) {
			return nil, errors.New("接口或代理主机名解析到了被禁止的本机或内网地址")
		}
		if chosen == nil || chosen.To4() == nil && answer.IP.To4() != nil {
			chosen = answer.IP
		}
	}
	return chosen, nil
}

type publicUserTransportKey struct {
	base          *http.Transport
	host          string
	proxy         string
	proxyHostname string
}

var publicUserTransports = struct {
	sync.Mutex
	entries map[publicUserTransportKey]*http.Transport
}{entries: make(map[publicUserTransportKey]*http.Transport)}

func closePublicUserTransports(base *http.Transport) {
	publicUserTransports.Lock()
	defer publicUserTransports.Unlock()
	for key, transport := range publicUserTransports.entries {
		if key.base == base {
			transport.CloseIdleConnections()
			delete(publicUserTransports.entries, key)
		}
	}
}

type publicUserTransport struct {
	base      *http.Transport
	userProxy bool
	lookup    userHostLookup
}

// Site/admin clients retain their existing network policy. Only user-owned
// destinations and user-owned proxy endpoints use this fail-closed client.
func publicUserHTTPClient(base *http.Client, userProxy bool) (*http.Client, error) {
	raw := base.Transport
	if raw == nil {
		raw = http.DefaultTransport
	}
	transport, ok := raw.(*http.Transport)
	if !ok {
		return nil, errors.New("用户自定义接口不支持当前 HTTP 传输配置")
	}
	client := *base
	client.Transport = &publicUserTransport{base: transport, userProxy: userProxy, lookup: net.DefaultResolver.LookupIPAddr}
	// Do not forward credentials or follow redirects into a different network.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

func pinnedUserHost(u *url.URL, ip net.IP) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(ip.String(), port)
}

func (t *publicUserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validatePublicUpstreamURL(req.URL.String(), true); err != nil {
		return nil, err
	}
	ip, err := resolvePublicUserHost(req.Context(), req.URL.Hostname(), t.lookup)
	if err != nil {
		return nil, err
	}
	var proxyURL *url.URL
	var proxyHostname string
	if t.base.Proxy != nil {
		proxyURL, err = t.base.Proxy(req)
		if err != nil {
			return nil, err
		}
	}
	if proxyURL != nil {
		copy := *proxyURL
		proxyURL = &copy
		proxyHostname = normalizedUserHost(proxyURL.Hostname())
		if t.userProxy {
			proxyIP, resolveErr := resolvePublicUserHost(req.Context(), proxyHostname, t.lookup)
			if resolveErr != nil {
				return nil, resolveErr
			}
			proxyURL.Host = pinnedUserHost(proxyURL, proxyIP)
		}
	}
	transport := t.transportForHost(normalizedUserHost(req.URL.Hostname()), proxyURL, proxyHostname)
	pinned := req.Clone(req.Context())
	copyURL := *req.URL
	pinned.URL = &copyURL
	pinned.URL.Host = pinnedUserHost(req.URL, ip)
	if pinned.Host == "" {
		pinned.Host = req.URL.Host
	}
	if proxyURL != nil && (proxyURL.Scheme == "http" || proxyURL.Scheme == "https") && req.URL.Scheme == "http" {
		// Request.WriteProxy otherwise rebuilds the absolute URI from Host,
		// undoing the IP pin. Keep the original Host header separate from it.
		path := req.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
		pinned.URL.Opaque = "//" + pinned.URL.Host + path
	}
	return transport.RoundTrip(pinned)
}

func (t *publicUserTransport) transportForHost(host string, proxyURL *url.URL, proxyHostname string) *http.Transport {
	key := publicUserTransportKey{base: t.base, host: host, proxyHostname: proxyHostname}
	if proxyURL != nil {
		key.proxy = proxyURL.String()
	}
	publicUserTransports.Lock()
	defer publicUserTransports.Unlock()
	if cached := publicUserTransports.entries[key]; cached != nil {
		return cached
	}
	transport := t.base.Clone()
	transport.Proxy = nil
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	tlsConfig := &tls.Config{}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}
	tlsConfig.ServerName = host // Preserve certificate verification and SNI after IP pinning.
	transport.TLSClientConfig = tlsConfig
	transport.DialTLS, transport.DialTLSContext = nil, nil
	if proxyURL != nil && proxyURL.Scheme == "https" {
		// The first TLS hop authenticates the proxy, not the destination.
		dial := transport.DialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		proxyTLS := tlsConfig.Clone()
		proxyTLS.ServerName = proxyHostname
		proxyTLS.NextProtos = []string{"http/1.1"}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if timeout := transport.TLSHandshakeTimeout; timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			secured := tls.Client(conn, proxyTLS)
			if err := secured.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			return secured, nil
		}
	}
	// Probe URLs are user-controlled; cap the number of cached origin/proxy pairs.
	if len(publicUserTransports.entries) >= 128 {
		for oldKey, old := range publicUserTransports.entries {
			old.CloseIdleConnections()
			delete(publicUserTransports.entries, oldKey)
			break
		}
	}
	publicUserTransports.entries[key] = transport
	return transport
}
