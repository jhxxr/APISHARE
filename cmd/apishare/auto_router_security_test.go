package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUserExternalClassifierCredentialIsolation(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "site-account")
	t.Setenv("CLOUDFLARE_API_TOKEN", "site-cloudflare-token")
	t.Setenv("CLOUDFLARE_API_KEY", "site-cloudflare-key")
	t.Setenv("TYPESAFE_API_KEY", "site-typesafe-key")
	t.Setenv("JEV_API_KEY", "site-jev-key")
	for _, provider := range []string{"cloudflare", "jev"} {
		t.Run(provider, func(t *testing.T) {
			cfg := defaultAutoRouterConfig()
			cfg.Classifier = provider
			cfg.EndpointURL = "https://user.example/classifier"
			_, _, _, key, source, err := cfg.effectiveClassifierCredentials()
			if err != nil || key == "" || source != "env" {
				t.Fatalf("site environment fallback must still work: key=%q source=%q err=%v", key, source, err)
			}
			cfg.userExternal = true
			_, _, _, key, source, err = cfg.effectiveClassifierCredentials()
			if err == nil || key != "" || source == "env" {
				t.Fatalf("personal classifier inherited site credentials: key=%q source=%q err=%v", key, source, err)
			}
			cfg.APIKey = "user-owned-key"
			_, endpoint, _, key, source, err := cfg.effectiveClassifierCredentials()
			if err != nil || key != cfg.APIKey || source != "settings" || endpoint != cfg.EndpointURL && provider == "cloudflare" {
				t.Fatalf("personal credentials did not resolve: endpoint=%q key=%q source=%q err=%v", endpoint, key, source, err)
			}
			if provider == "cloudflare" {
				cfg.EndpointURL = ""
				if _, _, _, _, _, err := cfg.effectiveClassifierCredentials(); err == nil {
					t.Fatal("personal classifier inherited the site Cloudflare account")
				}
			}
		})
	}
}

func TestUserExternalClassifierRejectsGatewayProvider(t *testing.T) {
	setupAutoRouterTestDB(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"COMPLEX"}}]}`)
	}))
	defer srv.Close()
	up := &Upstream{Name: "private", Type: "openai", OpenAIProtocol: "chat", BaseURL: srv.URL, APIKey: "site-key", Models: "private-model", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	invalidateRoutingSnapshot()
	for _, provider := range []string{"gateway_upstream", "gateway", "upstream", "unknown"} {
		t.Run(provider, func(t *testing.T) {
			cfg := defaultUserAutoRouterConfig()
			cfg.ClassifierSource = "user_external"
			cfg.UserClassifierProvider = provider
			cfg.UserClassifierModel = "private-model"
			for _, strict := range []bool{true, false} {
				copy := cfg
				if err := normalizeUserAutoRouterConfig("security-user", &copy, strict); err == nil {
					t.Fatalf("accepted provider %q with strict=%t", provider, strict)
				}
			}
			for _, dryRun := range []bool{true, false} {
				if _, err := classifyForUserAutoRouter(context.Background(), ApiKey{ID: 99, Models: "allowed-only"}, "security-user", cfg, "review prompt", dryRun); err == nil {
					t.Fatalf("runtime accepted provider %q with dryRun=%t", provider, dryRun)
				}
			}
		})
	}
	r := gin.New()
	group := r.Group("/api/user", func(c *gin.Context) {
		c.Set("xauthUser", xauthUserRecord{Sub: "security-user"})
		c.Set("xauthKey", ApiKey{ID: 99, Models: "allowed-only"})
	})
	registerUserRouterRoutes(group)
	body := `{"prompt":"Explain multi-file architecture with concurrent workers and retries in detail.","override":{"classifier_source":"user_external","user_classifier_provider":"gateway_upstream","user_classifier_model":"private-model"}}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/user/auto-router/dry-run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("dry-run reached private site model: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body)
	}
}

