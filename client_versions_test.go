package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testVersionSources(base string) []clientVersionSource {
	return []clientVersionSource{
		{"codex", "Codex", "@openai/codex", base + "/codex"},
		{"claude_code", "Claude Code", "@anthropic-ai/claude-code", base + "/claude"},
	}
}

func useTestClientVersions(t *testing.T, service *clientVersionService) {
	t.Helper()
	previous := clientVersions
	clientVersions = service
	t.Cleanup(func() { clientVersions = previous })
}

func TestClientVersionSourceValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"stable", `{"name":"@openai/codex","version":"0.200.1"}`, 200, true},
		{"prerelease", `{"name":"@openai/codex","version":"0.200.1-alpha.1"}`, 200, false},
		{"wrong package", `{"name":"other","version":"1.0.0"}`, 200, false},
		{"missing version", `{"name":"@openai/codex"}`, 200, false},
		{"invalid version", `{"name":"@openai/codex","version":"private\r\nInjected: value"}`, 200, false},
		{"leading zero", `{"name":"@openai/codex","version":"01.2.3"}`, 200, false},
		{"invalid JSON", `{"name":"@openai/codex"`, 200, false},
		{"oversize", strings.Repeat("x", (128<<10)+1), 200, false},
		{"unavailable", `private response`, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("version check sent credentials")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			source := testVersionSources(server.URL)[0]
			s := newClientVersionService(server.Client(), []clientVersionSource{source}, nil)
			_, err := s.fetchVersion(context.Background(), source)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("error exposed a remote response")
			}
		})
	}
}

func TestClientVersionsCachePersistenceAndIndependentFailures(t *testing.T) {
	var requests atomic.Int64
	var mu sync.Mutex
	codexVersion, claudeVersion, failCodex := "0.200.1", "2.10.1", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/codex" {
			if failCodex {
				w.WriteHeader(503)
				return
			}
			fmt.Fprintf(w, `{"name":"@openai/codex","version":%q}`, codexVersion)
		} else {
			fmt.Fprintf(w, `{"name":"@anthropic-ai/claude-code","version":%q}`, claudeVersion)
		}
	}))
	defer server.Close()
	saved := ""
	s := newClientVersionService(server.Client(), testVersionSources(server.URL), func(raw string) error { saved = raw; return nil })
	if err := s.refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"codex": "codex_cli_rs/0.200.1", "claude_code": "claude-cli/2.10.1 (external, cli)"} {
		if got, err := s.userAgent(id); err != nil || got != want {
			t.Fatalf("UA %s: %q %v", id, got, err)
		}
	}
	if err := s.refresh(context.Background(), false); err != nil || requests.Load() != 2 {
		t.Fatal("fresh versions were not cached")
	}
	entries, _ := s.snapshot()
	before := entries[0].CheckedAt
	mu.Lock()
	failCodex, claudeVersion = true, "2.10.2"
	mu.Unlock()
	if err := s.refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.snapshot()
	if entries[0].Version != "0.200.1" || entries[0].Error == "" || !entries[0].CheckedAt.Equal(before) {
		t.Fatal("failed check discarded last known Codex version")
	}
	if entries[1].Version != "2.10.2" || entries[1].Error != "" {
		t.Fatal("one failed client blocked the other client")
	}
	if err := s.refresh(context.Background(), false); err != nil || requests.Load() != 4 {
		t.Fatal("failed check did not respect retry interval")
	}
	restored := newClientVersionService(server.Client(), testVersionSources(server.URL), nil)
	if err := restored.restore(saved); err != nil {
		t.Fatal(err)
	}
	if got, err := restored.userAgent("claude_code"); err != nil || got != "claude-cli/2.10.2 (external, cli)" {
		t.Fatal("cache did not survive restart")
	}
	mu.Lock()
	failCodex, codexVersion = false, "0.200.2"
	mu.Unlock()
	s.Lock()
	s.attempted = time.Now().Add(-clientVersionRetryInterval - time.Second)
	s.Unlock()
	if err := s.refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	entries, _ = s.snapshot()
	if entries[0].Version != "0.200.2" || entries[0].Error != "" {
		t.Fatal("version check did not recover after retry")
	}
}

func TestClientVersionsSharedRefreshSurvivesCancellation(t *testing.T) {
	var requests atomic.Int64
	started, release := make(chan struct{}, 2), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-release
		pkg := "@openai/codex"
		if r.URL.Path == "/claude" {
			pkg = "@anthropic-ai/claude-code"
		}
		fmt.Fprintf(w, `{"name":%q,"version":"1.2.3"}`, pkg)
	}))
	defer server.Close()
	s := newClientVersionService(server.Client(), testVersionSources(server.URL), nil)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- s.refresh(ctx, true) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("refresh did not start")
		}
	}
	cancel()
	if err := <-first; err != context.Canceled {
		close(release)
		t.Fatal("caller cancellation not honored")
	}
	second := make(chan error, 1)
	go func() { second <- s.refresh(context.Background(), false) }()
	close(release)
	if err := <-second; err != nil || requests.Load() != 2 {
		t.Fatal("shared check was canceled or duplicated")
	}
	if got, err := s.userAgent("codex"); err != nil || got != "codex_cli_rs/1.2.3" {
		t.Fatal("shared refresh did not complete")
	}
}

