package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const structuredSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`

func structuredTask(t *testing.T, schema string, stream bool) *relayTask {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"model":"alias","max_tokens":256,"stream":%t,"system":"Original instructions","messages":[{"role":"user","content":"classify"}],"output_config":{"effort":"low","format":{"type":"json_schema","schema":%s}}}`, stream, schema))
	cr, err := parseClientRequest(FormatAnthropic, body, "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("structured request: %v / %v", err, cr.ConversionError)
	}
	return &relayTask{format: FormatAnthropic, endpoint: "/v1/messages", model: "alias", stream: stream, body: body, canon: cr}
}

func structuredChatResponse(text string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + jsonText(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`
}

func TestStructuredOutputSchemaTranslationAndAttemptIsolation(t *testing.T) {
	task := structuredTask(t, structuredSchema, false)
	for _, format := range protocolFormats {
		model := "upstream-model"
		if format == FormatGemini {
			model = "gemini-3-flash"
		}
		wire, err := renderProtocolRequest(format, task.canon, model)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := decodeProtocolObject(wire)
		var schema any
		switch format {
		case FormatOpenAIChat:
			config := object(body["response_format"])
			if config["type"] != "json_schema" || object(config["json_schema"])["strict"] != true {
				t.Fatalf("Chat lost strict schema: %s", wire)
			}
			schema = object(config["json_schema"])["schema"]
		case FormatOpenAIResponse:
			config := object(object(body["text"])["format"])
			if config["type"] != "json_schema" || config["name"] == "" {
				t.Fatalf("Responses lost format: %s", wire)
			}
			schema = config["schema"]
		case FormatAnthropic:
			config := object(body["output_config"])
			if config["effort"] != "low" {
				t.Fatal("format replaced reasoning effort")
			}
			schema = object(config["format"])["schema"]
		case FormatGemini:
			config := object(body["generationConfig"])
			if config["responseMimeType"] != "application/json" {
				t.Fatal("Gemini JSON MIME missing")
			}
			schema = config["responseJsonSchema"]
		}
		if jsonText(schema) != jsonText(task.canon.OutputFormat.Schema) {
			t.Fatalf("schema changed for %s", format)
		}
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse} {
		wire, err := renderProtocolRequest(format, task.canon, "deepseek-v4.1-flash", "json_object")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := decodeProtocolObject(wire)
		config := object(body["response_format"])
		prompt := str(body["instructions"])
		if format == FormatOpenAIResponse {
			config = object(object(body["text"])["format"])
		} else {
			prompt = str(object(array(body["messages"])[0])["content"])
		}
		if config["type"] != "json_object" || config["json_schema"] != nil || !strings.Contains(prompt, "JSON Schema:") || !strings.Contains(prompt, "Original instructions") {
			t.Fatalf("JSON compatibility lost format instructions: %s", wire)
		}
	}
	if task.canon.System != "Original instructions" || task.canon.Model != "alias" {
		t.Fatal("attempt mutated the original request")
	}
	if _, err := renderProtocolRequest(FormatOpenAIChat, task.canon, "mapped", "disabled"); err == nil {
		t.Fatal("disabled upstream accepted structured conversion")
	}
	arrayTask := structuredTask(t, `{"type":"array","items":{"type":"boolean"}}`, false)
	if _, err := renderProtocolRequest(FormatOpenAIChat, arrayTask.canon, "mapped", "json_object"); err == nil {
		t.Fatal("object-only compatibility accepted an array schema")
	}
}

func TestStructuredOutputSchemaValidationAndExceptions(t *testing.T) {
	task := structuredTask(t, `{"type":"object","$defs":{"id":{"type":"integer","const":9007199254740993}},"properties":{"id":{"$ref":"#/$defs/id"}},"required":["id"],"additionalProperties":false}`, false)
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{`{"id":9007199254740993}`, true},
		{`{"id":9007199254740992}`, false},
		{`{"id":"9007199254740993"}`, false},
		{`{"id":9007199254740993,"extra":true}`, false},
		{`{}`, false}, {``, false}, {"```json\n{}\n```", false}, {`{} {}`, false},
	} {
		response := &canonResponse{Blocks: []responseBlock{{Kind: "thinking", Text: "not JSON"}, {Kind: "text", Text: tc.text}}, Finish: "stop"}
		if err := validateStructuredOutput(task.canon.OutputFormat, response); (err == nil) != tc.valid {
			t.Errorf("validation for %q: %v", tc.text, err)
		}
	}
	for _, response := range []*canonResponse{
		{Blocks: []responseBlock{{Kind: "tool", Call: canonCall{"c", "read", `{}`}}}, Finish: "tool_calls"},
		{Blocks: []responseBlock{{Kind: "text", Text: "refused"}}, Finish: "content_filter"},
		{Blocks: []responseBlock{{Kind: "text", Text: `{"id":`}}, Finish: "length"},
	} {
		if err := validateStructuredOutput(task.canon.OutputFormat, response); err != nil {
			t.Fatal("tool, refusal or truncation semantics lost:", err)
		}
	}
	for _, schema := range []string{`{"type":"invalid"}`, `{"$ref":"file:///private.json"}`, `{"$ref":"https://example.invalid/private.json"}`} {
		_, err := parseAnthropicOutputFormat(map[string]any{"output_format": map[string]any{"type": "json_schema", "schema": mustStructuredSchema(t, schema)}})
		if err == nil || strings.Contains(err.Error(), "private.json") {
			t.Fatalf("invalid/external schema accepted or leaked: %v", err)
		}
	}
}

func mustStructuredSchema(t *testing.T, schema string) map[string]any {
	t.Helper()
	value, err := decodeProtocolObject([]byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestStructuredOutputMappedHaikuNonStreamAndNativePreservation(t *testing.T) {
	modelsTestDB(t)
	for _, native := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			task := structuredTask(t, structuredSchema, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, _ := io.ReadAll(r.Body)
				body, _ := decodeProtocolObject(wire)
				if body["model"] != "deepseek-v4.1-flash" {
					t.Error("mapped upstream model lost")
				}
				if native {
					expected := replaceModelInBody(task.body, "deepseek-v4.1-flash")
					if string(wire) != string(expected) {
						t.Error("native structured body changed")
					}
				} else if object(body["response_format"])["type"] != "json_object" {
					t.Error("compatibility not applied")
				}
				w.Header().Set("Content-Type", "application/json")
				if native {
					fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"native text"}],"stop_reason":"end_turn"}`)
				} else {
					output := `{"ok":true}`
					if !valid {
						output = `{"ok":"private-output"}`
					}
					fmt.Fprint(w, structuredChatResponse(output))
				}
			}))
			up := &Upstream{ID: 100, Type: "openai", OpenAIProtocol: "chat", StructuredOutput: "json_object", BaseURL: server.URL, ModelMap: `{"alias":"deepseek-v4.1-flash"}`}
			if native {
				up.Type = "anthropic"
				up.StructuredOutput = "disabled"
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			info, wrote, err := doRelay(c, up, task)
			server.Close()
			if valid || native {
				if err != nil || !wrote {
					t.Fatalf("valid/native request: %v", err)
				}
			} else if err == nil || wrote || relayResponseCommitted(c) || recorder.Body.Len() != 0 || strings.Contains(err.Error(), "private-output") || info.CompletionTok != 4 {
				t.Fatalf("invalid output escaped validation or lost usage: wrote=%v info=%+v err=%v body=%s", wrote, info, err, recorder.Body.String())
			}
		}
	}
}

