package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestOpenCodeFreeEndpointRoutes(t *testing.T) {
	for _, tc := range []struct {
		base, tier string
		format     clientFormat
		want       string
	}{
		{"https://opencode.ai/inference", "", FormatOpenAIChat, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/inference/v1/", "", FormatOpenAIResponse, "https://opencode.ai/inference/openai/v1/responses"},
		{"https://opencode.ai/inference/openai/v1", "", FormatAnthropic, "https://opencode.ai/inference/anthropic/v1/messages?beta=true"},
		{"https://opencode.ai/inference/anthropic", "", FormatOpenAIChat, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"https://opencode.ai/zen", "", FormatAnthropic, "https://opencode.ai/zen/v1/messages"},
		{"https://opencode.ai/zen/go", "go", FormatOpenAIChat, "https://opencode.ai/zen/go/v1/chat/completions"},
		{"https://custom.invalid/inference", "", FormatOpenAIChat, "https://custom.invalid/inference/v1/chat/completions"},
		{"https://custom.invalid/gateway/v1", "", FormatAnthropic, "https://custom.invalid/gateway/v1/messages"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			up := &Upstream{Type: "opencode", BaseURL: tc.base, OpenCodeTier: tc.tier}
			endpoint := nativeEndpoint(tc.format, &canonReq{})
			if got := up.openCodeFreeEndpointURL(tc.format, endpoint); got != tc.want {
				t.Fatalf("endpoint = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOpenCodeV2FreeBodyPreservesClientOptions(t *testing.T) {
	up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/inference", APIKey: "public"}
	original := []byte(`{"model":"free","messages":[{"role":"user","content":"\u003ctag\u003e\u0026 code"}],"metadata":{"large":9007199254740993,"literal":"\\u003c"},"reasoning_effort":"none","stream_options":{"custom":true}}`)
	unchanged := bytes.Clone(original)
	var compatibility openCodeToolCompatibility
	body, shaped, err := up.shapeOpenCodeFreeRequest(original, FormatOpenAIChat, "free", &compatibility)
	if err != nil || !shaped {
		t.Fatalf("shape failed: %v %v", shaped, err)
	}
	if !bytes.Equal(original, unchanged) {
		t.Fatal("shaping mutated the client's original body")
	}
	if !compatibility.Renames.Empty() {
		t.Fatal("a text-only request unexpectedly acquired client aliases")
	}
	for _, want := range []string{`"content":"<tag>& code"`, `"large":9007199254740993`, `"literal":"\\u003c"`, `"reasoning_effort":"none"`, `"max_completion_tokens":32000`, `"stream_options":{"custom":true,"include_usage":true}`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("missing %s in shaped request: %s", want, body)
		}
	}
	for _, escaped := range []string{`\u003e`, `\u0026`} {
		if bytes.Contains(body, []byte(escaped)) {
			t.Errorf("request retained HTML escape %q", escaped)
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, key := range openCodeV2ChatKeyOrder {
		position := bytes.Index(body, []byte(`"`+key+`":`))
		if position <= last {
			t.Errorf("client key order changed at %s: %s", key, body)
		}
		last = position
	}
	var tools []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Strict      *bool  `json:"strict"`
		} `json:"function"`
	}
	if err := json.Unmarshal(payload["tools"], &tools); err != nil || len(tools) != 13 {
		t.Fatalf("missing CLI tools: %s %v", payload["tools"], err)
	}
	for _, tool := range tools {
		if tool.Function.Strict == nil || *tool.Function.Strict {
			t.Errorf("%s did not default to strict:false", tool.Function.Name)
		}
		if tool.Function.Name == "shell" {
			want := "on Linux using bash"
			if runtime.GOOS == "windows" {
				want = "on Windows using powershell"
			}
			if !strings.Contains(tool.Function.Description, want) {
				t.Errorf("shell description does not match host: %q", tool.Function.Description)
			}
		}
	}
	// A caller's completion ceiling wins, even if the value is null or zero.
	for _, limit := range []string{`"max_tokens":64`, `"max_completion_tokens":128`, `"max_tokens":null`, `"max_completion_tokens":0`} {
		request := []byte(`{"model":"free","messages":[],` + limit + `}`)
		got, shaped, err := up.shapeOpenCodeFreeRequest(request, FormatOpenAIChat, "free")
		if err != nil || !shaped || !bytes.Contains(got, []byte(limit)) || bytes.Contains(got, []byte(`"max_completion_tokens":32000`)) {
			t.Errorf("explicit ceiling %s was replaced: %s %v", limit, got, err)
		}
	}
}

func TestOpenCodeV2FreeBodyPreservesNativeTools(t *testing.T) {
	up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/inference", APIKey: "public"}
	definitions, err := openCodeToolDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	var tools []json.RawMessage
	for _, name := range []string{"shell", "edit", "glob", "grep", "read"} {
		tools = append(tools, renderOpenCodeTool(name, definitions[name], FormatOpenAIChat))
	}
	tools = append(tools,
		json.RawMessage(`{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"n":{"const":9007199254740993}}},"strict":true},"cache_control":{"type":"ephemeral"}}`),
		json.RawMessage(`{"type":"function","function":{"name":"extra","parameters":{"type":"object"}}}`))
	original, _ := json.Marshal(map[string]any{"model": "free", "messages": []any{}, "tools": tools})
	body, _, err := up.shapeOpenCodeFreeRequest(original, FormatOpenAIChat, "free")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"strict":true`, `"const":9007199254740993`, `"cache_control":{"type":"ephemeral"}`, `"name":"clint_extra","parameters":{"type":"object"}`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("native tool option lost: %s in %s", want, body)
		}
	}
	foreign := []byte(`{"model":"free","messages":[],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}}]}`)
	body, _, err = up.shapeOpenCodeFreeRequest(foreign, FormatOpenAIChat, "free")
	if err != nil || !bytes.Contains(body, []byte(`"name":"clint_Read"`)) || !bytes.Contains(body, []byte(`"file_path":{"type":"string"}`)) {
		t.Fatalf("foreign tool contract did not survive inference shaping: %s %v", body, err)
	}
}

func TestOpenCodeFreeBodyLaneIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		format     clientFormat
		body       string
	}{
		{"legacy", "https://opencode.ai/zen", FormatOpenAIChat, `{"model":"free","messages":[{"role":"user","content":"<x>&"}]}`},
		{"responses", "https://opencode.ai/inference", FormatOpenAIResponse, `{"model":"free","input":"<x>&"}`},
		{"messages", "https://opencode.ai/inference", FormatAnthropic, `{"model":"free","max_tokens":64,"messages":[{"role":"user","content":"<x>&"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &Upstream{Type: openCodeFreeType, BaseURL: tc.base, APIKey: "public"}
			got, shaped, err := up.shapeOpenCodeFreeRequest([]byte(tc.body), tc.format, "free")
			if err != nil || !shaped || bytes.Contains(got, []byte("max_completion_tokens")) || bytes.Contains(got, []byte(`\u003c`)) {
				t.Fatalf("incorrect lane body: %s %v", got, err)
			}
			if tc.name == "legacy" && (bytes.Contains(got, []byte(`"strict"`)) || bytes.Contains(got, []byte(`"name":"shell"`))) {
				t.Errorf("legacy request received v2 tool defaults: %s", got)
			}
		})
	}
	paid := &Upstream{Type: "opencode", BaseURL: "https://opencode.ai/zen", APIKey: "paid-key"}
	original := []byte(`{"model":"paid","messages":[]}`)
	if got, shaped, err := paid.shapeOpenCodeFreeRequest(original, FormatOpenAIChat, "paid"); err != nil || shaped || !bytes.Equal(got, original) {
		t.Fatalf("paid request was shaped: %s %v %v", got, shaped, err)
	}
}

func TestOpenCodeFreeTierRetryUsesLatestShape(t *testing.T) {
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		for _, fromZen := range []bool{true, false} {
			direction := "inference-to-zen"
			if fromZen {
				direction = "zen-to-inference"
			}
			t.Run(string(format)+"/"+direction, func(t *testing.T) {
				up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/inference", APIKey: "public"}
				if fromZen {
					up.BaseURL = "https://opencode.ai/zen"
				}
				original := []byte(`{"model":"free","messages":[{"role":"user","content":"<hello>&"}]}`)
				if format == FormatOpenAIResponse {
					original = []byte(`{"model":"free","input":"<hello>&"}`)
				}
				session, _ := newOpenCodeSessionID()
				endpoint := nativeEndpoint(format, &canonReq{})
				req := httptest.NewRequest(http.MethodPost, up.openCodeFreeEndpointURL(format, endpoint), nil)
				setOpenCodeHeaders(req, "public", format, session, nil, nil, original)
				resp := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`))}
				attempt, err := retryOpenCodeFreeTier(up, req, resp, original, format, "free", nil, func(retry *http.Request) (*http.Response, error) {
					want := "https://opencode.ai/zen" + endpoint
					if fromZen {
						family := "openai"
						if format == FormatAnthropic {
							family = "anthropic"
						}
						want = "https://opencode.ai/inference/" + family + endpoint
						if format == FormatAnthropic {
							want += "?beta=true"
						}
					}
					if retry.URL.String() != want || retry.Header.Get("x-opencode-session") != session {
						t.Errorf("retry route or session changed: %s %v", retry.URL, retry.Header)
					}
					body, _ := io.ReadAll(retry.Body)
					if bytes.Contains(body, []byte(`\u003c`)) {
						t.Errorf("retry restored HTML escapes: %s", body)
					}
					if fromZen && format == FormatOpenAIChat && (!bytes.HasPrefix(body, []byte(`{"model":`)) || !bytes.Contains(body, []byte(`"max_completion_tokens":32000`))) {
						t.Errorf("retry omitted v2 body shape: %s", body)
					}
					if fromZen && retry.Header.Get("Accept") != "" || !fromZen && retry.Header.Get("Accept") != "*/*" {
						t.Errorf("retry used incorrect Accept: %v", retry.Header)
					}
					return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
				})
				if err != nil || attempt == nil || attempt.response.StatusCode != 200 {
					t.Fatalf("eligibility retry failed: %+v %v", attempt, err)
				}
			})
		}
	}
}

func TestOpenCodeFreeTierRejectsAbuseReplay(t *testing.T) {
	up := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/zen", APIKey: "public"}
	req := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	for _, message := range []string{
		"free models aren't available from your current network",
		"Forbidden, OpenCode Free Model Gateway Detect Abuse Traffic",
		"Abuse of traffic",
		"detected abusive traffic from this IP address",
	} {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "FreeTierError", "message": message}})
		resp := &http.Response{StatusCode: 403, Body: io.NopCloser(bytes.NewReader(body))}
		attempt, err := retryOpenCodeFreeTier(up, req, resp, []byte(`{"model":"free","messages":[]}`), FormatOpenAIChat, "free", nil, func(*http.Request) (*http.Response, error) {
			t.Error("IP abuse block was replayed through another entry")
			return nil, nil
		})
		if err != nil || attempt != nil {
			t.Fatalf("unexpected abuse retry: %+v %v", attempt, err)
		}
		restored, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(restored, body) {
			t.Errorf("classification consumed the original response: %s", restored)
		}
	}
}
