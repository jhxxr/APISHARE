package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func arenaRoutingTestSource(t *testing.T, board string, models ...arenaModel) {
	t.Helper()
	getArenaSnapshot(db, board)
	if err := storeArenaSnapshot(db, board, &arenaSnapshot{
		PublishedAt: time.Now().In(beijingLocation).Format("2006-01-02"),
		FetchedAt:   time.Now().UTC().Format(time.RFC3339), Models: models,
	}); err != nil {
		t.Fatal(err)
	}
}

func arenaRoutingTestModel(model string, rank int, score, low, high float64) arenaModel {
	samples := int64(1000)
	return arenaModel{Model: model, Family: "test", Rank: rank, Score: score,
		RatingLower: &low, RatingUpper: &high, Votes: &samples}
}

func arenaRoutingTestSetup(t *testing.T, models ...string) (*Upstream, arenaRoutingOptions, *canonReq, TaskDemandProfile) {
	t.Helper()
	setupAutoRouterTestDB(t)
	up := &Upstream{Name: "arena-test", Type: "openai", BaseURL: "http://127.0.0.1:9999",
		Models: strings.Join(models, ","), Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	opts := arenaRoutingOptions{Mode: "live", Strategy: StrategyQualityFirst, Format: FormatOpenAIChat,
		BanditEnabled: true}
	cr := &canonReq{Model: "auto", Messages: []canonMsg{{Role: "user", Text: "hello"}}, MaxTokens: 1024, Stream: true}
	demand := TaskDemandProfile{PrimaryDomain: DomainWritingKnowledge, RequiredDifficulty: 85, EquivalentTier: TierComplex}
	return up, opts, cr, demand
}

func arenaRoutingTestDecision(models ...string) *routerDecisionDetails {
	d := &routerDecisionDetails{Tier: TierComplex, Cause: "heuristic"}
	for i, model := range models {
		status := "standby"
		if i == 0 {
			status = "selected"
		}
		d.Steps = append(d.Steps, routerCandidateStep{Model: model, SourceTier: TierComplex, Priority: i + 1, Status: status})
	}
	return d
}

func arenaRoutingTestPrices(t *testing.T, prices map[string]Price) {
	t.Helper()
	settings := getSettings()
	original := settings
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = original; settingsMu.Unlock() })
	settings.Pricing, settings.AutoPricingEnabled = prices, false
	if err := persistSettings(settings); err != nil {
		t.Fatal(err)
	}
}

func TestArenaRoutingShadowAndLiveUseSameSourceProposal(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b", "unknown-pro")
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	models := []string{"bench-a", "unknown-pro", "bench-b"}
	shadow := arenaRoutingTestDecision(models...)
	opts.Mode = "shadow"
	if got := applyArenaRoutePolicy(shadow, models, opts, cr, demand); !reflect.DeepEqual(got, models) {
		t.Fatalf("shadow changed transfer order: %v", got)
	}
	if shadow.Arena.ProposedModel != "bench-b" || shadow.Arena.ActualModel != "bench-a" || !shadow.Arena.Different {
		t.Fatalf("missing contrast: %+v", shadow.Arena)
	}
	live := arenaRoutingTestDecision(models...)
	opts.Mode = "live"
	got := applyArenaRoutePolicy(live, models, opts, cr, demand)
	if got[0] != "bench-b" || got[len(got)-1] != "unknown-pro" || live.Arena.ActualModel != "bench-b" {
		t.Fatalf("wrong live order: %v / %+v", got, live.Arena)
	}
	if live.Steps[0].Arena.Score == nil || *live.Steps[0].Arena.Score != 1500 {
		t.Fatal("raw source score changed")
	}
	live.markRuntimeFailover("bench-b", nil, "bench-a", "")
	if live.Arena.ActualModel != "bench-a" || live.Arena.ProposedModel != "bench-b" {
		t.Fatal("runtime fallback was not distinguished from proposal")
	}
}

