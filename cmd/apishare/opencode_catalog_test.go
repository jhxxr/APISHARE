package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const openCodeCatalogFixture = `{
 "opencode":{"npm":"@ai-sdk/openai-compatible","models":{
  "chat-model":{},"shared-model":{"provider":{"npm":"@ai-sdk/openai"}},
  "blocked-model":{"provider":{"npm":"@ai-sdk/google"}},"docs-model":{}
 }},
 "opencode-go":{"npm":"@ai-sdk/openai-compatible","models":{
  "chat-model":{},"shared-model":{"provider":{"npm":"@ai-sdk/anthropic"}}
 }}
}`

func useOpenCodeCatalogSources(t *testing.T, base string) {
	t.Helper()
	previous := openCodeCatalogSources
	openCodeCatalogSources = [3]string{base + "/catalog", base + "/zen-docs", base + "/go-docs"}
	t.Cleanup(func() { openCodeCatalogSources = previous })
}

func TestOpenCodeAutomaticTierProtocols(t *testing.T) {
	modelsTestDB(t)
	var publicCalls atomic.Int32
	var relaySessions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" || strings.HasSuffix(r.URL.Path, "-docs") {
			publicCalls.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" || r.Header.Get("X-Channel-Secret") != "" {
				t.Errorf("public metadata received channel credentials: %v", r.Header)
			}
			if !strings.HasPrefix(r.UserAgent(), "opencode/") {
				t.Errorf("metadata UA: %q", r.UserAgent())
			}
			if r.URL.Path == "/catalog" {
				fmt.Fprint(w, openCodeCatalogFixture)
			}
			if r.URL.Path == "/zen-docs" {
				fmt.Fprint(w, "| Model | docs-model | `https://opencode.ai/inference/anthropic/v1/messages` | sdk |")
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1/models") {
			if r.URL.Path != "/inference/v1/models" && r.URL.Path != "/inference/go/v1/models" {
				t.Errorf("incorrect models path: %s", r.URL.Path)
			}
			fmt.Fprint(w, `{"data":[{"id":"shared-model"},{"id":"blocked-model"},{"id":"unknown-model"}]}`)
			return
		}
		format := FormatOpenAIResponse
		if r.URL.Path == "/inference/go/anthropic/v1/messages" {
			format = FormatAnthropic
		} else if r.URL.Path != "/inference/openai/v1/responses" {
			t.Errorf("incorrect inferred endpoint: %s", r.URL.Path)
		}
		if format == FormatAnthropic {
			if r.Header.Get("x-api-key") != "channel-key" || r.Header.Get("Authorization") != "" {
				t.Error("Messages used incorrect inference credentials")
			}
		} else if r.Header.Get("Authorization") != "Bearer channel-key" {
			t.Error("inference endpoint omitted bearer credentials")
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if json.Unmarshal(body, &request) != nil || request["model"] != "shared-model" {
			t.Errorf("mapped request: %s", body)
		}
		if format == FormatOpenAIResponse && request["input"] == nil || format == FormatAnthropic && request["messages"] == nil {
			t.Errorf("wrong request protocol: %s", body)
		}
		relaySessions = append(relaySessions, r.Header.Get("x-opencode-session"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fixtureSSE(format))
	}))
	defer server.Close()
	useOpenCodeCatalogSources(t, server.URL)
	task := fixtureTask(t, FormatOpenAIChat, false)
	task.apiKeyID = 1
	for _, tier := range []string{"zen", "go"} {
		up := &Upstream{Type: "opencode", OpenAIProtocol: "auto", OpenCodeTier: tier, BaseURL: server.URL + "/inference", APIKey: "channel-key", ModelMap: `{"alias":"shared-model"}`, HeaderOverrides: `{"X-Channel-Secret":"secret"}`}
		// The same task must retain its normalized session across tier failover.
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		ctx.Request.Header.Set("X-Session-Id", "same-conversation")
		info, wrote, err := doRelay(ctx, up, task)
		if err != nil || !wrote || info.Status != 200 {
			t.Fatalf("%s relay: %+v %v %v", tier, info, wrote, err)
		}
		if _, _, err := testUpstreamModel(context.Background(), up, "alias", "hi", 128, true); err != nil {
			t.Fatal(err)
		}
		ids, err := probeConfiguredModels(context.Background(), up)
		if err != nil || !reflect.DeepEqual(ids, []string{"shared-model"}) {
			t.Fatalf("unsupported models leaked: %v %v", ids, err)
		}
		if _, err := up.resolveWireFormat(context.Background(), FormatOpenAIChat, "blocked-model"); err == nil {
			t.Fatal("unsupported model silently fell back to Chat")
		}
		if _, err := up.resolveWireFormat(context.Background(), FormatOpenAIChat, "unknown-model"); err == nil {
			t.Fatal("unknown model silently fell back to Chat")
		}
		up.Models, up.Enabled, up.Weight = "alias,blocked-model,unknown-model", true, 1
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
		stored, err := dbGetUpstream(up.ID)
		if err != nil || stored.OpenCodeTier != tier {
			t.Fatalf("tier not persisted: %+v %v", stored, err)
		}
	}
	if publicCalls.Load() != 3 {
		t.Fatalf("metadata was not shared across tiers: %d", publicCalls.Load())
	}
	if len(relaySessions) != 4 || relaySessions[0] != relaySessions[2] {
		t.Fatalf("tier failover changed session: %v", relaySessions)
	}
	models, err := availableModels(context.Background())
	if err != nil || !reflect.DeepEqual(models, []catalogModel{{"alias", "opencode"}}) {
		t.Fatalf("explicit catalog exposed unsupported routes: %v %v", models, err)
	}
	up := &Upstream{Type: "opencode", OpenCodeTier: "zen"}
	if format, err := up.resolveWireFormat(context.Background(), FormatOpenAIChat, "docs-model"); err != nil || format != FormatAnthropic {
		t.Fatalf("docs did not override inherited SDK: %s %v", format, err)
	}
}

