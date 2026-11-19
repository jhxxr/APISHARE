package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func parseClientRequestLegacy(format clientFormat, body []byte, pathModel string) (*canonReq, error) {
	var (
		cr  *canonReq
		err error
	)
	switch format {
	case FormatOpenAIChat:
		cr, err = parseOpenAIChat(body)
	case FormatOpenAIResponse:
		cr, err = parseOpenAIResponses(body)
	case FormatAnthropic:
		cr, err = parseAnthropicMessages(body)
	case FormatGemini:
		cr, err = parseGeminiBody(body, pathModel)
	default:
		return nil, fmt.Errorf("unknown client format")
	}
	if err == nil {
		cr.ConversionError = enrichCanonRequest(cr, format, body)
	}
	return cr, err
}

func compareRequestParseResults(t *testing.T, format clientFormat, body, pathModel string) *canonReq {
	t.Helper()
	input := []byte(body)
	before := bytes.Clone(input)
	got, gotErr := parseClientRequest(format, input, pathModel)
	want, wantErr := parseClientRequestLegacy(format, []byte(body), pathModel)
	if !bytes.Equal(input, before) {
		t.Fatal("request body was mutated")
	}
	if errorText(gotErr) != errorText(wantErr) {
		t.Fatalf("parse error changed: got %v, want %v", gotErr, wantErr)
	}
	if (got == nil) != (want == nil) {
		t.Fatalf("request nilness changed: got %#v, want %#v", got, want)
	}
	if got != nil {
		gotConversionErr, wantConversionErr := errorText(got.ConversionError), errorText(want.ConversionError)
		gotCopy, wantCopy := *got, *want
		gotCopy.ConversionError, wantCopy.ConversionError = nil, nil
		if !reflect.DeepEqual(gotCopy, wantCopy) || gotConversionErr != wantConversionErr {
			t.Fatalf("canonical request changed:\n got: %#v (conversion error %q)\nwant: %#v (conversion error %q)", got, gotConversionErr, want, wantConversionErr)
		}
	}
	return got
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Comparison only; preserving the original error object is not needed by these tests.
func TestParseClientRequestReusesDecodedRootWithoutChangingCanonicalRequest(t *testing.T) {
	const big = `9007199254740993`
	samples := []struct {
		name      string
		format    clientFormat
		pathModel string
		body      string
	}{
		{
			name:   "chat",
			format: FormatOpenAIChat,
			body:   `{"model":"alias","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"id":{"const":` + big + `}}}}}],"provider_extension":{"opaque":[{"id":` + big + `,"payload":"keep"}]}}`,
		},
		{
			name:   "responses",
			format: FormatOpenAIResponse,
			body:   `{"model":"alias","instructions":"system","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"const":` + big + `}}}}],"provider_extension":{"opaque":{"id":` + big + `}}}`,
		},
		{
			name:   "anthropic",
			format: FormatAnthropic,
			body:   `{"model":"alias","max_tokens":1024,"system":"system","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"id":{"const":` + big + `}}}}],"provider_extension":{"opaque":{"id":` + big + `}}}`,
		},
		{
			name:      "gemini",
			format:    FormatGemini,
			pathModel: "alias",
			body:      `{"systemInstruction":{"parts":[{"text":"system"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":{"type":"OBJECT","properties":{"id":{"const":` + big + `}}}}]}],"provider_extension":{"opaque":{"id":` + big + `}}}`,
		},
	}
	for _, sample := range samples {
		t.Run(sample.name, func(t *testing.T) {
			cr := compareRequestParseResults(t, sample.format, sample.body, sample.pathModel)
			if cr == nil || cr.ConversionError != nil || len(cr.Tools) != 1 {
				t.Fatalf("expected valid canonical tool request: %#v", cr)
			}
			properties := object(object(cr.Tools[0].Schema)["properties"])
			if got := object(properties["id"])["const"]; got != json.Number(big) {
				t.Fatalf("large schema integer lost precision: %#v (%T)", got, got)
			}
			if !strings.Contains(sample.body, `"provider_extension"`) {
				t.Fatal("fixture must contain an opaque native extension")
			}
		})
	}
}

func TestParseClientRequestReusePreservesMalformedAndConversionErrors(t *testing.T) {
	cases := []struct {
		format clientFormat
		body   string
	}{
		{FormatOpenAIChat, `{"model":`},
		{FormatOpenAIResponse, `[]`},
		{FormatAnthropic, `null`},
		{FormatGemini, `{"contents":[`},
		{FormatOpenAIChat, `{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`},
		{FormatOpenAIResponse, `{"input":"hi","previous_response_id":"resp_1"}`},
		{FormatAnthropic, `{"thinking":{"type":"enabled","budget_tokens":5},"max_tokens":5,"messages":[]}`},
		{FormatGemini, `{"generationConfig":{"candidateCount":2},"contents":[]}`},
		{FormatOpenAIResponse, `{"Model":"alias","Instructions":"system","input":"hello","temperature":"invalid"}`},
		{FormatAnthropic, `{"Model":"alias","messages":[],"max_tokens":"invalid"}`},
		{FormatGemini, `{"cachedContent":"cache/1","contents":[]}`},
		{FormatGemini, `{"continuationToken":"continue","contents":[]}`},
		{FormatGemini, `{"safetySettings":[],"contents":[]}`},
	}
	for _, tc := range cases {
		t.Run(string(tc.format)+"/"+tc.body, func(t *testing.T) {
			compareRequestParseResults(t, tc.format, tc.body, "model")
		})
	}
}

func BenchmarkParseClientRequestRawReuse(b *testing.B) {
	body := []byte(`{"model":"alias","messages":[{"role":"user","content":"hello"}],"provider_extension":{"opaque":[{"payload":"` + strings.Repeat("x", 1<<16) + `","id":9007199254740993}]}}`)
	b.Run("RepeatedBodyDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := parseClientRequestLegacy(FormatOpenAIChat, body, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ReuseRawMap", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := parseClientRequest(FormatOpenAIChat, body, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
