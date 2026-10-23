package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderQuotaWindowsAndPrivacy(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		provider, payload string
		period            int64
		left              float64
	}{
		{"codex", `{"api_key":"PRIVATE","rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":3600},"secondary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_after_seconds":1800}}}`, 18000, 75},
		{"claude", `{"access_token":"PRIVATE","five_hour":{"utilization":42.5,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":16,"resets_at":"2099-01-07T00:00:00Z"},"seven_day_sonnet":{"utilization":10,"resets_at":"2099-01-07T00:00:00Z"}}`, 18000, 57.5},
		{"kimi", `{"usage":{"used":3,"limit":10,"reset_in":3600},"limits":[{"window":{"duration":5,"timeUnit":"HOURS"},"detail":{"used":20,"limit":100,"reset_in":3600}}],"usages":{"limit_month_total":{"used_ratio":0.25,"reset_time":"2099-02-01T00:00:00Z"}}}`, 18000, 80},
		{"antigravity", `{"groups":[{"displayName":"Claude models","buckets":[{"window":"5h","remainingFraction":0.8,"resetTime":"2099-01-01T00:00:00Z"},{"window":"weekly","remainingFraction":0.4,"resetTime":"2099-01-07T00:00:00Z"}]}]}`, 18000, 80},
		{"devin", `{"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":20,"dailyQuotaResetAtUnix":"1893456000","weeklyQuotaRemainingPercent":65,"weeklyQuotaResetAtUnix":1894060800,"planInfo":{"planName":"Pro"}}}}`, 604800, 65},
		{"meta", `{"api_key":"PRIVATE","email":"PRIVATE","subs_tier_name":"Pro","subs_usage":{"window":{"used_percent":35,"window_duration_mins":300,"resets_at":1893456000},"weekly":{"used_percent":60,"resets_at":1894060800}}}`, 18000, 65},
		{"xai", `{"config":{"currentPeriod":{"type":"weekly","start":"2099-01-01T00:00:00Z","end":"2099-01-08T00:00:00Z"},"creditUsagePercent":25,"monthlyLimit":{"val":1000},"used":{"val":250},"billingPeriodEnd":"2099-02-01T00:00:00Z"}}`, 604800, 75},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			var payload map[string]any
			if json.Unmarshal([]byte(test.payload), &payload) != nil {
				t.Fatal("fixture JSON")
			}
			view := parseProviderQuota(test.provider, payload, now)
			found := false
			for _, window := range view.Windows {
				if window.PeriodSeconds == test.period && window.RemainingPercent != nil && math.Abs(*window.RemainingPercent-test.left) < 1e-6 {
					found = true
				}
			}
			raw, err := json.Marshal(view)
			if !found || err != nil || strings.Contains(string(raw), "PRIVATE") {
				t.Fatalf("unsafe or incorrect quota: %s %v", raw, err)
			}
		})
	}
	for _, number := range []any{nil, "", "NaN", "Inf", -5.0, true, map[string]any{"api_key": "PRIVATE"}} {
		if quotaNumber(number) != nil {
			t.Fatalf("invalid number accepted: %v", number)
		}
	}
	var payload map[string]any
	json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_after_seconds":600}}}`), &payload)
	unknown := parseProviderQuota("codex", payload, now)
	if len(unknown.Windows) != 1 || unknown.Windows[0].RemainingPercent != nil || unknown.Windows[0].Label != "每周额度" {
		t.Fatal("unknown quota became full or weekly primary was mislabeled")
	}
	withoutReset := parseProviderQuota("codex", map[string]any{"rate_limit": map[string]any{"allowed": false, "primary_window": map[string]any{"limit_window_seconds": 18000.0}}}, now)
	if len(withoutReset.Windows) > 0 && withoutReset.Windows[0].RemainingPercent != nil {
		t.Fatal("a denied window without usage/reset data became zero remaining")
	}
	json.Unmarshal([]byte(`{"rate_limit":{"secondary_window":{"limit_window_seconds":2592000,"used_percent":80}}}`), &payload)
	monthly := parseProviderQuota("codex", payload, now)
	if monthly.Windows[len(monthly.Windows)-1].Label != "每月额度" {
		t.Fatal("monthly quota was mislabeled as weekly")
	}
	payload = map[string]any{}
	json.Unmarshal([]byte(`{"iguana_necktie":{"utilization":25,"resets_at":"2099-01-07T00:00:00Z"},"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1250,"utilization":25,"currency":"USD"}}`), &payload)
	claude := parseProviderQuota("claude", payload, now)
	if len(claude.Windows) != 2 || claude.Windows[0].Label != "Fable · 每周" || claude.Windows[1].Unit != "USD" || claude.Windows[1].Remaining == nil || *claude.Windows[1].Remaining != 37.5 {
		t.Fatalf("Claude scoped window or currency units: %+v", claude)
	}
}

