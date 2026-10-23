package main

import (
	"math"
	"testing"
	"time"
)

func TestPublicModelRecentCallsIndependentOfHistoryWindow(t *testing.T) {
	publicTestDB(t)
	retentionTestSettings(t, 7)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
	k := publicTestKey(t, "history-sort", 100, 0, false)
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true,
		Models: "recent,steady,quiet,unused,future"}); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, k, "recent", 200, 10, 5, now.Add(-10*time.Minute))
	publicTestLog(t, k, "recent", 503, 0, 0, now.Add(-2*time.Hour))
	publicTestLog(t, k, "recent", 200, 10, 5, now.Add(-23*time.Hour).UTC())
	publicTestLog(t, k, "recent", 200, 10, 5, now.Add(-24*time.Hour).UTC())
	publicTestLog(t, k, "recent", 200, 10, 5, now.Add(-24*time.Hour-time.Second))
	publicTestLog(t, k, "recent", 503, 0, 0, now.Add(time.Second))
	for i := 0; i < 3; i++ {
		publicTestLog(t, k, "steady", 200, 10, 5, now.Add(-2*time.Hour))
	}
	for i := 0; i < 6; i++ {
		publicTestLog(t, k, "quiet", 200, 10, 5, now.Add(-5*24*time.Hour))
	}
	publicTestLog(t, k, "future", 200, 10, 5, now.Add(time.Second))
	want := map[string]int64{"recent": 4, "steady": 3, "quiet": 0, "unused": 0, "future": 0}
	for _, interval := range []string{"auto", "hour", "minute"} {
		t.Run(interval, func(t *testing.T) {
			got, err := buildPublicOverview(now, interval)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Models) != len(want) {
				t.Fatalf("model count = %d, want %d", len(got.Models), len(want))
			}
			for _, m := range got.Models {
				expected, ok := want[m.Model]
				if !ok || m.Calls24H != expected {
					t.Errorf("%s 24h calls = %d, want %d", m.Model, m.Calls24H, expected)
				}
				if interval == "auto" {
					var displayed int64
					for _, bucket := range m.History {
						displayed += bucket.Calls
					}
					expected := map[string]int64{"recent": 1, "steady": 3, "quiet": 6, "unused": 0, "future": 0}[m.Model]
					if displayed != expected {
						t.Errorf("%s displayed calls = %d, want %d", m.Model, displayed, expected)
					}
				}
			}
		})
	}
}

func TestPublicAutoHistoryRetainedRequestsAndFixedAxis(t *testing.T) {
	publicTestDB(t)
	retentionTestSettings(t, 7)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
	k := publicTestKey(t, "dynamic-history", 100, 0, false)
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true,
		Models: "recent,quiet,boundary,expired,unused,future"}); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, k, "recent", 200, 10, 5, now.Add(-10*time.Minute))
	// Outside the recent model's one-hour window, even though still retained.
	publicTestLog(t, k, "recent", 503, 0, 0, now.Add(-2*time.Hour))
	publicTestLog(t, k, "quiet", 503, 0, 0, now.Add(-5*24*time.Hour))
	publicTestLog(t, k, "quiet", 200, 10, 5, now.Add(-6*24*time.Hour))
	publicTestLog(t, k, "boundary", 200, 10, 5, now.Add(-7*24*time.Hour))
	publicTestLog(t, k, "expired", 503, 0, 0, now.Add(-7*24*time.Hour-time.Second))
	publicTestLog(t, k, "future", 503, 0, 0, now.Add(time.Second))
	// A future import must not hide the latest real request.
	publicTestLog(t, k, "recent", 503, 0, 0, now.Add(time.Second))
	for _, interval := range []string{"auto", "hour", "minute"} {
		got, err := buildPublicOverview(now, interval)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range got.Models {
			if interval != "auto" {
				if m.HistoryWindowMinutes != 1440 {
					t.Fatalf("%s manual window changed: %+v", interval, m)
				}
				continue
			}
			wantMinutes := 60
			if m.Model == "quiet" || m.Model == "boundary" {
				wantMinutes = 7 * 1440
			}
			if len(m.History) != 24 || m.HistoryWindowMinutes != wantMinutes {
				t.Fatalf("%s should have 24 buckets over %d minutes: %+v", m.Model, wantMinutes, m)
			}
			start := now.Add(-time.Duration(wantMinutes) * time.Minute)
			if m.History[0].Start != start.Format(time.RFC3339) || m.History[23].End != now.Format(time.RFC3339) {
				t.Fatalf("%s trace has wrong endpoints: %+v", m.Model, m.History)
			}
			for i := 1; i < len(m.History); i++ {
				if m.History[i-1].End != m.History[i].Start {
					t.Fatalf("%s has a gap between buckets %d and %d", m.Model, i-1, i)
				}
			}
			switch m.Model {
			case "recent", "boundary":
				if m.Uptime == nil || *m.Uptime != 100 || m.Status != "operational" {
					t.Fatalf("%s retained success disappeared: %+v", m.Model, m)
				}
			case "quiet":
				if m.Uptime == nil || *m.Uptime != 50 || m.Status != "incident" {
					t.Fatalf("quiet retained failure disappeared: %+v", m)
				}
			default:
				if m.Uptime != nil || m.Status != "idle" {
					t.Fatalf("missing or expired details invented uptime: %+v", m)
				}
			}
		}
		if interval == "auto" && (got.Summary.Uptime == nil || *got.Summary.Uptime != 75) {
			t.Fatalf("dynamic summary should weight displayed requests, got %+v", got.Summary)
		}
	}
}

