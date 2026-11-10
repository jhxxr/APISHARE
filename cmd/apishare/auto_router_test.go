package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupAutoRouterTestDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := openDB(filepath.Join(t.TempDir(), "auto_router_test.db")); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})
	if err := loadSettingsCache(); err != nil {
		t.Fatalf("loadSettingsCache: %v", err)
	}
	if err := loadAutoRouterCache(); err != nil {
		t.Fatalf("loadAutoRouterCache: %v", err)
	}
	if err := bootstrapAdmin(); err != nil {
		t.Fatalf("bootstrapAdmin: %v", err)
	}
	resetAutoRouterBreaker()
	probedModelLimits.Lock()
	probedModelLimits.data = map[string]ModelTokenLimits{}
	probedModelLimits.Unlock()
	learnedModelLimits.Lock()
	learnedModelLimits.data = map[string]ModelTokenLimits{}
	learnedModelLimits.Unlock()
}

func TestAutoRouterHeuristicAndShortCircuit(t *testing.T) {
	setupAutoRouterTestDB(t)

	// 1. Short simple question -> short-circuit SIMPLE
	crSimple := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "把 hello world 翻译成中文"},
		},
	}
	lastSimple, _ := extractClassifierState(crSimple, 2, 6000)
	if !shouldShortCircuitSimple(lastSimple, 60) {
		t.Fatalf("expected short prompt to short-circuit to SIMPLE")
	}

	// 2. Code snippet with fence -> at least MEDIUM, not short-circuited
	crCode := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "帮我看看这段代码：\n```go\nfunc add(a, b int) int { return a + b }\n```"},
		},
	}
	lastCode, stateCode := extractClassifierState(crCode, 2, 6000)
	if shouldShortCircuitSimple(lastCode, 60) {
		t.Fatalf("expected fenced code prompt NOT to short-circuit to SIMPLE")
	}
	tierCode, _ := classifyHeuristic(lastCode, stateCode, crCode)
	if tierCode == TierSimple {
		t.Fatalf("expected code prompt heuristic tier >= MEDIUM, got %s", tierCode)
	}

	// 3. Multi-file architecture refactor -> COMPLEX
	crComplex := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "请帮我重构一个跨多文件的微服务架构，排查 deadlock 并发死锁与 race condition 根因，并给出完整工程迁移方案。\n```go\npackage main\n```"},
		},
	}
	lastComplex, stateComplex := extractClassifierState(crComplex, 2, 6000)
	tierComplex, _ := classifyHeuristic(lastComplex, stateComplex, crComplex)
	if tierComplex != TierComplex && tierComplex != TierReasoning {
		t.Fatalf("expected COMPLEX or REASONING for multi-file refactor, got %s", tierComplex)
	}

	// 4. Mathematical proof / derivation -> REASONING
	crReason := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "请用数学归纳法严格证明该定理（theorem proof step by step derivation），并分析动态规划状态转移方程的时间复杂度下界。"},
		},
	}
	lastReason, stateReason := extractClassifierState(crReason, 2, 6000)
	tierReason, _ := classifyHeuristic(lastReason, stateReason, crReason)
	if tierReason != TierReasoning {
		t.Fatalf("expected REASONING for mathematical proof, got %s", tierReason)
	}

	// 5. Housekeeping detection
	crHouse := &canonReq{
		Model:  "auto",
		System: "Generate a concise 3-5 word title for this conversation.",
		Messages: []canonMsg{
			{Role: "user", Text: "Here is a long conversation transcript..."},
		},
	}
	lastHouse, _ := extractClassifierState(crHouse, 2, 6000)
	if !isHousekeepingRequest(crHouse, lastHouse) {
		t.Fatalf("expected title generation prompt to be detected as housekeeping")
	}
}

func TestAutoRouterMultiClassifierProtocolsAndLowConfidence(t *testing.T) {
	setupAutoRouterTestDB(t)

	var receivedBody map[string]any
	clefServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &receivedBody)
		w.Header().Set("Content-Type", "application/json")
		// Return Cloudflare Workers AI / SystemOne wrapped response with probabilities
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"answers": map[string]any{
					"tier": map[string]any{
						"choice": "COMPLEX",
						"probabilities": map[string]float64{
							"SIMPLE":    0.05,
							"MEDIUM":    0.12,
							"COMPLEX":   0.76,
							"REASONING": 0.07,
						},
					},
				},
			},
		})
	}))
	defer clefServer.Close()

	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "systemone"
	cfg.ClassifierModel = "clef-flash"
	cfg.EndpointURL = clefServer.URL
	cfg.MinConfidence = 0.55

	res, err := classifyWithDecisionModel(context.Background(), cfg, "请写一段多文件架构分析与调试方案", true)
	if err != nil {
		t.Fatalf("classifyWithDecisionModel error: %v", err)
	}
	// Highest tier >= 0.55 is COMPLEX (0.76 >= 0.55, whereas REASONING is 0.12 < 0.55)
	if res.Tier != TierComplex {
		t.Fatalf("expected TierComplex from probability vector, got %s (probs=%v)", res.Tier, res.Probabilities)
	}
	if receivedBody["state"] == nil || receivedBody["questions"] == nil {
		t.Fatalf("expected SystemOne/Clef format with state and questions, got %v", receivedBody)
	}

	// Test low-confidence upgrade: when all probabilities are < 0.55 and argmax is SIMPLE
	lowConfResp := []byte(`{"probabilities":{"SIMPLE":0.42,"MEDIUM":0.38,"COMPLEX":0.30,"REASONING":0.10}}`)
	parsedLow, err := parseDecisionClassifierResponse(lowConfResp, 0.55)
	if err != nil {
		t.Fatalf("parseDecisionClassifierResponse failed: %v", err)
	}
	if parsedLow.Tier != TierComplex || !parsedLow.UpgradedLowConfidence {
		t.Fatalf("expected low-confidence fast tier to upgrade to COMPLEX, got tier=%s upgraded=%v", parsedLow.Tier, parsedLow.UpgradedLowConfidence)
	}
}

