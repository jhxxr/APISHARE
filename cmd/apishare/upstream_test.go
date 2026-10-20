package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
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
