package main

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestUnifiedRouterPoolMigratesLegacyConfig(t *testing.T) {
	setupAutoRouterTestDB(t)
	legacy := map[string][]string{
		TierSimple:    {"z", "variant(high,128k)"},
		TierReasoning: {"a", "z"},
	}
	want := []string{"a", "variant(high,128k)", "z"}
	cfg := defaultAutoRouterConfig()
	cfg.ModelPool, cfg.Tiers = []string{"z"}, legacy
	if err := normalizeAutoRouterConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ModelPool, want) || cfg.CascadeTiers {
		t.Fatalf("site migration retained priority: %+v", cfg)
	}
	for _, models := range cfg.Tiers {
		if len(models) != 0 {
			t.Fatal("legacy site candidates survived migration")
		}
	}
	if err := normalizeAutoRouterConfig(&cfg); err != nil || !reflect.DeepEqual(cfg.ModelPool, want) {
		t.Fatalf("site migration is not idempotent: %v / %v", cfg.ModelPool, err)
	}
	ucfg := defaultUserAutoRouterConfig()
	ucfg.ModelPool, ucfg.Tiers = []string{"z"}, legacy
	if err := normalizeUserAutoRouterConfig("owner", &ucfg, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ucfg.ModelPool, want) || ucfg.CascadeTiers {
		t.Fatalf("personal migration retained priority: %+v", ucfg)
	}
	for _, models := range ucfg.Tiers {
		if len(models) != 0 {
			t.Fatal("legacy personal candidates survived migration")
		}
	}
	for _, mode := range []string{"", "live", "shadow", "off"} {
		wantMode := mode
		if mode == "" {
			wantMode = "live"
		}
		if actual, err := normalizeArenaRoutingMode(mode); err != nil || actual != wantMode {
			t.Fatalf("mode %q: %q / %v", mode, actual, err)
		}
	}
	if defaultAutoRouterConfig().ArenaMode != "live" || defaultUserAutoRouterConfig().ArenaMode != "live" {
		t.Fatal("Arena routing should be directly enabled by default")
	}
	legacyJSON := `{"mode":"off","tiers":{"SIMPLE":["z"],"REASONING":["a"]}}`
	if err := settingsSet(autoRouterSettingsKey, legacyJSON); err != nil {
		t.Fatal(err)
	}
	if err := loadAutoRouterCache(); err != nil {
		t.Fatal(err)
	}
	loaded := getAutoRouterConfig()
	if loaded.ArenaMode != "live" || !reflect.DeepEqual(loaded.ModelPool, []string{"a", "z"}) {
		t.Fatalf("stored site config did not migrate: %+v", loaded)
	}
	if _, err := db.Exec(`INSERT INTO user_auto_routers(sub,config_json) VALUES(?,?)`, "old-owner", legacyJSON); err != nil {
		t.Fatal(err)
	}
	loadedUser := getUserAutoRouterConfig("old-owner")
	if loadedUser.ArenaMode != "live" || !reflect.DeepEqual(loadedUser.ModelPool, []string{"a", "z"}) {
		t.Fatalf("stored personal config did not migrate: %+v", loadedUser)
	}
}

func TestUnifiedRouterPoolLegacyTierCannotRestrictSelection(t *testing.T) {
	_, _, cr, _ := arenaRoutingTestSetup(t, "bench-a", "bench-b")
	arenaRoutingTestSource(t, "text",
		arenaRoutingTestModel("Bench A", 2, 1400, 1390, 1410),
		arenaRoutingTestModel("Bench B", 1, 1500, 1490, 1510))
	cfg := defaultAutoRouterConfig()
	cfg.Mode, cfg.Classifier = "live", "heuristic"
	cfg.StrategyProfile, cfg.BanditEnabled = StrategyQualityFirst, false
	// Former REASONING membership cannot hide the best model from a simple task.
	cfg.Tiers = map[string][]string{TierSimple: {"bench-a"}, TierReasoning: {"bench-b"}}
	cfg.CascadeTiers = false
	for _, prompt := range []string{"hello", "证明对于任意正整数 n，若 2^n-1 是素数，则 n 必为素数"} {
		cr.Messages[0].Text = prompt
		d, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
		if err != nil || len(candidates) != 2 || candidates[0] != "bench-b" || !d.AutonomousPool {
			t.Fatalf("site retained old tier boundary: %v / %+v / %v", candidates, d, err)
		}
		ucfg := defaultUserAutoRouterConfig()
		ucfg.Enabled, ucfg.BanditEnabled, ucfg.CascadeTiers = true, false, false
		ucfg.Tiers, ucfg.StrategyProfile = cfg.Tiers, StrategyQualityFirst
		cr.Model = ucfg.VirtualModel
		d, targets, err := resolveUserAutoRouteWithConfig(context.Background(), "owner", ucfg,
			ApiKey{Models: "bench-a,bench-b"}, FormatOpenAIChat, false, http.Header{}, cr, true)
		if err != nil || len(targets) != 2 || targets[0].Model != "bench-b" {
			t.Fatalf("personal retained old tier boundary: %v / %+v / %v", targets, d, err)
		}
		cr.Model = cfg.VirtualModel
	}
	// Explicit exclusions still win over old tier membership and source quality.
	cfg.ExcludedModels = []string{"bench-b"}
	_, candidates, err := resolveAutoRouteWithConfig(context.Background(), cfg, ApiKey{}, FormatOpenAIChat, false, http.Header{}, cr, true)
	if err != nil || len(candidates) != 1 || candidates[0] != "bench-a" {
		t.Fatalf("legacy migration bypassed exclusion: %v / %v", candidates, err)
	}
}
