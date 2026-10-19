package main

import (
	"path/filepath"
	"testing"
	"time"
)

func modelOverviewTestDB(t *testing.T) {
	t.Helper()
	resetUpstreamHealthForTest()
	if err := openDB(filepath.Join(t.TempDir(), "overview.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
}

func modelOverviewTestUpstream(t *testing.T, name, models string, weight int, enabled bool) *Upstream {
	t.Helper()
	u := &Upstream{Name: name, Type: "openai", BaseURL: "https://" + name + ".invalid", APIKey: "sk-" + name,
		Weight: weight, Models: models, Enabled: enabled}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	return u
}

func modelOverviewTestLog(t *testing.T, model string, upstreamID int64, name string, status int, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO call_logs(upstream_id,upstream_name,model,status,created_at) VALUES(?,?,?,?,?)`,
		upstreamID, name, model, status, at.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}

// The overview must only describe sources the current configuration can route
// to: calls from a deleted upstream, a disabled upstream, a zero-weight
// upstream or a request that never reached an upstream are all left out.
func TestModelOverviewOnlyListsConfiguredSources(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Now()
	live := modelOverviewTestUpstream(t, "live", "alpha,gamma", 1, true)
	retired := modelOverviewTestUpstream(t, "retired", "alpha", 1, true)
	disabled := modelOverviewTestUpstream(t, "disabled", "alpha", 1, false)
	zero := modelOverviewTestUpstream(t, "zero", "alpha", 0, true)
	if err := dbDeleteUpstream(retired.ID); err != nil {
		t.Fatal(err)
	}

	modelOverviewTestLog(t, "alpha", live.ID, live.Name, 200, now.Add(-2*time.Hour))
	modelOverviewTestLog(t, "alpha", live.ID, live.Name, 200, now.Add(-time.Hour))
	modelOverviewTestLog(t, "alpha", retired.ID, retired.Name, 200, now.Add(-30*time.Minute))
	modelOverviewTestLog(t, "alpha", disabled.ID, disabled.Name, 200, now.Add(-20*time.Minute))
	modelOverviewTestLog(t, "alpha", zero.ID, zero.Name, 200, now.Add(-10*time.Minute))
	modelOverviewTestLog(t, "alpha", 0, "", 200, now.Add(-5*time.Minute))
	// "beta" was only ever served by the deleted upstream; a 5xx there must not
	// survive as an incident anywhere.
	modelOverviewTestLog(t, "beta", retired.ID, retired.Name, 502, now.Add(-time.Minute))
	// A retired upstream failing later than the live one must not decide the
	// model status either.
	modelOverviewTestLog(t, "gamma", live.ID, live.Name, 200, now.Add(-2*time.Hour))
	modelOverviewTestLog(t, "gamma", retired.ID, retired.Name, 500, now.Add(-time.Hour))

	overview, err := buildModelOverview()
	if err != nil {
		t.Fatal(err)
	}
	byModel := make(map[string]modelOverviewItem, len(overview.Models))
	for _, item := range overview.Models {
		byModel[item.Model] = item
	}

	alpha, ok := byModel["alpha"]
	if !ok {
		t.Fatal("alpha disappeared although a configured upstream serves it")
	}
	if len(alpha.Sources) != 1 || alpha.Sources[0].Name != "live" {
		t.Fatalf("alpha sources = %+v, want only live", alpha.Sources)
	}
	if alpha.Sources[0].UpstreamID != live.ID {
		t.Fatalf("alpha source upstream = %d, want %d", alpha.Sources[0].UpstreamID, live.ID)
	}
	if alpha.Calls != 2 || alpha.Successes != 2 {
		t.Fatalf("alpha calls/successes = %d/%d, want 2/2 from the live upstream only", alpha.Calls, alpha.Successes)
	}
	if _, ok := byModel["beta"]; ok {
		t.Fatalf("beta is still listed although only a deleted upstream served it: %+v", byModel["beta"])
	}
	gamma, ok := byModel["gamma"]
	if !ok {
		t.Fatal("gamma disappeared although a configured upstream serves it")
	}
	if gamma.Status != "operational" || gamma.LastStatus != 200 {
		t.Fatalf("gamma status = %q last=%d, want operational/200 from the live upstream", gamma.Status, gamma.LastStatus)
	}
}

// A model can be served by several configured upstreams at once: the column
// lists them all (sorted by name), per-source uptime stays separate, and the
// model-level numbers add them up while the status follows the newest call.
func TestModelOverviewAggregatesMultipleSources(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Now()
	one := modelOverviewTestUpstream(t, "alpha-one", "shared", 1, true)
	two := modelOverviewTestUpstream(t, "alpha-two", "shared", 2, true)
	retired := modelOverviewTestUpstream(t, "alpha-retired", "shared", 1, true)
	if err := dbDeleteUpstream(retired.ID); err != nil {
		t.Fatal(err)
	}

	modelOverviewTestLog(t, "shared", one.ID, one.Name, 200, now.Add(-3*time.Hour))
	modelOverviewTestLog(t, "shared", one.ID, one.Name, 500, now.Add(-2*time.Hour))
	modelOverviewTestLog(t, "shared", two.ID, two.Name, 200, now.Add(-90*time.Minute))
	modelOverviewTestLog(t, "shared", two.ID, two.Name, 200, now.Add(-time.Hour))
	modelOverviewTestLog(t, "shared", retired.ID, retired.Name, 200, now.Add(-30*time.Minute))

	overview, err := buildModelOverview()
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Models) != 1 {
		t.Fatalf("models = %+v, want only shared", overview.Models)
	}
	item := overview.Models[0]
	if len(item.Sources) != 2 {
		t.Fatalf("sources = %+v, want both configured upstreams", item.Sources)
	}
	if item.Sources[0].Name != "alpha-one" || item.Sources[1].Name != "alpha-two" {
		t.Fatalf("sources = %q,%q, want alpha-one,alpha-two", item.Sources[0].Name, item.Sources[1].Name)
	}
	if item.Sources[0].Calls != 2 || item.Sources[1].Calls != 2 {
		t.Fatalf("per-source calls = %d,%d, want 2,2", item.Sources[0].Calls, item.Sources[1].Calls)
	}
	if uptime := item.Sources[0].Uptime; uptime == nil || *uptime != 50 {
		t.Fatalf("alpha-one uptime = %v, want 50", uptime)
	}
	if uptime := item.Sources[1].Uptime; uptime == nil || *uptime != 100 {
		t.Fatalf("alpha-two uptime = %v, want 100", uptime)
	}
	if item.Calls != 4 || item.Successes != 3 {
		t.Fatalf("model calls/successes = %d/%d, want 4/3", item.Calls, item.Successes)
	}
	if uptime := item.Uptime; uptime == nil || *uptime != 75 {
		t.Fatalf("model uptime = %v, want 75", uptime)
	}
	// The newest configured call (alpha-two, 200) decides the status; the newer
	// 200 from the deleted upstream is ignored either way.
	if item.Status != "operational" || item.LastStatus != 200 {
		t.Fatalf("status = %q last=%d, want operational/200", item.Status, item.LastStatus)
	}
}

// A still-configured upstream that is merely cooling down stays listed.
func TestModelOverviewKeepsConfiguredSourceWhileCooling(t *testing.T) {
	modelOverviewTestDB(t)
	now := time.Now()
	live := modelOverviewTestUpstream(t, "live", "alpha", 1, true)
	modelOverviewTestLog(t, "alpha", live.ID, live.Name, 200, now.Add(-time.Hour))

	overview, err := buildModelOverview()
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Models) != 1 || len(overview.Models[0].Sources) != 1 {
		t.Fatalf("models = %+v, want one model with one source", overview.Models)
	}
	if overview.Models[0].Sources[0].Name != "live" {
		t.Fatalf("source = %q, want live", overview.Models[0].Sources[0].Name)
	}
}