func TestPublicAutoHistoryExpandsThenShrinks(t *testing.T) {
	publicTestDB(t)
	retentionTestSettings(t, 7)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
	k := publicTestKey(t, "dynamic-refresh", 100, 0, true)
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: "changing"}); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, k, "changing", 503, 0, 0, now.Add(-10*time.Minute))
	for _, step := range []struct {
		elapsed time.Duration
		minutes int
	}{
		{0, 60},
		{2 * time.Hour, 180},
		{25 * time.Hour, 2880},
		{6 * 24 * time.Hour, 7 * 1440},
	} {
		got, err := buildPublicOverview(now.Add(step.elapsed), "auto")
		if err != nil {
			t.Fatal(err)
		}
		m := got.Models[0]
		if m.HistoryWindowMinutes != step.minutes || len(m.History) != 24 || m.Uptime == nil || *m.Uptime != 0 {
			t.Fatalf("after %s: window did not grow with inactivity: %+v", step.elapsed, m)
		}
	}
	later := now.Add(6 * 24 * time.Hour)
	publicTestLog(t, k, "changing", 200, 100, 50, later.UTC())
	got, err := buildPublicOverview(later, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if m := got.Models[0]; m.HistoryWindowMinutes != 60 || m.Uptime == nil || *m.Uptime != 100 || m.Status != "operational" {
		t.Fatalf("a new request did not zoom back in: %+v", m)
	}
}

func TestPublicAutoHistoryStopsAtRetentionAndSurvivesRollup(t *testing.T) {
	publicTestDB(t)
	retentionTestSettings(t, 7)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
	k := publicTestKey(t, "dynamic-expiry", 100, 0, true)
	if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: "edge"}); err != nil {
		t.Fatal(err)
	}
	publicTestLog(t, k, "edge", 200, 10, 5, now.Add(-7*24*time.Hour))
	before, err := buildPublicOverview(now, "auto")
	if err != nil || before.Models[0].Uptime == nil || before.Models[0].History[0].Calls != 1 {
		t.Fatalf("request on the cleanup boundary was lost: %+v %v", before, err)
	}
	later := now.Add(time.Second)
	unswept, err := buildPublicOverview(later, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if unswept.Models[0].Uptime != nil || unswept.Models[0].Status != "idle" {
		t.Fatalf("overdue sweep kept expired details on the trace: %+v", unswept.Models[0])
	}
	if deleted, err := sweepCallLogs(later); err != nil || deleted != 1 {
		t.Fatalf("sweep: %d, %v", deleted, err)
	}
	after, err := buildPublicOverview(later, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if after.Models[0].Uptime != nil || after.Models[0].Status != "idle" ||
		after.Models[0].LastCall != before.Models[0].LastCall || after.Summary.Calls != before.Summary.Calls ||
		after.Summary.PromptTokens != before.Summary.PromptTokens {
		t.Fatalf("rollup invented uptime or lost lifetime data: before=%+v after=%+v", before, after)
	}
}

func TestPublicAutoHistoryFollowsConfiguredRetentionAndSpeed(t *testing.T) {
	for _, days := range []int{7, 30, 90, 180, 365, 0} {
		t.Run(retentionOption(days).label, func(t *testing.T) {
			publicTestDB(t)
			retentionTestSettings(t, days)
			now := time.Date(2026, 10, 23, 12, 0, 0, 0, beijingLocation)
			if err := dbInsertUpstream(&Upstream{Type: "openai", Weight: 1, Enabled: true, Models: "retained-speed"}); err != nil {
				t.Fatal(err)
			}
			age := days
			if days == 0 {
				age = 400
			}
			_, err := db.Exec(`INSERT INTO call_logs(model,status,completion_tokens,first_token_ms,duration_ms,created_at)
				VALUES('retained-speed',200,300,500,3500,?)`, now.AddDate(0, 0, -age).UTC().Format(time.RFC3339))
			if err != nil {
				t.Fatal(err)
			}
			got, err := buildPublicOverview(now, "auto")
			if err != nil {
				t.Fatal(err)
			}
			m := got.Models[0]
			if m.HistoryWindowMinutes != age*1440 || m.Uptime == nil || *m.Uptime != 100 ||
				m.AvgTPS == nil || math.Abs(*m.AvgTPS-100) > 1e-9 || len(m.History) != 24 {
				t.Fatalf("configured retention or speed window was ignored: %+v", m)
			}
		})
	}
}
