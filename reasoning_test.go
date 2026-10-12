package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestReasoningEffortModelMapping(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"deepseek-v4-pro", "xhigh", "max"},
		{"deepseek/deepseek-flash", "ultra", "max"},
		{"DeepSeek-V4-Pro", "medium", "high"},
		{"deepseek-flash", "minimal", "low"},
		{"deepseek-flash", "max", "max"},
		{"deepseek-flash", "none", "none"},
		{"deepseek-flash", "custom", "custom"},
		{"gpt-5.1-2025-11-13", "xhigh", "high"},
		{"gpt-5.2", "xhigh", "xhigh"},
		{"gpt-5.2", "max", "xhigh"},
		{"claude-opus-4-6", "xhigh", "max"},
		{"claude-opus-4-7", "xhigh", "xhigh"},
		{"unknown-model", "xhigh", "xhigh"},
		{"gpt-5.1-custom", "xhigh", "xhigh"},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			if got := mapReasoningEffort(tc.effort, defaultReasoningEfforts(tc.model)); got != tc.want {
				t.Fatalf("effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNativeReasoningEffortPreservesOtherFields(t *testing.T) {
	for _, tc := range []struct {
		format clientFormat
		body   string
	}{
		{FormatOpenAIChat, `{"model":"deepseek-flash","reasoning_effort":"xhigh","messages":[],"metadata":{"id":9007199254740993},"custom":true}`},
		{FormatOpenAIResponse, `{"model":"deepseek-flash","reasoning":{"effort":"xhigh","summary":"auto","budget":9007199254740993},"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`},
		{FormatAnthropic, `{"model":"deepseek-flash","output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{}}},"thinking":{"type":"adaptive"},"messages":[]}`},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			got := mapReasoningEffortBody([]byte(tc.body), tc.format, defaultReasoningEfforts("deepseek-flash"))
			want := strings.Replace(tc.body, `"xhigh"`, `"max"`, 1)
			var actual, expected any
			if json.Unmarshal(got, &actual) != nil || json.Unmarshal([]byte(want), &expected) != nil || jsonText(actual) != jsonText(expected) {
				t.Fatalf("effort rewrite lost fields: %s", got)
			}
			if strings.Contains(tc.body, "9007199254740993") && !strings.Contains(string(got), "9007199254740993") {
				t.Fatalf("effort rewrite rounded a JSON number: %s", got)
			}
		})
	}
	for _, body := range []string{
		`{ "reasoning": { "effort": "max", "summary": "auto" } }`,
		`{ "reasoning": { "effort": "none" } }`,
		`{ "reasoning": { "effort": "custom" } }`,
		`{ "reasoning": { "effort": 7 } }`,
		`{ "reasoning": null }`, `{ "reasoning": [] }`, `{ "input": "hi" }`, `null`, `invalid`,
	} {
		if got := mapReasoningEffortBody([]byte(body), FormatOpenAIResponse, []string{"low", "high", "max"}); string(got) != body {
			t.Fatalf("unchanged request was rewritten: %s", got)
		}
	}
}

