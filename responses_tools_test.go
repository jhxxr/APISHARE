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

func parseCodexFixture(t *testing.T, body string) *canonReq {
	t.Helper()
	cr, err := parseClientRequest(FormatOpenAIResponse, []byte(body), "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("Codex request rejected: %v / %v", err, cr.ConversionError)
	}
	return cr
}

const codexToolFixture = `{"model":"alias","text":{"format":{"type":"text"}},"tools":[{"type":"custom","name":"apply_patch","description":"Edit files","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"goal":{"type":"string"}},"required":["goal"]}}],"input":[{"role":"user","content":"Edit and test"},{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}]}]}`

func TestCodexToolsReachAllConvertedUpstreams(t *testing.T) {
	cr := parseCodexFixture(t, codexToolFixture)
	if len(cr.Tools) != 3 || len(cr.Messages) != 1 {
		t.Fatalf("tools/messages = %d/%d", len(cr.Tools), len(cr.Messages))
	}
	if !strings.Contains(cr.Tools[0].Description, "lark grammar") {
		t.Fatal("custom grammar hint was lost")
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatAnthropic, FormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			body, err := renderProtocolRequest(format, cr, "mapped")
			if err != nil {
				t.Fatal(err)
			}
			var j map[string]any
			json.Unmarshal(body, &j)
			var tools []any
			if format == FormatGemini {
				tools = array(object(array(j["tools"])[0])["functionDeclarations"])
			} else {
				tools = array(j["tools"])
			}
			if len(tools) != 3 || strings.Contains(string(body), "additional_tools") {
				t.Fatalf("incorrect upstream tools: %s", body)
			}
			tool := object(tools[0])
			if format == FormatOpenAIChat {
				tool = object(tool["function"])
			}
			key := "parameters"
			if format == FormatAnthropic {
				key = "input_schema"
			} else if format == FormatGemini {
				key = "parametersJsonSchema"
			}
			if str(object(object(object(tool[key])["properties"])["input"])["type"]) != "string" {
				t.Fatal("custom tool was not encoded as a raw string function")
			}
		})
	}
}

func TestCodexParallelHistoryAndReasoningReplay(t *testing.T) {
	patch := "*** Begin Patch\n\"quoted\"\\中文\n*** End Patch"
	var j map[string]any
	json.Unmarshal([]byte(codexToolFixture), &j)
	j["input"] = append(array(j["input"]),
		map[string]any{"type": "reasoning", "id": "rs_previous", "summary": []any{map[string]any{"type": "summary_text", "text": "Need both tools"}}},
		map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "call_patch", "input": patch},
		map[string]any{"type": "function_call", "namespace": "functions", "name": "exec_command", "call_id": "call_exec", "arguments": `{"cmd":"go test"}`},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "patch applied"},
		map[string]any{"type": "function_call_output", "call_id": "call_exec", "output": "tests passed"})
	cr := parseCodexFixture(t, jsonText(j))
	if len(cr.Messages) != 4 || len(cr.Messages[1].Calls) != 2 {
		t.Fatalf("parallel calls are not one assistant batch: %+v", cr.Messages)
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatAnthropic, FormatGemini} {
		body, err := renderProtocolRequest(format, cr, "mapped")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "call_patch") || !strings.Contains(string(body), "call_exec") {
			t.Fatalf("tool result IDs lost: %s", body)
		}
		if format == FormatOpenAIChat {
			var wire map[string]any
			json.Unmarshal(body, &wire)
			msg := object(array(wire["messages"])[1])
			if str(msg["reasoning_content"]) != "Need both tools" || len(array(msg["tool_calls"])) != 2 {
				t.Fatalf("DeepSeek tool batch/reasoning lost: %s", body)
			}
			var args map[string]any
			json.Unmarshal([]byte(str(object(object(array(msg["tool_calls"])[0])["function"])["arguments"])), &args)
			if args["input"] != patch {
				t.Fatal("raw patch text was not preserved")
			}
		}
	}
}

func TestCodexToolSearchDiscoversToolsAndRestoresIdentity(t *testing.T) {
	cr := parseCodexFixture(t, `{"model":"alias","tools":[{"type":"tool_search","execution":"client"}],"input":[{"role":"user","content":"Find a tool"},{"type":"tool_search_call","execution":"client","call_id":"call_search","arguments":{"query":"docs"}},{"type":"tool_search_output","execution":"client","call_id":"call_search","tools":[{"type":"namespace","name":"docs","tools":[{"type":"function","name":"read","defer_loading":true,"parameters":{"type":"object"}}]}]}]}`)
	if len(cr.Tools) != 2 || cr.ResponseTools["docs__read"].Namespace != "docs" {
		t.Fatal("discovered namespace tool not promoted")
	}
	if !strings.Contains(cr.Messages[2].Text, "docs") {
		t.Fatal("search result did not reach upstream history")
	}
	search := restoreResponseTool(responseBlock{Kind: "tool", Call: canonCall{"call_search", "tool_search", `{"query":"docs"}`}}, cr.ResponseTools)
	item := responsesToolItem(search, "item_search")
	if str(item["type"]) != "tool_search_call" || str(item["execution"]) != "client" || object(item["arguments"])["query"] != "docs" {
		t.Fatalf("invalid client search call: %+v", item)
	}
}