func TestArenaRoutingHTTP200DoesNotBecomeAnswerQuality(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	for i := 0; i < 100; i++ {
		recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, true, 1, 1, 100, 10)
	}
	opts.Strategy = StrategyLatencyFirst
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-b" {
		t.Fatalf("HTTP successes/fast channel overrode source quality band: %v", got)
	}
	for _, step := range d.Steps {
		if step.Model == "bench-a" && (step.Arena.ChannelCalls != 100 || step.Arena.ServiceSuccess == nil || *step.Arena.ServiceSuccess != 1) {
			t.Fatalf("service metrics lost: %+v", step.Arena)
		}
	}
}

func TestArenaRoutingBackupFailuresAndRefusalsAreNotModelQuality(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1400, 1390, 1410))
	backup := &Upstream{Name: "backup", Type: "openai", BaseURL: "http://127.0.0.1:9998", Models: "bench-a", Weight: 1, Priority: 10, Enabled: true}
	if err := dbInsertUpstream(backup); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		recordBanditExplicitOutcome(backup.ID, "bench-a", demand.PrimaryDomain, false, 100, 100, 100, 0)
		recordBanditSpeculativeEscalation(up.ID, "bench-a", demand.PrimaryDomain)
	}
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-a" || d.Steps[0].Arena.ChannelCalls != 0 {
		t.Fatalf("backup/semantic refusal became transport or quality penalty: %v / %+v", got, d.Steps[0].Arena)
	}
	for i := 0; i < 6; i++ {
		recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, false, 100, 100, 100, 0)
	}
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	got = applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-b" {
		t.Fatalf("persistently failing primary channel was not deprioritized: %v", got)
	}
	for _, step := range d.Steps {
		if step.Model == "bench-a" && *step.Arena.Score != 1500 {
			t.Fatal("service failure changed original benchmark score")
		}
	}
	autoRouterBandit.Lock()
	autoRouterBandit.arms[banditArmKey(up.ID, "bench-a", demand.PrimaryDomain)].LastTransportAt = time.Now().Add(-banditTransportWindow - time.Minute).UTC().Format(time.RFC3339)
	autoRouterBandit.Unlock()
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatal("old outage permanently starved a recovered candidate")
	}
	recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, true, 100, 100, 200, 10)
	stats, _ := lookupBanditArmSummary(up.ID, "bench-a", demand.PrimaryDomain)
	if stats.TransportSamples != 1 || stats.TransportEWMA != 1 {
		t.Fatalf("new transport run did not reset after idle expiry: %+v", stats)
	}
}

func TestArenaRoutingPrivateCostsAndMetricsStayPrivate(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	result, err := db.Exec(`INSERT INTO user_upstreams(sub,name,type,base_url,api_key,models) VALUES(?,?,?,?,?,?)`,
		"owner", "private", "openai", "http://127.0.0.1:9998", "private-key", "bench-a,bench-b")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 2, 1497, 1487, 1507), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 0, Out: 0}, "bench-b": {In: 10, Out: 40}})
	for i := 0; i < 6; i++ {
		recordBanditExplicitOutcome(up.ID, "bench-b", demand.PrimaryDomain, false, 9000, 9000, 9000, 0)
	}
	opts.UserSub, opts.Strategy = "owner", StrategyCostFirst
	opts.Targets = map[string]userRouteTarget{"bench-a": {Model: "bench-a", AllowBYOK: true, PreferBYOK: true}, "bench-b": {Model: "bench-b", AllowBYOK: true, PreferBYOK: true}}
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-b" {
		t.Fatalf("private model inherited site failures/prices: %v", got)
	}
	for _, step := range d.Steps {
		if step.Arena.PriceKnown || step.Arena.ComparisonCost != nil || step.Arena.ChannelCalls != 0 {
			t.Fatalf("invented private provider cost or reused site metrics: %+v", step.Arena)
		}
	}
	for i := 0; i < 6; i++ {
		recordBanditExplicitOutcome(-id, "bench-b", demand.PrimaryDomain, false, 200, 200, 200, 0)
	}
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatalf("own private channel history was ignored: %v", got)
	}
}

