package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestOpenCodeFreeExitCooldownIsolation(t *testing.T) {
	modelsTestDB(t)
	proxy := testSavedProxy(t, "http://proxy.invalid:8080", "proxy-user", "proxy-secret")
	copyProxy := testSavedProxy(t, "http://proxy.invalid:8080", "proxy-user", "proxy-secret")
	up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/zen", APIKey: "zen-secret", ProxyID: proxy.ID}
	until := time.Now().Add(30 * time.Minute)
	limit := recordOpenCodeFreeExitBlock(up, until)
	if limit == nil || limit.statusCode() != http.StatusServiceUnavailable || limit.until.Before(until) {
		t.Fatalf("block did not honor Retry-After: %+v", limit)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer override-secret")
	rememberOpenCodeRateDetail(up, limit, []byte("abusive traffic: zen-secret override-secret"), headers)
	if strings.Contains(limit.Error(), "zen-secret") || strings.Contains(limit.Error(), "override-secret") {
		t.Fatalf("IP cooldown leaked a credential: %s", limit.Error())
	}
	for _, tc := range []struct {
		name, kind, base, tier, key, model string
		proxy                              int64
		blocked                            bool
	}{
		{"same channel", openCodeFreeType, "https://opencode.ai/zen", "", "zen-secret", "free-model", proxy.ID, true},
		{"other entry and key", openCodeFreeType, "https://opencode.ai/inference", "", "public", "free-model", proxy.ID, true},
		{"copied proxy", openCodeFreeType, "https://opencode.ai/inference/openai/v1", "", "another-key", "free-model", copyProxy.ID, true},
		{"direct exit", openCodeFreeType, "https://opencode.ai/zen", "", "public", "free-model", 0, false},
		{"other exit", openCodeFreeType, "https://opencode.ai/zen", "", "public", "free-model", proxy.ID + 100, false},
		{"legacy free model", "opencode", "https://opencode.ai/zen", "", "legacy-key", "model-free", proxy.ID, true},
		{"legacy paid model", "opencode", "https://opencode.ai/zen", "", "legacy-key", "paid-model", proxy.ID, false},
		{"Go lane", "opencode", "https://opencode.ai/zen/go", "go", "go-key", "model-free", proxy.ID, false},
		{"custom gateway", openCodeFreeType, "https://custom.invalid/zen", "", "public", "free-model", proxy.ID, false},
		{"other provider", "openai", "https://opencode.ai/zen", "", "public", "free-model", proxy.ID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := &Upstream{Type: tc.kind, BaseURL: tc.base, OpenCodeTier: tc.tier, APIKey: tc.key, ProxyID: tc.proxy}
			got := channel.openCodeRateLimit(tc.model)
			if (got != nil) != tc.blocked {
				t.Fatalf("cooldown = %+v, want blocked=%v", got, tc.blocked)
			}
			if got != nil && (got.statusCode() != 503 || got.detail != limit.detail) {
				t.Errorf("shared cooldown lost its status or redacted detail: %+v", got)
			}
		})
	}
	legacy := &Upstream{Type: "opencode", BaseURL: "https://opencode.ai/zen", APIKey: "legacy-key", ProxyID: proxy.ID, ModelMap: `{"alias":"model-free"}`}
	if legacy.availableForRelay("alias") {
		t.Error("session affinity ignored the mapped model's blocked exit")
	}
	markOpenCodeRateSuccess(up, time.Now().Add(-time.Minute))
	if up.openCodeRateLimit() == nil {
		t.Error("an in-flight success erased the exit's later block")
	}
	// A later credential success also leaves the separate exit block intact.
	markOpenCodeRateSuccess(up, time.Now())
	if up.openCodeRateLimit() == nil {
		t.Error("credential success erased a separate exit block")
	}
	if err := resetUpstreamCooldown(context.Background(), up.ID, up); err != nil {
		t.Fatal(err)
	}
	if up.openCodeRateLimit() != nil || legacy.openCodeRateLimit("model-free") != nil {
		t.Fatal("manual recovery did not clear the shared exit block")
	}
}

