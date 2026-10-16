package main

import (
	"path/filepath"
	"testing"
	"time"
)

// 调用明细的保留与汇总：超期的逐条详情被删除，但累计调用次数、Token、
// 费用与最后一次调用都汇总到 call_log_daily 后保留，不会丢失。
func retentionTestDB(t *testing.T) {
	t.Helper()
	resetUpstreamHealthForTest()
	if err := openDB(filepath.Join(t.TempDir(), "retention.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}

func retentionTestSettings(t *testing.T, days int) {
	t.Helper()
	previous := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previous
		settingsMu.Unlock()
	})
	settingsMu.Lock()
	settingsCache.LogRetentionDays = days
	settingsMu.Unlock()
	if err := persistSettings(getSettings()); err != nil {
		t.Fatal(err)
	}
}

func retentionTestLog(t *testing.T, model string, status int, pt, ct int, cost float64, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO call_logs(model,status,prompt_tokens,completion_tokens,cost_usd,created_at)
		VALUES(?,?,?,?,?,?)`, model, status, pt, ct, cost, at.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

func retentionRowCount(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLogRetentionRollupBeforeDelete(t *testing.T) {
	retentionTestDB(t)
	retentionTestSettings(t, 7)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	now := beijingNow()
	// 最近的明细（保留期内）+ 一条刚好在边界上的记录 + 过期的明细。
	retentionTestLog(t, "retention-model", 200, 100, 20, 0.5, now.Add(-2*time.Hour))
	retentionTestLog(t, "retention-model", 502, 0, 0, 0, now.Add(-time.Hour))
	retentionTestLog(t, "retention-model", 200, 900, 180, 4.5, now.AddDate(0, 0, -40))
	retentionTestLog(t, "retention-other", 200, 10, 2, 0.1, now.AddDate(0, 0, -9))
	if retentionRowCount(t, "call_logs") != 4 {
		t.Fatal("expected 4 call logs before the sweep")
	}
	deleted, err := sweepCallLogs(now)
	if err != nil {
		t.Fatal(err)
	}
	// 过期的是 40 天前和 9 天前的两条记录。
	if deleted != 2 || retentionRowCount(t, "call_logs") != 2 {
		t.Fatalf("sweep kept wrong rows: deleted=%d remaining=%d", deleted, retentionRowCount(t, "call_logs"))
	}
	if retentionRowCount(t, "call_log_daily") != 2 {
		t.Fatalf("expired logs were not rolled up before deletion")
	}
	var calls, successes int64
	var pt, ct int64
	var cost float64
	err = db.QueryRow(`SELECT COALESCE(SUM(calls),0),COALESCE(SUM(successes),0),COALESCE(SUM(prompt_tokens),0),
	 COALESCE(SUM(completion_tokens),0),COALESCE(SUM(cost_usd),0) FROM call_log_daily`).Scan(&calls, &successes, &pt, &ct, &cost)
	if err != nil || calls != 2 || successes != 2 || pt != 910 || ct != 182 || cost != 4.6 {
		t.Fatalf("rollup totals wrong: %d/%d/%d/%d/%v %v", calls, successes, pt, ct, cost, err)
	}
}

func TestLogRetentionPreservesCumulativeOverview(t *testing.T) {
	retentionTestDB(t)
	retentionTestSettings(t, 7)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	now := beijingNow()
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: "retention-model,retention-old,stale-model"}); err != nil {
		t.Fatal(err)
	}
	retentionTestLog(t, "retention-model", 200, 100, 20, 0.5, now.Add(-2*time.Hour))
	retentionTestLog(t, "retention-old", 200, 300, 40, 1.5, now.AddDate(0, 0, -10))
	retentionTestLog(t, "stale-model", 502, 0, 0, 0, now.AddDate(0, 0, -20))
	before, err := buildPublicOverview(now, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sweepCallLogs(now); err != nil {
		t.Fatal(err)
	}
	if retentionRowCount(t, "call_logs") != 1 {
		t.Fatalf("expired details were not deleted")
	}
	after, err := buildPublicOverview(now, "hour")
	if err != nil {
		t.Fatal(err)
	}
	// 累计用量与最近一次调用在删除后保持不变。
	if before.Summary.Calls != after.Summary.Calls || before.Summary.PromptTokens != after.Summary.PromptTokens ||
		before.Summary.CompletionTokens != after.Summary.CompletionTokens || after.Summary.Calls != 3 {
		t.Fatalf("cumulative summary changed: %+v -> %+v", before.Summary, after.Summary)
	}
	beforeUsage, afterUsage := map[string]PublicUsage{}, map[string]PublicUsage{}
	for _, u := range before.Usage {
		beforeUsage[u.Model] = u
	}
	for _, u := range after.Usage {
		afterUsage[u.Model] = u
	}
	for model, u := range beforeUsage {
		got, ok := afterUsage[model]
		if !ok || got != u {
			t.Fatalf("cumulative usage for %s changed: %+v -> %+v", model, u, got)
		}
	}
	beforeLast, afterLast := map[string]string{}, map[string]string{}
	for _, m := range before.Models {
		beforeLast[m.Model] = m.LastCall
	}
	for _, m := range after.Models {
		afterLast[m.Model] = m.LastCall
	}
	for model, at := range beforeLast {
		if afterLast[model] != at {
			t.Fatalf("last call for %s changed: %q -> %q", model, at, afterLast[model])
		}
	}
	// 30 天的日维度口径同样不受影响：十天前的调用仍在图表里。
	beforeDay, afterDay := int64(0), int64(0)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	tenDaysAgo := today.AddDate(0, 0, -10).Format("2006-01-02")
	for _, day := range before.Daily {
		if day.Date == tenDaysAgo {
			beforeDay = day.Calls
		}
	}
	for _, day := range after.Daily {
		if day.Date == tenDaysAgo {
			afterDay = day.Calls
		}
	}
	if beforeDay != 1 || afterDay != 1 {
		t.Fatalf("daily totals changed for %s: %d -> %d", tenDaysAgo, beforeDay, afterDay)
	}
	if before.Summary.Uptime == nil || after.Summary.Uptime == nil || *before.Summary.Uptime != *after.Summary.Uptime {
		t.Fatalf("24h uptime changed: %+v -> %+v", before.Summary.Uptime, after.Summary.Uptime)
	}
}

func TestLogRetentionKeepForever(t *testing.T) {
	retentionTestDB(t)
	retentionTestSettings(t, 0)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	now := beijingNow()
	retentionTestLog(t, "retention-model", 200, 5, 1, 0.01, now.AddDate(0, 0, -400))
	deleted, err := sweepCallLogs(now)
	if err != nil || deleted != 0 || retentionRowCount(t, "call_logs") != 1 {
		t.Fatalf("permanent retention must not delete: %d %v", deleted, err)
	}
}

func TestLogRetentionSweepIsIdempotent(t *testing.T) {
	retentionTestDB(t)
	retentionTestSettings(t, 7)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	now := beijingNow()
	retentionTestLog(t, "retention-model", 200, 50, 5, 0.2, now.AddDate(0, 0, -30))
	first, err := sweepCallLogs(now)
	if err != nil || first != 1 {
		t.Fatalf("first sweep: %d %v", first, err)
	}
	second, err := sweepCallLogs(now)
	if err != nil || second != 0 {
		t.Fatalf("second sweep must be a no-op: %d %v", second, err)
	}
	var calls, pt, ct int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(calls),0),COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0)
		FROM call_log_daily`).Scan(&calls, &pt, &ct); err != nil || calls != 1 || pt != 50 || ct != 5 {
		t.Fatalf("rollup double counted: %d/%d/%d %v", calls, pt, ct, err)
	}
}

func TestLogRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, beijingLocation)
	cutoff, ok := logRetentionCutoff(7, now)
	if !ok || cutoff != now.AddDate(0, 0, -7).Unix() {
		t.Fatalf("cutoff wrong: %d %v", cutoff, ok)
	}
	if _, ok := logRetentionCutoff(0, now); ok {
		t.Fatal("permanent retention must report no cutoff")
	}
	for _, days := range []int{-1, 3651} {
		if validLogRetentionDays(days) {
			t.Fatalf("out-of-range retention accepted: %d", days)
		}
	}
	for _, days := range []int{0, 7, 30, 90, 180, 365} {
		if !validLogRetentionDays(days) {
			t.Fatalf("preset retention rejected: %d", days)
		}
	}
}

func TestLogRetentionSettingsAPI(t *testing.T) {
	r := testProxyAdminRouter(t)
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	previous := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previous
		settingsMu.Unlock()
	})
	unauthenticated := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":30}`, false)
	if unauthenticated.Code != 401 {
		t.Fatalf("unauthenticated settings write: %d", unauthenticated.Code)
	}
	rejected := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":45}`, true)
	if rejected.Code != 400 {
		t.Fatalf("out-of-preset retention accepted: %d %s", rejected.Code, rejected.Body.String())
	}
	updated := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":30}`, true)
	if updated.Code != 200 || getSettings().LogRetentionDays != 30 {
		t.Fatalf("retention update failed: %d %s", updated.Code, updated.Body.String())
	}
	fetched := proxyAdminRequest(r, "GET", "settings", ``, true)
	if fetched.Code != 200 || !containsString(fetched.Body.String(), `"log_retention_days":30`) {
		t.Fatalf("retention not reflected in settings: %d %s", fetched.Code, fetched.Body.String())
	}
	kept := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":0}`, true)
	if kept.Code != 200 || getSettings().LogRetentionDays != 0 {
		t.Fatalf("permanent retention rejected: %d %s", kept.Code, kept.Body.String())
	}
}

