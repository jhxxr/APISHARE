package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConvertedSSEFailureBeforeOutputDoesNotCommit(t *testing.T) {
	for _, upstream := range []string{
		`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n",
		`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\n",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, upstream)
		}))
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		_, wrote, err := doRelay(c, fixtureUpstream(FormatOpenAIChat, server.URL), fixtureTask(t, FormatOpenAIResponse, true))
		server.Close()
		if err == nil || wrote || c.Writer.Written() || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
			t.Fatalf("early error prevented retry: wrote=%v err=%v headers=%v body=%s", wrote, err, rec.Header(), rec.Body.String())
		}
	}
}

func TestConvertedSSEFailureAfterOutputRemainsCommitted(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n"))}
	err := relayProtocolSSE(c, resp, FormatOpenAIChat, fixtureTask(t, FormatOpenAIResponse, true), &relayInfo{})
	if err == nil || !c.Writer.Written() || !strings.Contains(rec.Body.String(), "response.failed") || strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("committed failure could replay or look successful: %v / %s", err, rec.Body.String())
	}
}

func TestNonStreamClientAcceptsConvertedUpstreamSSE(t *testing.T) {
	for _, format := range protocolFormats {
		if format == FormatOpenAIResponse {
			continue
		}
		t.Run(string(format), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, fixtureSSE(format))
			}))
			defer server.Close()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			info, wrote, err := doRelay(c, fixtureUpstream(format, server.URL), fixtureTask(t, FormatOpenAIResponse, false))
			if err != nil || !wrote || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("buffered conversion failed: %v / %s", err, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "completed" || len(array(body["output"])) == 0 || !info.InKnown || !info.OutKnown {
				t.Fatalf("buffered result lost output or usage: %v / %s / %+v", err, rec.Body.String(), info)
			}
		})
	}
}

func TestBufferedSSERejectsInterruptedOrInvalidTools(t *testing.T) {
	for _, input := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]}}]}\n\ndata: [DONE]\n\n",
	} {
		result, err := collectProtocolSSE(strings.NewReader(input), FormatOpenAIChat)
		if err == nil || result != nil {
			t.Fatalf("invalid buffered stream accepted: %+v / %v", result, err)
		}
	}
}

func TestResponsesItemIDsMatchAcrossStreamEventsAndTerminal(t *testing.T) {
	blocks := []responseBlock{
		{Kind: "thinking", Text: "plan"},
		{Kind: "text", Text: "hello"},
		{Kind: "tool", ToolType: "function", Call: canonCall{"call_function", "read", `{}`}},
		{Kind: "tool", ToolType: "custom", Call: canonCall{"call_custom", "apply_patch", `{"input":"patch"}`}},
		{Kind: "tool", ToolType: "tool_search", Call: canonCall{"call_search", "tool_search", `{"query":"search"}`}},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newProtocolStream(c, FormatOpenAIResponse, "alias", false)
	for i, block := range blocks {
		if err := w.push(c, streamDelta{Key: fmt.Sprint(i), Block: block}, upstreamUsage{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.finish(c, upstreamUsage{}, "tool_calls", ""); err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"rs_", "msg_", "fc_", "ctc_", "tsc_"}
	ids := map[int]string{}
	for _, event := range codexStreamEvents(t, rec.Body.String()) {
		index := intFromAny(event["output_index"])
		if str(event["type"]) == "response.output_item.added" {
			item := object(event["item"])
			ids[index] = str(item["id"])
			if !strings.HasPrefix(ids[index], prefixes[index]) {
				t.Fatalf("wrong item ID prefix: %+v", item)
			}
		}
		if id := str(event["item_id"]); id != "" && id != ids[index] {
			t.Fatalf("delta/done refers to a different item: %+v", event)
		}
		if str(event["type"]) == "response.output_item.done" && object(event["item"])["id"] != ids[index] {
			t.Fatal("output item ID changed on done")
		}
		if str(event["type"]) == "response.completed" {
			for i, raw := range array(object(event["response"])["output"]) {
				item := object(raw)
				if item["id"] != ids[i] || (blocks[i].Kind == "tool" && item["call_id"] != blocks[i].Call.ID) {
					t.Fatal("terminal snapshot changed item ID or call ID")
				}
			}
		}
	}
}

func TestResponsesDoneOnlyAndTerminalSnapshotsDoNotLoseOrDuplicateOutput(t *testing.T) {
	item := map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}
	terminal := map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}
	for _, prelude := range []any{nil, map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "hello"}, map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": "hello"}, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}} {
		var frames strings.Builder
		for _, event := range []any{prelude, terminal} {
			if event != nil {
				fmt.Fprintf(&frames, "data: %s\n\n", jsonText(event))
			}
		}
		r, err := collectProtocolSSE(strings.NewReader(frames.String()), FormatOpenAIResponse)
		if err != nil || len(r.Blocks) != 1 || r.Blocks[0].Text != "hello" || !r.Usage.InKnown || !r.Usage.OutKnown {
			t.Fatalf("lost or duplicated done-only output: %+v / %v", r, err)
		}
	}
}

func TestResponsesObjectArgumentsAndArrayToolOutput(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"read"},{"type":"function_call","call_id":"c","name":"read","arguments":{"path":"x.go"}},{"type":"function_call_output","call_id":"c","output":[{"type":"output_text","text":"first"},{"type":"text","text":"second"}]}]}`
	cr := parseCodexFixture(t, body)
	if len(cr.Messages) != 3 || cr.Messages[1].Calls[0].Arguments != `{"path":"x.go"}` || !strings.Contains(cr.Messages[2].Text, "first") || !strings.Contains(cr.Messages[2].Text, "second") {
		t.Fatalf("polymorphic tool history was lost: %+v", cr.Messages)
	}
}

func TestResponsesInitialStreamFailureRetriesAnotherUpstream(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("retry", "1"); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "stream retry", Key: "stream-retry-test", Enabled: true, Models: "alias"}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	failedCalls, successfulCalls := 0, 0
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failedCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	defer failed.Close()
	successful := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		successfulCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
	}))
	defer successful.Close()
	first := fixtureUpstream(FormatOpenAIChat, failed.URL)
	second := fixtureUpstream(FormatOpenAIChat, successful.URL)
	first.Name, second.Name = "failed", "successful"
	for _, up := range []*Upstream{first, second} {
		up.Enabled, up.Weight, up.Models = true, 1, "alias"
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"model":"alias","stream":true,"input":"hello"}`
	cr := parseCodexFixture(t, body)
	router := gin.New()
	group := router.Group("/v1")
	group.Use(keyAuth())
	group.POST("/responses", relayHandler(FormatOpenAIResponse, false))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("X-Session-Id", "stream-retry-session")
	affinitySet(scopedSessionKey(key.ID, sessionKeyFromCanon("/v1/responses", req.Header, cr)), first.ID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 || failedCalls != 1 || successfulCalls != 1 || !strings.Contains(rec.Body.String(), "response.completed") || strings.Contains(rec.Body.String(), "response.failed") {
		t.Fatalf("initial error did not fail over cleanly: calls=%d/%d body=%s", failedCalls, successfulCalls, rec.Body.String())
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].Status != 200 || logs[0].UpstreamID != second.ID {
		t.Fatalf("retry logged more than the final request: %+v / %v", logs, err)
	}
}