func TestQuotaRequestPlansAreFixedAndRespectKimiSite(t *testing.T) {
	for _, test := range []struct {
		provider string
		metadata map[string]any
		host     string
	}{
		{"codex", map[string]any{"account_id": "acct"}, "chatgpt.com"},
		{"claude", nil, "api.anthropic.com"},
		{"kimi", map[string]any{"domain": "ai", "base_url": "https://evil.invalid"}, "api.kimi.ai"},
		{"kimi-ai", map[string]any{"domain": "com", "base_url": "https://api.kimi.ai"}, "api.kimi.com"},
		{"kimi", map[string]any{"base_url": "https://api.kimi.ai"}, "api.kimi.ai"},
		{"antigravity", map[string]any{"project_id": "project"}, "daily-cloudcode-pa.googleapis.com"},
		{"meta", map[string]any{"dca_token": "dca:" + strings.Repeat("x", 200)}, "api.meta.ai"},
		{"devin", nil, "server.codeium.com"},
		{"xai", nil, "cli-chat-proxy.grok.com"},
	} {
		requests, err := quotaRequests(test.provider, test.metadata)
		if err != nil || len(requests) == 0 || !strings.Contains(requests[0].url, test.host) || strings.Contains(requests[0].url, "evil.invalid") {
			t.Fatalf("request plan %s: %+v %v", test.provider, requests, err)
		}
		for _, request := range requests {
			if strings.Contains(request.url, "chat/completions") || request.method == "DELETE" {
				t.Fatal("quota query must not run inference or destructive actions")
			}
		}
	}
}

func TestQuotaQueryCacheSingleFlightAndAccountReplacement(t *testing.T) {
	var calls atomic.Int64
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			fmt.Fprint(w, `{"files":[{"name":"one.json","auth_index":"one-index"}]}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/requests/api-call") {
			t.Errorf("unexpected private route %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var request struct {
			Index  string            `json:"auth_index"`
			URL    string            `json:"url"`
			Header map[string]string `json:"header"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if request.Index != "one-index" || request.URL != "https://chatgpt.com/backend-api/wham/usage" || request.Header["Authorization"] != "Bearer $TOKEN$" {
			t.Error("wrong credential or token escaped the private management boundary")
		}
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		status, body := 200, `{"rate_limit":{"primary_window":{"used_percent":30,"limit_window_seconds":18000}},"api_key":"PRIVATE"}`
		if fail.Load() {
			status = 401
			body = `{"error":"PRIVATE"}`
		}
		json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": body})
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	manager, err := NewManager(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SeedCredentials(map[string]json.RawMessage{"one.json": json.RawMessage(`{"type":"codex","email":"one@example.invalid","access_token":"PRIVATE"}`)}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.cancel = func() {}
	manager.mu.Unlock()
	defer func() { manager.mu.Lock(); manager.cancel = nil; manager.mu.Unlock() }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := manager.QueryQuota(context.Background(), "one.json", false)
			if err != nil || len(view.Windows) != 1 {
				t.Errorf("query: %+v %v", view, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("same-account queries were not coalesced: %d", calls.Load())
	}
	manager.quotaMu.Lock()
	entry := manager.quotaCache["one.json"]
	entry.at = time.Now().Add(-2 * time.Minute)
	manager.quotaCache["one.json"] = entry
	manager.quotaMu.Unlock()
	fail.Store(true)
	view, err := manager.QueryQuota(context.Background(), "one.json", true)
	raw, _ := json.Marshal(view)
	if err != nil || !view.Stale || view.Error != "unauthorized" || len(view.Windows) != 1 || strings.Contains(string(raw), "PRIVATE") {
		t.Fatalf("failed refresh lost safe cached data: %s %v", raw, err)
	}
	if err := manager.SeedCredentials(map[string]json.RawMessage{"one.json": json.RawMessage(`{"type":"codex","email":"another@example.invalid","access_token":"PRIVATE-NEW"}`)}); err != nil {
		t.Fatal(err)
	}
	if manager.CachedQuota("one.json") != nil {
		t.Fatal("a new account inherited the previous account's quota")
	}
}

func TestQuotaUsesNativeSDKAccountProxy(t *testing.T) {
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "quota-upstream.invalid" || r.Header.Get("Authorization") != "Bearer private-local" {
			t.Error("quota request lost its account or proxy")
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":70,"limit_window_seconds":604800}},"api_key":"PRIVATE"}`)
	}))
	defer proxy.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	manager, err := NewManager(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	raw, _ := json.Marshal(map[string]any{"type": "codex", "email": "local@example.invalid", "access_token": "private-local", "expired": "2099-01-01T00:00:00Z", "proxy_url": proxy.URL})
	if err := manager.SeedCredentials(map[string]json.RawMessage{"local.json": raw}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := manager.fetchQuota(ctx, "local.json", []quotaRequest{{method: "GET", url: "http://quota-upstream.invalid/status", header: map[string]string{"Authorization": "Bearer $TOKEN$"}}})
	result, _ := json.Marshal(view)
	if err != nil || calls.Load() != 1 || len(view.Windows) != 2 || strings.Contains(string(result), "PRIVATE") || strings.Contains(string(result), "private-local") {
		t.Fatalf("native quota egress: %s %v", result, err)
	}
}
