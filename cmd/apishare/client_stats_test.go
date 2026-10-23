package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func insertClientStatFixture(t *testing.T, id string, status int, prompt, completion int, cost float64, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO call_logs(client_id,model,status,prompt_tokens,completion_tokens,cost_usd,created_at)
		VALUES(?,'client-model',?,?,?,?,?)`, id, status, prompt, completion, cost, at.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

func TestClientStatsSurviveRetention(t *testing.T) {
	modelsTestDB(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
	insertClientStatFixture(t, "codex", 200, 10, 5, 1, now.AddDate(0, 0, -40))
	insertClientStatFixture(t, "codex", 503, 2, 3, 99, now)
	insertClientStatFixture(t, "cherry_studio", 200, 20, 10, 2, now.AddDate(0, 0, -2))
	insertClientStatFixture(t, "unknown", 200, 1, 1, .5, now)
	query := func(period string) *clientStatsResult {
		t.Helper()
		result, err := queryClientStats(context.Background(), period, now)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := query("all")
	if before.Summary != (logQuerySummary{Calls: 4, Successes: 3, PromptTokens: 33, CompletionTokens: 19, CostUSD: 3.5}) {
		t.Fatalf("client totals = %+v", before)
	}
	recent := query("30d")
	if recent.Summary.Calls != 3 || recent.Summary.CostUSD != 2.5 || query("today").Summary.Calls != 2 {
		t.Fatalf("date filters = %+v", recent)
	}
	for pass, want := range []int64{2, 0} {
		deleted, err := rollupExpiredLogs(now.AddDate(0, 0, -1).Unix())
		if err != nil || deleted != want {
			t.Fatalf("rollup %d: %d / %v", pass, deleted, err)
		}
		if after := query("all"); !reflect.DeepEqual(before, after) {
			t.Fatalf("retention changed client statistics: before=%+v after=%+v", before, after)
		}
		if after := query("30d"); !reflect.DeepEqual(recent, after) {
			t.Fatalf("retention changed date-filtered statistics: before=%+v after=%+v", recent, after)
		}
	}
	insertClientStatFixture(t, "codex", 200, 4, 2, .25, now.AddDate(0, 0, -40).Add(time.Hour))
	if _, err := rollupExpiredLogs(now.AddDate(0, 0, -1).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	after := query("all")
	if after.Summary.Calls != 5 || after.Summary.CostUSD != 3.75 || after.Items[0].ID != "codex" || after.Items[0].Calls != 3 {
		t.Fatalf("incremental rollup or migration double counted history: %+v", after)
	}
}

func TestClientRetentionRollsBackBothAggregates(t *testing.T) {
	modelsTestDB(t)
	now := beijingNow()
	insertClientStatFixture(t, "codex", 200, 10, 5, 1, now.AddDate(0, 0, -40))
	if _, err := db.Exec(`CREATE TRIGGER reject_client_rollup BEFORE INSERT ON call_log_client_daily
		BEGIN SELECT RAISE(ABORT,'client rollup rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := rollupExpiredLogs(now.Unix()); err == nil {
		t.Fatal("client rollup failure did not abort retention")
	}
	for _, table := range []string{"call_log_daily", "call_log_client_daily"} {
		if count := retentionRowCount(t, table); count != 0 {
			t.Fatalf("partial archive committed: %s=%d", table, count)
		}
	}
	if count := retentionRowCount(t, "call_logs"); count != 1 {
		t.Fatal("detail deleted despite failed archive")
	}
}

