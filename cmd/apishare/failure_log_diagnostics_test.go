package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"apishare/internal/requestredact"
	"github.com/gin-gonic/gin"
)

func TestFailureRedactedSummary(t *testing.T) {
	const private = "private prompt with secret-key"
	for _, tc := range []struct {
		name string
		info relayInfo
		err  error
		want string
	}{
		{"canceled", relayInfo{Status: 200}, fmt.Errorf("%s: %w", private, context.Canceled), "client_disconnected: request canceled or client disconnected (upstream HTTP 200)"},
		{"downstream write", relayInfo{Status: 200}, fmt.Errorf("%w: %s", errDownstreamWrite, private), "client_disconnected: writing client response failed (upstream HTTP 200)"},
		{"first output", relayInfo{Status: 200}, errUpstreamFirstOutputTimeout, "upstream_timeout: upstream first-output timeout (upstream HTTP 200)"},
		{"idle", relayInfo{Status: 200}, errUpstreamIdleTimeout, "upstream_timeout: upstream response body idle timeout (upstream HTTP 200)"},
		{"deadline", relayInfo{}, context.DeadlineExceeded, "upstream_timeout: request deadline exceeded"},
		{"network timeout", relayInfo{UpstreamID: 1}, &net.DNSError{Name: private, Err: private, IsTimeout: true}, "upstream_timeout: network timeout"},
		{"DNS", relayInfo{UpstreamID: 1}, &net.DNSError{Name: private, Err: private}, "upstream_unreachable: DNS lookup failed"},
		{"refused", relayInfo{UpstreamID: 1}, &url.Error{Op: "Post", URL: "https://" + private, Err: syscall.ECONNREFUSED}, "upstream_unreachable: connection refused"},
		{"reset", relayInfo{Status: 200}, fmt.Errorf("%s: %w", private, syscall.ECONNRESET), "upstream_response_error: connection reset by peer (upstream HTTP 200)"},
		{"pipe", relayInfo{Status: 200}, syscall.EPIPE, "upstream_response_error: broken pipe (upstream HTTP 200)"},
		{"closed", relayInfo{Status: 200}, net.ErrClosed, "upstream_response_error: network connection closed (upstream HTTP 200)"},
		{"unexpected EOF", relayInfo{Status: 200}, io.ErrUnexpectedEOF, "upstream_response_error: upstream response ended unexpectedly (upstream HTTP 200)"},
		{"EOF", relayInfo{Status: 200}, io.EOF, "upstream_response_error: upstream response ended (upstream HTTP 200)"},
		{"HTTP error", relayInfo{Status: 503, failureHTTPStatus: 503}, errors.New(private), "upstream_http_error (upstream HTTP 503)"},
		{"actual HTTP status", relayInfo{Status: 502, failureHTTPStatus: 200}, errors.New(private), "upstream_http_error (upstream HTTP 200)"},
		{"conversion", relayInfo{}, &protocolConversionError{cause: errors.New(private)}, "protocol_conversion"},
		{"tool limit", relayInfo{}, &upstreamToolLimitError{Limit: 128, ClientCount: 130, WireCount: 130}, "upstream_tool_limit: " + (&upstreamToolLimitError{Limit: 128, ClientCount: 130, WireCount: 130}).Error()},
		{"rate limit", relayInfo{}, &openCodeRateLimitError{detail: private}, "upstream_rate_limit"},
		{"unknown", relayInfo{}, errors.New(private), "routing_failure"},
		{"nil", relayInfo{}, nil, "routing_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := failureRedactedSummary(tc.info, relayFailureKind(tc.info, tc.err), tc.err)
			if got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordRelayFailureRedactionKeepsDiagnostics(t *testing.T) {
	for _, fullContent := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			status  int
			err     error
			summary string
		}{
			{"canceled", 200, fmt.Errorf("private-error-detail: %w", context.Canceled), "client_disconnected: request canceled or client disconnected (upstream HTTP 200)"},
			{"HTTP error", 400, errors.New("private-error-detail"), "upstream_http_error (upstream HTTP 400)"},
		} {
			t.Run(fmt.Sprintf("%s/full=%v", tc.name, fullContent), func(t *testing.T) {
				store := testFailureLogs(t, 64<<10)
				store.fullContent = fullContent
				body := []byte(`{"model":"alias","messages":[{"role":"user","content":"private-request"}]}`)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
				c.Request.Header.Set("Authorization", "Bearer gateway-secret")
				c.Request.Header.Set("X-Custom", "private-header")
				task := &relayTask{model: "alias", requestRedaction: &requestRedactionSnapshot{Config: requestredact.Config{Mode: requestredact.ModeEnforce}}}
				task.initFailureLog(c, body)
				info := relayInfo{UpstreamID: 14, UpstreamName: "provider", UpstreamModel: "alias-free", Status: tc.status, failureHTTPStatus: tc.status,
					failureReply: failureReply{data: []byte(`{"error":"private-response provider-secret"}`)}}
				recordRelayFailure(c, task, info, http.StatusBadGateway, tc.err, "gateway-secret")
				entry, raw := readTestFailureLog(t, store)
				if entry.ContentOmittedReason != failureContentOmittedRedaction || entry.Upstream.Summary != tc.summary || entry.Upstream.Kind != relayFailureKind(info, tc.err) || entry.Upstream.Status != tc.status {
					t.Fatalf("diagnostics lost: %+v", entry)
				}
				if !entry.Request.Redacted || !entry.Upstream.Redacted || entry.Request.Body != nil || entry.Request.Raw != "" || entry.Request.Headers != nil || entry.Upstream.Body != "" {
					t.Fatalf("redaction not enforced: %+v", entry)
				}
				for _, secret := range []string{"private-request", "private-response", "private-header", "private-error-detail", "gateway-secret", "provider-secret"} {
					if strings.Contains(raw, secret) {
						t.Fatalf("diagnostic leaked %q", secret)
					}
				}
				items, total := store.list(0, 10)
				if total != 1 || len(items) != 1 || items[0].Summary != tc.summary {
					t.Fatalf("list lost diagnostics: %+v, total=%d", items, total)
				}
			})
		}
	}
}
