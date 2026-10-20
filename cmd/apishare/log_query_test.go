package main

import (
	"context"
	"encoding/csv"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLogQueryFiltersBeforePagination(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Date(2026, 10, 20, 0, 5, 0, 0, beijingLocation)
	insert := func(model, key, upstream string, status int, at time.Time, duration int64, billing string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO call_logs(model,key_id,key_name,upstream_id,upstream_name,status,created_at,prompt_tokens,completion_tokens,cost_usd,duration_ms,latency_ms,billing_json) VALUES(?,1,?,2,?,?,?,10,5,?,?,?,?)`, model, key, upstream, status, at.Format(time.RFC3339), 1.25, duration, 6000, billing)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("outside-page", "special", "provider", 200, now.UTC(), 100, `{"usage":{"cache_read":8,"reasoning":2}}`)
	for i := 0; i < 60; i++ {
		insert("ordinary", "normal", "provider", 200, now, 100, "")
	}
	insert("failed-model", "normal", "provider", 500, now, 6000, "invalid-json")
	insert("previous-day", "normal", "provider", 200, now.Add(-10*time.Minute), 100, "")
	query := func(values url.Values) *logQueryResult {
		t.Helper()
		parsed, err := parseLogQuery(values)
		if err != nil {
			t.Fatal(err)
		}
		result, err := queryLogs(context.Background(), parsed)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := query(url.Values{"q": {"special"}, "size": {"1"}})
	if result.Total != 1 || len(result.Items) != 1 || result.Items[0].Model != "outside-page" {
		t.Fatalf("search = %+v", result)
	}
	result = query(url.Values{"start": {"2026-10-20T00:00:00+08:00"}, "end": {"2026-10-20T23:59:59+08:00"}, "key_id": {"1"}, "upstream_id": {"2"}, "size": {"50"}})
	if result.Total != 62 || len(result.Items) != 50 || result.Summary.Successes != 61 || result.Summary.CostUSD != 76.25 || result.Summary.PromptTokens != 620 {
		t.Fatalf("range/summary = %+v", result)
	}
	result = query(url.Values{"features": {"cache,reasoning"}})
	if result.Total != 1 || result.Items[0].Model != "outside-page" {
		t.Fatalf("JSON features = %+v", result)
	}
	result = query(url.Values{"features": {"failed,slow"}})
	if result.Total != 1 || result.Items[0].Status != 500 {
		t.Fatalf("failed/slow = %+v", result)
	}
	result = query(url.Values{"features": {"slow"}, "model": {"outside-page"}})
	if result.Total != 0 {
		t.Fatal("duration must take precedence over legacy latency")
	}
	result = query(url.Values{"q": {"' OR 1=1 --"}})
	if result.Total != 0 {
		t.Fatal("query text must stay a bound literal")
	}
	result = query(url.Values{"q": {"special"}, "page": {"999"}})
	if result.Page != 1 || len(result.Items) != 1 {
		t.Fatalf("clamped page = %+v", result)
	}
	result = query(url.Values{"q": {"#1"}})
	if result.Total != 1 || result.Items[0].ID != 1 {
		t.Fatal("record ID search must be exact")
	}
}

func TestLogQueryValidationAndCSV(t *testing.T) {
	r := testProxyAdminRouter(t)
	for _, query := range []string{"start=invalid", "start=2026-10-21T00%3A00%3A00Z&end=2026-10-20T00%3A00%3A00Z", "status=invalid", "features=unknown", "key_id=-1", "export=json"} {
		w := proxyAdminRequest(r, "GET", "logs?"+query, "", true)
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
	if w := proxyAdminRequest(r, "GET", "logs?export=csv", "", false); w.Code != 401 {
		t.Fatalf("export auth = %d", w.Code)
	}
	_, err := db.Exec(`INSERT INTO call_logs(model,key_name,upstream_name,error,status,created_at) VALUES(?,?,?,?,?,?)`, "=1+1", "+formula", "@provider", "line1\nline2,\"quoted\"", 200, time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	w := proxyAdminRequest(r, "GET", "logs?export=csv", "", true)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("CSV = %d %s", w.Code, w.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1][2] != "'=1+1" || records[1][3] != "'@provider" || records[1][4] != "'+formula" || records[1][12] != "line1\nline2,\"quoted\"" {
		t.Fatalf("CSV fields = %v", records)
	}
	_, err = db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO call_logs(model) SELECT 'bulk' FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	w = proxyAdminRequest(r, "GET", "logs?export=csv", "", true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "10000") {
		t.Fatalf("export limit = %d %s", w.Code, w.Body.String())
	}
}
