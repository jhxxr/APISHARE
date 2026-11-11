package main

import (
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setArenaFixture(t *testing.T, board string, models []arenaModel) {
	t.Helper()
	getArenaSnapshot(db, board)
	if err := storeArenaSnapshot(db, board, &arenaSnapshot{PublishedAt: "2026-11-09", FetchedAt: time.Now().UTC().Format(time.RFC3339), Models: models}); err != nil {
		t.Fatal(err)
	}
	invalidatePublicOverviews()
}
func TestPublicRankingsFullSourceAndSiteBadges(t *testing.T) {
	setupAutoRouterTestDB(t)
	if err := dbInsertUpstream(&Upstream{Name: "private-upstream", Type: "openai", BaseURL: "https://private.invalid", APIKey: "private-key", Models: "anthropic/claude-sonnet-4-5,gpt-5.2-pro,public-alias", ModelMap: `{"public-alias":"gpt-5.2"}`, Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := dbInsertUpstream(&Upstream{Name: "disabled", Type: "openai", Models: "disabled-model", Weight: 1, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	var models []arenaModel
	models = append(models, arenaModel{Rank: 1, Model: "Claude Sonnet 4.5", Family: "Anthropic", Score: 1470}, arenaModel{Rank: 2, Model: "GPT-5.2", Family: "OpenAI", Score: 1450}, arenaModel{Rank: 2, Model: "GPT-5.2 (High)", Family: "OpenAI", Score: 1440}, arenaModel{Rank: 4, Model: "disabled-model", Family: "Example", Score: 1430})
	for i := 0; i < 60; i++ {
		models = append(models, arenaModel{Rank: i + 5, Model: fmt.Sprintf("external-%02d", i), Family: "Example", Score: 1400 - float64(i)})
	}
	setArenaFixture(t, "text", models)
	board, _ := findArenaBoard("text")
	got, err := buildPublicRankings(time.Now(), board)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 64 || got.Models[0].Rank != 1 || got.Models[2].Rank != 2 || got.Models[0].Score != 1470 {
		t.Fatalf("source ranks/scores altered or truncated: %+v", got)
	}
	if !got.Models[0].SiteAvailable || !got.Models[1].SiteAvailable || !got.Models[2].SiteAvailable || got.Models[3].SiteAvailable || got.Models[63].SiteAvailable {
		t.Fatalf("incorrect membership badges: %+v", got.Models[:4])
	}
	if !strings.Contains(strings.Join(got.Models[1].SiteModels, ","), "public-alias") {
		t.Fatal("explicit site alias not matched")
	}
	blob, _ := json.Marshal(got)
	for _, private := range []string{"private-", `"upstream_id"`, `"api_key"`, `"base_url"`, `"capabilities"`, `"bandit_win_rate"`} {
		if strings.Contains(string(blob), private) {
			t.Fatalf("private or inferred data leaked: %s", private)
		}
	}
}

func TestArenaPublicIdentityIgnoresOnlyThinkingNotVersions(t *testing.T) {
	for _, pair := range [][2]string{
		{"Claude Opus 4.6 (High)", "anthropic/claude-opus-4-6"},
		{"Muse Wonder (Max)", "muse-wonder"},
		{"DeepSeek-V3.2 (Thinking)", "deepseek-v3.2"},
		{"DeepSeek-V4-Pro (High, 20260813)", "deepseek-v4-pro-20260813"},
		{"DeepSeek-V4.1-Flash-1017 (Max)", "deepseek-v4.1-flash-20261017"},
	} {
		if arenaPublicModelIdentity(pair[0]) != arenaPublicModelIdentity(pair[1]) {
			t.Fatalf("public model-level match missed: %v", pair)
		}
		if arenaModelIdentity(pair[0]) == arenaModelIdentity(pair[1]) {
			t.Fatalf("public matching weakened the router's exact identity: %v", pair)
		}
	}
	for _, pair := range [][2]string{
		{"gpt-5.2", "gpt-5.2-pro"}, {"gpt-5.2", "gpt-5.2-mini"},
		{"deepseek-v4.1-flash-20260910 (Max)", "deepseek-v4.1-flash-1017"},
		{"deepseek-r1 (0528)", "deepseek-r1"},
		{"qwen-max", "qwen"}, {"muse-wonder-preview (Max)", "muse-wonder"},
		{"model-high", "model"}, {"model (Preview)", "model"},
	} {
		if arenaPublicModelIdentity(pair[0]) == arenaPublicModelIdentity(pair[1]) {
			t.Fatalf("unrelated version/product matched: %v", pair)
		}
	}
}

func TestArenaPublicDeepSeekOfficial1017DoesNotBorrow0910Score(t *testing.T) {
	setupAutoRouterTestDB(t)
	for _, up := range []*Upstream{
		{Name: "official", Type: "openai", BaseURL: "https://api.deepseek.com/v1", Models: "deepseek-flash,deepseek-v4-flash,deepseek-v4-pro", Weight: 1, Enabled: true},
		{Name: "third-party", Type: "openai", BaseURL: "https://example.invalid", Models: "third-party-flash", ModelMap: `{"third-party-flash":"deepseek-flash"}`, Weight: 1, Enabled: true},
		{Name: "actual-mapping", Type: "openai", BaseURL: "https://example.invalid", Models: "gpt-5.2", ModelMap: `{"gpt-5.2":"gpt-5.1"}`, Weight: 1, Enabled: true},
		{Name: "hidden-deployment", Type: "openai", BaseURL: "https://example.invalid", Models: "visible-client-alias", ModelMap: `{"visible-client-alias":"private-deployment-id"}`, Weight: 1, Enabled: true},
	} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	models := []arenaModel{
		{Rank: 1, Model: "DeepSeek-V4.1-Flash-20260910 (Max)", Score: 1500},
		{Rank: 2, Model: "DeepSeek-V4-Pro", Score: 1400},
		{Rank: 3, Model: "GPT-5.2 (High)", Score: 1300},
		{Rank: 4, Model: "GPT-5.1 (High)", Score: 1200},
	}
	setArenaFixture(t, "text", models)
	board, _ := findArenaBoard("text")
	got, err := buildPublicRankings(time.Now(), board)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models[0].SiteAvailable || len(got.Models[0].RelatedSiteModels) != 3 || got.Models[0].Score != 1500 {
		t.Fatalf("official 1017 falsely inherited old 0910 result: %+v", got.Models[0])
	}
	if got.Models[1].SiteAvailable || got.Models[2].SiteAvailable || !got.Models[3].SiteAvailable {
		t.Fatalf("request aliases overrode actual upstream identities: %+v", got.Models)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "private-deployment-id") {
		t.Fatal("unranked metadata exposed an unpublished mapped deployment ID")
	}
	for _, pending := range got.UnrankedSiteModels {
		if pending.Model == "deepseek-flash" && (!strings.Contains(pending.Note, "1017") || pending.Versions[0] != "DeepSeek-V4.1-Flash-20261017") {
			t.Fatalf("missing official version explanation: %+v", pending)
		}
		if pending.Model == "third-party-flash" && strings.Contains(strings.Join(pending.Versions, ","), "1017") {
			t.Fatal("third-party gateway inherited an unverified official alias")
		}
	}
	// When the source actually contains the release, only that row is marked.
	models = append(models, arenaModel{Rank: 5, Model: "DeepSeek-V4.1-Flash-1017 (Max)", Score: 1100})
	setArenaFixture(t, "text", models)
	got, err = buildPublicRankings(time.Now(), board)
	if err != nil || !got.Models[4].SiteAvailable || len(got.Models[4].SiteModels) != 3 || got.Models[0].SiteAvailable {
		t.Fatalf("official 1017 aliases not matched to the precise dated row: %+v / %v", got, err)
	}
	for _, pending := range got.UnrankedSiteModels {
		if pending.Model == "deepseek-flash" {
			t.Fatal("a matched 1017 release still appeared as unranked")
		}
	}
}

func TestArenaOfficialDeepSeekAliasRequiresTrustedHost(t *testing.T) {
	for _, host := range []string{"https://api.deepseek.com.evil.invalid", "https://deepseek.com", "http://api.deepseek.com", "https://api.deepseek.com:8443"} {
		got, official := arenaOfficialDeepSeekVersion(&Upstream{BaseURL: host}, "deepseek-flash")
		if official || got != "deepseek-flash" {
			t.Fatalf("unverified endpoint inherited official version: %s", host)
		}
	}
}
func TestArenaModelIdentityIsConservative(t *testing.T) {
	for _, pair := range [][2]string{{"Claude Sonnet 4.5", "anthropic/claude-sonnet-4-5"}, {"GPT-5.2 (High)", "gpt-5.2(high)"}} {
		if arenaModelIdentity(pair[0]) != arenaModelIdentity(pair[1]) {
			t.Fatalf("presentation match failed: %v", pair)
		}
	}
	for _, pair := range [][2]string{{"GPT-5.2", "gpt-5.2-pro"}, {"GPT-5.2", "gpt-5.2-2026-01-01"}, {"GPT-5.2", "gpt-5.2(high)"}, {"gpt-5.2-mini", "gpt-5.2"}} {
		if arenaModelIdentity(pair[0]) == arenaModelIdentity(pair[1]) {
			t.Fatalf("variants conflated: %v", pair)
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
		if path == "/api/public/rankings" {
			var got PublicRankings
			if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Models == nil || len(got.Models) != 0 || got.SourceURL != arenaDatasetURL {
				t.Fatalf("missing source should not invent rankings: %s", w.Body)
			}
		}
		if path == "/public/rankings/" && (!strings.Contains(w.Body.String(), "完整模型榜单") || strings.Contains(w.Body.String(), "{{SITE_TITLE}}")) {
			t.Fatal("page branding or content missing")
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/public/rankings?board=not-a-board", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unvalidated board: %d", w.Code)
	}
	home, _ := webFS.ReadFile("web/public/index.html")
	if !strings.Contains(string(home), `href="/public/rankings/"`) {
		t.Fatal("homepage entry missing")
	}
}
