package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apishare/internal/opencodecompat"

	"github.com/gin-gonic/gin"
)

func TestOpenCodeFreeRelayRestoresToolsAcrossProtocols(t *testing.T) {
	for _, tc := range []struct{ name, client, upstream string }{
		{"opaque", "weather", opencodecompat.ClientToolWireName("weather", nil)},
		{"official", "Read", "read"},
		{"shell", "Bash", "shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testOpenCodeFreeRelayRestoresToolsAcrossProtocols(t, tc.client, tc.upstream)
		})
	}
}

func testOpenCodeFreeRelayRestoresToolsAcrossProtocols(t *testing.T, clientName, upstreamName string) {
	t.Helper()
	modelsTestDB(t)
	wireWeather := opencodecompat.ClientToolWireName(clientName, nil)
	openCodeCatalogs.Lock()
	openCodeCatalogs.entries[openCodeCatalogCacheKey(nil)] = &openCodeCatalogEntry{catalog: openCodeCatalog{
		At: time.Now(), Tiers: map[string]map[string]clientFormat{"zen": {"mapped-free": FormatOpenAIChat}},
		FreeModels: map[string]map[string]bool{"zen": {"mapped-free": true}},
	}}
	openCodeCatalogs.Unlock()
	for _, wire := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		for _, client := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", wire, client, stream), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != nativeEndpoint(wire, &canonReq{}) {
							t.Errorf("custom gateway path changed: %s", r.URL.Path)
						}
						body, _ := io.ReadAll(r.Body)
						if !strings.Contains(string(body), `"stream":true`) || !strings.Contains(string(body), fmt.Sprintf(`"name":%q`, wireWeather)) {
							t.Errorf("free request lost streaming/tool shaping: %s", body)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						upstream := fixtureSSE(wire)
						if wire == FormatAnthropic {
							upstream = strings.Replace(upstream, `"message":{"usage":`, `"message":{"type":"message","usage":`, 1)
							for _, index := range []int{0, 1, 2} {
								boundary := `{"type":"message_delta"`
								if index < 2 {
									boundary = fmt.Sprintf(`{"type":"content_block_start","index":%d`, index+1)
								}
								stop := fmt.Sprintf("{\"type\":\"content_block_stop\",\"index\":%d}\r\n\r\ndata: ", index)
								upstream = strings.Replace(upstream, boundary, stop+boundary, 1)
							}
						}
						fmt.Fprint(w, strings.ReplaceAll(upstream, `"name":"weather"`, fmt.Sprintf(`"name":%q`, upstreamName)))
					}))
					defer server.Close()
					up := &Upstream{Type: openCodeFreeType, BaseURL: server.URL, APIKey: "public", ModelMap: `{"alias":"mapped-free"}`,
						OpenAIProtocol: map[clientFormat]string{FormatOpenAIChat: "chat", FormatOpenAIResponse: "responses", FormatAnthropic: "anthropic"}[wire]}
					task := fixtureTask(t, client, stream)
					task.body = []byte(strings.ReplaceAll(string(task.body), `"weather"`, fmt.Sprintf("%q", clientName)))
					var parseErr error
					task.canon, parseErr = parseClientRequest(client, task.body, task.model)
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, task.endpoint, nil)
					info, wrote, err := doRelay(c, up, task)
					if err != nil || !wrote || info.Status != http.StatusOK {
						t.Fatalf("free relay: status=%d wrote=%t err=%v\n%s", info.Status, wrote, err, rec.Body.String())
					}
					var response *canonResponse
					if stream {
						response, err = collectProtocolSSE(strings.NewReader(rec.Body.String()), client)
					} else {
						response, err = parseProtocolResponse(client, rec.Body.Bytes())
					}
					if err != nil {
						t.Fatalf("read client response: %v\n%s", err, rec.Body.String())
					}
					tools := map[string]string{}
					for _, block := range response.Blocks {
						if block.Kind == "tool" {
							tools[block.Call.Name] = block.Call.Arguments
						}
					}
					if len(tools) != 2 || tools[clientName] != `{"city":"Paris"}` || tools["sum"] != `{"a":1}` || strings.Contains(rec.Body.String(), opencodecompat.ClientToolPrefix) {
						t.Fatalf("client tools were not restored exactly: %+v\n%s", tools, rec.Body.String())
					}
				})
			}
		}
	}
}

func TestOpenCodeFreeRelayPreservesTitleRequests(t *testing.T) {
	modelsTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payload, err := opencodecompat.Decode(body)
		if err != nil || len(opencodecompat.SliceAt(payload, "tools")) != 0 || payload["stream"] != true || !strings.Contains(string(body), "Generate a title for this conversation") {
			t.Errorf("title acquired agent shaping: %s / %v", body, err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Fix build\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	up := &Upstream{Type: "opencode", BaseURL: server.URL, APIKey: "public", OpenAIProtocol: "chat"}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				request := &canonReq{Model: "title-free", Stream: stream, MaxTokens: 64, Messages: []canonMsg{{Role: "user", Text: "Generate a title for this conversation: Fix the build"}}}
				body, err := renderProtocolRequest(format, request, request.Model)
				if err != nil {
					t.Fatal(err)
				}
				task := &relayTask{format: format, endpoint: nativeEndpoint(format, request), model: request.Model, stream: stream, body: body, canon: request}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, task.endpoint, nil)
				info, wrote, err := doRelay(c, up, task)
				if err != nil || !wrote || info.Status != http.StatusOK || !strings.Contains(rec.Body.String(), "Fix build") {
					t.Fatalf("title relay failed: %d wrote=%t err=%v\n%s", info.Status, wrote, err, rec.Body.String())
				}
			})
		}
	}
}
