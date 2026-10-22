package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPoolRoutingAndAttemptAttribution(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(8 * time.Millisecond)
		if r.Header.Get("Authorization") == "Bearer fail-key" {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"PRIVATE-ERROR-TOKEN"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"ok","object":"chat.completion","model":"shared","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	defer provider.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	manager, err := NewManager(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if err := manager.SetRouting(ctx, PoolRouting{Strategy: "fill-first", SessionAffinity: false}); err != nil {
		t.Fatal(err)
	}
	// Use OAuth-shaped local credentials so settings exercise actual account
	// metadata rather than config-only API-key entries.
	for _, name := range []string{"a", "b"} {
		raw, _ := json.Marshal(map[string]any{"type": "kimi", "email": name + "@example.invalid",
			"access_token": name + "-key", "base_url": provider.URL + "/v1", "expired": "2099-01-01T00:00:00Z"})
		if err := manager.SeedCredentials(map[string]json.RawMessage{name + ".json": raw}); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, settings := range []PoolRouting{
		{Strategy: "weighted-round-robin", SessionAffinity: true},
		{Strategy: "round-robin", SessionAffinity: false},
	} {
		if err := manager.SetRouting(ctx, settings); err != nil {
			t.Fatal(err)
		}
		if saved, err := manager.Routing(); err != nil || saved != settings {
			t.Fatalf("hot routing did not persist: %+v %v", saved, err)
		}
	}
	if err := manager.PatchCredential(ctx, "a.json", map[string]any{"priority": 10, "weight": 3}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RefreshRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Runtime()
	sort.Slice(snapshot.Accounts, func(i, j int) bool { return snapshot.Accounts[i].Name < snapshot.Accounts[j].Name })
	if len(snapshot.Accounts) != 2 || snapshot.Accounts[0].Priority != 10 || snapshot.Accounts[0].Weight != 3 {
		t.Fatalf("hot account settings: %+v", snapshot)
	}
	model := snapshot.Accounts[0].Models[0].ID
	base, key := manager.Connection()
	trace := "0123456789abcdef0123456789abcdef"
	request := func() {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(TraceHeader, trace)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("request failed: %d %s", res.StatusCode, result)
		}
	}
	request()
	if err := manager.Client().Do(ctx, "POST", "/apishare/flush", nil, nil); err != nil {
		t.Fatal(err)
	}
	page, err := manager.Attempts(ctx, "", trace, 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].AuthID != snapshot.Accounts[0].ID ||
		page.Items[0].InputTokens != 7 || page.Items[0].OutputTokens != 3 || page.Items[0].LatencyMS < 8 {
		t.Fatalf("trace/account/usage attribution: %+v %v", page, err)
	}
	// Force the highest priority credential to fail, then verify the same
	// gateway trace contains its failure followed by the healthy sibling.
	if err := manager.PatchCredential(ctx, "a.json", map[string]any{"access_token": "fail-key"}); err != nil {
		t.Fatal(err)
	}
	trace = "1123456789abcdef0123456789abcdef"
	request()
	if err := manager.Client().Do(ctx, "POST", "/apishare/flush", nil, nil); err != nil {
		t.Fatal(err)
	}
	page, err = manager.Attempts(ctx, "", trace, 0, 50)
	raw, _ := json.Marshal(page)
	if err != nil || len(page.Items) != 2 || page.Items[0].Status != 200 || page.Items[1].Status != 503 ||
		page.Items[0].Attempt != 2 || page.Items[1].Attempt != 1 || strings.Contains(string(raw), "PRIVATE") {
		t.Fatalf("retry trace: %s %v", raw, err)
	}
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetRouting(ctx, PoolRouting{Strategy: "fill-first", SessionAffinity: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.PatchCredential(ctx, "b.json", map[string]any{"disabled": true, "priority": 20}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("stopped routing/settings prevented restart: %v", err)
	}
	page, err = manager.Attempts(ctx, "", trace, 0, 1)
	if err != nil || len(page.Items) != 1 || page.NextID == 0 {
		t.Fatalf("persisted pagination: %+v %v", page, err)
	}
	manager.Stop()
	if err := manager.PatchCredential(ctx, "a.json", map[string]any{"access_token": "a-key", "priority": 0}); err != nil {
		t.Fatal(err)
	}
	if err := manager.PatchCredential(ctx, "b.json", map[string]any{"disabled": false, "priority": 0}); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetRouting(ctx, PoolRouting{Strategy: "weighted-round-robin", SessionAffinity: false}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	trace = "2123456789abcdef0123456789abcdef"
	for i := 0; i < 12; i++ {
		request()
	}
	if err := manager.Client().Do(ctx, "POST", "/apishare/flush", nil, nil); err != nil {
		t.Fatal(err)
	}
	page, err = manager.Attempts(ctx, "", trace, 0, 50)
	counts := make(map[string]int)
	for _, item := range page.Items {
		counts[item.AuthID]++
	}
	if err != nil || len(page.Items) != 12 || counts[snapshot.Accounts[0].ID] != 9 || counts[snapshot.Accounts[1].ID] != 3 {
		t.Fatalf("actual weighted dispatch was not 3:1: %+v %v", counts, err)
	}
}

func TestPoolSIWCAndVertexProjection(t *testing.T) {
	now := time.Now()
	auth := &coreauth.Auth{ID: "codex.json", Provider: "codex", Metadata: map[string]any{
		"use_siwc": true, "access_token": "PRIVATE-OUTER",
		"siwc": map[string]any{"access_token": "PRIVATE-INNER", "expires_at": now.Add(time.Hour).Format(time.RFC3339)},
	}}
	if got := projectAccount(auth, nil, now); got.SIWCStatus != "valid" || got.SIWCExpiresAt == "" {
		t.Fatalf("bound: %+v", got)
	}
	if got := projectAccount(auth, nil, now.Add(2*time.Hour)); got.SIWCStatus != "invalid" {
		t.Fatalf("expired: %+v", got)
	}
	auth.Metadata["use_siwc"] = false
	raw, _ := json.Marshal(projectAccount(auth, nil, now))
	if strings.Contains(string(raw), "PRIVATE") || !strings.Contains(string(raw), `"siwc_status":"off"`) {
		t.Fatalf("unsafe/unbound: %s", raw)
	}
}