func TestAutoRouterPriorityCandidatesSkipQuotaAndFailover(t *testing.T) {
	setupAutoRouterTestDB(t)

	var dsFlashCalls, glmFlashCalls, grokCalls int32

	// Upstream 1: serves deepseek-v4.1-flash and glm-5.3-flash, but deepseek-v4.1-flash returns 502!
	fastUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		model, _ := payload["model"].(string)
		switch model {
		case "deepseek-v4.1-flash":
			atomic.AddInt32(&dsFlashCalls, 1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream 502"}}`))
		case "glm-5.3-flash":
			atomic.AddInt32(&glmFlashCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-glm","object":"chat.completion","model":"glm-5.3-flash","choices":[{"index":0,"message":{"role":"assistant","content":"reply from glm-5.3-flash"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer fastUpstream.Close()

	// Upstream 2: serves grok-4.7
	strongUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&grokCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-grok","object":"chat.completion","model":"grok-4.7","choices":[{"index":0,"message":{"role":"assistant","content":"reply from grok-4.7"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":10,"total_tokens":25}}`))
	}))
	defer strongUpstream.Close()

	if err := dbInsertUpstream(&Upstream{
		Name:           "fast-pool",
		Type:           "openai",
		OpenAIProtocol: "chat",
		BaseURL:        fastUpstream.URL,
		APIKey:         "sk-fast",
		Models:         "deepseek-v4.1-flash,glm-5.3-flash,mimo-v2.6-flash",
		Priority:       0,
		Weight:         1,
		Enabled:        true,
	}); err != nil {
		t.Fatalf("dbInsertUpstream fast: %v", err)
	}
	if err := dbInsertUpstream(&Upstream{
		Name:           "strong-pool",
		Type:           "openai",
		OpenAIProtocol: "chat",
		BaseURL:        strongUpstream.URL,
		APIKey:         "sk-strong",
		Models:         "grok-4.7",
		Priority:       0,
		Weight:         1,
		Enabled:        true,
	}); err != nil {
		t.Fatalf("dbInsertUpstream strong: %v", err)
	}
	invalidateRoutingSnapshot()

	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.SkipShortChars = 60
	cfg.Tiers[TierSimple] = []string{"mimo-v2.6-flash", "deepseek-v4.1-flash", "glm-5.3-flash"}
	cfg.DefaultModel = "grok-4.7"
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatalf("saveAutoRouterConfig: %v", err)
	}

	// Create API Key with "auto" and "mimo-v2.6-flash", where mimo-v2.6-flash has exhausted its independent quota (402)!
	rawKey := "sk-router-test-key-001"
	ak := &ApiKey{
		Name:        "router-user",
		Key:         rawKey,
		QPS:         50,
		Concurrency: 10,
		Models:      "auto,mimo-v2.6-flash",
		QuotaUSD:    10.0,
		Enabled:     true,
		ModelQuotas: map[string]ModelQuota{
			"mimo-v2.6-flash": {QuotaUSD: 1.0},
		},
	}
	if err := dbInsertKey(ak); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}
	keyID := ak.ID
	// Exhaust mimo-v2.6-flash independent quota (used 1.5 >= quota 1.0)
	if err := dbChargeKey(keyID, "mimo-v2.6-flash", 1.5, true); err != nil {
		t.Fatalf("dbChargeKey: %v", err)
	}

	router := newRouter()

	// 1. Check GET /v1/models includes "auto"
	reqModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqModels.Header.Set("Authorization", "Bearer "+rawKey)
	wModels := httptest.NewRecorder()
	router.ServeHTTP(wModels, reqModels)
	if wModels.Code != http.StatusOK || !strings.Contains(wModels.Body.String(), `"id":"auto"`) {
		t.Fatalf("expected /v1/models to expose virtual model auto, status=%d body=%s", wModels.Code, wModels.Body.String())
	}

	// 2. Send POST /v1/chat/completions with model="auto"
	// Priority #1 (mimo-v2.6-flash) should be skipped at selection time due to 402 quota exhausted.
	// Priority #2 (deepseek-v4.1-flash) should be attempted first, fail with 502, and failover to Priority #3 (glm-5.3-flash) which succeeds!
	reqBody := `{"model":"auto","messages":[{"role":"user","content":"你好，帮我翻译 hello"}]}`
	reqChat := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	reqChat.Header.Set("Authorization", "Bearer "+rawKey)
	reqChat.Header.Set("Content-Type", "application/json")
	wChat := httptest.NewRecorder()
	router.ServeHTTP(wChat, reqChat)

	if wChat.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after priority failover, got %d: %s", wChat.Code, wChat.Body.String())
	}
	if gotTier := wChat.Header().Get("X-APISHARE-Router-Tier"); gotTier != TierSimple {
		t.Fatalf("expected X-APISHARE-Router-Tier=SIMPLE, got %q", gotTier)
	}
	if gotModel := wChat.Header().Get("X-APISHARE-Router-Model"); gotModel != "glm-5.3-flash" {
		t.Fatalf("expected X-APISHARE-Router-Model=glm-5.3-flash after failover, got %q", gotModel)
	}
	if atomic.LoadInt32(&dsFlashCalls) == 0 || atomic.LoadInt32(&glmFlashCalls) != 1 {
		t.Fatalf("expected deepseek-v4.1-flash to be tried and failover to glm-5.3-flash, ds=%d glm=%d", dsFlashCalls, glmFlashCalls)
	}

	// 3. Verify call_logs recorded real model "glm-5.3-flash" and billing_json.router metadata
	var loggedModel, billingJSON string
	if err := db.QueryRow(`SELECT model, billing_json FROM call_logs ORDER BY id DESC LIMIT 1`).Scan(&loggedModel, &billingJSON); err != nil {
		t.Fatalf("query call_logs: %v", err)
	}
	if loggedModel != "glm-5.3-flash" {
		t.Fatalf("expected call_logs.model to be glm-5.3-flash, got %q", loggedModel)
	}
	if !strings.Contains(billingJSON, `"requested_model":"auto"`) || !strings.Contains(billingJSON, `"deepseek-v4.1-flash"`) {
		t.Fatalf("expected billing_json to contain router metadata and fallbacks_tried, got %s", billingJSON)
	}

	// 3b. Verify final backup model (DefaultModel = "grok-4.7") when ALL models in the matched tier (SIMPLE) are down:
	// Set SIMPLE to only ["mimo-v2.6-flash" (402 exhausted), "deepseek-v4.1-flash" (returns 502)] with CascadeTiers=false.
	cfg.Tiers[TierSimple] = []string{"mimo-v2.6-flash", "deepseek-v4.1-flash"}
	cfg.CascadeTiers = false
	cfg.DefaultModel = "grok-4.7"
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatalf("saveAutoRouterConfig for final backup test: %v", err)
	}
	reqAllDown := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"再帮我翻译一句 world"}]}`))
	reqAllDown.Header.Set("Authorization", "Bearer "+rawKey)
	reqAllDown.Header.Set("Content-Type", "application/json")
	wAllDown := httptest.NewRecorder()
	router.ServeHTTP(wAllDown, reqAllDown)
	if wAllDown.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after falling back to DefaultModel grok-4.7, got %d: %s", wAllDown.Code, wAllDown.Body.String())
	}
	if wAllDown.Header().Get("X-APISHARE-Router-Model") != "grok-4.7" || wAllDown.Header().Get("X-APISHARE-Router-Degraded") != "default" {
		t.Fatalf("expected final fallback to grok-4.7 with X-APISHARE-Router-Degraded=default, got model=%q degraded=%q",
			wAllDown.Header().Get("X-APISHARE-Router-Model"), wAllDown.Header().Get("X-APISHARE-Router-Degraded"))
	}
	var fallbackModel, fallbackBilling string
	if err := db.QueryRow(`SELECT model, billing_json FROM call_logs ORDER BY id DESC LIMIT 1`).Scan(&fallbackModel, &fallbackBilling); err != nil {
		t.Fatalf("query fallback call_log: %v", err)
	}
	if fallbackModel != "grok-4.7" ||
		!strings.Contains(fallbackBilling, `"degraded_to_default":true`) ||
		!strings.Contains(fallbackBilling, `"selected_source_tier":"DEFAULT"`) ||
		!strings.Contains(fallbackBilling, `"mimo-v2.6-flash"`) ||
		!strings.Contains(fallbackBilling, `"deepseek-v4.1-flash"`) {
		t.Fatalf("expected call_log to record degraded_to_default=true, selected_source_tier=DEFAULT, and failed_models, got model=%q billing=%s",
			fallbackModel, fallbackBilling)
	}
	// Restore SIMPLE tier for subsequent subtests
	cfg.Tiers[TierSimple] = []string{"mimo-v2.6-flash", "deepseek-v4.1-flash", "glm-5.3-flash"}
	cfg.CascadeTiers = true

	// 4. Test Shadow mode: classifies as SIMPLE (glm-5.3-flash/deepseek-v4.1-flash), but actually forwards to default_model (grok-4.7)
	cfg.Mode = "shadow"
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatalf("saveAutoRouterConfig shadow: %v", err)
	}
	reqShadow := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	reqShadow.Header.Set("Authorization", "Bearer "+rawKey)
	reqShadow.Header.Set("Content-Type", "application/json")
	wShadow := httptest.NewRecorder()
	router.ServeHTTP(wShadow, reqShadow)

	if wShadow.Code != http.StatusOK {
		t.Fatalf("expected 200 OK in shadow mode, got %d: %s", wShadow.Code, wShadow.Body.String())
	}
	if wShadow.Header().Get("X-APISHARE-Router-Shadow") != "1" {
		t.Fatalf("expected X-APISHARE-Router-Shadow=1 header in shadow mode")
	}
	if wShadow.Header().Get("X-APISHARE-Router-Model") != "grok-4.7" {
		t.Fatalf("expected actual model in shadow mode to be grok-4.7, got %q", wShadow.Header().Get("X-APISHARE-Router-Model"))
	}
	if wShadow.Header().Get("X-APISHARE-Router-Shadow-Model") != "deepseek-v4.1-flash" {
		t.Fatalf("expected shadow would-be model to be deepseek-v4.1-flash (since mimo-v2.6-flash is 402 exhausted), got %q", wShadow.Header().Get("X-APISHARE-Router-Shadow-Model"))
	}

	// 5. Test Admin API endpoints (/api/auto-router/config, /api/auto-router/dry-run, /api/auto-router/stats)
	adminToken := makeAdminToken()

	reqDry := httptest.NewRequest(http.MethodPost, "/admin/api/auto-router/dry-run", bytes.NewBufferString(`{"prompt":"请用数学归纳法证明定理","key_id":1}`))
	reqDry.Header.Set("Authorization", "Bearer "+adminToken)
	reqDry.Header.Set("Content-Type", "application/json")
	wDry := httptest.NewRecorder()
	router.ServeHTTP(wDry, reqDry)
	if wDry.Code != http.StatusOK || !strings.Contains(wDry.Body.String(), `"REASONING"`) {
		t.Fatalf("expected dry-run to return 200 and REASONING, got %d: %s", wDry.Code, wDry.Body.String())
	}

	reqStats := httptest.NewRequest(http.MethodGet, "/admin/api/auto-router/stats?period=24h", nil)
	reqStats.Header.Set("Authorization", "Bearer "+adminToken)
	wStats := httptest.NewRecorder()
	router.ServeHTTP(wStats, reqStats)
	if wStats.Code != http.StatusOK || !strings.Contains(wStats.Body.String(), `"total_calls":3`) || !strings.Contains(wStats.Body.String(), `"fallback_calls":1`) {
		t.Fatalf("expected stats to report 3 routed calls and 1 fallback call, got %d: %s", wStats.Code, wStats.Body.String())
	}
}

