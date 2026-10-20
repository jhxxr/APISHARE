package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUpstreamOverviewUsageAndHealth(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Date(2026, 10, 20, 0, 5, 0, 0, beijingLocation)
	live := modelOverviewTestUpstream(t, "live", "alpha", 1, true)
	disabled := modelOverviewTestUpstream(t, "disabled", "alpha", 1, false)
	insert := func(id int64, status int, at time.Time, tokens, firstToken int64, cost float64) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO call_logs(upstream_id,model,status,prompt_tokens,completion_tokens,first_token_ms,cost_usd,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, "alpha", status, tokens, 2, firstToken, cost, at.Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
	}
	// UTC timestamps must still contribute to the correct Beijing day.
	insert(live.ID, 200, now.Add(-2*time.Minute).UTC(), 10, 100, .02)
	insert(live.ID, 500, now.Add(-time.Minute), 5, 900, 9)
	insert(live.ID, 200, now.Add(-24*time.Hour), 20, 300, .03)
	insert(live.ID, 200, now.Add(-48*time.Hour), 30, 0, .04)
	insert(disabled.ID, 200, now, 7, 200, .05)
	// Deleted, unassigned, future and older-than-window calls are excluded.
	for _, id := range []int64{0, 999} {
		insert(id, 200, now, 900, 1000, 90)
	}
	insert(live.ID, 200, now.Add(time.Minute), 900, 1000, 90)
	insert(live.ID, 200, now.AddDate(0, 0, -8), 900, 1000, 90)
	result, err := buildUpstreamOverview(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Upstreams) != 2 {
		t.Fatalf("upstreams = %d", len(result.Upstreams))
	}
	var item upstreamOverviewItem
	for _, row := range result.Upstreams {
		if row.UpstreamID == live.ID {
			item = row
		}
	}
	if item.Today.Calls != 2 || item.Today.Successes != 1 || item.Today.PromptTokens != 15 || item.Today.CompletionTokens != 4 || item.Today.CostUSD != .02 {
		t.Fatalf("today = %+v", item.Today)
	}
	if item.Week.Calls != 4 || item.Week.Successes != 3 || item.Week.FirstTokenMS == nil || *item.Week.FirstTokenMS != 200 {
		t.Fatalf("week = %+v", item.Week)
	}
	if len(item.Health) != 20 || item.Health[19].Calls != 2 || item.Health[19].Successes != 1 || item.Health[0].Calls != 0 {
		t.Fatalf("health = %+v", item.Health)
	}
	if result.Week.Calls != 5 || result.Today.Calls != 3 || result.Today.Successes != 2 || result.Week.FirstTokenMS == nil || *result.Week.FirstTokenMS != 200 {
		t.Fatalf("summary = %+v", result)
	}
}

func TestUpstreamOverviewEmptyAndAuthenticated(t *testing.T) {
	r := testProxyAdminRouter(t)
	if got := proxyAdminRequest(r, "GET", "upstreams/overview", "", false); got.Code != 401 {
		t.Fatalf("unauthenticated = %d", got.Code)
	}
	modelOverviewTestUpstream(t, "unused", "alpha", 1, true)
	w := proxyAdminRequest(r, "GET", "upstreams/overview", "", true)
	if w.Code != 200 {
		t.Fatalf("overview = %d %s", w.Code, w.Body.String())
	}
	var result upstreamOverview
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Today.Calls != 0 || result.Week.FirstTokenMS != nil || len(result.Upstreams) != 1 || len(result.Upstreams[0].Health) != 20 {
		t.Fatalf("empty overview = %+v", result)
	}
	if strings.Contains(w.Body.String(), "api_key") || strings.Contains(w.Body.String(), "sk-unused") {
		t.Fatal("overview leaked credentials")
	}
}
