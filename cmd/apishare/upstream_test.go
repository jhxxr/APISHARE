package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func resetUpstreamHealthForTest() {
	upHealth.Lock()
	defer upHealth.Unlock()
	upHealth.fails = map[int64]int{}
	upHealth.until = map[int64]time.Time{}
}

func TestPickUpstreamPriorityBeatsWeight(t *testing.T) {
	modelsTestDB(t)
	primary := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "shared-model", Priority: 0, Weight: 1, Enabled: true}
	standby := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "shared-model", Priority: 1, Weight: 1000000, Enabled: true}
	for _, up := range []*Upstream{primary, standby} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
		healthMarkSuccess(up.ID)
	}
	for i := 0; i < 50; i++ {
		selected, err := pickUpstream(FormatOpenAIChat, "shared-model", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if selected.ID != primary.ID {
			t.Fatalf("priority 0 must beat priority 1 regardless of weight, got %d", selected.ID)
		}
	}
	// The lower-priority channel is the failover target once the primary is out.
	selected, err := pickUpstream(FormatOpenAIChat, "shared-model", map[int64]bool{primary.ID: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != standby.ID {
		t.Fatalf("selected %d, want standby %d", selected.ID, standby.ID)
	}
}

func TestPickUpstreamPriorityOutranksFormatTier(t *testing.T) {
	modelsTestDB(t)
	// Native format but lower priority.
	native := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "shared-model", Priority: 1, Weight: 1, Enabled: true}
	// Conversion format but explicitly the main channel.
	conversion := &Upstream{Type: "openai", OpenAIProtocol: "responses", Models: "shared-model", Priority: 0, Weight: 1, Enabled: true}
	for _, up := range []*Upstream{native, conversion} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
		healthMarkSuccess(up.ID)
	}
	selected, err := pickUpstream(FormatOpenAIChat, "shared-model", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != conversion.ID {
		t.Fatalf("user priority must outrank format tier, got %d want %d", selected.ID, conversion.ID)
	}
}

func TestPickUpstreamSamePriorityWeighted(t *testing.T) {
	modelsTestDB(t)
	first := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "shared-model", Weight: 1, Enabled: true}
	second := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "shared-model", Weight: 1, Enabled: true}
	for _, up := range []*Upstream{first, second} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
		healthMarkSuccess(up.ID)
	}
	seen := map[int64]bool{}
	for i := 0; i < 200; i++ {
		selected, err := pickUpstream(FormatOpenAIChat, "shared-model", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		seen[selected.ID] = true
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("weighted selection at the same priority should reach both upstreams: %v", seen)
	}
}