func TestXAuthUserBYOKAndPersonalAutoRouter(t *testing.T) {
	setupAutoRouterTestDB(t)
	resetAutoRouterBreaker()

	// 1. Mock Gateway Upstream (serves "site-pro-exhausted", "site-fast-ok", and shared classifier "qwen-classifier-shared")
	var gwClassifierCalls, gwFastCalls, userByokCalls int32
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model, _ := req["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		if model == "qwen-classifier-shared" {
			atomic.AddInt32(&gwClassifierCalls, 1)
			_, _ = w.Write([]byte(`{"id":"clf-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"COMPLEX"}}],"usage":{"prompt_tokens":20,"completion_tokens":1}}`))
			return
		}
		if model == "site-fast-ok" {
			atomic.AddInt32(&gwFastCalls, 1)
		}
		_, _ = w.Write([]byte(`{"id":"gw-1","object":"chat.completion","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"from gateway ` + model + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer gwSrv.Close()

	// 2. Mock User BYOK Upstream (serves "my-byok-sonnet" and "my-byok-flash")
	userByokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"my-byok-sonnet"},{"id":"my-byok-flash"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model, _ := req["model"].(string)
		atomic.AddInt32(&userByokCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"byok-1","object":"chat.completion","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"from user byok ` + model + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":8}}`))
	}))
	defer userByokSrv.Close()

	if err := dbInsertUpstream(&Upstream{
		Name:     "gw-main",
		Type:     "openai",
		BaseURL:  gwSrv.URL,
		APIKey:   "sk-gw",
		Models:   "site-pro-exhausted,site-fast-ok,qwen-classifier-shared",
		ModelMap: "{}",
		Priority: 0,
		Weight:   10,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("dbInsertUpstream: %v", err)
	}
	invalidateRoutingSnapshot()

	// Configure site auto-router:
	// - VirtualModel = "auto"
	// - ShareSiteClassifierWithUsers = false (site-exclusive classifier stays private!)
	// - SharedClassifierModels = ["qwen-classifier-shared"] (explicitly shared gateway upstream model)
	siteCfg := defaultAutoRouterConfig()
	siteCfg.Enabled = true
	siteCfg.Mode = "live"
	siteCfg.VirtualModel = "auto"
	siteCfg.AllowUserAutoRouter = true
	siteCfg.ShareSiteClassifierWithUsers = false
	siteCfg.SharedClassifierModels = []string{"qwen-classifier-shared"}
	if err := saveAutoRouterConfig(siteCfg); err != nil {
		t.Fatalf("saveAutoRouterConfig: %v", err)
	}

	// 3. Provision XAuth user & session
	rawKey := "sk-xauth-user-001"
	ak := &ApiKey{
		Name:        "Alice (XAuth)",
		Key:         rawKey,
		QPS:         20,
		Concurrency: 5,
		QuotaUSD:    5.0,
		Models:      "auto,site-pro-exhausted,site-fast-ok,qwen-classifier-shared",
		Enabled:     true,
		ModelQuotas: map[string]ModelQuota{
			"site-pro-exhausted": {QuotaUSD: 1.0},
		},
	}
	if err := dbInsertKey(ak); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}
	// Exhaust site-pro-exhausted (402)
	if err := dbChargeKey(ak.ID, "site-pro-exhausted", 1.2, true); err != nil {
		t.Fatalf("dbChargeKey: %v", err)
	}
	sub := "xauth-sub-alice-01"
	if _, err := db.Exec(`INSERT INTO xauth_users(sub,email,name,key_id,gift_claimed,created_at,last_login_at) VALUES(?,?,?,?,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		sub, "alice@example.com", "Alice", ak.ID); err != nil {
		t.Fatalf("insert xauth_users: %v", err)
	}
	sessToken, err := createXAuthUserSession(sub, ak.ID, "")
	if err != nil {
		t.Fatalf("createXAuthUserSession: %v", err)
	}

	router := newRouter()
	userReq := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.AddCookie(&http.Cookie{Name: xauthUserSessionCookie, Value: sessToken})
		r.Header.Set(xauthUserRequestHeader, "user-console")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	// 4. SSRF protection check: when allowLoopbackUserUpstreamForTest is false, loopback URL is rejected
	allowLoopbackUserUpstreamForTest = false
	wSSRF := userReq(http.MethodPost, "/api/user/upstreams", `{"name":"bad-local","type":"openai","base_url":"http://127.0.0.1:9999","api_key":"sk-1","models":"m1"}`)
	if wSSRF.Code != http.StatusBadRequest {
		t.Fatalf("expected SSRF check to reject loopback URL with 400, got %d: %s", wSSRF.Code, wSSRF.Body.String())
	}

	// Enable test loopback hook so we can point BYOK upstream to userByokSrv
	allowLoopbackUserUpstreamForTest = true
	defer func() { allowLoopbackUserUpstreamForTest = false }()

	// 5. Add user BYOK channel via /api/user/upstreams
	wAddUp := userReq(http.MethodPost, "/api/user/upstreams", fmt.Sprintf(
		`{"name":"My Private Sonnet","type":"openai","base_url":%q,"api_key":"sk-user-byok","models":"my-byok-sonnet,my-byok-flash","priority":20,"weight":1,"enabled":true}`,
		userByokSrv.URL,
	))
	if wAddUp.Code != http.StatusOK {
		t.Fatalf("expected POST /api/user/upstreams 200, got %d: %s", wAddUp.Code, wAddUp.Body.String())
	}

	// 6. Verify virtual_model collision with site "auto" is rejected
	wCollide := userReq(http.MethodPut, "/api/user/auto-router", `{"enabled":true,"virtual_model":"auto","default_model":"my-byok-sonnet"}`)
	if wCollide.Code != http.StatusBadRequest || !strings.Contains(wCollide.Body.String(), "不能与站点公共路由模型") {
		t.Fatalf("expected virtual_model=auto collision to be rejected, got %d: %s", wCollide.Code, wCollide.Body.String())
	}

	// 7. Verify site-exclusive classifier is protected (user cannot use "__site_default__" or unlisted model when ShareSiteClassifierWithUsers=false)
	wExclusive := userReq(http.MethodPut, "/api/user/auto-router",
		`{"enabled":true,"virtual_model":"smart-code","classifier_source":"gateway_shared","shared_classifier_model":"__site_default__","default_model":"my-byok-sonnet"}`)
	if wExclusive.Code != http.StatusBadRequest {
		t.Fatalf("expected site-exclusive classifier to be rejected for user router, got %d: %s", wExclusive.Code, wExclusive.Body.String())
	}

	// 8. Save valid personal auto-router:
	// - virtual_model: "smart-code" (renamed by user!)
	// - classifier_source: "gateway_shared" with "qwen-classifier-shared" (allowed in SharedClassifierModels)
	// - channel_preference: "gateway_first"
	// - COMPLEX candidates: #1 "site-pro-exhausted" (site model, 402 exhausted) -> #2 "my-byok-sonnet" (user's BYOK model!)
	wSaveRouter := userReq(http.MethodPut, "/api/user/auto-router", `{
		"enabled": true,
		"virtual_model": "smart-code",
		"channel_preference": "gateway_first",
		"classifier_source": "gateway_shared",
		"shared_classifier_model": "qwen-classifier-shared",
		"skip_short_chars": 10,
		"default_model": "my-byok-sonnet",
		"tiers": {
			"SIMPLE": ["site-fast-ok"],
			"MEDIUM": ["my-byok-flash"],
			"COMPLEX": ["site-pro-exhausted", "my-byok-sonnet"],
			"REASONING": ["my-byok-sonnet"]
		}
	}`)
	if wSaveRouter.Code != http.StatusOK {
		t.Fatalf("expected saving personal router to succeed, got %d: %s", wSaveRouter.Code, wSaveRouter.Body.String())
	}

	// 9. Verify GET /v1/models exposes both "auto" (site), "smart-code" (user-router), and "my-byok-sonnet" (user-byok)
	reqModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqModels.Header.Set("Authorization", "Bearer "+rawKey)
	wModels := httptest.NewRecorder()
	router.ServeHTTP(wModels, reqModels)
	if wModels.Code != http.StatusOK ||
		!strings.Contains(wModels.Body.String(), `"id":"smart-code"`) ||
		!strings.Contains(wModels.Body.String(), `"id":"my-byok-sonnet"`) {
		t.Fatalf("expected /v1/models to include smart-code and my-byok-sonnet, got: %s", wModels.Body.String())
	}

	// 10. Invoke POST /v1/chat/completions with model="smart-code"
	// - Gateway shared classifier ("qwen-classifier-shared") returns COMPLEX
	// - Candidate #1 ("site-pro-exhausted") is skipped because site quota is 402 exhausted
	// - Candidate #2 ("my-byok-sonnet") is selected and routed to userByokSrv!
	// - Answer call uses user BYOK channel so it does NOT debit site quota!
	keyBefore, _ := dbGetKeyByID(ak.ID)
	chatPayload := `{"model":"smart-code","messages":[{"role":"user","content":"请帮我设计一个完整的跨多文件微服务架构并实现连接池与退避重试"}]}`
	reqChat := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatPayload))
	reqChat.Header.Set("Authorization", "Bearer "+rawKey)
	reqChat.Header.Set("Content-Type", "application/json")
	wChat := httptest.NewRecorder()
	router.ServeHTTP(wChat, reqChat)

	if wChat.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from personal router model smart-code, got %d: %s", wChat.Code, wChat.Body.String())
	}
	if gotTier := wChat.Header().Get("X-APISHARE-Router-Tier"); gotTier != TierComplex {
		t.Fatalf("expected X-APISHARE-Router-Tier=COMPLEX, got %q", gotTier)
	}
	if gotModel := wChat.Header().Get("X-APISHARE-Router-Model"); gotModel != "my-byok-sonnet" {
		t.Fatalf("expected X-APISHARE-Router-Model=my-byok-sonnet, got %q", gotModel)
	}
	if atomic.LoadInt32(&gwClassifierCalls) != 1 || atomic.LoadInt32(&userByokCalls) != 1 {
		t.Fatalf("expected 1 gateway classifier call and 1 user BYOK answer call, got clf=%d byok=%d", gwClassifierCalls, userByokCalls)
	}

	// Verify the answer call log has quota_pool="byok" and cost_usd=0
	var ansModel, ansUpName, ansBilling string
	var ansCost float64
	if err := db.QueryRow(`SELECT model, upstream_name, cost_usd, billing_json FROM call_logs WHERE endpoint<>'router-classifier' ORDER BY id DESC LIMIT 1`).
		Scan(&ansModel, &ansUpName, &ansCost, &ansBilling); err != nil {
		t.Fatalf("query answer call_log: %v", err)
	}
	if ansModel != "my-byok-sonnet" || ansCost != 0 || !strings.Contains(ansBilling, `"quota_pool":"byok"`) || !strings.HasPrefix(ansUpName, "User BYOK · ") {
		t.Fatalf("expected BYOK call log with cost=0 and quota_pool=byok, got model=%q up=%q cost=%v billing=%s", ansModel, ansUpName, ansCost, ansBilling)
	}
	_ = keyBefore
}

func TestXAuthUserBYOKDefaultAndCustomProxy(t *testing.T) {
	setupAutoRouterTestDB(t)
	allowLoopbackUserUpstreamForTest = true
	defer func() { allowLoopbackUserUpstreamForTest = false }()

	var siteProxyHits, userCustomProxyHits, directUpstreamHits int32

	// Direct upstream server (used when proxy_mode="direct" or default proxy is 0)
	directUpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&directUpstreamHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"byok-proxied-model"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-direct","object":"chat.completion","model":"byok-proxied-model","choices":[{"index":0,"message":{"role":"assistant","content":"direct-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer directUpSrv.Close()

	// Admin bound HTTP proxy server
	siteProxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&siteProxyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"byok-proxied-model"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-siteproxy","object":"chat.completion","model":"byok-proxied-model","choices":[{"index":0,"message":{"role":"assistant","content":"via-site-default-proxy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer siteProxySrv.Close()

	// User custom HTTP proxy server
	userProxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&userCustomProxyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/models") {
			_, _ = w.Write([]byte(`{"data":[{"id":"byok-proxied-model"},{"id":"byok-custom-2"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl-userproxy","object":"chat.completion","model":"byok-proxied-model","choices":[{"index":0,"message":{"role":"assistant","content":"via-user-custom-proxy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer userProxySrv.Close()

	// Create XAuth user & API key
	rawKey := "sk-xauth-bob-proxy-test"
	ak := &ApiKey{
		Name:        "XAuth · bob",
		Key:         rawKey,
		QuotaUSD:    5.0,
		QPS:         20,
		Concurrency: 5,
		Enabled:     true,
		Models:      "auto",
	}
	if err := dbInsertKey(ak); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}
	sub := "xauth-sub-bob-proxy"
	if _, err := db.Exec(`INSERT INTO xauth_users(sub,email,name,key_id,gift_claimed,created_at,last_login_at) VALUES(?,?,?,?,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		sub, "bob@example.com", "Bob", ak.ID); err != nil {
		t.Fatalf("insert xauth_users: %v", err)
	}
	sessToken, err := createXAuthUserSession(sub, ak.ID, "")
	if err != nil {
		t.Fatalf("createXAuthUserSession: %v", err)
	}

	if err := settingsSet("session_secret", "test-admin-secret-proxy"); err != nil {
		t.Fatalf("settingsSet: %v", err)
	}
	router := newRouter()

	userReq := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.AddCookie(&http.Cookie{Name: xauthUserSessionCookie, Value: sessToken})
		r.Header.Set(xauthUserRequestHeader, "user-console")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	adminReq := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, adminPath+"/api/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+makeAdminToken())
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	// 1. Add user BYOK channel with proxy_mode="default" when admin default proxy is 0 (walks server direct)
	wAdd := userReq(http.MethodPost, "/api/user/upstreams", fmt.Sprintf(
		`{"name":"Bob Channel","type":"openai","base_url":%q,"api_key":"sk-bob","models":"byok-proxied-model","proxy_mode":"default"}`,
		directUpSrv.URL,
	))
	if wAdd.Code != http.StatusOK {
		t.Fatalf("expected 200 adding BYOK channel, got %d: %s", wAdd.Code, wAdd.Body.String())
	}
	var addResp struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(wAdd.Body.Bytes(), &addResp)

	chatCall := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"byok-proxied-model","messages":[{"role":"user","content":"ping"}]}`))
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := chatCall(); w.Code != http.StatusOK || atomic.LoadInt32(&directUpstreamHits) != 1 || atomic.LoadInt32(&siteProxyHits) != 0 {
		t.Fatalf("expected direct server call when xauth_user_default_proxy_id=0, code=%d direct=%d siteProxy=%d",
			w.Code, directUpstreamHits, siteProxyHits)
	}

	// 2. Admin adds a bound proxy and sets xauth_user_default_proxy_id in settings
	savedSiteProxy := testSavedProxy(t, siteProxySrv.URL, "", "")
	wSet := adminReq(http.MethodPut, "settings", fmt.Sprintf(`{"xauth_user_default_proxy_id":%d}`, savedSiteProxy.ID))
	if wSet.Code != http.StatusOK {
		t.Fatalf("expected PUT /api/settings 200, got %d: %s", wSet.Code, wSet.Body.String())
	}
	if getSettings().XAuthUserDefaultProxyID != savedSiteProxy.ID {
		t.Fatalf("expected XAuthUserDefaultProxyID=%d, got %d", savedSiteProxy.ID, getSettings().XAuthUserDefaultProxyID)
	}

	// Now user's channel (proxy_mode="default") should route through savedSiteProxy!
	// Note: HTTP proxy in Go net/http is used for non-loopback or HTTP URLs via Transport.Proxy.
	wUpdateHost := userReq(http.MethodPut, fmt.Sprintf("/api/user/upstreams/%d", addResp.ID),
		`{"base_url":"http://1.1.1.1","proxy_mode":"default"}`)
	if wUpdateHost.Code != http.StatusOK {
		t.Fatalf("expected 200 updating base_url, got %d: %s", wUpdateHost.Code, wUpdateHost.Body.String())
	}
	if w := chatCall(); w.Code != http.StatusOK || atomic.LoadInt32(&siteProxyHits) != 1 || !strings.Contains(w.Body.String(), "via-site-default-proxy") {
		t.Fatalf("expected BYOK channel with proxy_mode=default to use admin default proxy, code=%d siteProxyHits=%d body=%s",
			w.Code, siteProxyHits, w.Body.String())
	}

	// 3. Verify user cannot choose proxy_mode="direct" on their own
	wRejectDirect := userReq(http.MethodPut, fmt.Sprintf("/api/user/upstreams/%d", addResp.ID),
		`{"proxy_mode":"direct"}`)
	if wRejectDirect.Code != http.StatusBadRequest {
		t.Fatalf("expected proxy_mode=direct to be rejected with 400, got %d: %s", wRejectDirect.Code, wRejectDirect.Body.String())
	}

	// 4. User switches channel to proxy_mode="custom" with their own proxy URL
	// Even though admin default proxy (savedSiteProxy) is configured, the server connects directly
	// to the user's proxy without going through savedSiteProxy (siteProxyHits stays 1).
	wProbeCustom := userReq(http.MethodPost, "/api/user/upstreams/probe", fmt.Sprintf(
		`{"id":%d,"type":"openai","base_url":"http://1.1.1.1","proxy_mode":"custom","proxy_url":%q}`,
		addResp.ID, userProxySrv.URL,
	))
	if wProbeCustom.Code != http.StatusOK || !strings.Contains(wProbeCustom.Body.String(), `"byok-custom-2"`) || atomic.LoadInt32(&userCustomProxyHits) != 1 || atomic.LoadInt32(&siteProxyHits) != 1 {
		t.Fatalf("expected probe via custom proxy to hit userProxySrv directly (not siteProxySrv), code=%d userHits=%d siteHits=%d body=%s",
			wProbeCustom.Code, userCustomProxyHits, siteProxyHits, wProbeCustom.Body.String())
	}

	wSaveCustom := userReq(http.MethodPut, fmt.Sprintf("/api/user/upstreams/%d", addResp.ID), fmt.Sprintf(
		`{"proxy_mode":"custom","proxy_url":%q}`, userProxySrv.URL,
	))
	if wSaveCustom.Code != http.StatusOK {
		t.Fatalf("expected 200 saving custom proxy on BYOK channel, got %d: %s", wSaveCustom.Code, wSaveCustom.Body.String())
	}
	if w := chatCall(); w.Code != http.StatusOK || atomic.LoadInt32(&userCustomProxyHits) != 2 || atomic.LoadInt32(&siteProxyHits) != 1 || !strings.Contains(w.Body.String(), "via-user-custom-proxy") {
		t.Fatalf("expected BYOK channel with proxy_mode=custom to use user custom proxy directly without site proxy, code=%d userProxyHits=%d siteProxyHits=%d body=%s",
			w.Code, userCustomProxyHits, siteProxyHits, w.Body.String())
	}
}

func TestAutoRouterUpstreamBoundModelsAndNoPlaceholders(t *testing.T) {
	setupAutoRouterTestDB(t)
	router := newRouter()
	adminToken := makeAdminToken()

	// 1. Fresh config must not have any hardcoded placeholder models
	reqCfg := httptest.NewRequest(http.MethodGet, "/admin/api/auto-router/config", nil)
	reqCfg.Header.Set("Authorization", "Bearer "+adminToken)
	wCfg := httptest.NewRecorder()
	router.ServeHTTP(wCfg, reqCfg)
	if wCfg.Code != http.StatusOK {
		t.Fatalf("expected GET /admin/api/auto-router/config 200, got %d: %s", wCfg.Code, wCfg.Body.String())
	}
	bodyStr := wCfg.Body.String()
	for _, placeholder := range []string{"deepseek-v4.1-flash", "glm-5.3-flash", "mimo-v2.6-flash", "glm-5.3", "deepseek-v4-pro-0813", "grok-4.7"} {
		if strings.Contains(bodyStr, placeholder) {
			t.Fatalf("expected fresh auto-router config to not contain placeholder model %q, got %s", placeholder, bodyStr)
		}
	}

	// 2. Bind real models to two upstreams and verify available_models and upstream_models
	if err := dbInsertUpstream(&Upstream{
		Name:     "DashScope-Main",
		Type:     "openai",
		BaseURL:  "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKey:   "sk-dash",
		Models:   "qwen-plus,qwen-turbo",
		ModelMap: `{"qwen-alias":"qwen-max"}`,
		Priority: 0,
		Weight:   1,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("dbInsertUpstream 1: %v", err)
	}
	if err := dbInsertUpstream(&Upstream{
		Name:     "Anthropic-Pool",
		Type:     "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "sk-ant",
		Models:   "claude-3-7-sonnet",
		ModelMap: "{}",
		Priority: 1,
		Weight:   1,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("dbInsertUpstream 2: %v", err)
	}
	invalidateRoutingSnapshot()

	// Also simulate legacy persisted DB config that still had unbound placeholder models
	_ = settingsSet(autoRouterSettingsKey, `{"mode":"off","virtual_model":"auto","default_model":"grok-4.7","tiers":{"SIMPLE":["deepseek-v4.1-flash","qwen-turbo"],"REASONING":["grok-4.7"]}}`)
	_ = loadAutoRouterCache()

	reqCfg2 := httptest.NewRequest(http.MethodGet, "/admin/api/auto-router/config", nil)
	reqCfg2.Header.Set("Authorization", "Bearer "+adminToken)
	wCfg2 := httptest.NewRecorder()
	router.ServeHTTP(wCfg2, reqCfg2)
	if wCfg2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", wCfg2.Code, wCfg2.Body.String())
	}

	var parsed struct {
		DefaultModel    string                          `json:"default_model"`
		Tiers           map[string][]string             `json:"tiers"`
		AvailableModels []string                        `json:"available_models"`
		UpstreamModels  []autoRouterUpstreamModelGroup `json:"upstream_models"`
	}
	if err := json.Unmarshal(wCfg2.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal config view: %v", err)
	}
	if parsed.DefaultModel != "" {
		t.Fatalf("expected unbound legacy placeholder default_model grok-4.7 to be stripped, got %q", parsed.DefaultModel)
	}
	if len(parsed.Tiers[TierSimple]) != 1 || parsed.Tiers[TierSimple][0] != "qwen-turbo" {
		t.Fatalf("expected SIMPLE tier to keep bound qwen-turbo and strip unbound deepseek-v4.1-flash, got %v", parsed.Tiers[TierSimple])
	}
	if len(parsed.Tiers[TierReasoning]) != 0 {
		t.Fatalf("expected REASONING tier to strip unbound grok-4.7, got %v", parsed.Tiers[TierReasoning])
	}
	expectedAvail := "claude-3-7-sonnet,qwen-alias,qwen-plus,qwen-turbo"
	if strings.Join(parsed.AvailableModels, ",") != expectedAvail {
		t.Fatalf("expected available_models=%q, got %q", expectedAvail, strings.Join(parsed.AvailableModels, ","))
	}
	if len(parsed.UpstreamModels) != 2 {
		t.Fatalf("expected 2 upstream_models groups, got %d", len(parsed.UpstreamModels))
	}

	// 3. Verify /admin/api/keys/models ("分配模型给key") and /admin/api/models/overview ("模型列表") match available_models
	reqKeyModels := httptest.NewRequest(http.MethodGet, "/admin/api/keys/models", nil)
	reqKeyModels.Header.Set("Authorization", "Bearer "+adminToken)
	wKeyModels := httptest.NewRecorder()
	router.ServeHTTP(wKeyModels, reqKeyModels)
	if wKeyModels.Code != http.StatusOK {
		t.Fatalf("expected GET /admin/api/keys/models 200, got %d: %s", wKeyModels.Code, wKeyModels.Body.String())
	}
	var keyModelsResp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(wKeyModels.Body.Bytes(), &keyModelsResp); err != nil {
		t.Fatalf("unmarshal /admin/api/keys/models: %v", err)
	}
	if strings.Join(keyModelsResp.Models, ",") != expectedAvail {
		t.Fatalf("expected /admin/api/keys/models=%q to match router available_models=%q",
			strings.Join(keyModelsResp.Models, ","), strings.Join(parsed.AvailableModels, ","))
	}
}

func TestDecisionModelUpstreamAddProbeTestAndClassify(t *testing.T) {
	setupAutoRouterTestDB(t)

	var (
		systemOnePaths  []string
		systemOneBodies []string
		cfPaths         []string
		cfBodies        []string
	)
	var mu sync.Mutex

	mockSystemOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		systemOnePaths = append(systemOnePaths, r.URL.Path)
		systemOneBodies = append(systemOneBodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{"tier":{"choice":"COMPLEX","probabilities":{"SIMPLE":0.02,"MEDIUM":0.08,"COMPLEX":0.82,"REASONING":0.08}}}}`))
	}))
	defer mockSystemOne.Close()

	mockCloudflare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		cfPaths = append(cfPaths, r.URL.Path)
		cfBodies = append(cfBodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"answers":{"tier":{"choice":"REASONING","probabilities":{"SIMPLE":0.01,"MEDIUM":0.04,"COMPLEX":0.10,"REASONING":0.85}}}}}`))
	}))
	defer mockCloudflare.Close()

	// 1. Validate upstream types for Admin & User BYOK
	for _, tp := range []string{"openrouter", "systemone", "cloudflare"} {
		if !validUpstreamType(tp) {
			t.Fatalf("expected validUpstreamType(%q) == true", tp)
		}
		if !validUserUpstreamType(tp) {
			t.Fatalf("expected validUserUpstreamType(%q) == true", tp)
		}
	}

	// 2. Probe models on openrouter, systemone and cloudflare decision upstreams
	sysModels, err := probeUpstreamModelsWithClient(context.Background(), "systemone", mockSystemOne.URL, "sk-jev-test", mockSystemOne.Client())
	if err != nil {
		t.Fatalf("probeUpstreamModelsWithClient(systemone): %v", err)
	}
	if strings.Join(sysModels, ",") != "jev,Julia-1" {
		t.Fatalf("expected systemone probe models jev,Julia-1, got %v", sysModels)
	}

	orModels, err := probeUpstreamModelsWithClient(context.Background(), "openrouter", mockSystemOne.URL+"/api/alpha/decisions", "sk-or-test", mockSystemOne.Client())
	if err != nil {
		t.Fatalf("probeUpstreamModelsWithClient(openrouter): %v", err)
	}
	if strings.Join(orModels, ",") != "parallel/decisions-fast:free,parallel/decisions-fast" {
		t.Fatalf("expected openrouter probe models parallel/decisions-fast:free,parallel/decisions-fast, got %v", orModels)
	}

	cfBaseURL := mockCloudflare.URL + "/client/v4/accounts/acc123/ai/run"
	cfModels, err := probeUpstreamModelsWithClient(context.Background(), "cloudflare", cfBaseURL, "cf-token-test", mockCloudflare.Client())
	if err != nil {
		t.Fatalf("probeUpstreamModelsWithClient(cloudflare): %v", err)
	}
	if strings.Join(cfModels, ",") != "@cf/cloudflare/clef-flash,@cf/cloudflare/clef" {
		t.Fatalf("expected cloudflare probe models, got %v", cfModels)
	}

	// 3. Insert systemone and cloudflare upstreams into gateway upstreams (with empty Models to test default bound models)
	sysUp := &Upstream{
		Name:     "Jev-Decision-Upstream",
		Type:     "systemone",
		BaseURL:  mockSystemOne.URL,
		APIKey:   "sk-jev-test",
		Models:   "",
		ModelMap: "{}",
		Priority: 0,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(sysUp); err != nil {
		t.Fatalf("dbInsertUpstream(systemone): %v", err)
	}
	cfUp := &Upstream{
		Name:     "CF-Clef-Decision-Upstream",
		Type:     "cloudflare",
		BaseURL:  cfBaseURL,
		APIKey:   "cf-token-test",
		Models:   "",
		ModelMap: "{}",
		Priority: 1,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(cfUp); err != nil {
		t.Fatalf("dbInsertUpstream(cloudflare): %v", err)
	}
	invalidateRoutingSnapshot()

	// Verify collectGatewayUpstreamBoundModels includes default decision models
	boundModels, groups := collectGatewayUpstreamBoundModels(context.Background(), "auto")
	boundJoined := strings.Join(boundModels, ",")
	for _, want := range []string{"jev", "Julia-1", "@cf/cloudflare/clef-flash", "@cf/cloudflare/clef"} {
		if !strings.Contains(boundJoined, want) {
			t.Fatalf("expected boundModels %q to contain %q (groups=%+v)", boundJoined, want, groups)
		}
	}

	// 4. Test classifyViaGatewayUpstream for both "jev" (systemone) and "@cf/cloudflare/clef-flash" (cloudflare)
	resSys, usedSysUp, _, err := classifyViaGatewayUpstream(context.Background(), "jev", "Design a distributed Raft consensus engine in Go", nil, "", 0.5, 3000)
	if err != nil {
		t.Fatalf("classifyViaGatewayUpstream(jev): %v", err)
	}
	if resSys.Tier != TierComplex || usedSysUp == nil || usedSysUp.ID != sysUp.ID {
		t.Fatalf("expected Tier=COMPLEX from sysUp, got tier=%q up=%+v", resSys.Tier, usedSysUp)
	}

	resCF, usedCFUp, _, err := classifyViaGatewayUpstream(context.Background(), "@cf/cloudflare/clef-flash", "Prove the Riemann hypothesis step by step", nil, "", 0.5, 3000)
	if err != nil {
		t.Fatalf("classifyViaGatewayUpstream(clef-flash): %v", err)
	}
	if resCF.Tier != TierReasoning || usedCFUp == nil || usedCFUp.ID != cfUp.ID {
		t.Fatalf("expected Tier=REASONING from cfUp, got tier=%q up=%+v", resCF.Tier, usedCFUp)
	}

	// Verify exact wire paths and questions.tier decision payload format
	mu.Lock()
	lastSysPath := systemOnePaths[len(systemOnePaths)-1]
	lastSysBody := systemOneBodies[len(systemOneBodies)-1]
	lastCFPath := cfPaths[len(cfPaths)-1]
	lastCFBody := cfBodies[len(cfBodies)-1]
	mu.Unlock()

	if lastSysPath != "/v1/systemone" {
		t.Fatalf("expected systemone path /v1/systemone, got %q", lastSysPath)
	}
	if !strings.Contains(lastSysBody, `"questions":{"tier":{"criteria":`) || !strings.Contains(lastSysBody, `"type":"choice"`) {
		t.Fatalf("expected systemone payload to use questions.tier decision schema, got %s", lastSysBody)
	}
	if lastCFPath != "/client/v4/accounts/acc123/ai/run/@cf/cloudflare/clef-flash" {
		t.Fatalf("expected cloudflare path /client/v4/accounts/acc123/ai/run/@cf/cloudflare/clef-flash, got %q", lastCFPath)
	}
	if !strings.Contains(lastCFBody, `"model":"clef-flash"`) || !strings.Contains(lastCFBody, `"questions":{"tier":`) {
		t.Fatalf("expected cloudflare payload to use questions.tier decision schema with stripped model clef-flash, got %s", lastCFBody)
	}

	// 5. Test Admin "测试模型" (testUpstreamModel) on the decision upstream
	testResp, status, err := testUpstreamModel(context.Background(), sysUp, "jev", "Write a microservice in Go", 64, false)
	if err != nil || status != http.StatusOK || testResp == nil {
		t.Fatalf("testUpstreamModel(systemone): status=%d err=%v", status, err)
	}
	if !strings.Contains(testResp.text(), `"tier":"COMPLEX"`) {
		t.Fatalf("expected testUpstreamModel response to contain decision JSON with tier COMPLEX, got %s", testResp.text())
	}

	// 6. Test OpenRouter Decisions upstream + direct gateway POST /api/alpha/decisions (noul / choice / score)
	var orGotPath, orGotReferer, orGotTitle, orGotBody string
	mockOpenRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		orGotPath = r.URL.Path
		orGotReferer = r.Header.Get("HTTP-Referer")
		orGotTitle = r.Header.Get("X-OpenRouter-Title")
		orGotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"answers": {
				"is_urgent": {"noul": 0.92},
				"department": {"choice": "billing", "probabilities": {"billing": 0.91, "technical": 0.06, "sales": 0.03}},
				"frustration": {"score": 0.88}
			}
		}`))
	}))
	defer mockOpenRouter.Close()

	orUp := &Upstream{
		Name:     "OpenRouter-Decisions-Upstream",
		Type:     "openrouter",
		BaseURL:  mockOpenRouter.URL + "/api/v1", // Verify /api/v1 auto-normalizes to /api/alpha/decisions!
		APIKey:   "sk-or-v1-test",
		Models:   "",
		ModelMap: "{}",
		Priority: 2,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(orUp); err != nil {
		t.Fatalf("dbInsertUpstream(openrouter): %v", err)
	}
	invalidateRoutingSnapshot()

	rawKey := "sk-decisions-client-key"
	ak := &ApiKey{
		Name:        "decisions-user",
		Key:         rawKey,
		QPS:         50,
		Concurrency: 10,
		Models:      "parallel/decisions-fast:free,jev",
		QuotaUSD:    10.0,
		Enabled:     true,
	}
	if err := dbInsertKey(ak); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}

	router := newRouter()
	decisionsReqJSON := `{
		"model": "parallel/decisions-fast:free",
		"state": "Help! My payouts have been failing for 3 days.",
		"questions": {
			"is_urgent": {
				"type": "noul",
				"instructions": "Does this message convey urgency?",
				"criteria": {"true": "Explicitly time-sensitive", "false": "No urgency expressed"}
			},
			"department": {
				"type": "choice",
				"instructions": "Which team should handle this?",
				"criteria": {"billing": "Payments, invoicing, refunds", "technical": "Bugs, outages, integrations", "sales": "Pricing, upgrades, new accounts"}
			},
			"frustration": {
				"type": "score",
				"instructions": "How frustrated is the customer?",
				"criteria": ["Calm", "Frustrated", "Very angry"]
			}
		}
	}`
	reqDec := httptest.NewRequest(http.MethodPost, "/api/alpha/decisions", strings.NewReader(decisionsReqJSON))
	reqDec.Header.Set("Authorization", "Bearer "+rawKey)
	reqDec.Header.Set("Content-Type", "application/json")
	reqDec.Header.Set("HTTP-Referer", "https://example.com")
	reqDec.Header.Set("X-OpenRouter-Title", "MyApp")
	wDec := httptest.NewRecorder()
	router.ServeHTTP(wDec, reqDec)

	if wDec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from POST /api/alpha/decisions, got %d: %s", wDec.Code, wDec.Body.String())
	}
	mu.Lock()
	gotORPath, gotORRef, gotORTitle, gotORBody := orGotPath, orGotReferer, orGotTitle, orGotBody
	mu.Unlock()
	if gotORPath != "/api/alpha/decisions" {
		t.Fatalf("expected OpenRouter upstream path /api/alpha/decisions, got %q", gotORPath)
	}
	if gotORRef != "https://example.com" || gotORTitle != "MyApp" {
		t.Fatalf("expected HTTP-Referer and X-OpenRouter-Title forwarded, got referer=%q title=%q", gotORRef, gotORTitle)
	}
	if !strings.Contains(gotORBody, `"model":"parallel/decisions-fast:free"`) || !strings.Contains(gotORBody, `"is_urgent"`) {
		t.Fatalf("unexpected OpenRouter request body: %s", gotORBody)
	}
	// Verify @openrouter/sdk type fields ("noul", "choice", "score") are present in answers
	var decResp struct {
		Answers map[string]struct {
			Type          string             `json:"type"`
			Noul          float64            `json:"noul"`
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
			Score         float64            `json:"score"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(wDec.Body.Bytes(), &decResp); err != nil {
		t.Fatalf("unmarshal /api/alpha/decisions response: %v", err)
	}
	if decResp.Answers["is_urgent"].Type != "noul" || decResp.Answers["is_urgent"].Noul < 0.9 {
		t.Fatalf("unexpected is_urgent answer: %+v", decResp.Answers["is_urgent"])
	}
	if decResp.Answers["department"].Type != "choice" || decResp.Answers["department"].Choice != "billing" {
		t.Fatalf("unexpected department answer: %+v", decResp.Answers["department"])
	}
	if decResp.Answers["frustration"].Type != "score" || decResp.Answers["frustration"].Score < 0.8 {
		t.Fatalf("unexpected frustration answer: %+v", decResp.Answers["frustration"])
	}
}

