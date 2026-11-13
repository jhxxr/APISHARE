package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestArenaVersionReferenceGatewayAndPersonalUseNewRelease(t *testing.T) {
	old, newest := "deepseek-v4.1-flash-20260910(max)", "deepseek-v4.1-flash-1017(max)"
	_, _, cr, _ := arenaRoutingTestSetup(t, old, newest)
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510))
	cfg := defaultAutoRouterConfig()
	cfg.Mode, cfg.Classifier, cfg.StrategyProfile = "live", "heuristic", StrategyQualityFirst
	cfg.ModelPool = []string{old, newest}
	d, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(candidates) != 2 || candidates[0] != newest || d.Arena == nil || !d.Arena.VersionReference {
		t.Fatalf("gateway lost version policy: %v / %+v / %v", candidates, d, err)
	}
	ucfg := defaultUserAutoRouterConfig()
	ucfg.Enabled, ucfg.StrategyProfile, ucfg.ModelPool = true, StrategyQualityFirst, cfg.ModelPool
	cr.Model = ucfg.VirtualModel
	d, targets, err := resolveUserAutoRouteWithConfig(context.Background(), "owner", ucfg, ApiKey{Models: old + "," + newest},
		FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(targets) != 2 || targets[0].Model != newest || d.Arena == nil || !d.Arena.VersionReference {
		t.Fatalf("personal router lost version policy: %v / %+v / %v", targets, d, err)
	}
}

func TestArenaVersionReferencePrefersNewReleaseWithoutFabricatingScore(t *testing.T) {
	models := []string{"deepseek-v4.1-flash-20260910(max)", "deepseek-v4.1-flash-1017(max)", "bench-strong"}
	_, opts, cr, demand := arenaRoutingTestSetup(t, models...)
	// Explicitly dated versions are usable even on third-party endpoints;
	// only an official endpoint can resolve its rolling model aliases.
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (High)", 2, 1400, 1390, 1410),
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510))
	d := arenaRoutingTestDecision(models...)
	got := applyArenaRoutePolicy(d, models, opts, cr, demand)
	if got[0] != models[1] || !d.Arena.VersionReference || d.Steps[0].Arena.Status != "version_reference" {
		t.Fatalf("new release did not get minor preference: %v / %+v", got, d.Arena)
	}
	e := d.Steps[0].Arena
	if *e.Score != 1500 || e.SourceModel != "DeepSeek-V4.1-Flash-20260910 (Max)" || e.ReferenceThinkingUnverified ||
		!strings.Contains(e.VersionReferenceNote, "不是新版实测") || *e.Lower != 1490 {
		t.Fatalf("inference lost original evidence or inflated its score: %+v", e)
	}
	if arenaAllowsSpeculativeProbe(d, models[0], models[1]) {
		t.Fatal("inferred improvement became a measured stronger-model probe")
	}
	// Any higher actual score stays higher, even by less than one point.
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 2, 1500, 1490, 1510),
		arenaRoutingTestModel("Bench Strong", 1, 1500.01, 1490, 1510))
	d = arenaRoutingTestDecision(models...)
	got = applyArenaRoutePolicy(d, models, opts, cr, demand)
	if got[0] != "bench-strong" {
		t.Fatalf("minor preference jumped over measured evidence: %v", got)
	}
}

func TestArenaVersionReferenceRemainsStrategyAwareAndShadowOnlyWhenRequested(t *testing.T) {
	old, newest := "deepseek-v4.1-flash-20260910(max)", "deepseek-v4.1-flash-1017(max)"
	models := []string{old, newest}
	_, opts, cr, demand := arenaRoutingTestSetup(t, models...)
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510))
	opts.Strategy = StrategyCostFirst
	d := arenaRoutingTestDecision(models...)
	plans := prepareArenaPlans(d, models, opts, cr, demand)
	snapshot, _, _ := getArenaSnapshot(db, "text")
	assignArenaEvidence(plans, snapshot, time.Now())
	oldCost, newCost := 0.001, 0.01
	plans[0].evidence.ComparisonCost, plans[1].evidence.ComparisonCost = &oldCost, &newCost
	if got := orderArenaSegment(plans, opts, nil, false, ""); got[0].model != old {
		t.Fatal("version preference overrode cheaper model within the accepted group")
	}
	opts.Strategy, opts.Mode = StrategyQualityFirst, "shadow"
	d = arenaRoutingTestDecision(models...)
	got := applyArenaRoutePolicy(d, models, opts, cr, demand)
	if got[0] != old || d.Arena.ProposedModel != newest || d.Arena.ActualModel != old || !d.Arena.VersionReference {
		t.Fatalf("inferred shadow proposal changed actual routing: %v / %+v", got, d.Arena)
	}
}