func TestStructuredOutputStreamBuffersUntilValidationAndKeepsAlive(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			modelsTestDB(t)
			testMessagesKeepaliveConfig(t)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, auditFormatFrames(`{"choices":[{"index":0,"delta":{"content":"{\"ok\":"}}]}`))
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				tail := `true}`
				if !valid {
					tail = `"wrong"}`
				}
				fmt.Fprint(w, auditFormatFrames(`{"choices":[{"index":0,"delta":{"content":`+jsonText(tail)+`},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`, `[DONE]`))
			}))
			defer upstream.Close()
			defer unblock()
			up := fixtureUpstream(FormatOpenAIChat, upstream.URL)
			up.StructuredOutput = "json_object"
			task := structuredTask(t, structuredSchema, true)
			router := gin.New()
			router.POST("/v1/messages", func(c *gin.Context) {
				info, wrote, err := doRelay(c, up, task)
				if valid && err != nil {
					t.Error(err)
				}
				if !valid && (err == nil || wrote) {
					t.Errorf("invalid stream committed: %v/%v", wrote, err)
				}
				if err != nil {
					writeRelayError(c, FormatAnthropic, 502, err.Error())
				}
				if info.CompletionTok != 4 {
					t.Error("usage lost while buffering")
				}
			})
			gateway := httptest.NewServer(router)
			defer gateway.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader(string(task.body)))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var prelude strings.Builder
			for !strings.Contains(prelude.String(), "\n\n") {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				prelude.WriteString(line)
			}
			if !strings.Contains(prelude.String(), `"type":"ping"`) || strings.Contains(prelude.String(), "message_start") {
				t.Fatalf("content escaped before validation: %s", prelude.String())
			}
			unblock()
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if valid {
				if !strings.Contains(string(rest), "message_stop") || !strings.Contains(string(rest), `\"ok\":true`) {
					t.Fatalf("valid stream missing: %s", rest)
				}
			} else if !strings.Contains(string(rest), `"type":"error"`) || strings.Contains(string(rest), "message_start") {
				t.Fatalf("invalid stream returned success: %s", rest)
			}
		})
	}
}

