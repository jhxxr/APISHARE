package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func fixedThinkingTask(t *testing.T, source clientFormat, budget int, effort string, stream bool) *relayTask {
	t.Helper()
	body := map[string]any{"model": "alias", "stream": stream}
	if source == FormatAnthropic {
		body["max_tokens"] = budget + 2048
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		if effort != "" {
			body["output_config"] = map[string]any{"effort": effort}
		}
	} else {
		body["generationConfig"] = map[string]any{"maxOutputTokens": budget + 2048, "thinkingConfig": map[string]any{"thinkingBudget": budget}}
		body["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}
	}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := parseClientRequest(source, wire, "alias")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("parse: %v / %+v", err, cr)
	}
	return &relayTask{format: source, endpoint: nativeEndpoint(source, cr), model: "alias", body: wire, canon: cr, stream: stream}
}

func TestFixedThinkingBudgetEffortConversion(t *testing.T) {
	for _, source := range []clientFormat{FormatAnthropic, FormatGemini} {
		for _, target := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse} {
			for _, tc := range []struct {
				budget int
				want   string
			}{{1024, "low"}, {1025, "medium"}, {8192, "medium"}, {8193, "high"}, {32768, "high"}} {
				t.Run(fmt.Sprintf("%s/%s/%d", source, target, tc.budget), func(t *testing.T) {
					task := fixedThinkingTask(t, source, tc.budget, "", false)
					wire, err := renderProtocolRequest(target, task.canon, "mapped")
					if err != nil {
						t.Fatal(err)
					}
					body, err := decodeProtocolObject(wire)
					if err != nil || reasoningTestWireEffort(body, target) != tc.want {
						t.Fatalf("effort conversion: %s / %v", wire, err)
					}
					limit := "max_tokens"
					if target == FormatOpenAIResponse {
						limit = "max_output_tokens"
					}
					if body[limit] != json.Number(fmt.Sprint(tc.budget+2048)) || body["thinking"] != nil {
						t.Fatalf("invalid converted budget fields: %s", wire)
					}
					if task.canon.ReasoningEffort != "" || *task.canon.ThinkingBudget != tc.budget {
						t.Fatal("conversion changed the original request")
					}
				})
			}
		}
	}
	for _, target := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse} {
		task := fixedThinkingTask(t, FormatAnthropic, 1024, "xhigh", false)
		wire, err := renderProtocolRequest(target, task.canon, "mapped")
		body, _ := decodeProtocolObject(wire)
		if err != nil || reasoningTestWireEffort(body, target) != "xhigh" {
			t.Fatalf("explicit effort lost precedence: %s / %v", wire, err)
		}
	}
}

func TestFixedThinkingBudgetRelayAndFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, source := range []clientFormat{FormatAnthropic, FormatGemini} {
		for _, target := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", source, target, stream), func(t *testing.T) {
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						want := "high" // DeepSeek maps the derived medium to high.
						if body["model"] == "custom-budget-model" {
							want = "medium" // Catalog overrides the fallback; retry starts from the original budget.
						}
						if got := reasoningTestWireEffort(body, target); got != want {
							t.Errorf("model=%s effort=%q want=%q", body["model"], got, want)
						}
						if calls == 1 {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, fixtureSSE(target))
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, protocolResponses[target])
						}
					}))
					defer server.Close()
					up := fixtureUpstream(target, server.URL)
					up.ModelMap = `{"alias":"deepseek-flash"}`
					task := fixedThinkingTask(t, source, 8192, "", stream)
					original := string(task.body)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest("POST", task.endpoint, nil)
					if _, wrote, err := doRelay(c, up, task); err == nil || wrote || calls != 1 {
						t.Fatalf("first attempt: wrote=%t calls=%d err=%v", wrote, calls, err)
					}
					up.ModelMap = `{"alias":"custom-budget-model"}`
					key := upstreamModelCacheKey(up)
					modelsCache.Lock()
					previous, existed := modelsCache.m[key]
					modelsCache.m[key] = cachedModels{at: time.Now(), efforts: map[string][]string{"custom-budget-model": {"medium", "max"}}}
					modelsCache.Unlock()
					t.Cleanup(func() {
						modelsCache.Lock()
						defer modelsCache.Unlock()
						if existed {
							modelsCache.m[key] = previous
						} else {
							delete(modelsCache.m, key)
						}
					})
					if info, wrote, err := doRelay(c, up, task); err != nil || !wrote || info.Status != http.StatusOK || calls != 2 {
						t.Fatalf("retry: status=%d wrote=%t calls=%d err=%v", info.Status, wrote, calls, err)
					}
					if task.canon.ReasoningEffort != "" || *task.canon.ThinkingBudget != 8192 || string(task.body) != original {
						t.Fatal("relay changed the original thinking configuration")
					}
				})
			}
		}
	}
}