func TestAutoRouterContextWindowFilteringAndLongContextUpgrade(t *testing.T) {
	setupAutoRouterTestDB(t)

	up := &Upstream{
		Name:     "Multi-Window-Pool",
		Type:     "openai",
		BaseURL:  "https://api.example.com/v1",
		APIKey:   "sk-test",
		Models:   "deepseek-chat,gpt-4o-mini,gemini-2.5-pro",
		ModelMap: "{}",
		Priority: 0,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatalf("dbInsertUpstream: %v", err)
	}
	invalidateRoutingSnapshot()

	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.SkipShortChars = 60
	cfg.LongContextThreshold = 8000
	cfg.Tiers = map[string][]string{
		TierSimple:    {"gpt-4o-mini(none,4k)"},
		TierMedium:    {"deepseek-chat(low,6k)"},
		TierComplex:   {"deepseek-chat(medium,6k)", "gemini-2.5-pro(high)"},
		TierReasoning: {"gemini-2.5-pro(max)"},
	}
	cfg.DefaultModel = "gemini-2.5-pro"

	// Build a multi-turn conversation whose recent user turns are simple ("翻译上一段", "继续")
	// so the classifier alone would pick SIMPLE, while total conversation history exceeds 8,000 tokens.
	longChunk := strings.Repeat("The quick brown fox jumps over the lazy dog in the sunny afternoon meadow. ", 600)
	cr := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "Here is chapter one:\n" + longChunk},
			{Role: "assistant", Text: "Received chapter one:\n" + longChunk},
			{Role: "user", Text: "把最后一句翻译成中文"},
			{Role: "assistant", Text: "敏捷的棕色狐狸在阳光明媚的午后草地上跳过懒狗。"},
			{Role: "user", Text: "继续"},
		},
		InputText: "继续",
	}

	ak := ApiKey{ID: 1, Name: "ctx-user", Enabled: true, QuotaUSD: 100}
	dec, cands, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil {
		t.Fatalf("resolveAutoRouteWithConfig: %v", err)
	}
	if dec.EstimatedInputTokens < 8000 {
		t.Fatalf("expected EstimatedInputTokens >= 8000, got %d", dec.EstimatedInputTokens)
	}
	if !dec.ContextUpgraded || dec.Tier != TierComplex || !strings.Contains(dec.Cause, "long_context") {
		t.Fatalf("expected long context auto-upgrade to COMPLEX, got tier=%s cause=%s upgraded=%v", dec.Tier, dec.Cause, dec.ContextUpgraded)
	}
	if len(cands) == 0 || cands[0] != "gemini-2.5-pro(high)" {
		t.Fatalf("expected small-context candidate deepseek-chat(medium,6k) to be filtered out and gemini-2.5-pro(high) selected, got cands=%v steps=%+v", cands, dec.Steps)
	}
	sawContextExceeded := false
	for _, st := range dec.Steps {
		if st.Model == "deepseek-chat(medium,6k)" && st.Status == "skipped" && st.Reason == "context_exceeded" {
			sawContextExceeded = true
		}
	}
	if !sawContextExceeded {
		t.Fatalf("expected step for deepseek-chat(medium,6k) to be skipped with context_exceeded, got %+v", dec.Steps)
	}
}