func TestArenaVersionReferenceConservativeThinkingAndRealResultSupersedes(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "deepseek-v4.1-flash-1017(high)")
	models := []string{"deepseek-v4.1-flash-1017(high)"}
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510))
	d := arenaRoutingTestDecision(models...)
	applyArenaRoutePolicy(d, models, opts, cr, demand)
	if !d.Steps[0].Arena.VersionReference || !d.Steps[0].Arena.ReferenceThinkingUnverified {
		t.Fatalf("thinking configuration mismatch hidden: %+v", d.Steps[0].Arena)
	}
	// Even a lower actual result for 1017 replaces the provisional 0910 prior.
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510),
		arenaRoutingTestModel("DeepSeek-V4.1-Flash-20261017 (High)", 2, 1300, 1290, 1310))
	d = arenaRoutingTestDecision(models...)
	applyArenaRoutePolicy(d, models, opts, cr, demand)
	if d.Steps[0].Arena.VersionReference || d.Steps[0].Arena.Status != "matched" || *d.Steps[0].Arena.Score != 1300 {
		t.Fatalf("real new-version score did not supersede assumption: %+v", d.Steps[0].Arena)
	}
	// A real result in another mode also disables the inferred release policy.
	models = []string{"deepseek-v4.1-flash-1017(max)"}
	d = arenaRoutingTestDecision(models...)
	applyArenaRoutePolicy(d, models, opts, cr, demand)
	if d.Steps[0].Arena.VersionReference || d.Steps[0].Arena.Score != nil {
		t.Fatal("a missing configuration ignored real evidence for the new release")
	}
}

func TestArenaVersionReferenceDoesNotGuessOtherVersionsOrWeakEvidence(t *testing.T) {
	now := time.Now()
	old := arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510)
	for _, identity := range []string{"deepseek-flash", "deepseek-v-4-pro", "deepseek-v-4-1-flash-20261101",
		deepSeekFlash1017Identity + "-preview", deepSeekFlash1017Identity + "-turbo"} {
		if ref, _ := arenaOlderVersionReference(identity, []arenaModel{old}, now); ref != nil {
			t.Fatalf("unrequested model/version inferred: %s", identity)
		}
	}
	low := int64(5)
	old.Votes = &low
	if ref, _ := arenaOlderVersionReference(deepSeekFlash1017Identity, []arenaModel{old}, now); ref != nil {
		t.Fatal("insufficient samples became a release reference")
	}
	old = arenaRoutingTestModel("DeepSeek-V4.1-Flash-20260910 (Max)", 1, 1500, 1490, 1510)
	if ref, _ := arenaOlderVersionReference(deepSeekFlash1017Identity, []arenaModel{old, old}, now); ref != nil {
		t.Fatal("ambiguous source entries became a release reference")
	}
	// Existing snapshot freshness checks still precede any inference.
	snapshot := &arenaSnapshot{PublishedAt: now.Add(-8 * 24 * time.Hour).Format("2006-01-02"), FetchedAt: now.UTC().Format(time.RFC3339), Models: []arenaModel{old}}
	plan := &arenaRoutePlan{evidence: &arenaRoutingEvidence{Identity: deepSeekFlash1017Identity}}
	if assignArenaEvidence([]*arenaRoutePlan{plan}, snapshot, now) != 0 || plan.evidence.VersionReference || plan.evidence.Score != nil {
		t.Fatal("outdated snapshot was reused as new release evidence")
	}
}

func TestArenaVersionReferenceResolvesOfficialAliasButNotThirdParty(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"https://api.deepseek.com/v1", deepSeekFlash1017Identity},
		{"https://third-party.invalid", "deepseek-flash"},
	} {
		up := &Upstream{Type: "openai", BaseURL: tc.endpoint, Models: "deepseek-flash"}
		id, _, status := arenaIdentityForChannel(up, "deepseek-flash", TierMedium, "heuristic", "keep", &canonReq{})
		if status != "" || id != tc.want {
			t.Fatalf("official alias: %s -> %q (%s), want %s", tc.endpoint, id, status, tc.want)
		}
	}
}
