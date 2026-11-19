package bunhttp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBunTransportReplayAfterDroppedReusedConnection(t *testing.T) {
	for _, tt := range []struct {
		name, method, body, key string
		wantAttempts            int32
	}{
		{name: "GET", method: http.MethodGet, wantAttempts: 2},
		{name: "HEAD", method: http.MethodHead, wantAttempts: 2},
		{name: "POST", method: http.MethodPost, body: "billable payload", wantAttempts: 1},
		{name: "POST with client key", method: http.MethodPost, body: "billable payload", key: "request-1", wantAttempts: 1},
		{name: "empty POST", method: http.MethodPost, wantAttempts: 1},
		{name: "PATCH", method: http.MethodPatch, body: "update", wantAttempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/warm" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil || string(body) != tt.body || req.Method != tt.method || req.Header.Get("Idempotency-Key") != tt.key {
					t.Errorf("received %s with body %q, key %q and read error %v", req.Method, body, req.Header.Get("Idempotency-Key"), err)
				}
				if attempts.Add(1) == 1 {
					// The full request arrived, but no response bytes make it back.
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Errorf("hijack reused connection: %v", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					conn.Close()
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			transport, err := NewBunTransport(BunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			warm, err := client.Get(server.URL + "/warm")
			if err != nil {
				t.Fatal(err)
			}
			warm.Body.Close()

			var reused []bool
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { reused = append(reused, info.Reused) },
			})
			req, err := http.NewRequestWithContext(ctx, tt.method, server.URL+"/request", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if tt.key != "" {
				req.Header.Set("Idempotency-Key", tt.key)
			}
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if len(reused) != int(tt.wantAttempts) || !reused[0] || (len(reused) > 1 && reused[1]) {
				t.Fatalf("unexpected connection trace: %v", reused)
			}
			if got := attempts.Load(); got != tt.wantAttempts {
				t.Fatalf("upstream handled %d requests, want %d", got, tt.wantAttempts)
			}
			if tt.wantAttempts == 1 {
				if err == nil {
					t.Fatal("expected the ambiguous connection failure to be returned")
				}
			} else if err != nil || resp.StatusCode != http.StatusNoContent {
				t.Fatalf("safe request was not recovered: response=%v error=%v", resp, err)
			}
		})
	}
}
