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
	if !got.Models[0].SiteAvailable || !got.Models[1].SiteAvailable || got.Models[2].SiteAvailable || got.Models[3].SiteAvailable || got.Models[63].SiteAvailable {
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