func TestRequestClientMigrationSeedsLegacyOnce(t *testing.T) {
	modelsTestDB(t)
	now := beijingNow()
	insertClientStatFixture(t, "unknown", 200, 7, 3, 1, now)
	_, err := db.Exec(`DROP INDEX idx_logs_client_epoch;
		ALTER TABLE call_logs DROP COLUMN client_id;
		ALTER TABLE call_logs DROP COLUMN client_json;
		DROP TABLE call_log_client_daily;
		DELETE FROM settings WHERE k='migration_request_client_v1';
		INSERT INTO call_log_daily(day,model,calls,successes,prompt_tokens,completion_tokens,cost_usd,last_at)
		 VALUES('2020-01-01','legacy-a',3,2,30,20,4,'2020-01-01T06:00:00Z'),
		       ('2020-01-01','legacy-b',2,1,10,5,2,'2020-01-01T13:00:00+08:00');`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
	}
	var archivedLastAt string
	if err := db.QueryRow(`SELECT last_at FROM call_log_client_daily WHERE day='2020-01-01' AND client_id='unknown'`).Scan(&archivedLastAt); err != nil || archivedLastAt != "2020-01-01T14:00:00+08:00" {
		t.Fatalf("mixed-offset legacy last call = %q / %v", archivedLastAt, err)
	}
	stats, err := queryClientStats(context.Background(), "all", now)
	if err != nil || len(stats.Items) != 1 || stats.Items[0].ID != "unknown" ||
		stats.Summary != (logQuerySummary{Calls: 6, Successes: 4, PromptTokens: 47, CompletionTokens: 28, CostUSD: 7}) {
		t.Fatalf("legacy history lost or counted twice: %+v / %v", stats, err)
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].Client.ID != "unknown" || logs[0].Client.RuleVersion != 0 || logs[0].Client.UserAgent != "" {
		t.Fatalf("legacy log acquired invented identity: %+v / %v", logs, err)
	}
}

func TestClientLogFiltersExportAndStatsAuth(t *testing.T) {
	router := testProxyAdminRouter(t)
	for _, path := range []string{"clients", "client-stats"} {
		if response := proxyAdminRequest(router, "GET", path, "", false); response.Code != 401 {
			t.Fatalf("unauthenticated %s = %d", path, response.Code)
		}
	}
	for _, path := range []string{"client-stats?period=invalid", "logs?client_id=invalid"} {
		if response := proxyAdminRequest(router, "GET", path, "", true); response.Code != 400 {
			t.Fatalf("%s = %d", path, response.Code)
		}
	}
	for _, ua := range []string{"codex_cli_rs/1.2.3 (Linux 6.1; x64) terminal/1.0", "OpenAI/JS 6.0.0", "=HYPERLINK(\"example\")"} {
		if err := dbInsertLog(&CallLog{Client: detectRequestClient(http.Header{"User-Agent": {ua}}), Model: "client-model", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		values url.Values
		total  int64
	}{
		{url.Values{"client_id": {"codex"}, "client_version": {"1.2.3"}}, 1},
		{url.Values{"client_id": {"unknown"}}, 2},
		{url.Values{"sdk_id": {"openai_js"}}, 1},
		{url.Values{"q": {"OpenAI JavaScript SDK"}}, 1},
		{url.Values{"q": {"terminal/1.0"}}, 1},
		{url.Values{"client_version": {"' OR 1=1 --"}}, 0},
	} {
		query, err := parseLogQuery(tc.values)
		if err != nil {
			t.Fatal(err)
		}
		result, err := queryLogs(context.Background(), query)
		if err != nil || result.Total != tc.total {
			t.Fatalf("%v = %+v / %v", tc.values, result, err)
		}
	}
	response := proxyAdminRequest(router, "GET", "logs?client_id=unknown&export=csv", "", true)
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(response.Body.String(), "\ufeff"))).ReadAll()
	if response.Code != 200 || err != nil || len(records) != 3 || len(records[0]) != 22 {
		t.Fatalf("client CSV = %d %v / %v", response.Code, records, err)
	}
	if !strings.HasPrefix(records[1][18], "'=") {
		t.Fatalf("UA formula was not escaped: %v", records[1])
	}
	response = proxyAdminRequest(router, "GET", "client-stats?period=all", "", true)
	var stats clientStatsResult
	if err := json.Unmarshal(response.Body.Bytes(), &stats); response.Code != 200 || err != nil || stats.Summary.Calls != 3 || len(stats.Items) != 2 {
		t.Fatalf("client stats API = %d %+v / %v", response.Code, stats, err)
	}
}