func TestOpenCodeCatalogCacheAndFallback(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/catalog" {
			fmt.Fprint(w, openCodeCatalogFixture)
		}
	}))
	defer server.Close()
	useOpenCodeCatalogSources(t, server.URL)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			catalog, err := getOpenCodeCatalog(context.Background(), nil)
			if err != nil || catalog.Tiers["go"]["shared-model"] != FormatAnthropic {
				t.Errorf("coalesced catalog: %+v %v", catalog, err)
			}
		})
	}
	workers.Wait()
	if calls.Load() != 3 {
		t.Fatalf("concurrent catalog fetches were duplicated: %d", calls.Load())
	}
	key := openCodeCatalogCacheKey(nil)
	openCodeCatalogs.Lock()
	entry := openCodeCatalogs.entries[key]
	entry.catalog.At = time.Now().Add(-time.Hour)
	entry.retryAt = time.Time{}
	openCodeCatalogs.Unlock()
	unavailable.Store(true)
	for range 2 {
		catalog, err := getOpenCodeCatalog(context.Background(), nil)
		if err != nil || catalog.Tiers["zen"]["shared-model"] != FormatOpenAIResponse {
			t.Fatalf("refresh discarded last good metadata: %+v %v", catalog, err)
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("failed refresh did not back off: %d", calls.Load())
	}
	// Simulate an empty memory cache after a restart. The saved public catalog
	// is usable even with every metadata source unavailable.
	openCodeCatalogs.Lock()
	delete(openCodeCatalogs.entries, key)
	openCodeCatalogs.Unlock()
	catalog, err := getOpenCodeCatalog(context.Background(), nil)
	if err != nil || catalog.Tiers["go"]["shared-model"] != FormatAnthropic || calls.Load() != 6 {
		t.Fatalf("persistent metadata cache was lost: %+v %d %v", catalog, calls.Load(), err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := getOpenCodeCatalog(cancelCtx, nil); err != context.Canceled {
		t.Fatalf("canceled discovery proceeded: %v", err)
	}
	up := &Upstream{Type: "opencode", OpenAIProtocol: "responses"}
	if format, err := up.resolveWireFormat(context.Background(), FormatAnthropic, "custom-model"); err != nil || format != FormatOpenAIResponse {
		t.Fatalf("manual override depended on catalog: %s %v", format, err)
	}
}

func TestOpenCodeEffectiveBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, kind, base, key, tier, want string
	}{
		{"free default", openCodeFreeType, "", "", "", openCodeFreeBase},
		{"free whitespace default", openCodeFreeType, "  ", "", "", openCodeFreeBase},
		{"free zen", openCodeFreeType, "https://opencode.ai/zen", "public", "", openCodeFreeBase},
		{"free zen v1", openCodeFreeType, "https://opencode.ai/zen/v1/", "custom-key", "zen", openCodeFreeBase},
		{"legacy public", "opencode", "https://opencode.ai/zen/", " public ", "", openCodeFreeBase},
		{"legacy public v1", "opencode", "https://opencode.ai/zen/v1", "public", "zen", openCodeFreeBase},
		{"case insensitive host", openCodeFreeType, "https://OPENCODE.AI/zen", "", "", "https://OPENCODE.AI/inference"},
		{"paid", "opencode", "https://opencode.ai/zen", "paid-key", "", "https://opencode.ai/zen"},
		{"missing legacy key", "opencode", "https://opencode.ai/zen", "", "", "https://opencode.ai/zen"},
		{"go tier", "opencode", "https://opencode.ai/zen", "public", "go", "https://opencode.ai/zen"},
		{"free invalid go tier", openCodeFreeType, "https://opencode.ai/zen", "public", "go", "https://opencode.ai/zen"},
		{"go path", "opencode", "https://opencode.ai/zen/go/v1", "public", "", "https://opencode.ai/zen/go/v1"},
		{"custom origin", openCodeFreeType, "https://custom.invalid/zen/v1", "", "", "https://custom.invalid/zen/v1"},
		{"custom official path", openCodeFreeType, "https://opencode.ai/gateway/zen", "", "", "https://opencode.ai/gateway/zen"},
		{"zen child path", openCodeFreeType, "https://opencode.ai/zen/anthropic", "", "", "https://opencode.ai/zen/anthropic"},
		{"http", openCodeFreeType, "http://opencode.ai/zen", "", "", "http://opencode.ai/zen"},
		{"explicit port", openCodeFreeType, "https://opencode.ai:443/zen", "", "", "https://opencode.ai:443/zen"},
		{"credentials", openCodeFreeType, "https://user@opencode.ai/zen", "", "", "https://user@opencode.ai/zen"},
		{"query", openCodeFreeType, "https://opencode.ai/zen?beta=true", "", "", "https://opencode.ai/zen?beta=true"},
		{"empty query", openCodeFreeType, "https://opencode.ai/zen?", "", "", "https://opencode.ai/zen?"},
		{"fragment", openCodeFreeType, "https://opencode.ai/zen#fragment", "", "", "https://opencode.ai/zen#fragment"},
		{"empty fragment", openCodeFreeType, "https://opencode.ai/zen#", "", "", "https://opencode.ai/zen#"},
		{"encoded path", openCodeFreeType, "https://opencode.ai/%7Aen", "", "", "https://opencode.ai/%7Aen"},
		{"invalid URL", openCodeFreeType, "https://opencode.ai/%zz", "", "", "https://opencode.ai/%zz"},
		{"other provider", "openai", "https://opencode.ai/zen", "public", "", "https://opencode.ai/zen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &Upstream{Type: tc.kind, BaseURL: tc.base, APIKey: tc.key, OpenCodeTier: tc.tier}
			before := *up
			if got := up.openCodeEffectiveBaseURL(); got != tc.want {
				t.Fatalf("effective base = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(*up, before) {
				t.Fatal("effective routing changed saved configuration")
			}
		})
	}
	up := &Upstream{Type: openCodeFreeType}
	if err := up.normalizeOpenCodeFree(); err != nil || up.BaseURL != openCodeFreeBase || up.APIKey != "public" || up.OpenCodeTier != "zen" {
		t.Fatalf("free-only default normalization: %+v %v", up, err)
	}
}