func TestArenaRoutingOnlyProbesWhenFallbackIsStronger(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1497, 1487, 1507))
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 10, Out: 40}, "bench-b": {In: 0.1, Out: 1}})
	opts.Strategy = StrategyCostFirst
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-b" || !arenaAllowsSpeculativeProbe(d, got[0], got[1]) {
		t.Fatal("measured stronger fallback should permit probe")
	}
	if arenaAllowsSpeculativeProbe(d, got[1], got[0]) {
		t.Fatal("strong model should not be probed in order to escalate to weaker one")
	}
}

func TestArenaRoutingCostLatencyAndParetoStayWithinQualityBand(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1497, 1487, 1507))
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 10, Out: 40}, "bench-b": {In: 0.1, Out: 1}})
	recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, true, 800, 800, 1000, 10)
	recordBanditExplicitOutcome(up.ID, "bench-b", demand.PrimaryDomain, true, 100, 100, 1000, 10)
	for _, tc := range []struct{ strategy, want string }{{StrategyQualityFirst, "bench-a"}, {StrategyCostFirst, "bench-b"}, {StrategyLatencyFirst, "bench-b"}, {StrategyBalanced, "bench-b"}} {
		t.Run(tc.strategy, func(t *testing.T) {
			opts.Strategy = tc.strategy
			d := arenaRoutingTestDecision("bench-a", "bench-b")
			got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
			if got[0] != tc.want {
				t.Fatalf("wrong choice: %v / %+v", got, d.Arena)
			}
		})
	}
}

func TestArenaRoutingUnknownPriceIsNotFree(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "known-free", "unknown-price")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Known Free", 2, 1497, 1487, 1507), arenaRoutingTestModel("Unknown Price", 1, 1500, 1490, 1510))
	arenaRoutingTestPrices(t, map[string]Price{"known-free": {In: 0, Out: 0}})
	opts.Strategy = StrategyCostFirst
	d := arenaRoutingTestDecision("unknown-price", "known-free")
	got := applyArenaRoutePolicy(d, []string{"unknown-price", "known-free"}, opts, cr, demand)
	if got[0] != "known-free" {
		t.Fatalf("unknown price treated as zero: %v", got)
	}
	for _, step := range d.Steps {
		if step.Model == "unknown-price" && (step.Arena.PriceKnown || step.Arena.ComparisonCost != nil) {
			t.Fatal("invented price")
		}
		if step.Model == "known-free" && (!step.Arena.PriceKnown || step.Arena.ComparisonCost == nil || *step.Arena.ComparisonCost != 0) {
			t.Fatal("explicit free price lost")
		}
	}
}

func TestArenaRoutingResponseModesAndClientErrorsDoNotPolluteLatency(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1497, 1487, 1507))
	opts.Strategy = StrategyLatencyFirst
	for i := 0; i < 6; i++ {
		recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, false, 0, 1, 1, 0, banditTransportObservation{Valid: false, Stream: false})
	}
	stats, _ := lookupBanditArmSummary(up.ID, "bench-a", demand.PrimaryDomain)
	if stats.TransportSamples != 0 {
		t.Fatal("ordinary client errors became service failures")
	}
	recordBanditExplicitOutcome(up.ID, "bench-a", demand.PrimaryDomain, true, 1, 1, 5000, 10, banditTransportObservation{Valid: true, Stream: false})
	recordBanditExplicitOutcome(up.ID, "bench-b", demand.PrimaryDomain, true, 100, 100, 1000, 10, banditTransportObservation{Valid: true, Stream: false})
	cr.Stream = false
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-b" || d.Steps[0].Arena.ResponseKind != "nonstream_total" {
		t.Fatalf("response headers were mistaken for full response time: %v", got)
	}
	cr.Stream = true
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
	if got[0] != "bench-a" || d.Steps[0].Arena.ResponseMS != nil {
		t.Fatal("nonstream durations were reused as streaming TTFT")
	}
}