func TestClientVersionCachedDataValidation(t *testing.T) {
	s := newClientVersionService(http.DefaultClient, officialClientVersionSources, nil)
	raw, _ := json.Marshal([]clientVersion{
		{ID: "codex", Version: "0.200.1", CheckedAt: time.Now().Add(-time.Minute), UserAgent: "private injected UA", Source: "https://other.invalid"},
		{ID: "claude_code", Version: "1.0.0\r\nInjected: value", CheckedAt: time.Now()},
	})
	if err := s.restore(string(raw)); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.snapshot()
	if entries[0].UserAgent != "codex_cli_rs/0.200.1" || entries[0].Source != officialClientVersionSources[0].URL || entries[1].Version != "" {
		t.Fatal("untrusted cache fields accepted")
	}
	if _, err := s.userAgent("claude_code"); err == nil {
		t.Fatal("missing version generated a fabricated UA")
	}
}

func TestClientUserAgentTemplatesOnRelayAndProbes(t *testing.T) {
	modelsTestDB(t)
	s := newClientVersionService(http.DefaultClient, officialClientVersionSources, nil)
	raw, _ := json.Marshal([]clientVersion{{ID: "codex", Version: "0.200.1", CheckedAt: time.Now().Add(-time.Minute)}, {ID: "claude_code", Version: "2.10.1", CheckedAt: time.Now().Add(-time.Minute)}})
	if err := s.restore(string(raw)); err != nil {
		t.Fatal(err)
	}
	useTestClientVersions(t, s)
	for _, id := range []string{"codex", "claude_code"} {
		t.Run(id, func(t *testing.T) {
			want := clientUserAgent(id, s.entries[id].Version)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() != want || r.Header.Get("X-Keep") != "kept" {
					t.Errorf("template did not replace UA or lost other headers: %q", r.UserAgent())
				}
				if r.URL.Path == "/v1/models" {
					fmt.Fprint(w, `{"data":[{"id":"model"}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {}\n\n")
			}))
			defer server.Close()
			u := &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "key", HeaderOverrides: `{"*":true,"X-Keep":"kept","User-Agent":"{` + id + `_user_agent}"}`}
			for _, stream := range []bool{false, true} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "http://gateway.invalid", nil)
				c.Request.Header.Set("User-Agent", "old-client")
				if _, wrote, err := doRelay(c, u, &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "model", stream: stream, body: []byte(`{"model":"model"}`)}); err != nil || !wrote {
					t.Fatalf("relay %v", err)
				}
			}
			if _, err := probeUpstreamModelsWithProxy(context.Background(), u.Type, u.BaseURL, u.APIKey, 0, u.HeaderOverrides); err != nil {
				t.Fatal(err)
			}
			before := upstreamModelCacheKey(u)
			s.Lock()
			entry := s.entries[id]
			entry.Version = "3.0.0"
			entry.UserAgent = clientUserAgent(id, entry.Version)
			s.entries[id] = entry
			s.Unlock()
			if before == upstreamModelCacheKey(u) {
				t.Fatal("version change did not invalidate model catalog cache")
			}
			req := httptest.NewRequest("POST", "http://upstream.invalid", nil)
			if err := applyHeaderOverrides(req, u.HeaderOverrides, "key", nil); err != nil || req.UserAgent() != clientUserAgent(id, "3.0.0") {
				t.Fatal("saved template did not follow detected new version")
			}
		})
	}
	useTestClientVersions(t, newClientVersionService(http.DefaultClient, officialClientVersionSources, nil))
	req := httptest.NewRequest("POST", "http://upstream.invalid", nil)
	if err := applyHeaderOverrides(req, `{"User-Agent":"{codex_user_agent}"}`, "key", nil); err == nil {
		t.Fatal("undetected template sent an unresolved placeholder")
	}
}

func TestClientVersionsAdminRoutes(t *testing.T) {
	r := testProxyAdminRouter(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		pkg := "@openai/codex"
		if req.URL.Path == "/claude" {
			pkg = "@anthropic-ai/claude-code"
		}
		fmt.Fprintf(w, `{"name":%q,"version":"1.2.3"}`, pkg)
	}))
	defer server.Close()
	s := newClientVersionService(server.Client(), testVersionSources(server.URL), func(raw string) error { return settingsSet(clientVersionsSetting, raw) })
	useTestClientVersions(t, s)
	for _, tc := range []struct{ method, path string }{{"GET", "client-versions"}, {"POST", "client-versions/refresh"}} {
		if w := proxyAdminRequest(r, tc.method, tc.path, "", false); w.Code != 401 {
			t.Fatal("version route allowed anonymous access")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("anonymous request triggered registry access")
	}
	for _, tc := range []struct {
		method, path string
		count        int64
	}{{"GET", "client-versions", 2}, {"GET", "client-versions", 2}, {"POST", "client-versions/refresh", 4}} {
		w := proxyAdminRequest(r, tc.method, tc.path, "", true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "codex_cli_rs/1.2.3") || requests.Load() != tc.count {
			t.Fatalf("version route: %d %s requests=%d", w.Code, w.Body.String(), requests.Load())
		}
	}
	if raw, err := settingsGet(clientVersionsSetting); err != nil || !strings.Contains(raw, "1.2.3") {
		t.Fatal("latest versions were not saved to database")
	}
}
