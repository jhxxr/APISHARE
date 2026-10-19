package main

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"apishare/internal/bunhttp"

	utls "github.com/refraction-networking/utls"
)

// The official OpenCode clients run on Bun. Free-model gates on /zen and
// /inference can inspect the client beyond its headers, so the gateway
// reproduces Bun's TLS ClientHello and its HTTP/1.1 request layout for that
// traffic. Requests to /zen/go and paid models keep the normal transport.
//
// The wire implementation lives in internal/bunhttp. It is ported from
// opencode2api (GPL-3.0-or-later, Copyright (C) Jason Xu) and modified for
// this gateway; see internal/bunhttp/NOTICE and the repository LICENSE.

func openCodeV2UserAgent() string {
	if ua, err := clientVersions.userAgent("opencode"); err == nil && ua != "" {
		return ua
	}
	return bunhttp.OpenCodeV2UserAgent()
}

func openCodeV1ModelUserAgent(format clientFormat) string {
	provider := bunhttp.OpenCodeV1ChatProviderUtils
	switch format {
	case FormatOpenAIResponse:
		provider = bunhttp.OpenCodeV1ResponsesProviderUtils
	case FormatAnthropic:
		provider = bunhttp.OpenCodeV1AnthropicProviderUtils
	}
	return bunhttp.OpenCodeV1ModelUserAgent(provider)
}

// openCodeV1Lane reports whether req targets the v1 /zen endpoint (not
// /zen/go) that expects the OpenCode 1.x client shape.
func openCodeV1Lane(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	path := strings.ToLower(strings.TrimRight(req.URL.Path, "/"))
	if path == "/zen/go" || strings.HasPrefix(path, "/zen/go/") {
		return false
	}
	return path == "/zen" || strings.HasPrefix(path, "/zen/")
}

// openCodeFreeFingerprintRequest reports whether req belongs to the traffic
// that must use the Bun-compatible transport: a free-model request to the
// official /zen or /inference gates. Paid and /zen/go traffic stays on the
// regular client.
func openCodeFreeFingerprintRequest(up *Upstream, req *http.Request, model string) bool {
	if up == nil || !up.isOpenCode() || req == nil || req.URL == nil || !up.openCodeFreeEligible(model) {
		return false
	}
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), "opencode.ai") {
		return false
	}
	path := strings.ToLower(strings.TrimRight(req.URL.Path, "/"))
	if path == "/zen" || strings.HasPrefix(path, "/zen/") {
		return !(path == "/zen/go" || strings.HasPrefix(path, "/zen/go/"))
	}
	return path == "/inference" || strings.HasPrefix(path, "/inference/")
}

func (u *Upstream) openCodeFreeEligible(model string) bool {
	return u.openCodePublic() || u.openCodeFreeModel(model)
}

// newBunFingerprintTransport builds the Bun-compatible transport for the
// official free-model gates. A nil endpoint follows the deployment's
// HTTP(S)_PROXY / NO_PROXY settings; otherwise that proxy is used for every
// request, including loopback destinations.
func newBunFingerprintTransport(config relayNetworkConfig, endpoint *url.URL, tlsConfig *utls.Config) (*bunhttp.BunTransport, error) {
	options := bunhttp.BunOptions{
		Proxy:                 endpoint,
		TLSConfig:             tlsConfig,
		ConnectTimeout:        config.DialTimeout,
		ResponseHeaderTimeout: config.HeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   config.MaxIdleConnsPerHost,
	}
	if endpoint == nil {
		options.ProxyFunc = http.ProxyFromEnvironment
	}
	return bunhttp.NewBunTransport(options)
}

// bunTLSConfigFor carries the certificate policy of the relay client into the
// fingerprint transport. Protocol-level settings stay under the Bun
// ClientHello's control.
func bunTLSConfigFor(base *http.Client) *utls.Config {
	if base == nil {
		return nil
	}
	transport, _ := base.Transport.(*http.Transport)
	if transport == nil {
		transport, _ = http.DefaultTransport.(*http.Transport)
	}
	if transport == nil || transport.TLSClientConfig == nil {
		return nil
	}
	source := transport.TLSClientConfig
	return &utls.Config{
		RootCAs:            source.RootCAs,
		InsecureSkipVerify: source.InsecureSkipVerify,
	}
}

// bunHTTPHead exposes the Bun request layout for tests and diagnostics.
func bunHTTPHead(req *http.Request, body []byte) ([]byte, bool, error) {
	return bunhttp.RequestHead(req, body)
}
