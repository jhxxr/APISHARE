package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetResponseReasoningForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		responseReasoningCache.Lock()
		defer responseReasoningCache.Unlock()
		clear(responseReasoningCache.Items)
		responseReasoningCache.Order.Init()
		responseReasoningCache.Bytes = 0
	}
	reset()
	t.Cleanup(reset)
}

func TestResponsesReasoningReplayIsScopedAndBounded(t *testing.T) {
	resetResponseReasoningForTest(t)
	scope := responseReplayScope{1, 2, "alias", "deepseek"}
	cacheResponseReasoning(scope, "rs_1", "Plan the edit")
	messages := []canonMsg{{Role: "assistant", ReasoningIDs: []string{"rs_1"}, Calls: []canonCall{{"call_1", "exec", "{}"}}}}
	if got := replayResponseReasoning(messages, scope)[0].Reasoning; got != "Plan the edit" {
		t.Fatalf("reasoning = %q", got)
	}
	if messages[0].Reasoning != "" {
		t.Fatal("retry mutated the original request")
	}
	for _, other := range []responseReplayScope{{9, 2, "alias", "deepseek"}, {1, 9, "alias", "deepseek"}, {1, 2, "other", "deepseek"}, {1, 2, "alias", "other"}} {
		if got := replayResponseReasoning(messages, other)[0].Reasoning; got != "" {
			t.Fatal("reasoning leaked across key/model/upstream")
		}
	}
	for i := 0; i <= responseReasoningMaxItems; i++ {
		cacheResponseReasoning(scope, fmt.Sprint(i), "plan")
	}
	if lookupResponseReasoning(scope, "rs_1") != "" {
		t.Fatal("old reasoning was not evicted")
	}
	if len(responseReasoningCache.Items) > responseReasoningMaxItems {
		t.Fatal("entry limit exceeded")
	}
	for i := 0; i < 10; i++ {
		cacheResponseReasoning(scope, fmt.Sprint("large", i), strings.Repeat("x", 1<<20))
	}
	if responseReasoningCache.Bytes > responseReasoningMaxBytes {
		t.Fatal("memory limit exceeded")
	}
	cacheResponseReasoning(scope, "expired", "old")
	responseReasoningCache.Lock()
	e := responseReasoningCache.Items[responseReasoningKey{scope, "expired"}]
	entry := e.Value.(responseReasoningEntry)
	entry.Expires = time.Now().Add(-time.Second)
	e.Value = entry
	// Writes order entries by expiry; move this deliberately expired test entry first.
	responseReasoningCache.Order.MoveToFront(e)
	responseReasoningCache.Unlock()
	if lookupResponseReasoning(scope, "expired") != "" {
		t.Fatal("expired reasoning was replayed")
	}
}

func TestResponsesReasoningFollowsAssistantTurns(t *testing.T) {
	cr := parseCodexFixture(t, `{"model":"alias","input":[{"role":"user","content":"first"},{"type":"reasoning","id":"rs_first","summary":[{"type":"summary_text","text":"First plan"}]},{"type":"function_call","call_id":"c1","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"done"},{"role":"assistant","content":"finished"},{"role":"user","content":"next"},{"role":"assistant","content":"second answer"}]}`)
	if cr.Messages[1].Reasoning != "First plan" || cr.Messages[3].Reasoning != "First plan" || cr.Messages[5].Reasoning != "" {
		t.Fatalf("reasoning attached to the wrong turn: %+v", cr.Messages)
	}
}

func TestResponsesReasoningConcurrentReplay(t *testing.T) {
	resetResponseReasoningForTest(t)
	var workers sync.WaitGroup
	for keyID := int64(1); keyID <= 8; keyID++ {
		workers.Add(1)
		go func(keyID int64) {
			defer workers.Done()
			scope := responseReplayScope{keyID, 2, "alias", "deepseek"}
			want := fmt.Sprint("reasoning-for-key-", keyID)
			for i := 0; i < 100; i++ {
				cacheResponseReasoning(scope, "same-item-id", want)
				if got := lookupResponseReasoning(scope, "same-item-id"); got != want {
					t.Errorf("concurrent reasoning lookup = %q", got)
				}
			}
		}(keyID)
	}
	workers.Wait()
	cacheResponseReasoning(responseReplayScope{1, 2, "alias", "deepseek"}, strings.Repeat("x", 257), "oversized id")
	if len(responseReasoningCache.Items) != 8 {
		t.Fatal("oversized replay key was cached")
	}
}

func TestResponsesMultipleReasoningItemsKeepIndividualCacheEntries(t *testing.T) {
	resetResponseReasoningForTest(t)
	scope := responseReplayScope{1, 2, "alias", "deepseek"}
	cr := parseCodexFixture(t, `{"model":"alias","input":[{"role":"user","content":"work"},{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"first plan"}]},{"type":"reasoning","id":"rs_b","summary":[{"type":"summary_text","text":"second plan"}]},{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},{"type":"function_call","call_id":"b","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"b","output":"B"},{"type":"function_call_output","call_id":"a","output":"A"}]}`)
	if cr.Messages[1].Reasoning != "first plan\nsecond plan" || len(cr.Messages[1].ReasoningItems) != 2 || len(cr.Messages[1].Calls) != 2 {
		t.Fatalf("parallel tool batch lost or duplicated reasoning: %+v", cr.Messages)
	}
	got := replayResponseReasoning(cr.Messages, scope)
	if got[1].Reasoning != "first plan\nsecond plan" || lookupResponseReasoning(scope, "rs_a") != "first plan" || lookupResponseReasoning(scope, "rs_b") != "second plan" {
		t.Fatal("combined reasoning was cached under individual item IDs")
	}
	opaque := []canonMsg{{Role: "assistant", ReasoningIDs: []string{"rs_a", "rs_b"}}}
	if replayResponseReasoning(opaque, scope)[0].Reasoning != "first plan\nsecond plan" || lookupResponseReasoning(scope, "rs_b") != "second plan" {
		t.Fatal("opaque multi-item replay dropped an item or corrupted its cache")
	}
}

func TestResponsesMixedPlaintextAndOpaqueReasoningStayDistinct(t *testing.T) {
	resetResponseReasoningForTest(t)
	scope := responseReplayScope{1, 2, "alias", "deepseek"}
	cacheResponseReasoning(scope, "rs_opaque", "cached plan")
	cr := parseCodexFixture(t, `{"model":"alias","input":[{"role":"user","content":"work"},{"type":"reasoning","id":"rs_plain","summary":[{"type":"summary_text","text":"plain plan"}]},{"type":"reasoning","id":"rs_opaque","summary":[]},{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},{"type":"reasoning","id":"rs_after","summary":[{"type":"summary_text","text":"extra plan"}]},{"type":"function_call","call_id":"b","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"A"},{"type":"function_call_output","call_id":"b","output":"B"},{"role":"user","content":"next"},{"role":"assistant","content":"answer"}]}`)
	got := replayResponseReasoning(cr.Messages, scope)
	if got[1].Reasoning != "plain plan\ncached plan\nextra plan" || lookupResponseReasoning(scope, "rs_plain") != "plain plan" || lookupResponseReasoning(scope, "rs_opaque") != "cached plan" || lookupResponseReasoning(scope, "rs_after") != "extra plan" {
		t.Fatalf("reasoning identities were mixed: %+v", got)
	}
	if got[len(got)-1].Reasoning != "" {
		t.Fatal("reasoning carried into a later user turn")
	}
	if cr.Messages[1].Reasoning != "plain plan\nextra plan" {
		t.Fatal("retry changed the original reasoning span")
	}
}
