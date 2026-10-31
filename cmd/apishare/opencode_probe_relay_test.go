package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"apishare/internal/opencodecompat"

	"github.com/gin-gonic/gin"
)

// Model testing declares only the official tools. Exercise a real client request
// on the same proxy, TLS transport, endpoint and key, including the tool pair
// upstream identified as a client fingerprint in opencode2api #161.
func TestOpenCodeFreeProbeAndClientToolRelayUseOpaqueNames(t *testing.T) {
	modelsTestDB(t)
	opaque := regexp.MustCompile(`^clint_[0-9a-f]{12}(?:_[0-9]+)?$`)
	reportWire := opencodecompat.ClientToolWireName("ReportFindings", nil)
	var probes, relays atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "opencode.ai" || r.URL.Path != "/inference/openai/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer public" {
			t.Errorf("probe and relay did not use the same route/key: %s %s", r.Host, r.URL)
		}
		if !strings.HasPrefix(r.UserAgent(), "opencode/") || r.Header.Get("Accept") != "*/*" || r.ProtoMajor != 1 {
			t.Errorf("caller headers replaced the OpenCode profile: %v %s", r.Header, r.Proto)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		payload, err := opencodecompat.Decode(body)
		if err != nil {
			t.Error(err)
			return
		}
		var aliases []string
		for _, entry := range payload["tools"].([]any) {
			name := opencodecompat.StringAt(entry.(map[string]any), "function", "name")
			if strings.HasPrefix(name, opencodecompat.ClientToolPrefix) {
				aliases = append(aliases, name)
				if !opaque.MatchString(name) {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":{"type":"FreeTierError","message":"The free tier can only be used from within OpenCode"}}`)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(aliases) == 0 {
			probes.Add(1)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"probe-ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		relays.Add(1)
		if len(aliases) != 2 || aliases[0] != reportWire {
			t.Errorf("client tools were not retained: %v", aliases)
		}
		// Split an actual opaque wire name across chunks, with empty argument
		// fragments before the real arguments, as a live Chat stream can do.
		for index, part := range []string{reportWire[:7], reportWire[7:12], reportWire[12:]} {
			arguments := ""
			if index == 2 {
				arguments = `{"count":9007199254740993}`
			}
			chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "call_report", "type": "function", "function": map[string]any{"name": part, "arguments": arguments}}},
			}, "finish_reason": nil}}}
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	originalRelay, originalProbe := upstreamHTTPClient, modelTestClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	upstreamHTTPClient = &http.Client{Transport: transport}
	modelTestClient = upstreamHTTPClient
	t.Cleanup(func() {
		upstreamHTTPClient, modelTestClient = originalRelay, originalProbe
		transport.CloseIdleConnections()
	})
	observed := make(chan string, 16)
	endpoint := testSOCKSProxy(t, server.Listener.Addr().String(), observed)
	proxy := testSavedProxy(t, "socks5h://"+endpoint, "alice", "secret")
	up := &Upstream{Type: "opencode", BaseURL: "https://opencode.ai/inference", APIKey: "public", OpenAIProtocol: "chat",
		ProxyID: proxy.ID, ModelMap: `{"alias":"mapped-free"}`, HeaderOverrides: `{"*":true}`}
	if _, status, err := testUpstreamModel(context.Background(), up, "alias", "hello", 128, false); err != nil || status != 200 {
		t.Fatalf("control probe failed: %d %v", status, err)
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				var tools []any
				for _, name := range []string{"ReportFindings", "ScheduleWakeup"} {
					tool := map[string]any{"name": name, "description": "Client action", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
					switch format {
					case FormatOpenAIChat:
						tools = append(tools, map[string]any{"type": "function", "function": tool})
					case FormatOpenAIResponse:
						tool["type"] = "function"
						tools = append(tools, tool)
					case FormatAnthropic:
						tool["input_schema"] = tool["parameters"]
						delete(tool, "parameters")
						tools = append(tools, tool)
					}
				}
				payload := map[string]any{"model": "alias", "tools": tools, "stream": stream}
				if format == FormatOpenAIResponse {
					payload["input"] = "hello"
				} else {
					payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					payload["max_tokens"] = 128
				}
				body, _ := json.Marshal(payload)
				canon, err := parseClientRequest(format, body, "alias")
				if err != nil {
					t.Fatal(err)
				}
				task := &relayTask{apiKeyID: 7, format: format, model: "alias", body: body, canon: canon, stream: stream, endpoint: nativeEndpoint(format, canon)}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, task.endpoint, nil)
				c.Request.Header.Set("User-Agent", "claude-cli/client")
				c.Request.Header.Set("Accept", "application/json")
				info, wrote, err := doRelay(c, up, task)
				if err != nil || !wrote || info.Status != 200 {
					t.Fatalf("tool-bearing relay failed after a successful probe: %+v wrote=%t err=%v", info, wrote, err)
				}
				var response *canonResponse
				if stream {
					response, err = collectProtocolSSE(strings.NewReader(recorder.Body.String()), format)
				} else {
					response, err = parseProtocolResponse(format, recorder.Body.Bytes())
				}
				if err != nil {
					t.Fatalf("response decode failed: %v\n%s", err, recorder.Body.String())
				}
				var name, arguments string
				for _, block := range response.Blocks {
					if block.Kind == "tool" {
						name, arguments = block.Call.Name, block.Call.Arguments
					}
				}
				if name != "ReportFindings" || arguments != `{"count":9007199254740993}` || strings.Contains(recorder.Body.String(), reportWire) {
					t.Fatalf("client received the wire alias or corrupted arguments: %q %q", name, arguments)
				}
			})
		}
	}
	if probes.Load() != 1 || relays.Load() != 6 {
		t.Fatalf("missing paired coverage: probes=%d relays=%d", probes.Load(), relays.Load())
	}
}
