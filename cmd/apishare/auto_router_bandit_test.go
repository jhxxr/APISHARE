package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func setupBanditPoolTest(t *testing.T) (AutoRouterConfig, ApiKey, *canonReq, *Upstream) {
	t.Helper()
	setupAutoRouterTestDB(t)
	up := &Upstream{Name: "bandit-test", Type: "openai", BaseURL: "http://127.0.0.1:9999",
		APIKey: "test", Models: "gemini-2.5-flash,claude-sonnet-4-5", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	invalidateRoutingSnapshot()
	cfg := defaultAutoRouterConfig()
	cfg.Enabled, cfg.Mode, cfg.Classifier = true, "live", "heuristic"
	cfg.Tiers = map[string][]string{}
	cfg.ModelPool = []string{"gemini-2.5-flash", "claude-sonnet-4-5"}
	cr := &canonReq{Model: "auto", Messages: []canonMsg{{Role: "user",
		Text: "请用 React + Tailwind CSS + Framer Motion 实现复杂的响应式 Glassmorphism 数据可视化仪表盘组件，包含暗黑模式过渡动画与多图表联动：\n```tsx\nexport function Dashboard() {}\n```"}}}
	return cfg, ApiKey{}, cr, up
}

func TestBanditDisabledIgnoresHistory(t *testing.T) {
	cfg, ak, cr, up := setupBanditPoolTest(t)
	cfg.BanditEnabled = false
	before, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(candidates) < 2 {
		t.Fatalf("baseline: %v / %v", candidates, err)
	}
	for i := 0; i < 8; i++ {
		recordBanditExplicitOutcome(up.ID, candidates[0], before.PrimaryDomain, false, 4500, 4500, 5000, 0)
	}
	after, afterCandidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || !reflect.DeepEqual(before.Steps, after.Steps) || !reflect.DeepEqual(candidates, afterCandidates) {
		t.Fatalf("disabled history changed scoring: before=%+v after=%+v err=%v", before.Steps, after.Steps, err)
	}
	ucfg := defaultUserAutoRouterConfig()
	ucfg.ChannelPreference, ucfg.BanditEnabled = "gateway_first", false
	ucfg.ModelPool = cfg.ModelPool
	dec, targets, err := resolveUserAutoRouteWithConfig(context.Background(), "test", ucfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(targets) == 0 || targets[0].Model != candidates[0] || dec.Steps[0].UtilityScore != before.Steps[0].UtilityScore {
		t.Fatalf("personal router used disabled history: %+v / %+v / %v", dec, targets, err)
	}
}

func TestBanditDisabledDoesNotLearnImplicitFeedback(t *testing.T) {
	cfg, _, _, up := setupBanditPoolTest(t)
	cfg.BanditEnabled = false
	ak := ApiKey{ID: 1, Models: "auto,claude-sonnet-4-5,gemini-2.5-flash"}
	cr := &canonReq{Model: "auto", SessionID: "no-feedback", Messages: []canonMsg{
		{Role: "user", Text: "写一个Go服务"}, {Role: "assistant", Text: "```go\nfunc main(){}\n```"}, {Role: "user", Text: "继续"},
	}}
	recordBanditExplicitOutcome(up.ID, "claude-sonnet-4-5", DomainCodingSWE, true, 100, 100, 500, 10)
	before, _ := lookupBanditArmSummary(up.ID, "claude-sonnet-4-5", DomainCodingSWE)
	key := scopedSessionKey(ak.ID, sessionKeyFromCanon(nativeEndpoint(FormatOpenAIChat, cr), http.Header{}, cr))
	rememberAutoRouterSessionWithDomain(key, "claude-sonnet-4-5", TierComplex, DomainCodingSWE, up.ID, "claude-sonnet-4-5")
	if _, _, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, false); err != nil {
		t.Fatal(err)
	}
	after, _ := lookupBanditArmSummary(up.ID, "claude-sonnet-4-5", DomainCodingSWE)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("disabled implicit feedback changed history: %+v -> %+v", before, after)
	}
}