func TestAutoRouterMaxTokensClampingAndCrossModelHistorySanitization(t *testing.T) {
	// 1. Native Anthropic request routed from Claude Opus 4.6 history (with thinking + signature + redacted_thinking)
	// and huge max_tokens=64000 to claude-3-5-sonnet (non-thinking, max output 8192) on a different upstream.
	upSonnet35 := &Upstream{
		ID:      10,
		Name:    "Anthropic-35",
		Type:    "anthropic",
		BaseURL: "https://api.anthropic.com",
	}
	task := &relayTask{
		format:            FormatAnthropic,
		model:             "claude-3-5-sonnet-20241022",
		candidateRawModel: "claude-3-5-sonnet-20241022",
		sourceTier:        TierSimple,
		prevUpstreamID:    99,
		prevUpstreamModel: "claude-opus-4-6",
		router:            &routerDecisionDetails{Tier: TierSimple, Cause: "heuristic"},
	}
	rawAnthropic := `{
		"model": "claude-3-5-sonnet-20241022",
		"max_tokens": 64000,
		"thinking": {"type": "adaptive"},
		"output_config": {"effort": "high"},
		"messages": [
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "deep internal reasoning", "signature": "sig_opus_46"},
				{"type": "redacted_thinking", "data": "enc_blob"},
				{"type": "text", "text": "Hi there!"}
			]},
			{"role": "user", "content": "Follow up"}
		]
	}`
	adapted := adaptAutoRouterNativeBody([]byte(rawAnthropic), FormatAnthropic, upSonnet35, "claude-3-5-sonnet-20241022", task, nil)
	var gotObj map[string]any
	if err := json.Unmarshal(adapted, &gotObj); err != nil {
		t.Fatalf("unmarshal adapted anthropic body: %v", err)
	}
	if _, hasThinking := gotObj["thinking"]; hasThinking {
		t.Fatalf("expected thinking to be stripped for non-thinking claude-3-5-sonnet, got %v", gotObj["thinking"])
	}
	if _, hasOutCfg := gotObj["output_config"]; hasOutCfg {
		t.Fatalf("expected output_config.effort to be stripped for non-thinking claude-3-5-sonnet, got %v", gotObj["output_config"])
	}
	if int(gotObj["max_tokens"].(float64)) != 8192 {
		t.Fatalf("expected max_tokens=64000 to be clamped to 8192 for claude-3-5-sonnet, got %v", gotObj["max_tokens"])
	}
	msgs := gotObj["messages"].([]any)
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	for _, rawB := range blocks {
		bm := rawB.(map[string]any)
		if bm["type"] == "thinking" || bm["type"] == "redacted_thinking" {
			t.Fatalf("expected cross-model assistant thinking/redacted_thinking blocks to be sanitized, got %+v", blocks)
		}
	}

	// 2. Converted canonReq path: Claude 4.6 adaptive <-> Claude 4.5 fixed budget + Gemini 2.5 thinkingBudget
	upClaude45 := &Upstream{ID: 11, Name: "Anthropic-45", Type: "anthropic"}
	task45 := &relayTask{
		format:            FormatOpenAIChat,
		model:             "claude-sonnet-4-5",
		candidateRawModel: "claude-sonnet-4-5(high)",
		sourceTier:        TierComplex,
		router:            &routerDecisionDetails{Tier: TierComplex, Cause: "heuristic"},
	}
	cr45 := canonReq{
		Model:        "claude-sonnet-4-5",
		MaxTokens:    2000,
		ThinkingMode: "adaptive",
		Messages:     []canonMsg{{Role: "user", Text: "Design a compiler"}},
	}
	adaptAutoRouterCanonReq(&cr45, FormatAnthropic, upClaude45, "claude-sonnet-4-5", task45, []string{"low", "medium", "high"})
	if cr45.ThinkingMode != "enabled" || cr45.ThinkingBudget == nil || *cr45.ThinkingBudget != 8192 {
		t.Fatalf("expected claude-sonnet-4-5(high) to adapt to enabled with budget=8192, got mode=%q budget=%v", cr45.ThinkingMode, cr45.ThinkingBudget)
	}
	if cr45.MaxTokens <= *cr45.ThinkingBudget {
		t.Fatalf("expected MaxTokens (%d) to be raised above ThinkingBudget (%d) for Anthropic invariant", cr45.MaxTokens, *cr45.ThinkingBudget)
	}
}