func TestUpstreamPriorityPersists(t *testing.T) {
	modelsTestDB(t)
	up := &Upstream{Name: "priority", Type: "openai", BaseURL: "https://example.com", APIKey: "k", Models: "m", Priority: 7, Weight: 3, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	got, err := dbGetUpstream(up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 7 {
		t.Fatalf("priority roundtrip got %d, want 7", got.Priority)
	}
	list, err := dbListUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Priority != 7 {
		t.Fatalf("list priority roundtrip: %+v", list)
	}
	got.Priority = 2
	if err := dbUpdateUpstream(got); err != nil {
		t.Fatal(err)
	}
	updated, err := dbGetUpstream(up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Priority != 2 {
		t.Fatalf("priority update got %d, want 2", updated.Priority)
	}
}

func TestMigrateLegacyUpstreamPriority(t *testing.T) {
	resetUpstreamHealthForTest()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepathToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE upstreams (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT 'openai',
		base_url TEXT NOT NULL DEFAULT '',
		api_key TEXT NOT NULL DEFAULT '',
		weight INTEGER NOT NULL DEFAULT 1,
		models TEXT NOT NULL DEFAULT '',
		model_map TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO upstreams(name,weight,enabled) VALUES('legacy',3,1)`); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := openDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ups, err := dbListUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("got %d upstreams, want 1", len(ups))
	}
	if ups[0].Priority != 0 || ups[0].Weight != 3 {
		t.Fatalf("legacy row migrated as %+v, want priority 0 and weight 3", ups[0])
	}
}

func TestPickUpstreamDistinguishesOpenAIFormats(t *testing.T) {
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse} {
		t.Run(string(format), func(t *testing.T) {
			modelsTestDB(t)
			protocol, otherProtocol := "chat", "responses"
			if format == FormatOpenAIResponse {
				protocol, otherProtocol = otherProtocol, protocol
			}
			native := &Upstream{Type: "openai", OpenAIProtocol: protocol, Models: "shared-model", Weight: 1, Enabled: true}
			other := &Upstream{Type: "openai", OpenAIProtocol: otherProtocol, Models: "shared-model", Weight: 1000000, Enabled: true}
			for _, up := range []*Upstream{native, other} {
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
				healthMarkSuccess(up.ID)
			}
			expect := func(id int64, exclude map[int64]bool) {
				t.Helper()
				selected, err := pickUpstream(format, "shared-model", exclude, false)
				if err != nil {
					t.Fatal(err)
				}
				if selected.ID != id {
					t.Fatalf("selected protocol %q (id %d), want id %d", selected.OpenAIProtocol, selected.ID, id)
				}
			}
			// A large conversion weight must not outrank the matching wire format.
			expect(native.ID, nil)
			// Conversion remains available when the matching upstream has failed.
			expect(other.ID, map[int64]bool{native.ID: true})
			native.Models = ""
			if err := dbUpdateUpstream(native); err != nil {
				t.Fatal(err)
			}
			// An explicit model match still takes priority over a native wildcard.
			expect(other.ID, nil)
			other.Models = ""
			if err := dbUpdateUpstream(other); err != nil {
				t.Fatal(err)
			}
			expect(native.ID, nil)
			other.Models = "shared-model"
			if err := dbUpdateUpstream(other); err != nil {
				t.Fatal(err)
			}
			legacy := &Upstream{Type: "openai", Models: "shared-model", Weight: 1, Enabled: true}
			if err := dbInsertUpstream(legacy); err != nil {
				t.Fatal(err)
			}
			healthMarkSuccess(legacy.ID)
			for _, setting := range []string{"", "auto"} {
				legacy.OpenAIProtocol = setting
				if err := dbUpdateUpstream(legacy); err != nil {
					t.Fatal(err)
				}
				expect(legacy.ID, map[int64]bool{native.ID: true})
			}
		})
	}
}

func TestUpstreamCustomCooldownConfigAndDisableForPool(t *testing.T) {
	r := testProxyAdminRouter(t)
	resetUpstreamHealthForTest()

	// 1. Custom threshold, initial cooldown seconds, and max cooldown seconds.
	custom := &Upstream{
		Name:               "自定义冷却渠道",
		Type:               "openai",
		OpenAIProtocol:     "chat",
		BaseURL:            "https://example.invalid/v1",
		APIKey:             "key-custom",
		Models:             "m1",
		Weight:             1,
		Enabled:            true,
		CooldownThreshold:  2,
		CooldownSeconds:    10,
		CooldownMaxSeconds: 15,
	}
	if err := dbInsertUpstream(custom); err != nil {
		t.Fatal(err)
	}
	// First failure (< threshold 2) does not cool down.
	custom.markRelayFailure(502, time.Time{})
	if !custom.availableForRelay("m1") {
		t.Fatal("upstream entered cooldown before reaching configured threshold 2")
	}
	// Second failure (== threshold 2) enters ~10s cooldown.
	before := time.Now()
	custom.markRelayFailure(502, time.Time{})
	if custom.availableForRelay("m1") {
		t.Fatal("upstream did not enter cooldown on reaching threshold 2")
	}
	until := custom.cooldownUntil()
	if until.Before(before.Add(8*time.Second)) || until.After(before.Add(13*time.Second)) {
		t.Fatalf("initial custom cooldown until = %v (before=%v), expected ~10s", until, before)
	}
	// Third failure caps at CooldownMaxSeconds (15s instead of 20s).
	before = time.Now()
	custom.markRelayFailure(502, time.Time{})
	until = custom.cooldownUntil()
	if until.Before(before.Add(13*time.Second)) || until.After(before.Add(18*time.Second)) {
		t.Fatalf("capped custom cooldown until = %v (before=%v), expected ~15s", until, before)
	}

	// 2. Account pool upstream with CooldownDisabled = true never enters gateway cooldown.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"pool account rate limited"}`)
	}))
	defer server.Close()

	poolUp := &Upstream{
		Name:             "外部号池",
		Type:             "opencode",
		OpenAIProtocol:   "chat",
		BaseURL:          server.URL,
		APIKey:           "pool-key",
		Models:           "m1",
		Weight:           1,
		Enabled:          true,
		CooldownDisabled: true,
	}
	if err := dbInsertUpstream(poolUp); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		poolUp.markRelayFailure(502, time.Now().Add(time.Minute))
		healthMarkFailure(poolUp.ID)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	_, _, _ = doRelay(ctx, poolUp, fixtureTask(t, FormatOpenAIChat, false))
	if !poolUp.availableForRelay("m1") || poolUp.relayFailures() != 0 || !poolUp.cooldownUntil().IsZero() {
		t.Fatalf("cooldown-disabled pool upstream became unavailable: available=%v fails=%d until=%v",
			poolUp.availableForRelay("m1"), poolUp.relayFailures(), poolUp.cooldownUntil())
	}

	// 3. Multi-key upstream with custom threshold and CooldownDisabled toggle.
	multi := &Upstream{
		Name:              "多密钥自定义阈值",
		Type:              "openai",
		BaseURL:           "https://example.invalid/v1",
		Models:            "m1",
		Weight:            1,
		Enabled:           true,
		CooldownThreshold: 2,
		CooldownSeconds:   20,
		APIKeys: []UpstreamCredential{
			{ID: "k1", APIKey: "sec-1"},
		},
	}
	if err := dbInsertUpstream(multi); err != nil {
		t.Fatal(err)
	}
	sel := multi.withAPIKey(multi.APIKeys[0])
	sel.markRelayFailure(500, time.Time{})
	if !sel.credentialAvailable("m1") {
		t.Fatal("pooled key cooled on first failure despite CooldownThreshold=2")
	}
	sel.markRelayFailure(500, time.Time{})
	if sel.credentialAvailable("m1") {
		t.Fatal("pooled key did not cool on second failure with CooldownThreshold=2")
	}

	// 4. Admin API validation and enabling CooldownDisabled via PATCH clears existing cooldown.
	if w := proxyAdminRequest(r, "POST", "upstreams", `{"name":"bad","type":"openai","base_url":"https://example.invalid","api_key":"k","cooldown_seconds":60,"cooldown_max_seconds":30}`, true); w.Code != 400 {
		t.Fatalf("expected 400 when cooldown_max_seconds < cooldown_seconds, got %d: %s", w.Code, w.Body.String())
	}
	if w := proxyAdminRequest(r, "PATCH", fmt.Sprintf("upstreams/%d", custom.ID), `{"cooldown_disabled":true}`, true); w.Code != 200 {
		t.Fatalf("PATCH cooldown_disabled failed: %d %s", w.Code, w.Body.String())
	}
	reloaded, err := dbGetUpstream(custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.CooldownDisabled || !reloaded.availableForRelay("m1") || reloaded.relayFailures() != 0 {
		t.Fatalf("enabling cooldown_disabled did not clear cooldown: %+v", reloaded)
	}
	listResp := proxyAdminRequest(r, "GET", "upstreams", "", true)
	if listResp.Code != 200 || !strings.Contains(listResp.Body.String(), `"cooldown_disabled":true`) {
		t.Fatalf("GET /api/upstreams missing cooldown_disabled: %d %s", listResp.Code, listResp.Body.String())
	}
	var views []map[string]any
	if err := json.Unmarshal(listResp.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
}
