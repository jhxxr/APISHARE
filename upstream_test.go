package main

import "testing"

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
