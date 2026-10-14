package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestResponsesToolImagesFollowWholeParallelBatch(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"inspect"},{"type":"function_call","call_id":"a","name":"view_image","arguments":"{}"},{"type":"custom_tool_call","call_id":"b","name":"exec","input":"inspect"},{"role":"user","content":"notice"},{"type":"custom_tool_call_output","call_id":"b","output":[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"image A"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},{"role":"user","content":"continue"}]}`
	cr := parseCodexFixture(t, body)
	original := jsonText(cr.Messages)
	for _, format := range []clientFormat{FormatOpenAIChat, FormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			wire, err := renderProtocolRequest(format, cr, "mapped")
			if err != nil {
				t.Fatal(err)
			}
			var req map[string]any
			json.Unmarshal(wire, &req)
			field := "messages"
			if format == FormatGemini {
				field = "contents"
			}
			messages := array(req[field])
			if len(messages) != 7 {
				t.Fatalf("incorrect tool image batch: %s", wire)
			}
			if format == FormatOpenAIChat {
				for i, id := range []string{"a", "b"} {
					msg := object(messages[2+i])
					if msg["role"] != "tool" || msg["tool_call_id"] != id || strings.Contains(str(msg["content"]), "base64,") {
						t.Fatalf("image became tool text or interrupted the batch: %s", wire)
					}
				}
				parts := array(object(messages[4])["content"])
				if len(parts) != 4 || !strings.Contains(str(object(parts[0])["text"]), "call a") || str(object(object(parts[1])["image_url"])["url"]) != "data:image/png;base64,YQ==" || !strings.Contains(str(object(parts[2])["text"]), "call b") || str(object(object(parts[3])["image_url"])["url"]) != "data:image/png;base64,Yg==" {
					t.Fatalf("images lost their owning calls: %s", wire)
				}
			} else {
				for _, raw := range messages[2:4] {
					if strings.Contains(jsonText(object(raw)["parts"]), "base64,") || strings.Contains(jsonText(object(raw)["parts"]), "inlineData") {
						t.Fatal("Gemini functionResponse contains serialized image data")
					}
				}
				parts := array(object(messages[4])["parts"])
				if len(parts) != 4 || object(object(parts[1])["inlineData"])["data"] != "YQ==" || object(object(parts[3])["inlineData"])["data"] != "Yg==" {
					t.Fatalf("Gemini tool images were dropped: %s", wire)
				}
			}
			if jsonText(cr.Messages) != original {
				t.Fatal("image lifting mutated history used by another upstream retry")
			}
		})
	}
}

func TestResponsesOrphanAndReusedCallImagesStayWithTheirBatch(t *testing.T) {
	body := `{"model":"alias","input":[{"role":"user","content":"one"},{"type":"function_call","call_id":"c","name":"view","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},{"role":"user","content":"two"},{"type":"function_call","call_id":"c","name":"view","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,Yg=="}]},{"type":"function_call_output","call_id":"orphan","output":[{"type":"input_image","image_url":"data:image/png;base64,b3JwaGFu"}]}]}`
	cr := parseCodexFixture(t, body)
	msgs := liftToolResultImages(cr.Messages)
	if len(msgs) != 8 || msgs[3].Parts[1].URL != "data:image/png;base64,YQ==" || msgs[7].Parts[1].URL != "data:image/png;base64,Yg==" || strings.Contains(jsonText(msgs), "b3JwaGFu") {
		t.Fatalf("image crossed tool batch boundaries: %+v", msgs)
	}
}

func TestResponsesWrappedToolImagesPreserveOrdinaryJSON(t *testing.T) {
	for _, output := range []any{
		"data:image/png;base64,YQ==",
		`[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]`,
		`{"number":9007199254740993,"content":[{"type":"input_text","text":"caption"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}`,
	} {
		parts, err := parseResponsesToolOutput(output)
		if err != nil || len(parts) == 0 {
			t.Fatalf("wrapped media rejected: %+v / %v", parts, err)
		}
		images := 0
		for _, part := range parts {
			if part.Kind == "image" {
				images++
			} else if strings.Contains(part.Text, "base64,") {
				t.Fatal("wrapped image was left in text")
			}
		}
		if images != 1 || (strings.Contains(fmt.Sprint(output), "9007199254740993") && !strings.Contains(partsText(parts), "9007199254740993")) {
			t.Fatal("image or large JSON integer was lost")
		}
	}
	for _, text := range []string{` {"number":9007199254740993,"status":"ok"} `, `{"image_url":"data:image/png;base64,YQ=="}`, strings.Repeat("[", 40) + "1" + strings.Repeat("]", 40)} {
		parts, err := parseResponsesToolOutput(text)
		if err != nil || !reflect.DeepEqual(parts, []canonPart{{Kind: "text", Text: text}}) {
			t.Fatalf("media-free JSON changed: %+v / %v", parts, err)
		}
	}
}

func TestResponsesNativeToolMediaRequestRemainsUnchanged(t *testing.T) {
	body := `{"model":"alias","input":[{"type":"custom_tool_call","call_id":"c","name":"exec","input":"inspect"},{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var actual map[string]any
		json.NewDecoder(r.Body).Decode(&actual)
		var expected map[string]any
		json.Unmarshal([]byte(body), &expected)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("native image history was changed: %+v", actual)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, protocolResponses[FormatOpenAIResponse])
	}))
	defer server.Close()
	up := fixtureUpstream(FormatOpenAIResponse, server.URL)
	up.ModelMap = ""
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	cr := parseCodexFixture(t, body)
	_, wrote, err := doRelay(c, up, &relayTask{format: FormatOpenAIResponse, endpoint: "/v1/responses", model: "alias", body: []byte(body), canon: cr})
	if err != nil || !wrote || rec.Code != 200 {
		t.Fatalf("native media relay failed: %v / %s", err, rec.Body.String())
	}
}

func TestStreamRequestsPreferUncompressedUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("stream may be buffered by compression: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
	}))
	defer server.Close()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	_, _, err := doRelay(c, fixtureUpstream(FormatOpenAIChat, server.URL), fixtureTask(t, FormatOpenAIResponse, true))
	if err != nil {
		t.Fatal(err)
	}
}
