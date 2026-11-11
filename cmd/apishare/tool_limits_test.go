package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

const toolsLimitFixture = `{"error":{"param":"tools","type":"invalid_request_error","message":"Upstream request failed: [invalid_value] Invalid value for 'tools': maximum number of items is 128."}}`

func toolsLimitTestBody(model string, count int, format clientFormat) []byte {
	tools := make([]any, count)
	for i := range tools {
		tool := map[string]any{"name": fmt.Sprintf("tool_%03d", i), "description": "Client tool",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}
		if format == FormatAnthropic {
			tool["input_schema"] = tool["parameters"]
			delete(tool, "parameters")
			tools[i] = tool
		} else {
			tools[i] = map[string]any{"type": "function", "function": tool}
		}
	}
	body := map[string]any{"model": model, "max_tokens": 32, "tools": tools,
		"messages": []any{map[string]any{"role": "user", "content": "Use the last declared tool"}}}
	if format == FormatAnthropic {
		body["tool_choice"] = map[string]any{"type": "tool", "name": fmt.Sprintf("tool_%03d", count-1)}
	} else {
		body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": fmt.Sprintf("tool_%03d", count-1)}}
	}
	b, _ := json.Marshal(body)
	return b
}

func TestToolsLimitRecognitionRequiresActualArrayOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		status, count, want int
	}{
		{"reported", toolsLimitFixture, 400, 129, 128},
		{"equal-not-over", toolsLimitFixture, 400, 128, 0},
		{"no-tool-overflow", toolsLimitFixture, 400, 1, 0},
		{"not-request-error", toolsLimitFixture, 500, 129, 0},
		{"array-length", `{"error":{"param":"tools","message":"Invalid 'tools': array too long. Expected an array with maximum length 128, but got length 129."}}`, 400, 129, 128},
		{"at-most", `{"error":{"param":"tools","message":"Expected at most 128 items"}}`, 422, 129, 128},
		{"nested-schema", `{"error":{"param":"tools[0].parameters.properties.items","message":"maximum number of items is 128"}}`, 400, 129, 0},
		{"invalid-schema", `{"error":{"param":"tools","message":"Invalid schema: maxItems must be 128"}}`, 400, 129, 0},
		{"different-array", `{"error":{"param":"messages","message":"maximum number of items is 128"}}`, 400, 129, 0},
		{"invalid-name", `{"error":{"param":"tools","message":"invalid tool name"}}`, 400, 129, 0},
		{"raw", `Invalid value for 'tools': maximum number of items is 128.`, 400, 129, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamToolsLimit(tc.status, []byte(tc.body), tc.count); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestToolsLimitFallbackPreservesAllToolsAndChoice(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto=%v", auto), func(t *testing.T) {
			setupAutoRouterTestDB(t)
			settings := getSettings()
			settings.Retry = 0 // A rejected endpoint is not retried, even with no network retry budget.
			if err := persistSettings(settings); err != nil {
				t.Fatal(err)
			}
			var badCalls, goodCalls atomic.Int32
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				badCalls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if tools := array(body["tools"]); len(tools) != 129 || str(object(object(tools[len(tools)-1])["function"])["name"]) != "tool_128" {
					t.Errorf("converted upstream lost tools: %v entries", len(tools))
				}
				if str(object(object(body["tool_choice"])["function"])["name"]) != "tool_128" {
					t.Error("converted request lost forced last-tool choice")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_, _ = io.WriteString(w, toolsLimitFixture)
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				goodCalls.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if tools := array(body["tools"]); len(tools) != 129 || str(object(tools[len(tools)-1])["name"]) != "tool_128" || object(tools[len(tools)-1])["input_schema"] == nil {
					t.Errorf("native fallback lost tools/schema: %v entries", len(tools))
				}
				if str(object(body["tool_choice"])["name"]) != "tool_128" {
					t.Error("native fallback lost forced last-tool choice")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg-ok","type":"message","role":"assistant","model":"tool-b","content":[{"type":"text","text":"all tools retained"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer good.Close()
			upBad := &Upstream{Name: "limited-chat", Type: "openai", OpenAIProtocol: "chat", BaseURL: bad.URL, Models: "tool-a", Weight: 1, Enabled: true}
			goodModel := "tool-a"
			if auto {
				goodModel = "tool-b"
			}
			upGood := &Upstream{Name: "compatible-native", Type: "anthropic", BaseURL: good.URL, Models: goodModel, Priority: 1, Weight: 1, Enabled: true}
			for _, up := range []*Upstream{upBad, upGood} {
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			model := "tool-a"
			if auto {
				cfg := defaultAutoRouterConfig()
				cfg.Mode, cfg.Classifier, cfg.StrategyProfile = "live", "heuristic", StrategyQualityFirst
				cfg.ModelPool = []string{"tool-a", "tool-b"}
				if err := saveAutoRouterConfig(cfg); err != nil {
					t.Fatal(err)
				}
				arenaRoutingTestSource(t, "text", arenaRoutingTestModel("tool-a", 1, 1500, 1490, 1510), arenaRoutingTestModel("tool-b", 2, 1400, 1390, 1410))
				model = "auto"
			}
			r := gin.New()
			r.POST("/relay", func(c *gin.Context) { c.Set("apiKey", ApiKey{Models: "auto,tool-a,tool-b"}) }, relayHandler(FormatAnthropic, false))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/relay", strings.NewReader(string(toolsLimitTestBody(model, 129, FormatAnthropic)))))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "all tools retained") || badCalls.Load() != 1 || goodCalls.Load() != 1 {
				t.Fatalf("lossless fallback failed: HTTP %d bad=%d good=%d body=%s", w.Code, badCalls.Load(), goodCalls.Load(), w.Body)
			}
			if !upBad.availableForRelay("tool-a") {
				t.Fatal("tool-count rejection incorrectly cooled the channel")
			}
			if _, ok := lookupBanditArmSummary(upBad.ID, "tool-a", DomainAgenticTools); ok {
				t.Fatal("tool-count rejection incorrectly became a quality/service observation")
			}
		})
	}
}

func TestToolsLimitAllChannelsRejectedReturnsActionable400(t *testing.T) {
	setupAutoRouterTestDB(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, toolsLimitFixture)
	}))
	defer server.Close()
	up := &Upstream{Type: "openai", BaseURL: server.URL, Models: "tool-a", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/relay", func(c *gin.Context) { c.Set("apiKey", ApiKey{Models: "tool-a"}) }, relayHandler(FormatOpenAIChat, false))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/relay", strings.NewReader(string(toolsLimitTestBody("tool-a", 129, FormatOpenAIChat)))))
	if w.Code != 400 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "client tool entries: 129") ||
		!strings.Contains(w.Body.String(), "final upstream tool entries: 129") || !strings.Contains(w.Body.String(), "all tools were preserved") {
		t.Fatalf("wrong final error: HTTP %d calls=%d body=%s", w.Code, calls.Load(), w.Body)
	}
}

func TestToolsLimitCountsOpenCodeExpansionWithoutDroppingClientTools(t *testing.T) {
	up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/inference", APIKey: "public"}
	original := toolsLimitTestBody("free", 116, FormatOpenAIChat)
	var compatibility openCodeToolCompatibility
	shaped, ok, err := up.shapeOpenCodeFreeRequest(original, FormatOpenAIChat, "free", &compatibility)
	if err != nil || !ok || requestToolEntries(original) != 116 || requestToolEntries(shaped) != 129 {
		t.Fatalf("wrong tool expansion counts: client=%d wire=%d / %v", requestToolEntries(original), requestToolEntries(shaped), err)
	}
	if compatibility.Renames.Count() != 116 || upstreamToolsLimit(400, []byte(toolsLimitFixture), requestToolEntries(shaped)) != 128 {
		t.Fatal("injected tools hid the real client/upstream tool counts")
	}
}