func reasoningTestRequest(t *testing.T, format clientFormat, stream bool) *relayTask {
	t.Helper()
	j := map[string]any{"model": "gpt-5.2", "stream": stream}
	switch format {
	case FormatOpenAIResponse:
		j["input"] = "hi"
		j["reasoning"] = map[string]any{"effort": "xhigh"}
	case FormatAnthropic:
		j["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		j["max_tokens"] = 64
		j["output_config"] = map[string]any{"effort": "xhigh"}
	default:
		j["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		j["reasoning_effort"] = "xhigh"
	}
	body, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := parseClientRequest(format, body, "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("parse: %v / %+v", err, cr)
	}
	return &relayTask{format: format, endpoint: nativeEndpoint(format, cr), body: body, canon: cr, model: cr.Model, stream: stream}
}

func reasoningTestWireEffort(j map[string]any, format clientFormat) string {
	switch format {
	case FormatOpenAIResponse:
		return str(object(j["reasoning"])["effort"])
	case FormatAnthropic:
		return str(object(j["output_config"])["effort"])
	default:
		return str(j["reasoning_effort"])
	}
}

func TestReasoningEffortRelayMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	formats := []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic}
	for _, in := range formats {
		for _, out := range formats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", in, out, stream), func(t *testing.T) {
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						var j map[string]any
						if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
							t.Error(err)
						}
						if j["model"] != "deepseek-flash" || reasoningTestWireEffort(j, out) != "max" {
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"error":{"message":"思考强度不在当前模型声明的选项中"}}`)
							return
						}
						if r.URL.Path != nativeEndpoint(out, &canonReq{Model: "deepseek-flash", Stream: stream}) {
							t.Errorf("wrong path: %s", r.URL.Path)
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, fixtureSSE(out))
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, protocolResponses[out])
						}
					}))
					defer server.Close()
					up := fixtureUpstream(out, server.URL)
					up.ModelMap = `{"gpt-5.2":"deepseek-flash"}`
					task := reasoningTestRequest(t, in, stream)
					original := string(task.body)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest("POST", task.endpoint, nil)
					info, wrote, err := doRelay(c, up, task)
					if err != nil || !wrote || info.Status != http.StatusOK || rec.Code != http.StatusOK || calls != 1 {
						t.Fatalf("relay: status=%d wrote=%t calls=%d err=%v body=%s", info.Status, wrote, calls, err, rec.Body.String())
					}
					if task.canon.ReasoningEffort != "xhigh" || string(task.body) != original {
						t.Fatal("relay modified the client's original effort")
					}
				})
			}
		}
	}
}

func TestReasoningEffortFailoverRemapsOriginalRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		want := "xhigh"
		if j["model"] == "deepseek-flash" {
			want = "max"
		}
		if got := reasoningTestWireEffort(j, FormatOpenAIChat); got != want {
			t.Errorf("model=%s effort=%q want=%q", j["model"], got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		if j["model"] == "deepseek-flash" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
	}))
	defer server.Close()
	up := fixtureUpstream(FormatOpenAIChat, server.URL)
	up.ModelMap = `{"gpt-5.2":"deepseek-flash"}`
	task := reasoningTestRequest(t, FormatOpenAIResponse, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", task.endpoint, nil)
	if _, wrote, err := doRelay(c, up, task); err == nil || wrote {
		t.Fatal("failed first attempt did not allow failover")
	}
	up.ModelMap = ""
	if info, wrote, err := doRelay(c, up, task); err != nil || !wrote || info.Status != http.StatusOK {
		t.Fatalf("failover failed: status=%d wrote=%t err=%v", info.Status, wrote, err)
	}
}

func TestReasoningEffortCatalogDeclarationsAndCacheIsolation(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			var j map[string]any
			json.NewDecoder(r.Body).Decode(&j)
			if j["model"] != "custom-model" || reasoningTestWireEffort(j, FormatOpenAIChat) != "max" {
				t.Errorf("relay ignored catalog declarations: %v", j)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
			return
		}
		calls++
		io.WriteString(w, `{"data":[{"id":"custom-model","effort":{"supported_levels":["max","high"]}},{"id":"deepseek-flash","effort":{"supported_levels":["low","high"]}},{"id":"legacy-model","effort":"unknown"}]}`)
	}))
	defer server.Close()
	up := &Upstream{Type: "openai", BaseURL: server.URL, APIKey: "first-key"}
	if _, err := cachedUpstreamModels(context.Background(), up); err != nil {
		t.Fatal(err)
	}
	if got := mapReasoningEffort("xhigh", upstreamReasoningEfforts(up, "custom-model")); got != "max" {
		t.Fatalf("custom model declaration ignored: %q", got)
	}
	if got := mapReasoningEffort("xhigh", upstreamReasoningEfforts(up, "deepseek-flash")); got != "high" {
		t.Fatalf("declaration did not override fallback: %q", got)
	}
	up.ModelMap = `{"gpt-5.2":"custom-model"}`
	task := reasoningTestRequest(t, FormatOpenAIChat, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", task.endpoint, nil)
	if info, wrote, err := doRelay(c, up, task); err != nil || !wrote || info.Status != http.StatusOK {
		t.Fatalf("catalog relay failed: status=%d wrote=%t err=%v", info.Status, wrote, err)
	}
	key := upstreamModelCacheKey(up)
	modelsCache.Lock()
	cached := modelsCache.m[key]
	cached.at = time.Now().Add(-2 * modelsCacheTTL)
	modelsCache.m[key] = cached
	modelsCache.Unlock()
	if got := mapReasoningEffort("xhigh", upstreamReasoningEfforts(up, "deepseek-flash")); got != "max" {
		t.Fatalf("expired declaration was used: %q", got)
	}
	up.APIKey = "second-key"
	if got := mapReasoningEffort("xhigh", upstreamReasoningEfforts(up, "custom-model")); got != "xhigh" {
		t.Fatalf("another connection inherited declarations: %q", got)
	}
	if calls != 1 {
		t.Fatalf("mapping made an extra catalog request: %d", calls)
	}
}
