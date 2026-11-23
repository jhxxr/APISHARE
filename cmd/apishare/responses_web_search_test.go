package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const responsesSearchInput = `{"model":"alias","input":[{"role":"user","content":"earlier"},{"role":"user","content":[{"type":"input_text","text":"Go"},{"type":"input_text","text":"documentation"}]}],"tools":[{"type":"web_search"}],"tool_choice":"required","include":["web_search_call.action.sources"],"store":false}`

func TestResponsesSearchFallbackLimits(t *testing.T) {
	if req := parseResponsesSearchFallback([]byte(responsesSearchInput)); req == nil || req.query != "Go documentation" || req.choice != "required" {
		t.Fatalf("wrong query or choice: %+v", req)
	}
	base := `{"model":"alias","input":"Go documentation","tools":[{"type":"web_search"}]}`
	for _, tc := range []struct{ name, body string }{
		{"mixed tools", strings.Replace(base, `"type":"web_search"}]`, `"type":"web_search"},{"type":"function","name":"lookup"}]`, 1)},
		{"history", strings.Replace(base, `"Go documentation"`, `[{"type":"web_search_call","id":"ws_1"},{"role":"user","content":"again"}]`, 1)},
		{"no search", strings.TrimSuffix(base, "}") + `,"tool_choice":"none"}`},
		{"provider state", strings.TrimSuffix(base, "}") + `,"previous_response_id":"resp_previous"}`},
		{"structured output", strings.TrimSuffix(base, "}") + `,"text":{"format":{"type":"json_schema"}}}`},
		{"only cached access", strings.Replace(base, `"type":"web_search"`, `"type":"web_search","external_web_access":false`, 1)},
		{"domain filter", strings.Replace(base, `"type":"web_search"`, `"type":"web_search","filters":{"allowed_domains":["go.dev"]}`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if req := parseResponsesSearchFallback([]byte(tc.body)); req != nil {
				t.Fatalf("unsupported fallback accepted: %+v", req)
			}
		})
	}
}

func TestResponsesHostedWebSearchRouting(t *testing.T) {
	for _, mode := range []string{"json", "sse", "disabled", "provider_error", "native", "native_error", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			modelsTestDB(t)
			if err := settingsSet("retry", "0"); err != nil {
				t.Fatal(err)
			}
			apiKey := "fake-brave-credential"
			if mode == "disabled" {
				apiKey = ""
			}
			t.Setenv("RESPONSES_BRAVE_SEARCH_API_KEY", apiKey)
			searchCalls := 0
			originalTransport := braveSearchClient.Transport
			t.Cleanup(func() { braveSearchClient.Transport = originalTransport })
			braveSearchClient.Transport = jobRoundTripper(func(r *http.Request) (*http.Response, error) {
				searchCalls++
				if r.URL.Scheme != "https" || r.URL.Host != "api.search.brave.com" || r.URL.Query().Get("q") != "Go documentation" || r.Header.Get("X-Subscription-Token") != apiKey || r.Header.Get("Authorization") != "" {
					t.Error("search did not use the configured provider credential and latest query")
				}
				if mode == "provider_error" {
					return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(apiKey))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"type":"search","web":{"results":[{"url":"https://go.dev/doc/","title":"Go 文档","description":"Official documentation"},{"url":"javascript:alert(1)","title":"invalid"}]}}`))}, nil
			})
			body := responsesSearchInput
			if mode == "sse" {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			} else if mode == "mixed" {
				body = strings.Replace(body, `"type":"web_search"}]`, `"type":"web_search"},{"type":"function","name":"lookup"}]`, 1)
			}
			key := &ApiKey{Name: "search", Key: "search-route-key", Enabled: true, Models: "alias"}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			chat := &Upstream{Type: "openai", OpenAIProtocol: "chat", Name: "chat", BaseURL: "http://127.0.0.1:1", Models: "alias", Enabled: true, Weight: 1}
			if err := dbInsertUpstream(chat); err != nil {
				t.Fatal(err)
			}
			if mode == "native" || mode == "native_error" {
				native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, _ := io.ReadAll(r.Body)
					if string(got) != body || r.URL.Path != "/v1/responses" {
						t.Error("native request was modified")
					}
					if mode == "native_error" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, protocolResponses[FormatOpenAIResponse])
				}))
				defer native.Close()
				up := &Upstream{Type: "openai", OpenAIProtocol: "responses", Name: "native", BaseURL: native.URL, Models: "alias", Enabled: true, Weight: 1}
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			router := gin.New()
			router.POST("/v1/responses", keyAuth(), relayHandler(FormatOpenAIResponse, false))
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("X-Session-Id", "hosted-search")
			affinitySet(scopedSessionKey(key.ID, "sid:hosted-search"), chat.ID)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			wantStatus, wantSearch := http.StatusOK, 0
			switch mode {
			case "json", "sse":
				wantSearch = 1
			case "disabled", "mixed":
				wantStatus = http.StatusBadRequest
			case "provider_error", "native_error":
				wantStatus = http.StatusBadGateway
				if mode == "provider_error" {
					wantSearch = 1
				}
			}
			if rec.Code != wantStatus || searchCalls != wantSearch || strings.Contains(rec.Body.String(), "fake-brave-credential") {
				t.Fatalf("status=%d searches=%d body=%s", rec.Code, searchCalls, rec.Body.String())
			}
			if mode != "json" && mode != "sse" {
				return
			}
			if rec.Header().Get("X-APISHARE-Web-Search") != "brave" || strings.Contains(rec.Body.String(), "javascript:") {
				t.Fatalf("provider response not sanitized or marked: %s", rec.Body.String())
			}
			var response map[string]any
			if mode == "sse" {
				events := codexStreamEvents(t, rec.Body.String())
				last := events[len(events)-1]
				if last["type"] != "response.completed" || !strings.Contains(rec.Body.String(), "event: response.web_search_call.completed\n") || !strings.Contains(rec.Body.String(), "event: response.output_text.annotation.added\n") {
					t.Fatalf("incomplete hosted search stream: %s", rec.Body.String())
				}
				response = object(last["response"])
			} else {
				var err error
				response, err = decodeProtocolObject(rec.Body.Bytes())
				if err != nil {
					t.Fatal(err)
				}
			}
			output := array(response["output"])
			if len(output) != 2 || object(output[0])["type"] != "web_search_call" || !strings.Contains(jsonText(output), `"url":"https://go.dev/doc/"`) || !strings.Contains(jsonText(output), `"type":"url_citation"`) {
				t.Fatalf("search result or citations missing: %v", output)
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].UpstreamName != "gateway:brave-search" || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 || logs[0].CostUSD != 0 {
				t.Fatalf("invalid search billing: %+v / %v", logs, err)
			}
		})
	}
}