func TestArenaRoutingWarmAffinityAndLegacyFallbackHaveNoInventedScores(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 1, Out: 1}, "bench-b": {In: 1, Out: 1}})
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 2, 1497, 1487, 1507), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	opts.Strategy, opts.PreviousModel = StrategyBalanced, "bench-a"
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-b" {
		t.Fatal("short request was treated as a warm cache")
	}
	cr.Messages = []canonMsg{{Role: "user", Text: strings.Repeat("word ", 4000)}}
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	d.EstimatedInputTokens = estimateRequestContextTokens(cr)
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatal("same-band warm affinity was lost")
	}
	d = arenaRoutingTestDecision("bench-a")
	d.Steps[0].Status = "skipped"
	d.Steps[0].Reason = "context_exceeded"
	applyArenaRoutePolicy(d, []string{"bench-a"}, opts, cr, demand)
	if d.Steps[0].Arena.Score != nil || d.Arena.Reason != "existing_precheck_fallback_not_reranked" {
		t.Fatal("a legacy precheck fallback acquired a benchmark endorsement")
	}
}

func TestArenaRoutingVariantsAliasesAndAmbiguousMappings(t *testing.T) {
	up, opts, cr, demand := arenaRoutingTestSetup(t, "gpt-5.2", "gpt-5.2-2026-01-01", "gpt-5.2-skyhigh")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("GPT-5.2 (High)", 1, 1500, 1490, 1510), arenaRoutingTestModel("GPT-5.2-skyhigh", 2, 1495, 1485, 1505))
	d := arenaRoutingTestDecision("gpt-5.2")
	applyArenaRoutePolicy(d, []string{"gpt-5.2"}, opts, cr, demand)
	if d.Steps[0].Arena.Status == "matched" {
		t.Fatal("plain provider default borrowed High score")
	}
	opts.TierThinking = map[string]string{TierComplex: "high"}
	d = arenaRoutingTestDecision("gpt-5.2")
	applyArenaRoutePolicy(d, []string{"gpt-5.2"}, opts, cr, demand)
	if d.Steps[0].Arena.Status != "matched" {
		t.Fatalf("resolved High variant did not match: %+v", d.Steps[0].Arena)
	}
	d = arenaRoutingTestDecision("gpt-5.2-skyhigh")
	applyArenaRoutePolicy(d, []string{"gpt-5.2-skyhigh"}, opts, cr, demand)
	if d.Steps[0].Arena.Status == "matched" {
		t.Fatal("a word ending in high was confused with an explicit High variant")
	}
	d = arenaRoutingTestDecision("gpt-5.2-2026-01-01")
	applyArenaRoutePolicy(d, []string{"gpt-5.2-2026-01-01"}, opts, cr, demand)
	if d.Steps[0].Arena.Status == "matched" {
		t.Fatal("dated version borrowed unversioned score")
	}
	opts.TierThinking = nil
	up.Models = "public-alias"
	up.ModelMap = `{"public-alias":"bench-a"}`
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510))
	d = arenaRoutingTestDecision("public-alias")
	applyArenaRoutePolicy(d, []string{"public-alias"}, opts, cr, demand)
	if d.Steps[0].Arena.Status != "matched" {
		t.Fatalf("wire alias was not resolved: %+v", d.Steps[0].Arena)
	}
	if err := dbInsertUpstream(&Upstream{Name: "other-target", Type: "openai", BaseURL: "http://127.0.0.1:9998", Models: "public-alias", ModelMap: `{"public-alias":"bench-b"}`, Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	d = arenaRoutingTestDecision("public-alias")
	applyArenaRoutePolicy(d, []string{"public-alias"}, opts, cr, demand)
	if d.Steps[0].Arena.Status != "ambiguous_upstream_mapping" {
		t.Fatalf("mixed wire aliases chose best score: %+v", d.Steps[0].Arena)
	}
}

func TestArenaRoutingLowSamplesDuplicatesAndOutdatedFallback(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b", "unknown-max")
	a := arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410)
	b := arenaRoutingTestModel("Bench B", 1, 2000, 1990, 2010)
	few := int64(1)
	b.Votes = &few
	arenaRoutingTestSource(t, "text", a, b)
	d := arenaRoutingTestDecision("bench-b", "bench-a")
	if got := applyArenaRoutePolicy(d, []string{"bench-b", "bench-a"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatalf("one vote beat reliable evidence: %v", got)
	}
	arenaRoutingTestSource(t, "text", a, b, arenaRoutingTestModel("Bench A", 3, 1300, 1290, 1310))
	d = arenaRoutingTestDecision("bench-a")
	applyArenaRoutePolicy(d, []string{"bench-a"}, opts, cr, demand)
	if d.Steps[0].Arena.Status != "ambiguous_source_model" {
		t.Fatal("ambiguous display name borrowed a favorable entry")
	}
	getArenaSnapshot(db, "text")
	arenaLeaderboards.Lock()
	arenaLeaderboards.entries["text"].snapshot = &arenaSnapshot{PublishedAt: time.Now().Add(-8 * 24 * time.Hour).Format("2006-01-02"), FetchedAt: time.Now().UTC().Format(time.RFC3339), Models: []arenaModel{a, b}}
	arenaLeaderboards.Unlock()
	opts.DefaultModel = "unknown-max"
	d = arenaRoutingTestDecision("bench-b", "bench-a", "unknown-max")
	got := applyArenaRoutePolicy(d, []string{"bench-b", "bench-a", "unknown-max"}, opts, cr, demand)
	if got[0] != "unknown-max" || d.Arena.Reason != "no_trusted_benchmark_configured_fallback" {
		t.Fatalf("outdated data not rejected: %v / %+v", got, d.Arena)
	}
}

func TestArenaRoutingDomainScaleAndGeneralFallback(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1400, 1390, 1410))
	arenaRoutingTestSource(t, "webdev", arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	arenaRoutingTestSource(t, "agent", arenaRoutingTestModel("Bench A", 2, 0.1, 0.09, 0.11), arenaRoutingTestModel("Bench B", 1, 0.2, 0.19, 0.21))
	for _, tc := range []struct {
		domain, board, want string
		weak                bool
	}{
		{DomainWritingKnowledge, "text", "bench-a", false}, {DomainFrontendUI, "webdev", "bench-b", false},
		{DomainAgenticTools, "agent", "bench-b", false}, {DomainCodingSWE, "text", "bench-a", true},
		{DomainReasoningMath, "text", "bench-a", true}, {DomainLongContextMM, "text", "bench-a", true},
	} {
		demand.PrimaryDomain = tc.domain
		d := arenaRoutingTestDecision("bench-a", "bench-b")
		got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand)
		if got[0] != tc.want || d.Arena.Board != tc.board || d.Arena.ReferenceOnly != tc.weak {
			t.Fatalf("%s: %v / %+v", tc.domain, got, d.Arena)
		}
		if tc.board == "agent" && *d.Steps[0].Arena.Score != 0.2 {
			t.Fatal("IPS score rescaled or mixed with Text Elo")
		}
	}
}