func TestCodexToolChoiceAndNamespaceConflicts(t *testing.T) {
	for _, choice := range []string{
		`{"type":"custom","name":"apply_patch"}`,
		`{"type":"function","namespace":"functions","name":"exec_command"}`,
		`{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"apply_patch"}]}`,
	} {
		var j map[string]any
		json.Unmarshal([]byte(codexToolFixture), &j)
		var c any
		json.Unmarshal([]byte(choice), &c)
		j["tool_choice"] = c
		cr := parseCodexFixture(t, jsonText(j))
		if _, err := renderProtocolRequest(FormatOpenAIChat, cr, "mapped"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(choice, "allowed_tools") && (len(cr.Tools) != 1 || cr.ToolChoice != "required") {
			t.Fatal("allowed tools were not narrowed")
		}
	}
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"exec"},{"type":"custom","name":"exec"}]}`,
		`{"tools":[{"type":"function","name":"ns__exec"},{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"exec"}]}]}`,
		`{"tools":[{"type":"custom","name":"exec"}],"input":[{"type":"additional_tools","tools":[{"type":"function","name":"exec"}]}]}`,
		`{"tools":[{"type":"tool_search","execution":"server"}]}`,
		`{"tools":[{"type":"tool_search"}]}`,
		`{"input":[{"type":"additional_tools","tools":{}}]}`,
		`{"tools":[{"type":"custom","name":"exec"}],"tool_choice":{"type":"custom","name":"missing"}}`,
		`{"input":[{"type":"custom_tool_call","name":"exec","call_id":"c","input":{}}]}`,
	} {
		cr, err := parseClientRequest(FormatOpenAIResponse, []byte(`{"model":"alias",`+body[1:]), "")
		if err != nil || cr.ConversionError == nil {
			t.Fatalf("invalid conversion accepted: %s (%v)", body, err)
		}
	}
	long := responsesFunctionName(strings.Repeat("ns", 40), strings.Repeat("tool", 40))
	if len(long) > 64 || long == responsesFunctionName(strings.Repeat("ns", 40), strings.Repeat("tool", 40)+"x") {
		t.Fatal("namespace flattening is not bounded and stable")
	}
}

func codexStreamEvents(t *testing.T, s string) []map[string]any {
	t.Helper()
	var events []map[string]any
	if err := readSSEFrames(strings.NewReader(s), func(payload string) (bool, error) {
		var j map[string]any
		if err := json.Unmarshal([]byte(payload), &j); err != nil {
			return false, err
		}
		if intFromAny(j["sequence_number"]) != len(events) {
			t.Error("Responses event sequence is not contiguous")
		}
		events = append(events, j)
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestCodexToolsRelayJSONAndFragmentedSSE(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: sample.txt\n+中文 \"quotes\" \\\n*** End Patch"
	for _, format := range []clientFormat{FormatOpenAIChat, FormatAnthropic, FormatGemini} {
		for _, transport := range []string{"json", "sse", "json_to_sse"} {
			t.Run(string(format)+"/"+transport, func(t *testing.T) {
				cr := parseCodexFixture(t, codexToolFixture)
				cr.Stream = transport != "json"
				upstreamResult := &canonResponse{Finish: "tool_calls", Usage: upstreamUsage{Prompt: 8, Completion: 9, InKnown: true, OutKnown: true}, Blocks: []responseBlock{
					{Kind: "thinking", Text: "Need a patch"},
					{Kind: "tool", Call: canonCall{"call_patch", "apply_patch", jsonText(map[string]any{"input": patch})}},
					{Kind: "tool", Call: canonCall{"call_exec", "functions__exec_command", `{"cmd":"go test"}`}},
				}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.RequestURI() != nativeEndpoint(format, &canonReq{Model: "mapped", Stream: cr.Stream}) {
						t.Errorf("wrong upstream endpoint: %s", r.URL.Path)
					}
					body, _ := io.ReadAll(r.Body)
					if strings.Contains(string(body), `"type":"custom"`) || strings.Contains(string(body), "additional_tools") {
						t.Errorf("client tools leaked to upstream: %s", body)
					}
					if transport == "sse" {
						c, _ := gin.CreateTestContext(w)
						writer := newProtocolStream(c, format, "mapped", true)
						for i, b := range upstreamResult.Blocks {
							if b.Kind != "tool" {
								writer.push(c, streamDelta{Key: fmt.Sprint(i), Block: b}, upstreamResult.Usage)
								continue
							}
							args := b.Call.Arguments
							b.Call.Arguments = ""
							writer.push(c, streamDelta{Key: fmt.Sprint(i), Block: b}, upstreamResult.Usage)
							// Split JSON escaping while keeping every delta a valid UTF-8 string.
							runes := []rune(args)
							for start := 0; start < len(runes); start += 7 {
								end := start + 7
								if end > len(runes) {
									end = len(runes)
								}
								b.Call.Arguments = string(runes[start:end])
								writer.push(c, streamDelta{Key: fmt.Sprint(i), Block: b}, upstreamResult.Usage)
							}
						}
						if err := writer.finish(c, upstreamResult.Usage, "tool_calls", ""); err != nil {
							t.Error(err)
						}
					} else {
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(renderProtocolResponse(format, "mapped", "", upstreamResult))
					}
				}))
				defer server.Close()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				task := &relayTask{format: FormatOpenAIResponse, endpoint: "/v1/responses", model: "alias", body: []byte(codexToolFixture), canon: cr, stream: cr.Stream}
				info, wrote, err := doRelay(c, fixtureUpstream(format, server.URL), task)
				if err != nil || !wrote || rec.Code != 200 {
					t.Fatalf("relay: %v / %d / %s", err, rec.Code, rec.Body.String())
				}
				if info.PromptTok != 8 || info.CompletionTok != 9 {
					t.Fatalf("usage lost: %+v", info)
				}
				var result map[string]any
				if transport == "json" {
					json.Unmarshal(rec.Body.Bytes(), &result)
				} else {
					events := codexStreamEvents(t, rec.Body.String())
					for _, event := range events {
						if str(event["type"]) == "response.custom_tool_call_input.delta" && event["delta"] != patch {
							t.Error("custom delta included JSON wrapping")
						}
						if str(event["type"]) == "response.completed" {
							result = object(event["response"])
						}
					}
				}
				output := array(result["output"])
				if len(output) != 3 {
					t.Fatalf("output lost: %+v", result)
				}
				custom, function := object(output[1]), object(output[2])
				if custom["type"] != "custom_tool_call" || custom["input"] != patch || custom["call_id"] != "call_patch" {
					t.Fatalf("custom call was corrupted: %+v", custom)
				}
				if function["name"] != "exec_command" || function["namespace"] != "functions" || function["call_id"] != "call_exec" {
					t.Fatalf("namespace call was corrupted: %+v", function)
				}
			})
		}
	}
}

func TestCodexFragmentedToolNameAndLateName(t *testing.T) {
	for _, late := range []bool{false, true} {
		cr := parseCodexFixture(t, codexToolFixture)
		var calls []map[string]any
		args := `{"input":"raw text"}`
		if late {
			calls = append(calls, map[string]any{"arguments": args})
		}
		calls = append(calls, map[string]any{"name": "apply_"}, map[string]any{"name": "patch"})
		if !late {
			calls = append(calls, map[string]any{"arguments": args})
		}
		var upstream strings.Builder
		for _, call := range calls {
			upstream.WriteString("data: " + jsonText(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_patch", "type": "function", "function": call}}}}}}) + "\n\n")
		}
		upstream.WriteString("data: [DONE]\n\n")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		err := relayProtocolSSE(c, &http.Response{Body: io.NopCloser(strings.NewReader(upstream.String()))}, FormatOpenAIChat, &relayTask{format: FormatOpenAIResponse, model: "alias", canon: cr}, &relayInfo{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(rec.Body.String(), `"input":"raw text"`) || !strings.Contains(rec.Body.String(), "custom_tool_call") {
			t.Fatal("fragmented name was not restored")
		}
	}
}

func TestCodexTruncatedOrInvalidCustomCallNeverCompletes(t *testing.T) {
	for _, ending := range []string{"", "data: [DONE]\n\n"} {
		cr := parseCodexFixture(t, codexToolFixture)
		upstream := "data: " + jsonText(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_patch", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": `{"input":"partial`}}}}}}}) + "\n\n" + ending
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		err := relayProtocolSSE(c, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, FormatOpenAIChat, &relayTask{format: FormatOpenAIResponse, model: "alias", canon: cr}, &relayInfo{})
		if err == nil || strings.Contains(rec.Body.String(), "response.completed") || strings.Contains(rec.Body.String(), "response.output_item.done") || !strings.Contains(rec.Body.String(), "response.failed") {
			t.Fatalf("truncated custom call was executable: %v / %s", err, rec.Body.String())
		}
	}
}