func TestStructuredOutputSettingsPersistenceAndMigration(t *testing.T) {
	router := testProxyAdminRouter(t)
	created := proxyAdminRequest(router, "POST", "upstreams", `{"name":"JSON compatible","type":"openai","openai_protocol":"chat","structured_output":"json_object","base_url":"https://example.invalid","api_key":"saved-key"}`, true)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	var result struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(created.Body.Bytes(), &result)
	path := fmt.Sprintf("upstreams/%d", result.ID)
	proxyAdminRequest(router, "PATCH", path, `{"name":"renamed"}`, true)
	up, err := dbGetUpstream(result.ID)
	if err != nil || up.StructuredOutput != "json_object" {
		t.Fatal("setting lost on unrelated edit", err)
	}
	if got := proxyAdminRequest(router, "PATCH", path, `{"structured_output":"ignore"}`, true); got.Code != 400 {
		t.Fatal("invalid mode accepted")
	}
	if got := proxyAdminRequest(router, "PATCH", path, `{"structured_output":"disabled"}`, true); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	up, _ = cachedRoutingUpstream(result.ID)
	if up.StructuredOutput != "disabled" {
		t.Fatal("routing snapshot did not refresh")
	}
	if _, err := db.Exec(`ALTER TABLE upstreams DROP COLUMN structured_output`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(); err != nil {
		t.Fatal(err)
	}
	up, err = dbGetUpstream(result.ID)
	if err != nil || up.StructuredOutput != "" || up.APIKey != "saved-key" {
		t.Fatal("migration changed credentials or defaults", err)
	}
}

func TestStructuredOutputRoutingAndSafeFailover(t *testing.T) {
	for _, scenario := range []string{"disabled-affinity", "invalid-output", "invalid-with-disabled-fallback"} {
		t.Run(scenario, func(t *testing.T) {
			modelsTestDB(t)
			if err := settingsSet("retry", "1"); err != nil {
				t.Fatal(err)
			}
			key := &ApiKey{Name: "haiku", Key: "haiku-test", Models: "alias", Enabled: true}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			firstCalls, secondCalls := 0, 0
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCalls++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, structuredChatResponse(`{"ok":"wrong"}`))
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, structuredChatResponse(`{"ok":true}`))
			}))
			defer second.Close()
			firstUp := &Upstream{Name: "first", Type: "openai", OpenAIProtocol: "chat", StructuredOutput: "json_object", Models: "alias", BaseURL: first.URL, Enabled: true, Weight: 1}
			if scenario == "disabled-affinity" {
				firstUp.StructuredOutput = "disabled"
				if err := settingsSet("retry", "0"); err != nil {
					t.Fatal(err)
				}
			}
			secondUp := &Upstream{Name: "second", Type: "openai", OpenAIProtocol: "chat", Models: "alias", BaseURL: second.URL, Enabled: true, Weight: 1}
			if scenario == "invalid-with-disabled-fallback" {
				secondUp.StructuredOutput = "disabled"
			}
			for _, up := range []*Upstream{firstUp, secondUp} {
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
			}
			task := structuredTask(t, structuredSchema, false)
			// Fresh routing prefers the schema-capable candidate within the
			// explicit-model tier, regardless of compatibility channel weight.
			firstUp.Weight = 100000
			if err := dbUpdateUpstream(firstUp); err != nil {
				t.Fatal(err)
			}
			picked, err := pickUpstream(FormatAnthropic, "alias", map[int64]bool{}, false, task.canon)
			wantPicked := secondUp.ID
			if scenario == "invalid-with-disabled-fallback" {
				wantPicked = firstUp.ID
			}
			if err != nil || picked.ID != wantPicked {
				t.Fatal("weaker capability won routing tier", err)
			}
			router := gin.New()
			v1 := router.Group("/v1", keyAuth())
			v1.POST("/messages", relayHandler(FormatAnthropic, false))
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(task.body)))
			req.Header.Set("Authorization", "Bearer "+key.Key)
			req.Header.Set("X-Session-Id", "structured-route")
			affinitySet(sessionKeyFromCanon("/v1/messages", req.Header, task.canon), firstUp.ID)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if scenario == "invalid-with-disabled-fallback" {
				logs, err := dbListLogs(10, 0)
				if rec.Code != 502 || firstCalls != 1 || secondCalls != 0 || !strings.Contains(rec.Body.String(), "does not match") || err != nil || len(logs) != 1 || logs[0].UpstreamID != firstUp.ID || logs[0].Status != 502 {
					t.Fatalf("local rejection overwrote actual failure: status=%d calls=%d/%d logs=%+v err=%v body=%s", rec.Code, firstCalls, secondCalls, logs, err, rec.Body.String())
				}
				return
			}
			wantFirstCalls := 1
			if scenario == "disabled-affinity" {
				wantFirstCalls = 0
			}
			if rec.Code != 200 || firstCalls != wantFirstCalls || secondCalls != 1 || strings.Contains(rec.Body.String(), "wrong") || !strings.Contains(rec.Body.String(), `\"ok\":true`) {
				t.Fatalf("failover: %d calls=%d/%d body=%s", rec.Code, firstCalls, secondCalls, rec.Body.String())
			}
			if scenario == "disabled-affinity" && healthFails(firstUp.ID) != 0 {
				t.Fatal("local capability rejection penalized upstream")
			}
		})
	}
}