type openCodeRoutingTransport func(*http.Request) (*http.Response, error)

func (f openCodeRoutingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOpenCodeDiscoveryEffectiveRoutes(t *testing.T) {
	modelsTestDB(t)
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			fmt.Fprint(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{"model-free":{"cost":{"input":0,"output":0}}}},"opencode-go":{"npm":"@ai-sdk/openai-compatible","models":{"model-free":{}}}}`)
		}
	}))
	t.Cleanup(metadata.Close)
	useOpenCodeCatalogSources(t, metadata.URL)
	if _, err := getOpenCodeCatalog(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for index, tc := range []struct {
		name, kind, base, key, want string
	}{
		{"free default", openCodeFreeType, "", "", "https://opencode.ai/inference/v1/models"},
		{"free zen", openCodeFreeType, "https://opencode.ai/zen/v1", "", "https://opencode.ai/inference/v1/models"},
		{"free inference", openCodeFreeType, "https://opencode.ai/inference/anthropic/v1", "public", "https://opencode.ai/inference/v1/models"},
		{"legacy public zen", "opencode", "https://opencode.ai/zen", "public", "https://opencode.ai/inference/v1/models"},
		{"legacy paid zen", "opencode", "https://opencode.ai/zen/v1", "paid-key", "https://opencode.ai/zen/v1/models"},
		{"go inference", "opencode", "https://opencode.ai/inference/go/openai/v1", "go-key", "https://opencode.ai/inference/go/v1/models"},
		{"go zen", "opencode", "https://opencode.ai/zen/go/v1", "go-key", "https://opencode.ai/zen/go/v1/models"},
		{"custom free zen", openCodeFreeType, "https://custom.invalid/zen/v1", "public", "https://custom.invalid/zen/v1/models"},
		{"custom gateway", "opencode", "https://custom.invalid/gateway/v1", "paid-key", "https://custom.invalid/gateway/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: openCodeRoutingTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				key := tc.key
				if key == "" {
					key = "public"
				}
				if req.URL.String() != tc.want || req.Header.Get("Authorization") != "Bearer "+key || req.Header.Get("x-api-key") != "" {
					t.Errorf("discovery route/credential = %s %v, want %s", req.URL, req.Header, tc.want)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"model-free"}]}`)), Header: http.Header{}}, nil
			})}
			up := &Upstream{ID: int64(8000 + index), Type: tc.kind, BaseURL: tc.base, APIKey: tc.key}
			for _, mode := range []string{"saved", "unsaved", "cached"} {
				before := *up
				var ids []string
				var err error
				switch mode {
				case "saved":
					ids, err = probeConfiguredModelList(context.Background(), up, client, nil)
				case "unsaved":
					ids, err = probeUpstreamModelsWithClient(context.Background(), up.Type, up.BaseURL, up.APIKey, client)
				case "cached":
					previous := modelsFetchClient
					modelsFetchClient = client
					ids, err = cachedUpstreamModels(context.Background(), up)
					modelsFetchClient = previous
				}
				if err != nil || !reflect.DeepEqual(ids, []string{"model-free"}) {
					t.Fatalf("%s discovery = %v %v", mode, ids, err)
				}
				if !reflect.DeepEqual(*up, before) {
					t.Fatalf("%s discovery mutated configuration", mode)
				}
			}
			if calls != 3 {
				t.Fatalf("discovery calls = %d, want 3", calls)
			}
		})
	}
}