func TestAutoRouterThinkingTierAndRuntimeFailover(t *testing.T) {
	setupAutoRouterTestDB(t)

	var (
		smallCalls  int
		largeCalls  int
		gotLargeReq string
		mu          sync.Mutex
	)

	mockSmall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		smallCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 32768 tokens. However, your messages resulted in 45000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}))
	defer mockSmall.Close()

	mockLarge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		largeCalls++
		gotLargeReq = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","model":"gpt-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"Handled by large context model with high thinking"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":20,"total_tokens":140}}`))
	}))
	defer mockLarge.Close()

	upSmall := &Upstream{
		Name:     "Small-Window-Upstream",
		Type:     "openai",
		BaseURL:  mockSmall.URL + "/v1",
		APIKey:   "sk-small",
		Models:   "deepseek-chat",
		ModelMap: "{}",
		Priority: 0,
		Weight:   1,
		Enabled:  true,
	}
	upLarge := &Upstream{
		Name:     "Large-Window-Upstream",
		Type:     "openai",
		BaseURL:  mockLarge.URL + "/v1",
		APIKey:   "sk-large",
		Models:   "gpt-5.2",
		ModelMap: "{}",
		Priority: 0,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(upSmall); err != nil {
		t.Fatalf("dbInsertUpstream small: %v", err)
	}
	if err := dbInsertUpstream(upLarge); err != nil {
		t.Fatalf("dbInsertUpstream large: %v", err)
	}
	invalidateRoutingSnapshot()

	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.ResponseHeaders = true
	cfg.Tiers = map[string][]string{
		TierSimple:    {"deepseek-chat(none)", "gpt-5.2(high)"},
		TierMedium:    {"deepseek-chat(none)", "gpt-5.2(high)"},
		TierComplex:   {"deepseek-chat(none)", "gpt-5.2(high)"},
		TierReasoning: {"gpt-5.2(xhigh)"},
	}
	cfg.DefaultModel = "gpt-5.2(high)"
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatalf("saveAutoRouterConfig: %v", err)
	}

	rawKey := "sk-failover-context-key"
	if err := dbInsertKey(&ApiKey{
		Name:        "ctx-failover-user",
		Key:         rawKey,
		QPS:         50,
		Concurrency: 10,
		Models:      "auto,deepseek-chat,gpt-5.2",
		QuotaUSD:    50,
		Enabled:     true,
	}); err != nil {
		t.Fatalf("dbInsertKey: %v", err)
	}

	router := newRouter()
	reqBody := `{"model":"auto","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected runtime failover to 200 OK when candidate #1 returns 400 context_length_exceeded, got %d: %s", w.Code, w.Body.String())
	}
	mu.Lock()
	sCalls, lCalls, largePayload := smallCalls, largeCalls, gotLargeReq
	mu.Unlock()
	if sCalls != 1 || lCalls != 1 {
		t.Fatalf("expected 1 call to small candidate and 1 failover call to large candidate, got small=%d large=%d", sCalls, lCalls)
	}
	if !strings.Contains(largePayload, `"model":"gpt-5.2"`) || !strings.Contains(largePayload, `"reasoning_effort":"high"`) {
		t.Fatalf("expected failover candidate gpt-5.2(high) to send clean model=\"gpt-5.2\" and reasoning_effort=\"high\", got %s", largePayload)
	}
	if gotModelHdr := w.Header().Get("X-APISHARE-Router-Model"); gotModelHdr != "gpt-5.2(high)" {
		t.Fatalf("expected X-APISHARE-Router-Model=gpt-5.2(high), got %q", gotModelHdr)
	}
	if gotThinkHdr := w.Header().Get("X-APISHARE-Router-Thinking"); gotThinkHdr != "high" {
		t.Fatalf("expected X-APISHARE-Router-Thinking=high, got %q", gotThinkHdr)
	}
	// Verify upSmall was not penalized into cooldown for a model context mismatch
	if !upSmall.availableForRelay("deepseek-chat") {
		t.Fatalf("expected upSmall to remain healthy after model-incompatible 400 failover")
	}
}

func TestAutoContextLimitDiscoveryCatalogProbingAndErrorSelfLearning(t *testing.T) {
	setupAutoRouterTestDB(t)

	// Reset dynamic registries and restore on cleanup
	probedModelLimits.Lock()
	probedModelLimits.data = map[string]ModelTokenLimits{}
	probedModelLimits.Unlock()
	learnedModelLimits.Lock()
	learnedModelLimits.data = map[string]ModelTokenLimits{}
	learnedModelLimits.Unlock()
	autoModelLimits.Lock()
	autoModelLimits.data = map[string]ModelTokenLimits{}
	autoModelLimits.Unlock()
	t.Cleanup(func() {
		probedModelLimits.Lock()
		probedModelLimits.data = map[string]ModelTokenLimits{}
		probedModelLimits.Unlock()
		learnedModelLimits.Lock()
		learnedModelLimits.data = map[string]ModelTokenLimits{}
		learnedModelLimits.Unlock()
		autoModelLimits.Lock()
		autoModelLimits.data = map[string]ModelTokenLimits{}
		autoModelLimits.Unlock()
	})

	// 1. Layer A: LiteLLM model_prices_and_context_window.json sync
	mockCatalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"openai/custom-catalog-model": {
				"input_cost_per_token": 0.000001,
				"output_cost_per_token": 0.000002,
				"max_input_tokens": 96000,
				"max_output_tokens": 12288
			}
		}`))
	}))
	defer mockCatalog.Close()

	s := getSettings()
	s.AutoPricingEnabled = true
	s.AutoPricingURL = mockCatalog.URL
	if err := persistSettings(s); err != nil {
		t.Fatalf("persistSettings: %v", err)
	}

	if _, err := refreshAutoPricing(); err != nil {
		t.Fatalf("refreshAutoPricing failed: %v", err)
	}
	ctxLim, ctxSrc := effectiveModelContextLimitWithSource("custom-catalog-model", nil)
	if ctxLim != 96000 || ctxSrc != "catalog" {
		t.Fatalf("expected catalog limit 96000 (source=catalog), got %d (source=%q)", ctxLim, ctxSrc)
	}
	outLim, outSrc := modelMaxOutputLimitWithSource("custom-catalog-model")
	if outLim != 12288 || outSrc != "catalog" {
		t.Fatalf("expected catalog max_output 12288 (source=catalog), got %d (source=%q)", outLim, outSrc)
	}

	// 2. Layer B: Upstream /v1/models (vLLM/OpenRouter style) and /v1beta/models (Gemini style) probing
	mockOpenAIModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "custom-catalog-model",
					"max_model_len": 48000,
					"max_completion_tokens": 6144
				},
				{
					"id": "openrouter-probed-model",
					"top_provider": {
						"context_length": 160000,
						"max_completion_tokens": 24576
					}
				}
			]
		}`))
	}))
	defer mockOpenAIModels.Close()

	if _, err := probeUpstreamModelsWithClient(context.Background(), "openai", mockOpenAIModels.URL, "sk-test", mockOpenAIModels.Client()); err != nil {
		t.Fatalf("probeUpstreamModelsWithClient(openai) failed: %v", err)
	}

	// Probed limit overrides catalog limit!
	ctxLim, ctxSrc = effectiveModelContextLimitWithSource("custom-catalog-model", nil)
	if ctxLim != 48000 || ctxSrc != "probed" {
		t.Fatalf("expected probed limit 48000 (source=probed) to override catalog, got %d (source=%q)", ctxLim, ctxSrc)
	}
	outLim, outSrc = modelMaxOutputLimitWithSource("custom-catalog-model")
	if outLim != 6144 || outSrc != "probed" {
		t.Fatalf("expected probed max_output 6144 (source=probed), got %d (source=%q)", outLim, outSrc)
	}
	ctxLim, ctxSrc = effectiveModelContextLimitWithSource("openrouter-probed-model", nil)
	if ctxLim != 160000 || ctxSrc != "probed" {
		t.Fatalf("expected openrouter probed limit 160000 (source=probed), got %d (source=%q)", ctxLim, ctxSrc)
	}

	mockGeminiModels := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{
					"name": "models/gemini-custom-exp",
					"inputTokenLimit": 524288,
					"outputTokenLimit": 32768
				}
			]
		}`))
	}))
	defer mockGeminiModels.Close()

	if _, err := probeUpstreamModelsWithClient(context.Background(), "gemini", mockGeminiModels.URL, "gm-test", mockGeminiModels.Client()); err != nil {
		t.Fatalf("probeUpstreamModelsWithClient(gemini) failed: %v", err)
	}
	ctxLim, ctxSrc = effectiveModelContextLimitWithSource("gemini-custom-exp", nil)
	if ctxLim != 524288 || ctxSrc != "probed" {
		t.Fatalf("expected gemini probed limit 524288 (source=probed), got %d (source=%q)", ctxLim, ctxSrc)
	}

	// 3. Layer C: Runtime 400 Error Self-Learning
	learnedCtx, learnedOut := learnModelLimitFromErrorBody(
		"custom-catalog-model",
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"This model's maximum context length is 16384 tokens. Moreover, max_tokens must be less than or equal to 2048."}}`),
	)
	if learnedCtx != 16384 || learnedOut != 2048 {
		t.Fatalf("expected learnedCtx=16384 learnedOut=2048, got ctx=%d out=%d", learnedCtx, learnedOut)
	}
	// Learned limit overrides both probed and catalog limits!
	ctxLim, ctxSrc = effectiveModelContextLimitWithSource("custom-catalog-model", nil)
	if ctxLim != 16384 || ctxSrc != "learned" {
		t.Fatalf("expected learned limit 16384 (source=learned) to override probed/catalog, got %d (source=%q)", ctxLim, ctxSrc)
	}
	outLim, outSrc = modelMaxOutputLimitWithSource("custom-catalog-model")
	if outLim != 2048 || outSrc != "learned" {
		t.Fatalf("expected learned max_output 2048 (source=learned), got %d (source=%q)", outLim, outSrc)
	}
}

func TestSmartRouterV2MultiSignalRatchetModalityAndCompression(t *testing.T) {
	setupAutoRouterTestDB(t)

	up := &Upstream{
		Name:     "All-In-One-Upstream",
		Type:     "openai",
		BaseURL:  "http://127.0.0.1:9999",
		APIKey:   "sk-test",
		Models:   "qwen-turbo,deepseek-reasoner,gpt-4o,claude-sonnet-4-5,gemini-2.5-flash",
		Priority: 1,
		Weight:   1,
		Enabled:  true,
	}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatalf("dbInsertUpstream: %v", err)
	}

	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.SessionMode = "session_lock"
	cfg.Tiers = map[string][]string{
		TierSimple:    {"qwen-turbo"},
		TierMedium:    {"gemini-2.5-flash"},
		TierComplex:   {"deepseek-reasoner", "gpt-4o", "claude-sonnet-4-5"},
		TierReasoning: {"deepseek-reasoner", "claude-sonnet-4-5"},
	}
	cfg.DefaultModel = "gpt-4o"
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatalf("saveAutoRouterConfig: %v", err)
	}

	ak := ApiKey{
		ID:      1,
		Name:    "v2-test-key",
		Models:  "auto,qwen-turbo,deepseek-reasoner,gpt-4o,claude-sonnet-4-5,gemini-2.5-flash",
		Enabled: true,
	}

	// 1. Short follow-up ("改一下") in a coding conversation must NOT short-circuit to SIMPLE
	crFollowUp := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{Role: "user", Text: "请帮我写一个 Go 并发 Worker Pool"},
			{Role: "assistant", Text: "好的，代码如下：\n```go\nfunc worker() {}\n```"},
			{Role: "user", Text: "改一下"},
		},
	}
	decFollow, candsFollow, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, crFollowUp, false)
	if err != nil {
		t.Fatalf("resolveAutoRoute(follow-up): %v", err)
	}
	if decFollow.Tier != TierComplex && decFollow.Tier != TierReasoning {
		t.Fatalf("expected short coding follow-up '改一下' to route to COMPLEX/REASONING via structural floor, got %s (cause=%s)", decFollow.Tier, decFollow.Cause)
	}
	if len(candsFollow) == 0 || candsFollow[0] == "qwen-turbo" {
		t.Fatalf("expected coding follow-up NOT to pick SIMPLE model qwen-turbo, got %v", candsFollow)
	}

	// 2. Session Ratchet Up: session starts with SIMPLE ("你好"), then Turn 2 asks a COMPLEX question
	hdr := http.Header{}
	hdr.Set("X-Session-Id", "sess-ratchet-1")
	crTurn1 := &canonReq{
		Model:     "auto",
		SessionID: "sess-ratchet-1",
		Messages:  []canonMsg{{Role: "user", Text: "你好"}},
	}
	dec1, _, _ := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, hdr, crTurn1, false)
	if dec1.Tier != TierSimple {
		t.Fatalf("expected Turn 1 greeting to be SIMPLE, got %s", dec1.Tier)
	}
	skey := scopedSessionKey(ak.ID, sessionKeyFromCanon("/v1/chat/completions", hdr, crTurn1))
	rememberAutoRouterSessionFull(skey, "qwen-turbo", TierSimple, up.ID, "qwen-turbo")

	crTurn2 := &canonReq{
		Model:     "auto",
		SessionID: "sess-ratchet-1",
		Messages: []canonMsg{
			{Role: "user", Text: "你好"},
			{Role: "assistant", Text: "你好！请问有什么可以帮您？"},
			{Role: "user", Text: "还是报错 panic: deadlock，请帮我重构分布式并发状态机并修复内存泄漏\n```go\nfunc run() {}\n```"},
		},
	}
	dec2, _, _ := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, hdr, crTurn2, false)
	if dec2.Tier == TierSimple {
		t.Fatalf("expected Session Ratchet to unlock SIMPLE session and upgrade to COMPLEX/REASONING on Turn 2, got %s (cause=%s)", dec2.Tier, dec2.Cause)
	}
	if !strings.Contains(dec2.Cause, "retry_escalated") && dec2.SelectedThinking != "high" && dec2.SelectedThinking != "xhigh" {
		t.Fatalf("expected frustration/retry turn to trigger retry_escalated and deep thinking, got cause=%q thinking=%q", dec2.Cause, dec2.SelectedThinking)
	}

	// 3. Modality Capability Filter: request with image skips text-only deepseek-reasoner and selects gpt-4o
	crVision := &canonReq{
		Model: "auto",
		Messages: []canonMsg{
			{
				Role: "user",
				Text: "请分析这张架构图中的分布式死锁问题并重构代码",
				Parts: []canonPart{
					{Kind: "text", Text: "请分析这张架构图中的分布式死锁问题并重构代码"},
					{Kind: "image", URL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="},
				},
			},
		},
	}
	decVision, candsVision, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, crVision, false)
	if err != nil {
		t.Fatalf("resolveAutoRoute(vision): %v", err)
	}
	if len(candsVision) == 0 || candsVision[0] != "gpt-4o" {
		t.Fatalf("expected vision request in COMPLEX tier to skip text-only deepseek-reasoner and select gpt-4o, got candidates=%v steps=%+v", candsVision, decVision.Steps)
	}

	// 4. Loss-Minimizing Middle-Out Context Compression
	bloatedToolOutput := strings.Repeat("log line: compile check ok\n", 800) // ~21,600 chars
	crBloated := &canonReq{
		Model:  "auto",
		System: "You are a coding assistant.",
		Messages: []canonMsg{
			{Role: "user", Text: "Initial goal: fix the authentication middleware."},
			{Role: "assistant", Calls: []canonCall{{ID: "call_1", Name: "run_test", Arguments: "{}"}}},
			{Role: "tool", ToolID: "call_1", ToolName: "run_test", Text: bloatedToolOutput},
			{Role: "assistant", Text: "I see the test output."},
			{Role: "user", Text: "Now check the token validator."},
			{Role: "assistant", Text: "Checking validator."},
			{Role: "user", Text: "Apply the final fix."},
		},
	}
	beforeTok := estimateRequestContextTokens(crBloated)
	compressed := compressCanonReqToFitContext(crBloated, 4096)
	afterTok := estimateRequestContextTokens(crBloated)
	if !compressed || afterTok >= beforeTok || afterTok > 4096 {
		t.Fatalf("expected compressCanonReqToFitContext to shrink tokens below 4096, got compressed=%v before=%d after=%d", compressed, beforeTok, afterTok)
	}
	if crBloated.Messages[0].Text != "Initial goal: fix the authentication middleware." {
		t.Fatalf("expected initial user goal message[0] to be preserved after compression")
	}
	if crBloated.Messages[len(crBloated.Messages)-1].Text != "Apply the final fix." {
		t.Fatalf("expected latest user message to be preserved after compression")
	}
}
