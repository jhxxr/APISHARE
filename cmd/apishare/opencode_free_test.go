package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestOpenCodeFreeEndpointRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, base, tier, key string
		format                clientFormat
		want                  string
	}{
		{"inference-chat", "https://opencode.ai/inference", "", "public", FormatOpenAIChat, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"inference-responses", "https://opencode.ai/inference/v1/", "", "public", FormatOpenAIResponse, "https://opencode.ai/inference/openai/v1/responses"},
		{"inference-messages", "https://opencode.ai/inference/openai/v1", "", "public", FormatAnthropic, "https://opencode.ai/inference/anthropic/v1/messages?beta=true"},
		{"inference-other-family", "https://opencode.ai/inference/anthropic", "", "public", FormatOpenAIChat, "https://opencode.ai/inference/openai/v1/chat/completions"},
		{"public-zen-migrates", "https://opencode.ai/zen", "", "public", FormatAnthropic, "https://opencode.ai/inference/anthropic/v1/messages?beta=true"},
		{"keyed-zen-stays", "https://opencode.ai/zen", "", "paid-key", FormatAnthropic, "https://opencode.ai/zen/v1/messages"},
		{"go-stays", "https://opencode.ai/zen/go", "go", "public", FormatOpenAIChat, "https://opencode.ai/zen/go/v1/chat/completions"},
		{"custom-inference-stays", "https://custom.invalid/inference", "", "public", FormatOpenAIChat, "https://custom.invalid/inference/v1/chat/completions"},
		{"custom-gateway-stays", "https://custom.invalid/gateway/v1", "", "public", FormatAnthropic, "https://custom.invalid/gateway/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &Upstream{Type: "opencode", BaseURL: tc.base, OpenCodeTier: tc.tier, APIKey: tc.key}
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
		name, kind, base, key string
		format                clientFormat
		body                  string
	}{
		{"legacy", "opencode", "https://opencode.ai/zen", "paid-key", FormatOpenAIChat, `{"model":"free","messages":[{"role":"user","content":"<x>&"}]}`},
		{"responses", openCodeFreeType, "https://opencode.ai/inference", "public", FormatOpenAIResponse, `{"model":"free","input":"<x>&"}`},
		{"messages", openCodeFreeType, "https://opencode.ai/inference", "public", FormatAnthropic, `{"model":"free","max_tokens":64,"messages":[{"role":"user","content":"<x>&"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &Upstream{Type: tc.kind, BaseURL: tc.base, APIKey: tc.key}
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

func TestOpenCodeFreeTierErrorsDoNotReplay(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	type observedRequest struct {
		path, query string
		header      http.Header
		body        []byte
	}
	requests := make(chan observedRequest, 32)
	failure := `{"error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		requests <- observedRequest{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, failure)
	}))
	t.Cleanup(server.Close)
	originalRelay, originalDiagnostic := upstreamHTTPClient, modelTestClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	upstreamHTTPClient = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	modelTestClient = upstreamHTTPClient
	t.Cleanup(func() {
		upstreamHTTPClient, modelTestClient = originalRelay, originalDiagnostic
		transport.CloseIdleConnections()
	})
	proxy := testSavedProxy(t, "socks5h://"+testSOCKSProxy(t, server.Listener.Addr().String(), make(chan string, 32)), "alice", "secret")
	openCodeCatalogs.Lock()
	openCodeCatalogs.entries[openCodeCatalogCacheKey(proxy)] = &openCodeCatalogEntry{catalog: openCodeCatalog{
		At: time.Now(), Tiers: map[string]map[string]clientFormat{"zen": {"model-free": FormatOpenAIChat}},
		FreeModels: map[string]map[string]bool{"zen": {"model-free": true}},
	}}
	openCodeCatalogs.Unlock()

	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		for _, route := range []struct {
			name, kind, base, key string
			legacy                bool
		}{
			{"free-only-zen", openCodeFreeType, "https://opencode.ai/zen/v1", "", false},
			{"public-zen", "opencode", "https://opencode.ai/zen", "public", false},
			{"public-inference", "opencode", "https://opencode.ai/inference", "public", false},
			{"keyed-zen", "opencode", "https://opencode.ai/zen", "paid-key", true},
		} {
			t.Run(string(format)+"/"+route.name, func(t *testing.T) {
				protocol := map[clientFormat]string{FormatOpenAIChat: "chat", FormatOpenAIResponse: "responses", FormatAnthropic: "anthropic"}[format]
				up := &Upstream{Type: route.kind, BaseURL: route.base, APIKey: route.key, OpenAIProtocol: protocol, ProxyID: proxy.ID}
				before := calls.Load()
				if _, status, err := testUpstreamModel(context.Background(), up, "model-free", "<hello>&", 64, false); status != 403 || err == nil || !strings.Contains(err.Error(), failure) || strings.Contains(err.Error(), "已尝试另一入口") {
					t.Fatalf("diagnostic did not preserve eligibility error: status=%d err=%v", status, err)
				}
				if calls.Load() != before+1 {
					t.Fatalf("diagnostic replayed request: %d attempts", calls.Load()-before)
				}
				check := func(request observedRequest) {
					t.Helper()
					endpoint := nativeEndpoint(format, &canonReq{})
					path, query := "/zen"+endpoint, ""
					if !route.legacy {
						family := "openai"
						if format == FormatAnthropic {
							family, query = "anthropic", "beta=true"
						}
						path = "/inference/" + family + endpoint
					}
					if request.path != path || request.query != query {
						t.Errorf("initial route = %s?%s, want %s?%s", request.path, request.query, path, query)
					}
					if !json.Valid(request.body) || bytes.Contains(request.body, []byte(`\u003c`)) || !openCodeSessionUsable(request.header.Get("x-opencode-session"), time.Now()) {
						t.Errorf("initial request lost shaping or session: %s %v", request.body, request.header)
					}
					key := route.key
					if key == "" {
						key = "public"
					}
					if format == FormatAnthropic {
						if request.header.Get("x-api-key") != key || request.header.Get("Authorization") != "" {
							t.Errorf("Messages authentication changed: %v", request.header)
						}
					} else if request.header.Get("Authorization") != "Bearer "+key {
						t.Errorf("bearer credential changed: %v", request.header)
					}
					if route.legacy != (request.header.Get("x-opencode-request") != "") {
						t.Errorf("headers disagree with effective lane: %v", request.header)
					}
					if format == FormatOpenAIChat && !route.legacy && !bytes.HasPrefix(request.body, []byte(`{"model":`)) {
						t.Errorf("initial inference request omitted v2 body shape: %s", request.body)
					}
				}
				check(<-requests)
				body, err := renderProtocolRequest(format, &canonReq{Model: "model-free", Messages: []canonMsg{{Role: "user", Text: "<hello>&"}}}, "model-free")
				if err != nil {
					t.Fatal(err)
				}
				cr, err := parseClientRequest(format, body, "model-free")
				if err != nil {
					t.Fatal(err)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, nativeEndpoint(format, cr), nil)
				task := &relayTask{format: format, endpoint: nativeEndpoint(format, cr), model: "model-free", body: body, canon: cr}
				info, wrote, err := doRelay(c, up, task)
				if info.Status != 403 || wrote || err == nil || info.OutputText.String() != failure || strings.Contains(err.Error(), "已尝试另一入口") {
					t.Fatalf("relay did not preserve eligibility failure: %+v wrote=%v err=%v", info, wrote, err)
				}
				if calls.Load() != before+2 {
					t.Fatalf("relay replayed request: %d attempts", calls.Load()-before-1)
				}
				check(<-requests)
				if up.BaseURL != route.base || up.APIKey != route.key {
					t.Fatal("routing mutated configured URL or credential")
				}
			})
		}
	}
}

func TestOpenCodeFreeIPBlockedClassification(t *testing.T) {
	for _, message := range []string{
		"free models aren't available from your current network",
		"Forbidden, OpenCode Free Model Gateway Detect Abuse Traffic",
		"Abuse of traffic",
		"detected abusive traffic from this IP address",
	} {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "FreeTierError", "message": message}})
		original := bytes.Clone(body)
		if !openCodeFreeIPBlocked(403, body) || openCodeFreeIPBlocked(400, body) {
			t.Errorf("incorrect IP abuse classification: %s", body)
		}
		if !bytes.Equal(body, original) {
			t.Errorf("classification mutated the original response: %s", body)
		}
	}
	for _, body := range []string{
		`{"error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`,
		`{"error":{"type":"permission_error","message":"ordinary forbidden"}}`,
	} {
		if openCodeFreeIPBlocked(403, []byte(body)) {
			t.Errorf("ordinary eligibility error misclassified as an IP block: %s", body)
		}
	}
}