func TestUserExternalClassifierUsesOnlyPersonalCredentials(t *testing.T) {
	setupAutoRouterTestDB(t)
	t.Setenv("CLOUDFLARE_API_TOKEN", "site-only-token")
	t.Setenv("TYPESAFE_API_KEY", "site-only-typesafe-key")
	allowLoopbackUserUpstreamForTest = true
	t.Cleanup(func() { allowLoopbackUserUpstreamForTest = false })
	for _, provider := range []string{"cloudflare", "jev"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			var auth atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				auth.Store(r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"tier":"SIMPLE"}`)
			}))
			defer srv.Close()
			cfg := defaultUserAutoRouterConfig()
			cfg.ClassifierSource = "user_external"
			cfg.UserClassifierProvider = provider
			cfg.UserClassifierEndpoint = srv.URL
			if _, err := classifyForUserAutoRouter(context.Background(), ApiKey{}, "credential-user", cfg, "hello", true); err == nil {
				t.Fatal("missing personal credentials were accepted")
			}
			if calls.Load() != 0 {
				t.Fatal("external endpoint was contacted using site credentials")
			}
			cfg.UserClassifierAPIKey = "personal-only-key"
			res, err := classifyForUserAutoRouter(context.Background(), ApiKey{}, "credential-user", cfg, "hello", true)
			if err != nil || res.Tier != TierSimple || auth.Load() != "Bearer personal-only-key" || calls.Load() != 1 {
				t.Fatalf("personal credentials were not used: result=%+v calls=%d auth=%v err=%v", res, calls.Load(), auth.Load(), err)
			}
		})
	}
}

func TestAutoRouterFallbackPreservesPermissionAndQuota(t *testing.T) {
	setupAutoRouterTestDB(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"unexpected"}}]}`)
	}))
	defer srv.Close()
	up := &Upstream{Name: "site", Type: "openai", OpenAIProtocol: "chat", BaseURL: srv.URL, APIKey: "site-key", Models: "forbidden,allowed,exhausted", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	healthMarkSuccess(up.ID)
	invalidateRoutingSnapshot()
	tests := []struct {
		name       string
		mode       string
		candidates []string
		fallback   string
		status     int
	}{
		{"live-forbidden", "live", []string{"forbidden"}, "forbidden", http.StatusForbidden},
		{"live-mixed-quota-and-forbidden", "live", []string{"exhausted", "forbidden"}, "exhausted", http.StatusPaymentRequired},
		{"shadow-forbidden", "shadow", []string{"allowed"}, "forbidden", http.StatusForbidden},
		{"shadow-exhausted", "shadow", []string{"allowed"}, "exhausted", http.StatusPaymentRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultAutoRouterConfig()
			cfg.Mode = tt.mode
			cfg.Classifier = "heuristic"
			cfg.AllowUnlistedTargetModels = false
			cfg.CascadeTiers = false
			cfg.Tiers[TierSimple] = tt.candidates
			cfg.DefaultModel = tt.fallback
			if err := saveAutoRouterConfig(cfg); err != nil {
				t.Fatal(err)
			}
			ak := ApiKey{ID: 99, Models: "auto,allowed,exhausted", ModelQuotas: map[string]ModelQuota{
				"exhausted": {QuotaUSD: 1, UsedUSD: 1},
			}}
			cr := &canonReq{Model: "auto", Messages: []canonMsg{{Role: "user", Text: "hello"}}}
			_, candidates, err := resolveAutoRoute(context.Background(), ak, FormatOpenAIChat, false, http.Header{}, cr)
			if err == nil || len(candidates) != 0 {
				t.Fatalf("unsafe fallback returned: candidates=%v err=%v", candidates, err)
			}
			if tt.status == http.StatusForbidden && !errors.Is(err, errModelNotAllowed) {
				t.Fatalf("expected permission error, got %v", err)
			}
			r := gin.New()
			r.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", ak) }, relayHandler(FormatOpenAIChat, false))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)))
			if w.Code != tt.status || calls.Load() != 0 {
				t.Fatalf("expected status=%d without upstream calls, got status=%d calls=%d body=%s", tt.status, w.Code, calls.Load(), w.Body)
			}
		})
	}
}

func TestBYOKFailoverExhaustsEachChannelOnce(t *testing.T) {
	tests := []struct {
		preference string
		byokStatus int
		siteStatus int
		status     int
		byokCalls  int32
		siteCalls  int32
	}{
		{"personal_first", 502, 200, 200, 2, 1},
		{"personal_only", 502, 200, 502, 2, 0},
		{"gateway_first", 200, 502, 200, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.preference, func(t *testing.T) {
			setupAutoRouterTestDB(t)
			allowLoopbackUserUpstreamForTest = true
			t.Cleanup(func() { allowLoopbackUserUpstreamForTest = false })
			settings := getSettings()
			settings.Retry = 2
			if err := persistSettings(settings); err != nil {
				t.Fatal(err)
			}
			var personalCalls, siteCalls atomic.Int32
			var individual [2]atomic.Int32
			for i := range individual {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					personalCalls.Add(1)
					individual[i].Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.byokStatus)
					io.WriteString(w, `{"choices":[{"message":{"content":"byok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				}))
				defer srv.Close()
				result, err := db.Exec(`INSERT INTO user_upstreams(sub,name,type,openai_protocol,base_url,api_key,models,priority) VALUES(?,?,?,?,?,?,?,?)`,
					"failover-user", fmt.Sprintf("personal-%d", i), "openai", "chat", srv.URL, "byok-key", "shared-model", 20-i)
				if err != nil {
					t.Fatal(err)
				}
				id, _ := result.LastInsertId()
				healthMarkSuccess(-id)
				t.Cleanup(func() { healthMarkSuccess(-id) })
			}
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				siteCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.siteStatus)
				io.WriteString(w, `{"choices":[{"message":{"content":"site"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer site.Close()
			up := &Upstream{Name: "site", Type: "openai", OpenAIProtocol: "chat", BaseURL: site.URL, APIKey: "site-key", Models: "shared-model", Weight: 1, Enabled: true}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			healthMarkSuccess(up.ID)
			t.Cleanup(func() { healthMarkSuccess(up.ID) })
			invalidateRoutingSnapshot()
			cfg := defaultUserAutoRouterConfig()
			cfg.ChannelPreference = tt.preference
			if err := saveUserAutoRouterConfig("failover-user", cfg); err != nil {
				t.Fatal(err)
			}
			ak := ApiKey{Name: "user", Key: "sk-failover-user", Models: "shared-model", QuotaUSD: 10, Enabled: true}
			if err := dbInsertKey(&ak); err != nil {
				t.Fatal(err)
			}
			ak.XAuthUser = &XAuthKeyUser{Sub: "failover-user"}
			r := gin.New()
			r.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", ak) }, relayHandler(FormatOpenAIChat, false))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)))
			if w.Code != tt.status || personalCalls.Load() != tt.byokCalls || siteCalls.Load() != tt.siteCalls {
				t.Fatalf("status=%d personal=%d site=%d body=%s", w.Code, personalCalls.Load(), siteCalls.Load(), w.Body)
			}
			for i := range individual {
				if individual[i].Load() > 1 {
					t.Fatalf("personal channel %d was retried within the same request", i)
				}
			}
		})
	}
}
