package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func benchmarkNativeFrames(format clientFormat) string {
	var delta, end string
	switch format {
	case FormatOpenAIChat:
		delta = `{"choices":[{"index":0,"delta":{"content":"hello"}}]}`
		end = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"
	case FormatOpenAIResponse:
		delta = `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`
		end = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":17,\"output_tokens\":0}}}\n\n"
	case FormatAnthropic:
		delta = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`
		end = "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	case FormatGemini:
		delta = `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}]}`
		end = "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":17,\"candidatesTokenCount\":0}}\n\n"
	}
	prelude := nativePrelude(format)
	if format == FormatAnthropic {
		prelude = "data: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"content\":[],\"usage\":{\"input_tokens\":17}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	}
	return prelude + strings.Repeat("data: "+delta+"\n\n", 1024) + end
}

func BenchmarkNativeRelayStream(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, format := range protocolFormats {
		b.Run(string(format), func(b *testing.B) {
			frames := benchmarkNativeFrames(format)
			b.ReportAllocs()
			b.SetBytes(int64(len(frames)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c, _ := gin.CreateTestContext(&discardStreamResponse{header: make(http.Header)})
				info := relayInfo{}
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(frames))}
				if err := relaySSENative(c, resp, format, &info); err != nil {
					b.Fatal(err)
				}
				if info.OutputText.Len() != 1024*len("hello") || !info.OutKnown || info.CompletionTok != 0 {
					b.Fatal("native stream lost text or explicit zero usage")
				}
			}
		})
	}
}

func BenchmarkNativeCollectStream(b *testing.B) {
	for _, format := range protocolFormats {
		b.Run(string(format), func(b *testing.B) {
			frames := benchmarkNativeFrames(format)
			b.ReportAllocs()
			b.SetBytes(int64(len(frames)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				info := relayInfo{}
				if _, err := collectNativeProtocolSSE(strings.NewReader(frames), format, &info); err != nil {
					b.Fatal(err)
				}
				if info.OutputText.Len() != 1024*len("hello") || !info.OutKnown || info.CompletionTok != 0 {
					b.Fatal("buffered stream lost text or explicit zero usage")
				}
			}
		})
	}
}
