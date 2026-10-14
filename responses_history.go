package main

import (
	"fmt"
	"strings"
)

// A transcript may interleave parts and tool results, whereas Chat providers
// require an assistant call batch immediately followed by its matching replies. Repair
// history only; this never executes, repeats or invents an unanswered tool call.
func normalizeResponsesHistory(messages []canonMsg) ([]canonMsg, error) {
	type callKey struct {
		Batch int
		ID    string
	}
	replies := make(map[callKey]canonMsg)
	owners := make(map[string]callKey)
	for i, msg := range messages {
		for _, call := range msg.Calls {
			if owner, exists := owners[call.ID]; exists && owner.Batch == i {
				return nil, fmt.Errorf("tool batch repeats call ID %q", call.ID)
			}
			owners[call.ID] = callKey{i, call.ID}
		}
		if msg.Role == "tool" && msg.ToolID != "" {
			if owner, exists := owners[msg.ToolID]; exists {
				// Last notification wins within the owning batch. Some providers
				// reuse call IDs in later turns; do not attach their newer results
				// to earlier calls that happened to carry the same ID.
				replies[owner] = msg
			}
		}
	}
	out := make([]canonMsg, 0, len(messages))
	for i, msg := range messages {
		if msg.Role == "tool" {
			// Orphan results cannot be represented by a valid Chat tool message.
			continue
		}
		if len(msg.Calls) == 0 {
			out = append(out, msg)
			continue
		}
		calls := make([]canonCall, 0, len(msg.Calls))
		for _, call := range msg.Calls {
			if _, answered := replies[callKey{i, call.ID}]; answered {
				calls = append(calls, call)
			}
		}
		msg.Calls = calls
		if len(calls) == 0 {
			if len(messageParts(msg)) > 0 && strings.TrimSpace(partsText(messageParts(msg))) != "" {
				out = append(out, msg)
			} else {
				// Preserve assistant images even if its interrupted tool batch vanished.
				for _, part := range messageParts(msg) {
					if part.Kind != "text" {
						out = append(out, msg)
						break
					}
				}
			}
			continue
		}
		out = append(out, msg)
		for _, call := range calls {
			reply := replies[callKey{i, call.ID}]
			reply.ToolName = call.Name
			out = append(out, reply)
		}
	}
	return out, nil
}