func TestBanditPersonalObservationsStayIsolated(t *testing.T) {
	cfg, ak, cr, up := setupBanditPoolTest(t)
	decision, before, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(before) == 0 {
		t.Fatalf("baseline: %v / %v", before, err)
	}
	for i := 0; i < 8; i++ {
		recordBanditExplicitOutcome(-999, before[0], decision.PrimaryDomain, false, 4500, 4500, 5000, 0)
		recordBanditImplicitFeedback(-999, before[0], decision.PrimaryDomain, true)
		recordBanditSpeculativeEscalation(-999, before[0], decision.PrimaryDomain)
	}
	if len(snapshotBanditLeaderboard()) != 0 {
		t.Fatal("personal observations appeared on the public leaderboard")
	}
	if _, ok := lookupBanditArmSummary(up.ID, before[0], decision.PrimaryDomain); ok {
		t.Fatal("site inherited personal observations")
	}
	personal, ok := lookupBanditArmSummary(-999, before[0], decision.PrimaryDomain)
	if !ok || personal.Calls != 16 || personal.ImplicitNegative != 8 || personal.SpeculativeEscalations != 8 {
		t.Fatalf("personal history lost: %+v / %v", personal, ok)
	}
	_, after, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("personal failures changed site ranking: %v -> %v / %v", before, after, err)
	}
	autoRouterBandit.RLock()
	siteCalls := autoRouterBandit.domainCalls[decision.PrimaryDomain]
	autoRouterBandit.RUnlock()
	if siteCalls != 0 {
		t.Fatalf("personal calls changed site's exploration bonus: %d", siteCalls)
	}
	// Personal arms survive persistence, but never fall back to site priors or
	// another owner's globally unique negative upstream ID.
	loadAutoRouterBanditCache()
	if _, ok := lookupBanditArmSummary(-999, before[0], decision.PrimaryDomain); !ok {
		t.Fatal("personal history did not survive reload")
	}
	recordBanditExplicitOutcome(up.ID, before[0], decision.PrimaryDomain, true, 100, 100, 500, 10)
	if _, ok := lookupBanditArmSummary(-998, before[0], decision.PrimaryDomain); ok {
		t.Fatal("unobserved personal channel inherited another scope's history")
	}
}

