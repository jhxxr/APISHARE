package opencodecompat

import (
	"regexp"
	"strings"
	"testing"
)

var opaqueClientToolPattern = regexp.MustCompile(`^clint_[0-9a-f]{12}$`)

// A prefix alone still exposes Claude Code's identifying tool vocabulary.
// Keep names opaque without changing schemas or the response rename table.
func TestClientToolWireNameHidesClientIdentity(t *testing.T) {
	used := map[string]bool{}
	for _, client := range []string{"ReportFindings", "ScheduleWakeup", "Read", "read.file", "read/file", "中文", strings.Repeat("x", 300)} {
		wire := ClientToolWireName(client, used)
		if !opaqueClientToolPattern.MatchString(wire) {
			t.Errorf("client tool %q is recognizable on the wire: %q", client, wire)
		}
		if wire != ClientToolWireName(client, nil) {
			t.Errorf("wire name is not stable across requests: %q", client)
		}
	}
	first := ClientToolWireName("ReportFindings", nil)
	taken := map[string]bool{first: true, first + "_2": true}
	if got := ClientToolWireName("ReportFindings", taken); got != first+"_3" {
		t.Fatalf("digest collision was not kept invertible: %q", got)
	}
}
