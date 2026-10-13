package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func BenchmarkRoutingSelection(b *testing.B) {
	benchmarkPerformanceDB(b)
	models := make([]string, 128)
	for i := range models {
		models[i] = fmt.Sprintf("model-%d", i)
	}
	for i := 0; i < 32; i++ {
		if err := dbInsertUpstream(&Upstream{Name: fmt.Sprintf("up-%d", i), Type: "openai", Models: strings.Join(models, ","), ModelMap: `{"model-127":"actual-model"}`, Weight: i + 1, Enabled: true}); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := pickUpstream(FormatOpenAIChat, "model-127", nil, false); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pickUpstream(FormatOpenAIChat, "model-127", nil, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHeaderOverrides(b *testing.B) {
	raw := `{"*":true,"re:^X-Trace-.*$":true,"re:^X-Private-.*$":false,"Authorization":"Bearer {api_key}","X-Tenant":"{client_header:X-Tenant}"}`
	incoming := http.Header{"X-Tenant": {"one"}, "X-Trace-Id": {"trace"}, "X-Private-Id": {"private"}, "Authorization": {"Bearer client-key"}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", "http://upstream.invalid/v1/chat/completions", nil)
		if err := applyHeaderOverrides(req, raw, "upstream-key", incoming); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNativeEventAccounting(b *testing.B) {
	payload := `{"choices":[{"index":0,"delta":{"content":"hello"}}],"usage":{"prompt_tokens":17,"completion_tokens":23}}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var info relayInfo
		recordNativeEvent(FormatOpenAIChat, payload, &info)
	}
}

func BenchmarkCompiledHeaderOverrides(b *testing.B) {
	up := &Upstream{APIKey: "upstream-key", HeaderOverrides: `{"*":true,"re:^X-Trace-.*$":true,"re:^X-Private-.*$":false,"Authorization":"Bearer {api_key}","X-Tenant":"{client_header:X-Tenant}"}`}
	compileUpstreamConfig(up)
	incoming := http.Header{"X-Tenant": {"one"}, "X-Trace-Id": {"trace"}, "X-Private-Id": {"private"}, "Authorization": {"Bearer client-key"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("POST", "http://upstream.invalid/v1/chat/completions", nil)
		if err := up.applyHeaders(req, incoming); err != nil {
			b.Fatal(err)
		}
	}
}