func TestOpenCodeFreeExitCooldownRoutingAndExpiry(t *testing.T) {
	modelsTestDB(t)
	blocked := &Upstream{Name: "blocked", Type: openCodeFreeType, BaseURL: "https://opencode.ai/zen", Models: "alias", APIKey: "public", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(blocked); err != nil {
		t.Fatal(err)
	}
	limit := recordOpenCodeFreeExitBlock(blocked, time.Time{})
	if limit == nil || time.Until(limit.until) < 14*time.Minute {
		t.Fatalf("exit block did not receive a meaningful cooldown: %+v", limit)
	}
	if selected, err := pickUpstream(FormatOpenAIChat, "alias", nil, false); selected != nil || !errors.As(err, &limit) {
		t.Fatalf("blocked-only pool did not report the cooldown: %+v %v", selected, err)
	}
	task := fixtureTask(t, FormatOpenAIChat, false)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info, wrote, err := doRelay(c, blocked, task)
	if wrote || info.Status != 503 || !errors.As(err, &limit) {
		t.Fatalf("blocked connection attempted relay: %+v %v %v", info, wrote, err)
	}
	finalizeCall(c, ApiKey{}, task, info, wrote, err, -1)
	if recorder.Code != 503 || recorder.Header().Get("Retry-After") == "" || !strings.Contains(recorder.Body.String(), "出口") {
		t.Fatalf("cooldown response was not actionable: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	backup := &Upstream{Name: "backup", Type: "openai", BaseURL: "https://backup.invalid", Models: "alias", Enabled: true, Weight: 1, Priority: 1}
	if err := dbInsertUpstream(backup); err != nil {
		t.Fatal(err)
	}
	if selected, err := pickUpstream(FormatOpenAIChat, "alias", nil, false); err != nil || selected.ID != backup.ID {
		t.Fatalf("unblocked backup was not selected: %+v %v", selected, err)
	}
	exitKey, _ := blocked.openCodeFreeExitKey()
	openCodeRates.Lock()
	state := openCodeRates.entries[exitKey]
	state.limit.until = time.Now().Add(-time.Second)
	openCodeRates.entries[exitKey] = state
	openCodeRates.Unlock()
	if selected, err := pickUpstream(FormatOpenAIChat, "alias", nil, false); err != nil || selected.ID != blocked.ID {
		t.Fatalf("expired block did not recover automatically: %+v %v", selected, err)
	}
}

func TestOpenCodeFreeAbuseRelayThroughSelectedProxy(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/inference/openai/v1/chat/completions" || r.Host != "opencode.ai" {
			t.Errorf("incorrect official free route: %s %s", r.Host, r.URL)
		}
		if r.Header.Get("Accept") != "*/*" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("incorrect Bun or proxy headers: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if !json.Valid(body) || !strings.HasPrefix(string(body), `{"model":`) || strings.Contains(string(body), `\u003c`) {
			t.Errorf("request lost the v2 JSON shape: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"type":"FreeTierError","message":"detected abusive traffic from this IP address"}}`)
	}))
	t.Cleanup(server.Close)
	originalRelay, originalDiagnostic := upstreamHTTPClient, modelTestClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	upstreamHTTPClient = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	modelTestClient = upstreamHTTPClient
	t.Cleanup(func() {
		upstreamHTTPClient, modelTestClient = originalRelay, originalDiagnostic
		transport.CloseIdleConnections()
	})
	observed := make(chan string, 8)
	endpoint := testSOCKSProxy(t, server.Listener.Addr().String(), observed)
	proxy := testSavedProxy(t, "socks5h://"+endpoint, "alice", "secret")
	up := &Upstream{Type: "opencode", BaseURL: "https://opencode.ai/inference", APIKey: "public", OpenAIProtocol: "chat", ProxyID: proxy.ID}
	if _, status, err := testUpstreamModel(context.Background(), up, "alias", "<hello>&", 64, false); err == nil || status != 403 {
		t.Fatalf("diagnostic did not surface the upstream block: %d %v", status, err)
	}
	if up.openCodeRateLimit() != nil {
		t.Fatal("diagnostic changed production cooldown")
	}
	relay := func() (relayInfo, bool, error) {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		body := []byte(`{"model":"alias","messages":[{"role":"user","content":"<hello>&"}]}`)
		cr, err := parseClientRequest(FormatOpenAIChat, body, "alias")
		if err != nil {
			t.Fatal(err)
		}
		task := &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "alias", body: body, canon: cr}
		return doRelay(c, up, task)
	}
	info, wrote, err := relay()
	var block *openCodeRateLimitError
	if wrote || info.Status != 403 || !errors.As(err, &block) || block.statusCode() != 503 {
		t.Fatalf("relay did not record the upstream IP block: %+v %v %v", info, wrote, err)
	}
	if calls.Load() != 2 || healthFails(up.ID) != 0 {
		t.Fatalf("abuse rejection was replayed or double-counted: calls=%d failures=%d", calls.Load(), healthFails(up.ID))
	}
	if info, wrote, err := relay(); wrote || info.Status != 503 || !errors.As(err, &block) || calls.Load() != 2 {
		t.Fatalf("cooling exit was contacted again: %+v %v %v calls=%d", info, wrote, err, calls.Load())
	}
	until := block.until
	if _, status, err := testUpstreamModel(context.Background(), up, "alias", "<hello>&", 64, false); err == nil || status != 403 || calls.Load() != 3 {
		t.Fatalf("explicit diagnostic did not bypass cooldown: %d %v calls=%d", status, err, calls.Load())
	}
	if limit := up.openCodeRateLimit(); limit == nil || !limit.until.Equal(until) {
		t.Fatalf("diagnostic extended or cleared the production block: %+v", limit)
	}
	select {
	case destination := <-observed:
		if destination != "opencode.ai:443" {
			t.Fatalf("SOCKS destination = %s", destination)
		}
	default:
		t.Fatal("free requests bypassed the selected proxy")
	}
}
