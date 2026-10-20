package main

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestDashboardAggregatesUsageCacheAndRankings(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Date(2026, 10, 20, 12, 30, 0, 0, beijingLocation)
	insert := func(keyID int64, keyName string, upstreamID int64, upstreamName, model string, status int, at time.Time, prompt, completion int64, cost float64, duration int64, billing string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO call_logs(key_id,key_name,upstream_id,upstream_name,model,status,created_at,prompt_tokens,completion_tokens,cost_usd,duration_ms,billing_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, keyID, keyName, upstreamID, upstreamName, model, status, at.Format(time.RFC3339), prompt, completion, cost, duration, billing)
		if err != nil {
			t.Fatal(err)
		}
	}
	billing := `{"cost":{"cache_read":0.5,"cache_write":0.2,"input":1.5,"output":0.3,"total":2.5},"usage":{"cache_read":200,"input":100,"cache_write":50}}`
	insert(1, "alpha", 10, "prod", "model-a", 200, time.Date(2026, 10, 20, 10, 10, 0, 0, beijingLocation), 100, 50, 2.5, 400, billing)
	insert(1, "alpha", 10, "prod", "model-a", 500, time.Date(2026, 10, 20, 9, 0, 0, 0, beijingLocation), 0, 0, 0, 100, "")
	insert(1, "alpha", 10, "prod", "model-a", 200, time.Date(2026, 10, 19, 10, 10, 0, 0, beijingLocation), 40, 10, 1.0, 200, `{"cost":{"cache_read":0.1},"usage":{"cache_read":50,"input":50}}`)
	insert(2, "beta", 20, "backup", "model-b", 200, now.Add(-20*time.Second), 10, 5, 0.1, 50, "")
	insert(2, "beta", 20, "backup", "model-b", 200, now.Add(-90*time.Second), 8, 4, 0.05, 60, "")

	data, err := buildDashboard(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(data.KPI.RPM.Value-0.4) > 1e-9 || data.KPI.RPM.Change != nil {
		t.Fatalf("rpm = %+v", data.KPI.RPM)
	}
	if data.KPI.Cost.Value != 2.65 || data.KPI.Cost.Change == nil || math.Abs(*data.KPI.Cost.Change-165) > 1e-9 {
		t.Fatalf("cost = %+v", data.KPI.Cost)
	}
	if data.KPI.Requests.Value != 4 || data.KPI.Requests.Change == nil || math.Abs(*data.KPI.Requests.Change-300) > 1e-9 {
		t.Fatalf("requests = %+v", data.KPI.Requests)
	}
	if math.Abs(data.KPI.Success.Value-75) > 1e-9 || data.KPI.Success.Change == nil || math.Abs(*data.KPI.Success.Change+25) > 1e-9 {
		t.Fatalf("success = %+v", data.KPI.Success)
	}
	if data.KPI.Health.TPM != 15 || data.KPI.Health.ErrorRate != 0 || data.KPI.Health.P50MS != 60 {
		t.Fatalf("health = %+v", data.KPI.Health)
	}
	if len(data.Trend.Labels) != 24 || data.Trend.Labels[23] != "12:00" || data.Trend.TotalCalls != 4 || math.Abs(data.Trend.TotalCost-2.65) > 1e-9 {
		t.Fatalf("trend = %+v", data.Trend)
	}
	if data.Trend.Calls[21] != 1 || data.Trend.Calls[20] != 1 || data.Trend.Calls[23] != 2 || data.Trend.Success[21] != 1 || data.Trend.Failed[20] != 1 {
		t.Fatalf("trend buckets = %+v", data.Trend)
	}
	if math.Abs(data.Trend.CachedCost[21]-0.7) > 1e-9 || math.Abs(data.Trend.UncachedCost[21]-1.8) > 1e-9 {
		t.Fatalf("trend cost split = %+v", data.Trend)
	}
	if data.Cache.Read != 200 || data.Cache.Input != 100 || data.Cache.Write != 50 || math.Abs(data.Cache.Ratio-57.142857142857146) > 1e-9 {
		t.Fatalf("cache = %+v", data.Cache)
	}
	if len(data.Rankings.Keys) != 2 || data.Rankings.Keys[0].Name != "alpha" || data.Rankings.Keys[0].Cost != 2.5 || data.Rankings.Keys[0].Calls != 2 || data.Rankings.Keys[0].CostPrev != 1 {
		t.Fatalf("key ranking = %+v", data.Rankings.Keys)
	}
	// Renaming a key must not split its leaderboard row: the current name wins.
	key := &ApiKey{Name: "renamed", Key: "sk-renamed", Enabled: true}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE call_logs SET key_id=? WHERE key_name='alpha'`, key.ID); err != nil {
		t.Fatal(err)
	}
	data, err = buildDashboard(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Rankings.Keys) != 2 || data.Rankings.Keys[0].Name != "renamed" || data.Rankings.Keys[0].Cost != 2.5 {
		t.Fatalf("renamed key ranking = %+v", data.Rankings.Keys)
	}
	if data.Rankings.Keys[0].SeriesCost[21] != 2.5 || data.Rankings.Keys[0].SeriesCalls[21] != 1 || data.Rankings.Keys[0].SeriesCalls[20] != 1 {
		t.Fatalf("key series = %+v", data.Rankings.Keys[0])
	}
	if len(data.Rankings.Models) != 2 || len(data.Rankings.Upstreams) != 2 {
		t.Fatalf("rankings = %+v", data.Rankings)
	}
}

func TestDashboardEndpointAuthAndShape(t *testing.T) {
	r := testProxyAdminRouter(t)
	if got := proxyAdminRequest(r, "GET", "dashboard", "", false); got.Code != 401 {
		t.Fatalf("unauthenticated = %d", got.Code)
	}
	w := proxyAdminRequest(r, "GET", "dashboard", "", true)
	if w.Code != 200 {
		t.Fatalf("dashboard = %d %s", w.Code, w.Body.String())
	}
	var data dashboardData
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Trend.Labels) != 24 || data.Rankings.Keys == nil || data.Rankings.Models == nil || data.Rankings.Upstreams == nil {
		t.Fatalf("empty dashboard = %+v", data)
	}
}
