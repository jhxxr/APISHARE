package opencodecompat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestTitleRequestsAreClassifiedBeforeShaping(t *testing.T) {
	for _, v1 := range []bool{false, true} {
		for _, tc := range []struct {
			name, body string
		}{
			{"v1", `{"model":"free","messages":[{"role":"user","content":"Generate a title for this conversation:\nFix the build"}],"max_tokens":32}`},
			{"v2-system", `{"model":"free","messages":[{"role":"system","content":"You name conversations so the user can find them later in a list of past sessions."},{"role":"user","content":"Fix the build"}]}`},
			{"v2-developer", `{"messages":[{"role":"developer","content":[{"type":"text","text":"You generate a descriptive title for this conversation. Reply with a title and nothing else."}]},{"role":"user","content":"Fix the build"}],"tools":[]}`},
			{"v2-rename", `{"messages":[{"role":"system","content":"You decide whether the session title still matches the conversation."},{"role":"user","content":"Fix the build"}],"stream_options":{"custom":true}}`},
		} {
			t.Run(fmt.Sprintf("%s/v1=%t", tc.name, v1), func(t *testing.T) {
				payload, renames, err := Prepare([]byte(tc.body), Chat, v1)
				if err != nil || !renames.Empty() || len(SliceAt(payload, "tools")) != 0 {
					t.Fatalf("title acquired agent tools: %+v / %v", payload["tools"], err)
				}
				encoded, _ := Marshal(payload)
				if payload["stream"] != true || !bytes.Contains(encoded, []byte("Fix the build")) || !bytes.Contains(encoded, []byte(opencodePromptMarker)) {
					t.Fatalf("title lost required wire shape or instructions: %s", encoded)
				}
				if tc.name == "v1" && fmt.Sprint(payload["max_tokens"]) != "32" {
					t.Fatal("title budget replaced")
				}
				if tc.name == "v2-rename" && !bytes.Contains(encoded, []byte(`"custom":true`)) {
					t.Fatal("title stream options lost")
				}
			})
		}
	}
}

func TestTitleClassificationDoesNotConsumeAgentRequests(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"Reply with a title and nothing else"}]}`,
		`{"messages":[{"role":"user","content":"Discuss how to generate a title for this conversation"}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"},{"role":"assistant","content":"previous title"}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"},{"role":"tool","content":"result"}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"},{"role":"function","name":"read","content":"result"}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"},{"role":"user","content":"one"},{"role":"user","content":"two"},{"role":"user","content":"three"}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"}],"tools":[{"type":"function","function":{"name":"Read"}}]}`,
		`{"messages":[{"role":"system","content":"Reply with a title and nothing else"}],"functions":[{"name":"Read"}]}`,
	} {
		payload, _ := Decode([]byte(body))
		if isTitleGenerationRequest(payload, Chat) {
			t.Fatalf("agent request misclassified: %s", body)
		}
		shaped, _, err := Prepare([]byte(body), Chat, false)
		if err != nil || len(SliceAt(shaped, "tools")) < 13 {
			t.Fatalf("agent tool shaping bypassed: %s / %v", body, err)
		}
	}
	for _, body := range []string{`{"tools":{}}`, `{"functions":"invalid"}`} {
		if _, _, err := Prepare([]byte(body), Chat, false); err == nil || !strings.Contains(err.Error(), "必须是数组") {
			t.Fatalf("invalid tool list was hidden by title classification: %v", err)
		}
	}
}

func TestStructuredProtocolTitleClassification(t *testing.T) {
	for _, tc := range []struct {
		protocol Protocol
		body     string
		want     bool
	}{
		{Anthropic, `{"system":"You generate a descriptive title for this conversation","messages":[{"role":"user","content":"Fix the build"}]}`, true},
		{Anthropic, `{"messages":[{"role":"user","content":[{"type":"text","text":"Generate a title for this conversation: Fix the build"}]}]}`, true},
		{Anthropic, `{"system":"Reply with a title and nothing else","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"old","content":"result"}]}]}`, false},
		{Responses, `{"instructions":"Reply with a title and nothing else","input":"Fix the build"}`, true},
		{Responses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"Generate a title for this conversation: Fix the build"}]}]}`, true},
		{Responses, `{"instructions":"Reply with a title and nothing else","input":[{"type":"function_call","name":"Read","call_id":"old"}]}`, false},
		{Responses, `{"instructions":"Reply with a title and nothing else","input":[{"role":"assistant","content":"previous title"}]}`, false},
	} {
		payload, _ := Decode([]byte(tc.body))
		if got := isTitleGenerationRequest(payload, tc.protocol); got != tc.want {
			t.Errorf("%s classification = %t, want %t: %s", tc.protocol, got, tc.want, tc.body)
		}
		payload, _, err := Prepare([]byte(tc.body), tc.protocol, false)
		if err != nil || (len(SliceAt(payload, "tools")) == 0) != tc.want {
			t.Errorf("%s title shaping lost classification: %v / %v", tc.protocol, payload["tools"], err)
		}
	}
}
