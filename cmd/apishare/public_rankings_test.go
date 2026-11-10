package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestPublicRankingsIncludesFullCatalogAndProtectsPrivacy(t *testing.T) {
	setupAutoRouterTestDB(t)
	var names []string
	for i := 0; i < 55; i++ {
		names = append(names, fmt.Sprintf("custom-model-%02d", i))
	}
	names = append(names, "auto", "claude-sonnet-4-5", "gemini-2.5-flash", "vendor/claude-sonnet-4-5")
	if err := dbInsertUpstream(&Upstream{Name: "private-upstream-name", Type: "openai",
		BaseURL: "https://private-upstream.invalid", APIKey: "sk-private-credential",
		Models: strings.Join(names, ","), ModelMap: `{"public-alias":"claude-sonnet-4-5"}`, Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := dbInsertUpstream(&Upstream{Name: "disabled", Type: "openai", Models: "disabled-model", Weight: 1, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO user_upstreams(sub,name,type,base_url,api_key,models) VALUES(?,?,?,?,?,?)`,
		"private-owner", "private-personal-channel", "openai", "https://personal.invalid", "private-personal-key", "private-personal-model"); err != nil {
		t.Fatal(err)
	}
	cfg := getAutoRouterConfig()
	// Respect the same custom calibration as the routing engine.
	cfg.ModelCapabilities["custom-model-54"] = ModelCapabilityVector{
		CodingSWE: 100, FrontendUI: 100, ReasoningMath: 100, AgenticTools: 100,
		WritingKnowledge: 100, LongContextMM: 100, OverallScore: 100,
	}
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatal(err)
	}
	recordBanditExplicitOutcome(1, "claude-sonnet-4-5", DomainCodingSWE, true, 500, 500, 1000, 25)
	recordBanditExplicitOutcome(-1, "gemini-2.5-flash", DomainCodingSWE, false, 10000, 10000, 10000, 0)
	got, err := buildPublicRankings(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 59 || got.Models[0].Model != "custom-model-54" || got.Models[0].Capabilities.BenchmarkSource != "override" {
		t.Fatalf("full catalog or overrides missing: count=%d leader=%+v", len(got.Models), got.Models[0])
	}
	foundUnused := false
	for i, model := range got.Models {
		if model.Rank != i+1 {
			t.Fatalf("invalid rank: %+v", model)
		}
		if (model.Model == "claude-sonnet-4-5" || model.Model == "vendor/claude-sonnet-4-5") && (model.BanditCalls != 1 || model.EWMATTFTMS != 500) {
			t.Fatalf("missing online aggregate: %+v", model)
		}
		if model.Model == "gemini-2.5-flash" && model.BanditCalls != 0 {
			t.Fatalf("personal observations leaked: %+v", model)
		}
		if model.Model == "custom-model-00" && model.BanditCalls == 0 {
			foundUnused = true
		}
	}
	if !foundUnused {
		t.Fatal("models without calls must still appear")
	}
	blob, _ := json.Marshal(got)
	for _, private := range []string{"private-", "disabled-model", `"upstream_id"`, `"upstream_name"`, `"base_url"`, `"api_key"`, `"alpha"`, `"beta"`} {
		if strings.Contains(string(blob), private) {
			t.Fatalf("public rankings disclose %s", private)
		}
	}
}

func TestPublicRankingsPageAPIAndAssets(t *testing.T) {
	setupAutoRouterTestDB(t)
	r := gin.New()
	registerPublic(r)
	for _, path := range []string{"/public/rankings/", "/api/public/rankings", "/public/static/rankings.js", "/public/static/rankings.css"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %d / %s", path, w.Code, w.Body)
		}
		if path == "/public/rankings/" && (!strings.Contains(w.Body.String(), "完整模型榜单") || strings.Contains(w.Body.String(), "{{SITE_TITLE}}")) {
			t.Fatalf("ranking page not branded: %s", w.Body)
		}
		if path == "/api/public/rankings" {
			var got PublicRankings
			if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Models == nil || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid empty public API: %s", w.Body)
			}
		}
	}
	// Verify the embedded homepage links to the actual public page route.
	home, err := webFS.ReadFile("web/public/index.html")
	if err != nil || !strings.Contains(string(home), `href="/public/rankings/"`) {
		t.Fatalf("homepage ranking entry missing: %v", err)
	}
}