func TestOpenCodeEndpointCompatibility(t *testing.T) {
	for _, tc := range []struct{ base, tier, want, models string }{
		{"https://opencode.ai/inference", "go", "https://opencode.ai/inference/go/anthropic/v1/messages", "https://opencode.ai/inference/go/v1/models"},
		{"https://opencode.ai/inference/openai/v1", "zen", "https://opencode.ai/inference/anthropic/v1/messages", "https://opencode.ai/inference/v1/models"},
		{"https://opencode.ai/inference/go/v1", "", "https://opencode.ai/inference/go/anthropic/v1/messages", "https://opencode.ai/inference/go/v1/models"},
		{"https://opencode.ai/zen/go/v1", "", "https://opencode.ai/zen/go/v1/messages", "https://opencode.ai/zen/go/v1/models"},
		{"https://custom.invalid/gateway/v1", "go", "https://custom.invalid/gateway/v1/messages", "https://custom.invalid/gateway/v1/models"},
	} {
		up := &Upstream{Type: "opencode", BaseURL: tc.base, OpenCodeTier: tc.tier}
		if got := up.endpointURL(FormatAnthropic, "/v1/messages"); got != tc.want {
			t.Errorf("%s -> %s; want %s", tc.base, got, tc.want)
		}
		if got := up.endpointURL(FormatOpenAIChat, "/v1/models"); got != tc.models {
			t.Errorf("models %s -> %s; want %s", tc.base, got, tc.models)
		}
	}
	for sdk, want := range map[string]clientFormat{"@ai-sdk/anthropic": FormatAnthropic, "@ai-sdk/openai": FormatOpenAIResponse, "@ai-sdk/openai-compatible": FormatOpenAIChat, "@opencode/ai/providers/openai/responses": FormatOpenAIResponse, "@opencode/ai/providers/openai/chat": FormatOpenAIChat, "@ai-sdk/google": ""} {
		if got := openCodeSDKFormat(sdk); got != want {
			t.Errorf("SDK %s -> %s; want %s", sdk, got, want)
		}
	}
}