// 保存新的保留期后应立即触发一次清理，而不是等到下一个整点。
func TestLogRetentionSettingsChangeTriggersSweep(t *testing.T) {
	r := testProxyAdminRouter(t)
	drainLogRetentionKick()
	t.Cleanup(drainLogRetentionKick)
	retentionTestSettings(t, 0) // 先设为永久保留，确认保存后不会立刻删除
	if err := loadSettingsCache(); err != nil {
		t.Fatal(err)
	}
	now := beijingNow()
	retentionTestLog(t, "retention-model", 200, 10, 1, 0.2, now.AddDate(0, 0, -10))
	keep := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":0}`, true)
	if keep.Code != 200 || retentionRowCount(t, "call_logs") != 1 || len(logRetentionKick) != 0 {
		t.Fatalf("unchanged permanent retention must not sweep: %d rows=%d kicks=%d",
			keep.Code, retentionRowCount(t, "call_logs"), len(logRetentionKick))
	}
	// 调短到 7 天：请求返回前清理任务已入队，后台循环消费后立即执行清理。
	shorten := proxyAdminRequest(r, "PUT", "settings", `{"log_retention_days":7}`, true)
	if shorten.Code != 200 || getSettings().LogRetentionDays != 7 {
		t.Fatalf("retention update failed: %d %s", shorten.Code, shorten.Body.String())
	}
	if len(logRetentionKick) != 1 {
		t.Fatal("changing the retention window must queue an immediate sweep")
	}
	<-logRetentionKick // 模拟后台循环消费触发信号
	if deleted, err := sweepCallLogs(beijingNow()); err != nil || deleted != 1 {
		t.Fatalf("kicked sweep failed: %d %v", deleted, err)
	}
	if retentionRowCount(t, "call_logs") != 0 {
		t.Fatal("shrinking the retention window must sweep the expired detail")
	}
	if calls := retentionRowCount(t, "call_log_daily"); calls != 1 {
		t.Fatalf("swept call must be rolled up, daily rows=%d", calls)
	}
}

func drainLogRetentionKick() {
	for {
		select {
		case <-logRetentionKick:
		default:
			return
		}
	}
}

func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