func TestArenaRoutingTaskFloorIgnoresLegacyTiersAndPreservesSession(t *testing.T) {
	_, opts, cr, demand := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 10, Out: 40}, "bench-b": {In: 0.1, Out: 1}})
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 1, 1500, 1490, 1510), arenaRoutingTestModel("Bench B", 2, 1400, 1390, 1410),
		arenaRoutingTestModel("External Low", 3, 1000, 990, 1010))
	opts.Strategy = StrategyCostFirst
	demand.RequiredDifficulty = 24
	d := arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-b" || d.Arena.FloorPolicy != "source_median_for_simple" {
		t.Fatalf("simple task did not use measured floor: %v / %+v", got, d.Arena)
	}
	demand.RequiredDifficulty = 85
	d = arenaRoutingTestDecision("bench-a", "bench-b")
	if got := applyArenaRoutePolicy(d, []string{"bench-a", "bench-b"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatal("hard task used relaxed simple-task floor")
	}
	d = arenaRoutingTestDecision("bench-b", "bench-a")
	d.Steps[0].SourceTier = TierSimple
	d.Steps[1].SourceTier = TierReasoning
	if got := applyArenaRoutePolicy(d, []string{"bench-b", "bench-a"}, opts, cr, demand); got[0] != "bench-a" {
		t.Fatal("obsolete manual tier blocked the strongest model")
	}
	d = arenaRoutingTestDecision("bench-b", "bench-a")
	d.Cause = "session_affinity"
	d.SelectedModel = "bench-b"
	if got := applyArenaRoutePolicy(d, []string{"bench-b", "bench-a"}, opts, cr, demand); got[0] != "bench-b" || d.Arena.Reason != "session_affinity_preserved" {
		t.Fatal("active session affinity lost")
	}
}