func TestOpenCodeDiscoveryUsesSelectedProxy(t *testing.T) {
	modelsTestDB(t)
	var publicCalls, modelCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "catalog.invalid" {
			publicCalls.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
				t.Error("metadata proxy request leaked a channel key")
			}
			if r.URL.Path == "/catalog" {
				fmt.Fprint(w, openCodeCatalogFixture)
			}
			return
		}
		if r.URL.Host != "channel.invalid" || r.URL.Path != "/inference/go/v1/models" || r.Header.Get("Authorization") != "Bearer channel-key" {
			t.Errorf("incorrect proxied discovery: %s %v", r.URL, r.Header)
		}
		modelCalls.Add(1)
		fmt.Fprint(w, `{"data":[{"id":"shared-model"},{"id":"blocked-model"}]}`)
	}))
	defer proxy.Close()
	useOpenCodeCatalogSources(t, "http://catalog.invalid")
	result, err := db.Exec(`INSERT INTO outbound_proxies(name,url,enabled) VALUES(?,?,1)`, "metadata proxy", proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	defer closeProxyTransport(id)
	up := &Upstream{Type: "opencode", BaseURL: "http://channel.invalid/inference", OpenCodeTier: "go", APIKey: "channel-key", ProxyID: id}
	models, err := probeConfiguredModels(context.Background(), up)
	if err != nil || !reflect.DeepEqual(models, []string{"shared-model"}) || publicCalls.Load() != 3 || modelCalls.Load() != 1 {
		t.Fatalf("selected proxy not used throughout: models=%v public=%d models=%d err=%v", models, publicCalls.Load(), modelCalls.Load(), err)
	}
	if _, err := db.Exec(`UPDATE outbound_proxies SET enabled=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := up.resolveWireFormat(context.Background(), FormatOpenAIChat, "shared-model"); err == nil {
		t.Fatal("disabled proxy bypassed by cached metadata")
	}
}

func TestOpenCodeTierAdminAndMigration(t *testing.T) {
	router := testProxyAdminRouter(t)
	created := proxyAdminRequest(router, "POST", "upstreams", `{"type":"opencode","openai_protocol":"auto","opencode_tier":"go","base_url":"https://opencode.ai/inference/go","api_key":"saved-key"}`, true)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(created.Body.Bytes(), &result)
	path := fmt.Sprintf("upstreams/%d", result.ID)
	if patched := proxyAdminRequest(router, "PATCH", path, `{"opencode_tier":"zen","openai_protocol":"anthropic"}`, true); patched.Code != 200 {
		t.Fatal(patched.Body.String())
	}
	if patched := proxyAdminRequest(router, "PATCH", path, `{"name":"keep tier"}`, true); patched.Code != 200 {
		t.Fatal(patched.Body.String())
	}
	stored, err := dbGetUpstream(result.ID)
	if err != nil || stored.OpenCodeTier != "zen" || stored.OpenAIProtocol != "anthropic" {
		t.Fatalf("patch lost saved configuration: %+v %v", stored, err)
	}
	if invalid := proxyAdminRequest(router, "PATCH", path, `{"opencode_tier":"unknown"}`, true); invalid.Code != 400 {
		t.Fatal("accepted invalid tier")
	}
	if invalid := proxyAdminRequest(router, "POST", "upstreams", `{"type":"opencode","opencode_tier":"unknown","base_url":"https://invalid.example","api_key":"key"}`, true); invalid.Code != 400 {
		t.Fatal("creation accepted invalid tier")
	}
	if _, err := db.Exec(`ALTER TABLE upstreams DROP COLUMN opencode_tier`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	stored, err = dbGetUpstream(result.ID)
	if err != nil || stored.OpenCodeTier != "" || stored.openCodeTier() != "go" || stored.APIKey != "saved-key" {
		t.Fatalf("migration lost legacy settings: %+v %v", stored, err)
	}
}