func TestUserAutoRouterUsesOwnPersonalHistory(t *testing.T) {
	cfg, ak, cr, _ := setupBanditPoolTest(t)
	addPersonal := func(sub string) int64 {
		t.Helper()
		result, err := db.Exec(`INSERT INTO user_upstreams(sub,name,type,base_url,api_key,models) VALUES(?,?,?,?,?,?)`,
			sub, "personal", "openai", "http://127.0.0.1:9999", "private", strings.Join(cfg.ModelPool, ","))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		return -id
	}
	id := addPersonal("owner")
	addPersonal("other")
	ucfg := defaultUserAutoRouterConfig()
	ucfg.ChannelPreference, ucfg.ModelPool = "personal_only", cfg.ModelPool
	resolve := func(sub string) (*routerDecisionDetails, []userRouteTarget) {
		t.Helper()
		dec, targets, err := resolveUserAutoRouteWithConfig(context.Background(), sub, ucfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
		if err != nil || len(targets) < 2 {
			t.Fatalf("personal pool: %+v / %v", targets, err)
		}
		return dec, targets
	}
	dec, before := resolve("owner")
	for i := 0; i < 8; i++ {
		recordBanditExplicitOutcome(id, before[0].Model, dec.PrimaryDomain, false, 4500, 4500, 5000, 0)
	}
	_, after := resolve("owner")
	_, other := resolve("other")
	if after[0].Model == before[0].Model || other[0].Model != before[0].Model {
		t.Fatalf("personal scopes not applied: before=%+v owner=%+v other=%+v", before, after, other)
	}
}

func TestUserAutoRouterSessionHonorsExcludedModels(t *testing.T) {
	_, _, _, up := setupBanditPoolTest(t)
	for _, excluded := range []string{"claude-sonnet-4-5", "CLAUDE-SONNET-4-5(high)"} {
		t.Run(excluded, func(t *testing.T) {
			ucfg := defaultUserAutoRouterConfig()
			ucfg.ChannelPreference, ucfg.ModelPool = "gateway_first", []string{"gemini-2.5-flash"}
			ucfg.ExcludedModels = []string{excluded}
			ak := ApiKey{ID: 1, Models: "claude-sonnet-4-5,gemini-2.5-flash"}
			cr := &canonReq{Model: "my-auto", SessionID: "excluded-session", Messages: []canonMsg{
				{Role: "user", Text: "写一个Go服务"}, {Role: "assistant", Text: "```go\nfunc main(){}\n```"}, {Role: "user", Text: "继续"},
			}}
			key := "user:owner:" + scopedSessionKey(ak.ID, sessionKeyFromCanon(nativeEndpoint(FormatOpenAIChat, cr), http.Header{}, cr))
			rememberAutoRouterSessionWithDomain(key, "claude-sonnet-4-5", TierComplex, DomainCodingSWE, up.ID, "claude-sonnet-4-5")
			_, targets, err := resolveUserAutoRouteWithConfig(context.Background(), "owner", ucfg, ak, FormatOpenAIChat, false, http.Header{}, cr, false)
			if err != nil || len(targets) == 0 || targets[0].Model != "gemini-2.5-flash" {
				t.Fatalf("excluded model reused by follow-up: %+v / %v", targets, err)
			}
		})
	}
}

func TestBanditDisabledDoesNotLearnFromRelay(t *testing.T) {
	for _, personal := range []bool{false, true} {
		t.Run(map[bool]string{false: "site", true: "personal"}[personal], func(t *testing.T) {
			setupAutoRouterTestDB(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer server.Close()
			up := &Upstream{Name: "relay-test", Type: "openai", BaseURL: server.URL, Models: "gemini-2.5-flash", Weight: 1, Enabled: true}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			cfg := defaultAutoRouterConfig()
			cfg.Mode, cfg.Classifier, cfg.BanditEnabled = "live", "heuristic", false
			cfg.ModelPool = []string{"gemini-2.5-flash"}
			if err := saveAutoRouterConfig(cfg); err != nil {
				t.Fatal(err)
			}
			model := "auto"
			ak := ApiKey{Models: "auto,gemini-2.5-flash"}
			if personal {
				ucfg := defaultUserAutoRouterConfig()
				ucfg.Enabled, ucfg.BanditEnabled = true, false
				ucfg.ChannelPreference, ucfg.ModelPool = "gateway_first", cfg.ModelPool
				if err := saveUserAutoRouterConfig("owner", ucfg); err != nil {
					t.Fatal(err)
				}
				ak.XAuthUser = &XAuthKeyUser{Sub: "owner"}
				model = ucfg.VirtualModel
			}
			r := gin.New()
			r.POST("/relay", func(c *gin.Context) { c.Set("apiKey", ak) }, relayHandler(FormatOpenAIChat, false))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/relay", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`)))
			if w.Code != http.StatusOK || len(snapshotBanditLeaderboard()) != 0 {
				t.Fatalf("disabled relay learned history: status=%d body=%s leaderboard=%+v", w.Code, w.Body, snapshotBanditLeaderboard())
			}
		})
	}
}

func TestBanditResetClearsPersistenceAndPublicSnapshot(t *testing.T) {
	_, _, _, up := setupBanditPoolTest(t)
	for i := 0; i < 8; i++ {
		recordBanditExplicitOutcome(up.ID, "claude-sonnet-4-5", DomainCodingSWE, true, 500, 500, 1000, 25)
	}
	r := gin.New()
	setArenaFixture(t, "text", []arenaModel{{Rank: 1, Model: "claude-sonnet-4-5", Family: "Anthropic", Score: 1450}})
	registerPublic(r)
	registerAutoRouterAdminRoutes(r.Group("/api"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/rankings", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"score":1450`) {
		t.Fatalf("public snapshot did not include training: %d / %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auto-router/bandit/reset", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("reset failed: %d / %s", w.Code, w.Body)
	}
	loadAutoRouterBanditCache()
	if len(snapshotBanditLeaderboard()) != 0 {
		t.Fatal("persisted history survived reset")
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/rankings", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"score":1450`) {
		t.Fatalf("Bandit reset must not change official Arena scores: %d / %s", w.Code, w.Body)
	}
}
