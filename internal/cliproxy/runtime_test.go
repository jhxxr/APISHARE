package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPoolRuntimeAvailabilityAndSafeDetails(t *testing.T) {
	now := time.Now()
	models := []*sdk.ModelInfo{{ID: "a"}, {ID: "b"}}
	auth := &coreauth.Auth{ID: "one", FileName: "one.json", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "PRIVATE"}}
	if result := projectAccount(auth, models, now); !result.Ready || len(result.Models) != 2 {
		t.Fatalf("active account: %+v", result)
	}
	auth.Disabled = true
	if result := projectAccount(auth, models, now); result.Ready || result.Reason != "disabled" {
		t.Fatalf("disabled: %+v", result)
	}
	auth.Disabled = false
	if result := projectAccount(auth, nil, now); result.Ready || result.Reason != "no_models" {
		t.Fatalf("empty: %+v", result)
	}
	auth.ModelStates = map[string]*coreauth.ModelState{"a": {Unavailable: true, NextRetryAfter: now.Add(time.Minute)}}
	auth.ModelStates["a"].Quota = coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "90"}}
	partial := projectAccount(auth, models, now)
	if !partial.Ready || partial.Models[0].Ready || !partial.Models[1].Ready || len(partial.ModelQuotas) != 1 || len(partial.ModelQuotas[0].Metrics) != 1 {
		t.Fatalf("partial cooldown: %+v", partial)
	}
	auth.Quota = coreauth.QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Minute), Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "85", "X-Codex-Primary-Reset-At": "1893456000",
		"Authorization": "PRIVATE", "X-Codex-Plan-Type": "private-secret", "X-Codex-Credits-Balance": "25.5",
	}}
	cooling := projectAccount(auth, models, now)
	raw, _ := json.Marshal(cooling)
	if cooling.Ready || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "private-secret") || len(cooling.Quota) != 3 {
		t.Fatalf("unsafe/cooling details: %s", raw)
	}
	if result := projectAccount(auth, models, now.Add(2*time.Minute)); !result.Ready {
		t.Fatalf("expired cooldown did not recover: %+v", result)
	}
}

func TestPoolPinnedTestNeverUsesAnotherAccount(t *testing.T) {
	var fail atomic.Bool
	var mu sync.Mutex
	var used []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		mu.Lock()
		used = append(used, key)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() && key == "Bearer key-a" {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"PRIVATE-provider-body"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"ok","object":"chat.completion","model":"same-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	root := t.TempDir()
	manager, err := NewManager(root, port)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.ensureConfig(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	raw, _ := os.ReadFile(path)
	var document map[string]any
	json.Unmarshal(raw, &document)
	document["api-keys"] = map[string]any{"openai-compatibility": []any{map[string]any{
		"name": "local", "base-url": upstream.URL + "/v1", "keys": []any{map[string]any{"api-key": "key-a"}, map[string]any{"api-key": "key-b"}},
		"models": []any{map[string]any{"name": "same-model", "alias": "same-model"}},
	}}}
	raw, _ = json.Marshal(document)
	if err := writePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Runtime()
	if len(snapshot.Accounts) != 2 || !manager.Ready("same-model") || manager.Ready("missing-model") {
		t.Fatalf("runtime model availability: %+v", snapshot)
	}
	var a, b string
	for _, account := range snapshot.Accounts {
		var result AccountTest
		if err := manager.Client().Do(ctx, "POST", "/apishare/test", map[string]string{"id": account.ID, "model": "same-model"}, &result); err != nil || !result.OK || result.AccountID != account.ID {
			t.Fatalf("pinned test: %+v %v", result, err)
		}
		mu.Lock()
		last := used[len(used)-1]
		mu.Unlock()
		if last == "Bearer key-a" {
			a = account.ID
		} else if last == "Bearer key-b" {
			b = account.ID
		}
	}
	if a == "" || b == "" || a == b {
		t.Fatal("did not exercise both credentials")
	}
	mu.Lock()
	used = nil
	mu.Unlock()
	fail.Store(true)
	var result AccountTest
	if err := manager.Client().Do(ctx, "POST", "/apishare/test", map[string]string{"id": a, "model": "same-model"}, &result); err != nil || result.OK || result.Status != 503 {
		t.Fatalf("failed pinned test: %+v %v", result, err)
	}
	mu.Lock()
	for _, key := range used {
		if key != "Bearer key-a" {
			t.Fatal("test fell back to another account")
		}
	}
	mu.Unlock()
	if result.Reason != "upstream_unavailable" {
		t.Fatalf("unsafe error projection: %+v", result)
	}
	if err := manager.RefreshRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if !manager.Ready("same-model") {
		t.Fatal("one failed account removed a healthy sibling from routing")
	}
	for _, account := range manager.Runtime().Accounts {
		if account.ID == a && (account.Ready || len(account.Cooldowns) == 0) {
			t.Fatalf("failed credential still appeared ready: %+v", account)
		}
	}
	base, _ := manager.Connection()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+ManagementPath+"/apishare/runtime", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("runtime exposed without private authentication")
	}
}
