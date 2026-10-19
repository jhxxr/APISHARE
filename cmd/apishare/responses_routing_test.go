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

func TestResponsesIncompatibleAffinityFallsBackToNative(t *testing.T) {
	for _, nativeAvailable := range []bool{false, true} {
		t.Run(fmt.Sprint(nativeAvailable), func(t *testing.T) {
			modelsTestDB(t)
			if err := settingsSet("retry", "0"); err != nil {
				t.Fatal(err)
			}
			key := &ApiKey{Name: "responses route", Key: "responses-route-key", Enabled: true, Models: "alias"}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			convertedCalls, nativeCalls := 0, 0
			chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				convertedCalls++
				t.Error("incompatible conversion reached a Chat upstream")
			}))
			defer chat.Close()
			chatUp := &Upstream{Type: "openai", OpenAIProtocol: "chat", Name: "chat", BaseURL: chat.URL, APIKey: "test", Models: "alias", Enabled: true, Weight: 1}
			if err := dbInsertUpstream(chatUp); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"alias","tools":[{"type":"web_search"}],"input":"hello"}`
			if nativeAvailable {
				native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					nativeCalls++
					actual, _ := io.ReadAll(r.Body)
					if string(actual) != body {
						t.Error("native Responses body was changed")
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, protocolResponses[FormatOpenAIResponse])
				}))
				defer native.Close()
				up := &Upstream{Type: "openai", OpenAIProtocol: "responses", Name: "native", BaseURL: native.URL, APIKey: "test", Models: "alias", Enabled: true, Weight: 1}
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			cr, err := parseClientRequest(FormatOpenAIResponse, []byte(body), "")
			if err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			group := router.Group("/v1")
			group.Use(keyAuth())
			group.POST("/responses", relayHandler(FormatOpenAIResponse, false))
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("X-Session-Id", "responses-route")
			affinitySet(scopedSessionKey(key.ID, sessionKeyFromCanon("/v1/responses", req.Header, cr)), chatUp.ID)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			want := 400
			if nativeAvailable {
				want = 200
			}
			if rec.Code != want || convertedCalls != 0 || (nativeAvailable && nativeCalls != 1) {
				t.Fatalf("route failed: status=%d calls=%d/%d body=%s", rec.Code, convertedCalls, nativeCalls, rec.Body.String())
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].Status != want {
				t.Fatalf("failed conversion created incorrect logs: %+v / %v", logs, err)
			}
		})
	}
}

func TestResponsesToolLoopReasoningCacheAcrossRequests(t *testing.T) {
	resetResponseReasoningForTest(t)
	var firstID string
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var j map[string]any
		json.NewDecoder(r.Body).Decode(&j)
		requests++
		if requests == 2 {
			messages := array(j["messages"])
			if str(object(messages[1])["reasoning_content"]) != "Run the command first" {
				t.Errorf("cached reasoning missing on follow-up: %+v", messages)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"reasoning_content":"Run the command first","tool_calls":[{"id":"c","type":"function","function":{"name":"exec","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	up := &Upstream{ID: 2, Type: "openai", OpenAIProtocol: "chat", BaseURL: upstream.URL, APIKey: "test"}
	for i := 0; i < 2; i++ {
		body := `{"model":"alias","input":[{"role":"user","content":"run"}]}`
		if i == 1 {
			body = `{"model":"alias","input":[{"role":"user","content":"run"},{"type":"reasoning","id":` + jsonText(firstID) + `,"summary":[],"encrypted_content":"opaque"},{"type":"function_call","call_id":"c","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"done"}]}`
		}
		cr := parseCodexFixture(t, body)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		_, wrote, err := doRelay(c, up, &relayTask{apiKeyID: 1, format: FormatOpenAIResponse, model: "alias", endpoint: "/v1/responses", body: []byte(body), canon: cr})
		if err != nil || !wrote {
			t.Fatal(err)
		}
		var response map[string]any
		json.Unmarshal(rec.Body.Bytes(), &response)
		firstID = str(object(array(response["output"])[0])["id"])
	}
}