func TestArenaRoutingGatewayAndUserIntegration(t *testing.T) {
	_, _, cr, _ := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	cfg := defaultAutoRouterConfig()
	cfg.Enabled = true
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.StrategyProfile = StrategyQualityFirst
	cfg.ModelPool = []string{"bench-a", "bench-b"}
	cfg.BanditEnabled = false
	cfg.ModelCapabilities = map[string]ModelCapabilityVector{"bench-a": {OverallScore: 100, CodingSWE: 100, FrontendUI: 100, ReasoningMath: 100, AgenticTools: 100, WritingKnowledge: 100, LongContextMM: 100, SpeedPrior: 100}, "bench-b": {OverallScore: 1, CodingSWE: 1, FrontendUI: 1, ReasoningMath: 1, AgenticTools: 1, WritingKnowledge: 1, LongContextMM: 1, SpeedPrior: 1}}
	for _, mode := range []string{"shadow", "live"} {
		cfg.ArenaMode = mode
		d, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
		want := "bench-a"
		if mode == "live" {
			want = "bench-b"
		}
		if err != nil || len(candidates) == 0 || candidates[0] != want || d.Arena.ProposedModel != "bench-b" {
			t.Fatalf("site %s: %v / %+v / %v", mode, candidates, d.Arena, err)
		}
		if mode == "live" && d.Steps[0].CapabilityScore != 0 {
			t.Fatal("live mode still used manual capability scores")
		}
		ucfg := defaultUserAutoRouterConfig()
		ucfg.ArenaMode = mode
		ucfg.ChannelPreference = "gateway_first"
		ucfg.ModelPool = cfg.ModelPool
		ucfg.StrategyProfile = StrategyQualityFirst
		ucfg.BanditEnabled = false
		userWant := want
		if mode == "shadow" {
			baseline := ucfg
			baseline.ArenaMode = "off"
			_, original, baselineErr := resolveUserAutoRouteWithConfig(context.Background(), "owner", baseline, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
			if baselineErr != nil || len(original) == 0 {
				t.Fatalf("user baseline: %v", baselineErr)
			}
			userWant = original[0].Model
		}
		ud, targets, err := resolveUserAutoRouteWithConfig(context.Background(), "owner", ucfg, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
		if err != nil || len(targets) == 0 || targets[0].Model != userWant || ud.Arena.ProposedModel != "bench-b" {
			t.Fatalf("user %s: %+v / %+v / %v", mode, targets, ud.Arena, err)
		}
	}
	cfg.AllowUnlistedTargetModels = false
	ak := ApiKey{ID: 1, Models: "auto,bench-a"}
	d, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(candidates) != 1 || candidates[0] != "bench-a" || d.Arena.ProposedModel != "bench-a" {
		t.Fatalf("permission precheck bypassed: %v / %+v / %v", candidates, d.Arena, err)
	}
	ak.Models = "auto,bench-a,bench-b"
	ak.ModelQuotas = map[string]ModelQuota{"bench-b": {QuotaUSD: 1, UsedUSD: 1}}
	d, candidates, err = resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(candidates) != 1 || candidates[0] != "bench-a" || d.Arena.ProposedModel != "bench-a" {
		t.Fatalf("exhausted high-ranked model was proposed: %v / %v", candidates, err)
	}
	ak.ModelQuotas = nil
	cfg.ModelContextLimits = map[string]int{"bench-a": 1000000, "bench-b": 64}
	longRequest := *cr
	longRequest.Messages = []canonMsg{{Role: "user", Text: strings.Repeat("long input ", 1000)}}
	d, candidates, err = resolveAutoRouteWithConfig(context.Background(), cfg, ak, FormatOpenAIChat, false, http.Header{}, &longRequest, true)
	if err != nil || len(candidates) != 1 || candidates[0] != "bench-a" || d.Arena.ProposedModel != "bench-a" {
		t.Fatalf("context precheck bypassed by ranking: %v / %v", candidates, err)
	}
}

func TestArenaRoutingModeAPIAndZeroExtraModelCalls(t *testing.T) {
	setupAutoRouterTestDB(t)
	arenaRoutingTestPrices(t, map[string]Price{"bench-a": {In: 1, Out: 1}, "bench-b": {In: 1, Out: 1}})
	var aCalls, bCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "bench-a" {
			aCalls.Add(1)
		} else {
			bCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()
	if err := dbInsertUpstream(&Upstream{Name: "actual-only", Type: "openai", BaseURL: server.URL, Models: "bench-a,bench-b", Weight: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	arenaRoutingTestSource(t, "text", arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410), arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	cfg := defaultAutoRouterConfig()
	cfg.Mode = "live"
	cfg.Classifier = "heuristic"
	cfg.ModelPool = []string{"bench-a", "bench-b"}
	cfg.Tiers[TierSimple] = []string{"bench-a", "bench-b"}
	cfg.ArenaMode = "shadow" // Observation is explicitly selected, never a mandatory launch phase.
	cfg.StrategyProfile = StrategyQualityFirst
	cfg.BanditEnabled = false
	if err := saveAutoRouterConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if getAutoRouterConfig().ArenaMode != "shadow" {
		t.Fatal("explicit observation mode must remain available")
	}
	r := gin.New()
	ak := ApiKey{Models: "auto,bench-a,bench-b"}
	r.POST("/relay", func(c *gin.Context) { c.Set("apiKey", ak) }, relayHandler(FormatOpenAIChat, false))
	registerAutoRouterAdminRoutes(r.Group("/api"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/relay", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)))
	if w.Code != 200 || aCalls.Load() != 1 || bCalls.Load() != 0 || w.Header().Get("X-APISHARE-Router-Arena-Mode") != "shadow" {
		t.Fatalf("shadow invoked proposed model: %d / %d / %d / %s", w.Code, aCalls.Load(), bCalls.Load(), w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/auto-router/config", strings.NewReader(`{"arena_mode":"live"}`)))
	if w.Code != 200 || getAutoRouterConfig().ArenaMode != "live" {
		t.Fatalf("mode not saved: %d / %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/relay", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello"}]}`)))
	if w.Code != 200 || aCalls.Load() != 1 || bCalls.Load() != 1 || w.Header().Get("X-APISHARE-Router-Arena-Mode") != "live" {
		t.Fatalf("explicit activation did not change the actual choice: %d / %d / %d / %s", w.Code, aCalls.Load(), bCalls.Load(), w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/auto-router/config", strings.NewReader(`{"arena_mode":"invalid"}`)))
	if w.Code != 400 {
		t.Fatalf("invalid mode accepted: %d", w.Code)
	}
	ucfg := defaultUserAutoRouterConfig()
	ucfg.ArenaMode = "LIVE"
	if err := saveUserAutoRouterConfig("owner", ucfg); err != nil || getUserAutoRouterConfig("owner").ArenaMode != "live" {
		t.Fatalf("personal mode did not persist: %v", err)
	}
}
