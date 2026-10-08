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
	if err := openDB(filepath.Join(t.TempDir(), "public.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}
func publicTestKey(t *testing.T, name string, quota, used float64, home bool) *ApiKey {
	t.Helper()
	k := &ApiKey{Name: name, Key: "sk-private-" + name, QuotaUSD: quota, UsedUSD: used, Enabled: true, QPS: 3, Concurrency: 5, ShowOnHome: home, Note: "private-note"}
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
func TestPublicOverviewSelectionPrivacyAndUptime(t *testing.T) {
	publicTestDB(t)
	now := time.Now()
	a := publicTestKey(t, "private-group-A", 100, 20, true)
	b := publicTestKey(t, "private-group-B", 50, 60, true)
	hidden := publicTestKey(t, "private-hidden-group", 999, 1, false)
	u := &Upstream{Name: "private-upstream-name", Type: "openai", BaseURL: "https://private.example.invalid", APIKey: "private-upstream-credential", Weight: 1, Enabled: true, Models: "gpt-alpha,claude-beta,gemini-unused"}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, a, "gpt-alpha", 200, 100, 20, now.Add(-2*time.Hour))
	publicTestLog(t, b, "gpt-alpha", 503, 0, 0, now.Add(-time.Hour))
	publicTestLog(t, a, "claude-beta", 200, 20, 10, now.AddDate(0, 0, -35))
	publicTestLog(t, hidden, "gpt-alpha", 200, 999999, 999999, now)
	got, err := buildPublicOverview(now, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Summary.Configured || got.Summary.RemainingUSD == nil || *got.Summary.RemainingUSD != 80 {
		t.Fatalf("per-key clamped remainder: %+v", got.Summary)
	}
	if got.Summary.Calls != 3 || got.Summary.Successes != 2 || got.Summary.PromptTokens != 120 || got.Summary.CompletionTokens != 30 {
		t.Fatalf("selected usage only: %+v", got.Summary)
	}
	if got.Summary.Uptime == nil || *got.Summary.Uptime != 50 {
		t.Fatalf("24-hour request-weighted uptime: %+v", got.Summary)
	}
	if len(got.Daily) != 30 {
		t.Fatalf("history length %d", len(got.Daily))
	}
	var calls, success int64
	for _, d := range got.Daily {
		calls += d.Calls
		success += d.Successes
	}
	if calls != 2 || success != 1 {
		t.Fatalf("window includes old/private calls: %d/%d", success, calls)
	}
	models := make(map[string]PublicModel)
	for _, m := range got.Models {
		models[m.Model] = m
	}
	alpha := models["gpt-alpha"]
	if alpha.Status != "incident" || alpha.Uptime == nil || *alpha.Uptime != 50 || len(alpha.History) != 24 {
		t.Fatalf("failure feedback: %+v", alpha)
	}
	for _, name := range []string{"claude-beta", "gemini-unused"} {
		m := models[name]
		if m.Status != "idle" || m.Uptime != nil {
			t.Fatalf("no recent calls must be gray: %+v", m)
		}
	}
	encoded, _ := json.Marshal(got)
	for _, private := range []string{"sk-private", "private-group", "private-hidden", "private-upstream", "private-note", `"key"`, `"id"`, `"key_id"`, `"key_name"`, `"key_count"`, `"enabled_keys"`, `"api_key"`, `"base_url"`} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("public disclosure: %s", private)
		}
	}
	b.ShowOnHome = false
	if err := dbUpdateKey(b); err != nil {
		t.Fatal(err)
	}
	got, err = buildPublicOverview(now, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Calls != 2 || *got.Summary.Uptime != 100 {
		t.Fatalf("selection changes must apply immediately: %+v", got.Summary)
	}
	if k, err := dbGetKeyByKey(a.Key); err != nil || !k.ShowOnHome {
		t.Fatalf("auth read preserves selection: %+v %v", k, err)
	}
	keys, err := dbListKeys()
	if err != nil || len(keys) != 3 {
		t.Fatalf("admin listing: %v", err)
	}
}
func TestPublicOverviewEmptyAndUnlimited(t *testing.T) {
	publicTestDB(t)
	k := publicTestKey(t, "not-published", 10, 2, false)
	got, err := buildPublicOverview(time.Now(), "hour")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Configured || got.Summary.RemainingUSD != nil || got.Summary.Uptime != nil || got.Summary.Calls != 0 || len(got.Models) != 0 {
		t.Fatalf("empty publication: %+v", got)
	}
	k.ShowOnHome = true
	k.QuotaUSD = 0
	if err := dbUpdateKey(k); err != nil {
		t.Fatal(err)
	}
	publicTestKey(t, "finite-alongside-unlimited", 10, 3, true)
	got, err = buildPublicOverview(time.Now(), "hour")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Summary.Unlimited || got.Summary.RemainingUSD != nil || got.Summary.UsedUSD != 5 {
		t.Fatalf("unlimited must not pretend to be zero/finite: %+v", got.Summary)
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
func TestUnavailableRoutingRecordsFailure(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	k := publicTestKey(t, "published", 100, 0, true)
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
			wantRate := 4.0 / 7 * 100
			if got.Summary.Calls != 10 || got.Summary.Uptime == nil || math.Abs(*got.Summary.Uptime-wantRate) > 1e-9 {
				t.Fatalf("24-hour cutoff, offsets, or private calls: %+v", got.Summary)
			}
			count, size := 24, time.Hour
			wantBuckets := map[int][2]int64{0: {3, 2}, 1: {1, 0}, 23: {3, 2}}
			if interval == "minute" {
				count, size = 1440, time.Minute
				wantBuckets = map[int][2]int64{0: {2, 1}, 1: {1, 1}, 60: {1, 0}, 1438: {1, 1}, 1439: {2, 1}}
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
				if m.Model == "gpt-clock" && (calls != 7 || successes != 4 || m.Uptime == nil || math.Abs(*m.Uptime-wantRate) > 1e-9) {
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
