package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestRequestClientResearchedSamples(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "client-user-agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		ID         string `json:"id"`
		UserAgents []struct {
			Examples []string `json:"examples"`
		} `json:"user_agents"`
	}
	var catalog struct {
		Clients []entry `json:"clients"`
		SDKs    []entry `json:"sdk_fallbacks"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, item := range catalog.Clients {
		for _, rule := range item.UserAgents {
			for _, sample := range rule.Examples {
				result := detectRequestClient(http.Header{"User-Agent": {sample}})
				if result.ID != item.ID || result.RuleVersion != requestClientRuleVersion {
					t.Errorf("%q: got %+v, want %s", sample, result, item.ID)
				}
				count++
			}
		}
	}
	for _, item := range catalog.SDKs {
		for _, rule := range item.UserAgents {
			for _, sample := range rule.Examples {
				result := detectRequestClient(http.Header{"User-Agent": {sample}})
				if result.ID != "unknown" || result.SDKID != item.ID || result.Source != "sdk_only" || result.Version != "" {
					t.Errorf("SDK must not imply an app: %q -> %+v", sample, result)
				}
				count++
			}
		}
	}
	if count != 27 {
		t.Fatalf("researched sample coverage changed: %d", count)
	}
}

func TestRequestClientEvidenceAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, ua, title, referer, originator, id, sdk, version string
		conflict, ambiguous                                    bool
	}{
		{name: "headers and SDK", ua: "OpenAI/JS 6.0.0", title: "Cherry Studio", referer: "https://cherry-ai.com/", id: "cherry_studio", sdk: "openai_js"},
		{name: "conflicting app headers", ua: "RooCode/1.2.3", title: "Cline", referer: "https://cline.bot", id: "roo_code", version: "1.2.3", conflict: true},
		{name: "host suffix attack", title: "Cline", referer: "https://cline.bot.attacker.example", id: "unknown"},
		{name: "userinfo attack", title: "Cline", referer: "https://cline.bot@attacker.example", id: "unknown"},
		{name: "title substring", title: "Unofficial Cline Wrapper", referer: "https://cline.bot", id: "unknown"},
		{name: "shared github host", title: "Roo Code", referer: "https://github.com/unrelated/project", id: "unknown"},
		{name: "roo path", title: "Roo Code", referer: "https://github.com/RooVetGit/Roo-Cline", id: "roo_code"},
		{name: "originator fallback", originator: "codex_vscode", id: "codex"},
		{name: "originator conflict", ua: "QwenCode/3.2.1 (linux; x64)", originator: "codex_cli_rs", id: "qwen_code", version: "3.2.1", conflict: true},
		{name: "claude self-declared identity", ua: "claude-cli/2.1.20 (external, cli)", id: "claude_code", version: "2.1.20", ambiguous: true},
		{name: "other vscode proxy", ua: "CloudCodeVSCode/1.2.3 (proxy_client=another)", id: "unknown"},
		{name: "unknown browser", ua: "Mozilla/5.0 AppleWebKit/537.36", id: "unknown"},
		{name: "missing headers", id: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("User-Agent", tc.ua)
			header.Set("X-Title", tc.title)
			header.Set("HTTP-Referer", tc.referer)
			header.Set("originator", tc.originator)
			header.Set("x-app", "cli")
			header.Set("x-opencode-session", "not-an-app-identity")
			result := detectRequestClient(header)
			if result.ID != tc.id || result.SDKID != tc.sdk || result.Version != tc.version || result.Conflict != tc.conflict || result.Ambiguous != tc.ambiguous {
				t.Fatalf("identity = %+v", result)
			}
			if (result.Conflict || result.Ambiguous) && len(result.Candidates) < 2 {
				t.Fatal("ambiguity evidence was discarded")
			}
		})
	}
	result := detectRequestClient(http.Header{"User-Agent": {"QwenCode/1.2.3 " + strings.Repeat("测", 1500) + "\r\n\x00"}})
	if len(result.UserAgent) > maxRecordedUserAgent || !utf8.ValidString(result.UserAgent) || strings.ContainsAny(result.UserAgent, "\r\n\x00") {
		t.Fatalf("unbounded or invalid recorded UA: %d bytes", len(result.UserAgent))
	}
}

func TestRequestClientProductionFailoverAndEarlyErrors(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "client-member", Key: "sk-client-identity", Models: "alias,unavailable", Enabled: true, QPS: 1000000, Concurrency: 10}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Retry = 1
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	var attempts atomic.Int32
	for index := range 2 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			if r.Header.Get("User-Agent") != "RooCode/9.9.9" {
				t.Error("upstream override was not exercised")
			}
			w.Header().Set("Content-Type", "application/json")
			if index == 0 {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":{"message":"retry"}}`)
			} else {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}
		}))
		t.Cleanup(server.Close)
		up := &Upstream{Name: fmt.Sprint("client-up-", index), Type: "openai", OpenAIProtocol: "chat", BaseURL: server.URL,
			APIKey: "upstream-test", Models: "alias", Enabled: true, Weight: 1, Priority: index, HeaderOverrides: `{"User-Agent":"RooCode/9.9.9"}`}
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	router := newRouter()
	send := func(body, ua, auth string) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	ua := "codex_cli_rs/1.2.3 (Linux 6.1; x86_64) terminal/1.0"
	if status := send(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`, ua, key.Key); status != 200 {
		t.Fatalf("failover request = %d", status)
	}
	logs, err := dbListLogs(20, 0)
	if err != nil || len(logs) != 1 || attempts.Load() != 2 || logs[0].Client.ID != "codex" || logs[0].Client.UserAgent != ua {
		t.Fatalf("one inbound request became incorrect receipts: %+v / %v / attempts=%d", logs, err, attempts.Load())
	}
	if status := send(`{"model":"blocked","messages":[]}`, "QwenCode/3.2.1 (linux; x64)", key.Key); status != 403 {
		t.Fatalf("permission status = %d", status)
	}
	if status := send(`{`, "Cline/1.2.3", key.Key); status != 400 {
		t.Fatalf("invalid body status = %d", status)
	}
	if status := send(`{"model":"unavailable","messages":[]}`, "opencode/1.19.3", key.Key); status != 502 {
		t.Fatalf("no upstream status = %d", status)
	}
	limit := getLimit(*key)
	for range key.Concurrency {
		limit.sem <- struct{}{}
	}
	status := send(`{"model":"alias","messages":[]}`, "KimiCLI/1.2.3", key.Key)
	for range key.Concurrency {
		<-limit.sem
	}
	if status != 429 {
		t.Fatalf("concurrency limit status = %d", status)
	}
	if status := send(`{}`, ua, ""); status != 401 {
		t.Fatalf("unauthenticated status = %d", status)
	}
	logs, err = dbListLogs(20, 0)
	if err != nil || len(logs) != 5 {
		t.Fatalf("early errors duplicated or unauthenticated request recorded: %+v / %v", logs, err)
	}
	want := map[int]string{200: "codex", 403: "qwen_code", 400: "cline", 502: "opencode", 429: "kimi_cli"}
	for _, entry := range logs {
		if entry.Client.ID != want[entry.Status] || entry.Client.RuleVersion != requestClientRuleVersion {
			t.Fatalf("incorrect error identity: %+v", entry)
		}
	}
}

func TestRequestClientAllModelProtocols(t *testing.T) {
	for _, format := range protocolFormats {
		t.Run(string(format), func(t *testing.T) {
			modelsTestDB(t)
			key := &ApiKey{Name: "protocol-client", Key: "sk-client-protocol", Models: "alias", Enabled: true, QPS: 1000000, Concurrency: 10}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, protocolResponses[format])
			}))
			defer server.Close()
			up := fixtureUpstream(format, server.URL)
			up.Name, up.Models, up.Enabled, up.Weight = "client-protocol", "alias", true, 1
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			path := nativeEndpoint(format, &canonReq{Model: "alias"})
			req := httptest.NewRequest("POST", path, strings.NewReader(protocolRequests[format]))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("User-Agent", "opencode/latest/2.0.0/cli")
			w := httptest.NewRecorder()
			newRouter().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].Client.ID != "opencode" || logs[0].Client.Version != "2.0.0" {
				t.Fatalf("protocol client not retained: %+v / %v", logs, err)
			}
		})
	}
}
