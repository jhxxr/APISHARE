package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func publicTestDB(t *testing.T) {
	t.Helper()
	resetUpstreamHealthForTest()
	if err := openDB(filepath.Join(t.TempDir(), "public.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}
func publicTestKey(t *testing.T, name string, quota, used float64, home bool) *ApiKey {
	t.Helper()
	k := &ApiKey{Name: name, Key: "sk-private-" + name, QuotaUSD: quota, UsedUSD: used, Enabled: true, QPS: 3, Concurrency: 5, ShowOnHome: home, Note: "private-note", Models: "gpt-alpha,claude-beta,gemini-unused"}
	if err := dbInsertKey(k); err != nil {
		t.Fatal(err)
	}
	return k
}
func publicTestLog(t *testing.T, key *ApiKey, model string, status int, pt, ct int, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO call_logs(key_id,key_name,upstream_name,model,status,prompt_tokens,completion_tokens,error,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		key.ID, key.Name, "private-upstream-name", model, status, pt, ct, "private-upstream-error", at.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}
func TestPublicOverviewSiteWidePrivacyAndUptime(t *testing.T) {
	publicTestDB(t)
	now := time.Now()
	a := publicTestKey(t, "private-group-A", 100, 20, true)
	b := publicTestKey(t, "private-group-B", 50, 60, false)
	deleted := publicTestKey(t, "private-deleted", 999, 1, false)
	if err := dbInsertUpstream(&Upstream{Name: "private-upstream", Type: "openai", BaseURL: "https://private.invalid", APIKey: "private-credential", Weight: 1, Enabled: true, Models: "gpt-alpha,claude-beta,gemini-unused"}); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, a, "gpt-alpha", 200, 100, 20, now.Add(-2*time.Hour))
	publicTestLog(t, b, "gpt-alpha", 503, 0, 0, now.Add(-time.Hour))
	publicTestLog(t, a, "claude-beta", 200, 20, 10, now.AddDate(0, 0, -35))
	publicTestLog(t, deleted, "gpt-alpha", 200, 40, 30, now)
	if err := dbDeleteKey(deleted.ID); err != nil {
		t.Fatal(err)
	}
	b.Enabled = false
	if err := dbUpdateKey(b); err != nil {
		t.Fatal(err)
	}
	read := func() *PublicOverview {
		t.Helper()
		got, err := buildPublicOverview(now, "hour")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read()
	if got.Summary.Calls != 4 || got.Summary.Successes != 3 || got.Summary.PromptTokens != 160 || got.Summary.CompletionTokens != 60 {
		t.Fatalf("site-wide totals: %+v", got.Summary)
	}
	if got.Summary.Uptime == nil || math.Abs(*got.Summary.Uptime-200.0/3) > 1e-9 {
		t.Fatalf("weighted uptime: %+v", got.Summary)
	}
	var calls, successes int64
	for _, day := range got.Daily {
		calls += day.Calls
		successes += day.Successes
	}
	if calls != 3 || successes != 2 || len(got.Daily) != 30 {
		t.Fatalf("rolling daily totals: %d/%d", successes, calls)
	}
	for _, model := range got.Models {
		if model.Model == "gpt-alpha" && (model.Status != "operational" || model.Uptime == nil || math.Abs(*model.Uptime-200.0/3) > 1e-9 || len(model.History) != 24) {
			t.Fatalf("model uptime: %+v", model)
		}
		if model.Model == "gemini-unused" && (model.Status != "idle" || model.Uptime != nil) {
			t.Fatalf("unused model: %+v", model)
		}
	}
	encoded, _ := json.Marshal(got)
	for _, private := range []string{"sk-private", "private-group", "private-upstream", "private-note", `"key"`, `"id"`, `"key_id"`, `"key_name"`, `"key_count"`, `"api_key"`, `"base_url"`, `"quota"`, `"quota_usd"`, `"used_usd"`, `"remaining_usd"`, `"unlimited"`} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("public disclosure: %s", private)
		}
	}
	a.ShowOnHome = false
	if err := dbUpdateKey(a); err != nil {
		t.Fatal(err)
	}
	if next := read(); next.Summary.Calls != 4 || *next.Summary.Uptime != *got.Summary.Uptime {
		t.Fatal("legacy visibility flag changed site-wide telemetry")
	}
}

func TestPublicOverviewEmptyNeverExposesBalances(t *testing.T) {
	publicTestDB(t)
	publicTestKey(t, "finite", 10, 2, false)
	publicTestKey(t, "unlimited", 0, 3, true)
	got, err := buildPublicOverview(time.Now(), "hour")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Calls != 0 || got.Summary.Uptime != nil || got.Summary.PromptTokens != 0 || len(got.Models) != 0 {
		t.Fatalf("empty telemetry: %+v", got)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "usd") || strings.Contains(string(encoded), "quota") {
		t.Fatalf("anonymous balances: %s", encoded)
	}
}
func TestPublicMigrationKeepsExistingKeysPrivate(t *testing.T) {
	d, err := sql.Open("sqlite", "file:"+filepathToSlash(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	db = d
	t.Cleanup(func() { d.Close() })
	_, err = db.Exec(`CREATE TABLE api_keys(id INTEGER PRIMARY KEY,name TEXT NOT NULL DEFAULT '',key TEXT NOT NULL UNIQUE,quota_usd REAL NOT NULL DEFAULT 0,used_usd REAL NOT NULL DEFAULT 0,qps REAL NOT NULL DEFAULT 3,concurrency INTEGER NOT NULL DEFAULT 5,enabled INTEGER NOT NULL DEFAULT 1,note TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT '');
 INSERT INTO api_keys(id,name,key,quota_usd,used_usd) VALUES(7,'legacy','sk-legacy-private',42,3)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
	}
	k, err := dbGetKeyByID(7)
	if err != nil || k.ShowOnHome || k.QuotaUSD != 42 || k.UsedUSD != 3 || k.Key != "sk-legacy-private" {
		t.Fatalf("legacy data changed: %+v %v", k, err)
	}
}
func TestPublicHTTPAndPrivateAdmin(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerPublic(r)
	registerAdmin(r)
	for _, url := range []string{"/", "/public/static/home.css", "/public/static/home.js", "/api/public/overview", "/api/public/overview?interval=hour", "/api/public/overview?interval=minute"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status %d", url, w.Code)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing nosniff: %s", url)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", adminPath+"/api/keys", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("admin keys became public: %d", w.Code)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/public/overview", nil))
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public metrics must not remain cached after selection changes")
	}
	var overview PublicOverview
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.HistoryInterval != "hour" || overview.HistoryWindowMinutes != 1440 {
		t.Fatalf("default hourly 24-hour window: %+v", overview)
	}
	for _, interval := range []string{"day", "", "invalid"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/public/overview?interval="+interval, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unsupported interval %q accepted: %d", interval, w.Code)
		}
	}
}
func TestArchivedStationIsUnavailable(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	registerPublic(r)
	for _, path := range []string{"/metro", "/public/static/metro.html", "/public/static/metro.js", "/public/static/metro.css", "/public/static/vendor/three-0.186.1/three.module.js", "/public/static/textures/train-brushed-metal-v1.webp"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("archived station served: %s, %d", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/public/static/home.js", "/public/static/home.css"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if strings.Contains(w.Body.String(), "/metro") || strings.Contains(w.Body.String(), "three.module") {
			t.Fatalf("active page references station: %s", path)
		}
	}
}
func TestUnavailableRoutingRecordsFailure(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	k := publicTestKey(t, "published", 100, 0, true)
	k.Models = "text-embedding-fixture"
	r := gin.New()
	r.POST("/call", func(c *gin.Context) { c.Set("apiKey", *k); c.Next() }, relayHandler(FormatOpenAIChat, true))
	w := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/call", strings.NewReader(`{"model":"text-embedding-fixture","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, request)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("routing failure response: %d", w.Code)
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].Status != 502 || logs[0].KeyID != k.ID {
		t.Fatalf("missing failure log: %+v %v", logs, err)
	}
}
func TestInterruptedStreamIsNotSuccessfulUptime(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	k := publicTestKey(t, "published", 100, 0, true)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	task := &relayTask{format: FormatOpenAIChat, model: "text-embedding-fixture", stream: true}
	finalizeCall(c, *k, task, relayInfo{Status: 200, PromptTok: 2, CompletionTok: 3, InKnown: true, OutKnown: true}, true, errors.New("unexpected EOF"), 2)
	got, err := buildPublicOverview(time.Now(), "hour")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Calls != 1 || got.Summary.Successes != 0 || got.Summary.Uptime == nil || math.Abs(*got.Summary.Uptime) > 1e-9 {
		t.Fatalf("interrupted stream reported as healthy: %+v", got.Summary)
	}
	if got.Summary.PromptTokens != 2 || got.Summary.CompletionTokens != 3 || got.Daily[29].PromptTokens != 2 || got.Daily[29].CompletionTokens != 3 {
		t.Fatalf("partially consumed stream tokens were lost: %+v %+v", got.Summary, got.Daily[29])
	}
}

func TestPublicHistoryRolling24HoursAtBothResolutions(t *testing.T) {
	publicTestDB(t)
	now := time.Date(2026, 10, 8, 0, 30, 45, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	start := now.Add(-24 * time.Hour)
	k := publicTestKey(t, "published-clock", 100, 0, true)
	hidden := publicTestKey(t, "hidden-clock", 100, 0, false)
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: "gpt-clock,gpt-idle"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		at     time.Time
		status int
	}{
		{start.Add(-time.Second), 503},
		{start, 200},
		{start.Add(59 * time.Second), 503},
		{start.Add(time.Minute), 200},
		{start.Add(time.Hour), 503},
		{now.Add(-2 * time.Minute).UTC(), 200},
		{now.Add(-time.Minute), 503},
		{now, 200},
		{now.Add(time.Second), 503},
		{start.Add(-time.Hour), 200},
	} {
		publicTestLog(t, k, "gpt-clock", call.status, 0, 0, call.at)
	}
	publicTestLog(t, hidden, "gpt-clock", 503, 0, 0, now.Add(-time.Minute))
	for _, interval := range []string{"hour", "minute"} {
		t.Run(interval, func(t *testing.T) {
			got, err := buildPublicOverview(now, interval)
			if err != nil {
				t.Fatal(err)
			}
			if got.HistoryInterval != interval || got.HistoryWindowMinutes != 1440 {
				t.Fatalf("resolution changed the total window: %+v", got)
			}
			wantRate := 4.0 / 8 * 100
			if got.Summary.Calls != 11 || got.Summary.Uptime == nil || math.Abs(*got.Summary.Uptime-wantRate) > 1e-9 {
				t.Fatalf("24-hour cutoff, offsets, or private calls: %+v", got.Summary)
			}
			count, size := 24, time.Hour
			wantBuckets := map[int][2]int64{0: {3, 2}, 1: {1, 0}, 23: {4, 2}}
			if interval == "minute" {
				count, size = 1440, time.Minute
				wantBuckets = map[int][2]int64{0: {2, 1}, 1: {1, 1}, 60: {1, 0}, 1438: {1, 1}, 1439: {3, 1}}
			}
			for _, m := range got.Models {
				if len(m.History) != count || m.History[0].Start != start.Format(time.RFC3339) || m.History[count-1].End != now.Format(time.RFC3339) {
					t.Fatalf("history must span exactly 24 hours: %s, %d buckets", m.Model, len(m.History))
				}
				var calls, successes int64
				for i, bucket := range m.History {
					wantStart := start.Add(time.Duration(i) * size)
					if bucket.Start != wantStart.Format(time.RFC3339) || bucket.End != wantStart.Add(size).Format(time.RFC3339) {
						t.Fatalf("noncontiguous %s interval %d: %+v", interval, i, bucket)
					}
					want := wantBuckets[i]
					if m.Model == "gpt-idle" {
						want = [2]int64{}
					}
					if bucket.Calls != want[0] || bucket.Successes != want[1] {
						t.Fatalf("%s bucket %d: %+v, want %v", m.Model, i, bucket, want)
					}
					calls += bucket.Calls
					successes += bucket.Successes
				}
				if m.Model == "gpt-clock" && (calls != 8 || successes != 4 || m.Uptime == nil || math.Abs(*m.Uptime-wantRate) > 1e-9) {
					t.Fatalf("request-weighted model uptime changed with resolution: %+v", m)
				}
				if m.Model == "gpt-clock" && (m.Status != "operational" || m.LastCall != now.Format(time.RFC3339)) {
					t.Fatalf("late imports or future timestamps replaced the latest real call: %+v", m)
				}
				if m.Model == "gpt-idle" && (calls != 0 || m.Uptime != nil || m.Status != "idle") {
					t.Fatalf("empty intervals must remain unknown: %+v", m)
				}
			}
		})
	}
}