func TestStructuredOutputLegacyAndRefusalSemantics(t *testing.T) {
	task := structuredTask(t, structuredSchema, false)
	body, _ := decodeProtocolObject(task.body)
	body["output_format"] = object(body["output_config"])["format"]
	delete(body, "output_config")
	cr, err := parseClientRequest(FormatAnthropic, []byte(jsonText(body)), "")
	if err != nil || cr.ConversionError != nil || cr.OutputFormat == nil {
		t.Fatal("legacy format lost", err)
	}
	body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "array"}}}
	cr, _ = parseClientRequest(FormatAnthropic, []byte(jsonText(body)), "")
	if cr.ConversionError == nil {
		t.Fatal("conflicting formats silently accepted")
	}
	response, err := parseProtocolResponse(FormatOpenAIChat, []byte(`{"choices":[{"message":{"refusal":"Cannot answer"},"finish_reason":"stop"}]}`))
	if err != nil || response.Finish != "content_filter" {
		t.Fatal("refusal overwritten by generic stop", err)
	}
	if err := validateStructuredOutput(task.canon.OutputFormat, response); err != nil {
		t.Fatal("explicit refusal treated as schema corruption", err)
	}
	frames := auditFormatFrames(`{"choices":[{"index":0,"delta":{"refusal":"Cannot answer"}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, `[DONE]`)
	response, err = collectProtocolSSE(strings.NewReader(frames), FormatOpenAIChat)
	if err != nil || response.Finish != "content_filter" {
		t.Fatal("streamed refusal overwritten by generic stop", err)
	}
	if err := validateStructuredOutput(task.canon.OutputFormat, response); err != nil {
		t.Fatal("streamed refusal treated as schema corruption", err)
	}
}
