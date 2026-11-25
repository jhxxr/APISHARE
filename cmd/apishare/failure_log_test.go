package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testFailureLogs(t *testing.T, entryBytes int) *failureLogStore {
	t.Helper()
	s, err := openFailureLogStore(t.TempDir(), 1<<20, entryBytes)
	if err != nil {
		t.Fatal(err)
	}
	old := activeFailureLogs()
	setFailureLogs(s)
	t.Cleanup(func() { setFailureLogs(old) })
	return s
}

func readTestFailureLog(t *testing.T, s *failureLogStore) (failureLogEntry, string) {
	t.Helper()
	files, err := os.ReadDir(s.dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one paired record, got %v (%v)", files, err)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var entry failureLogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	return entry, string(data)
}

func TestHTTPFailureLogPairsOriginalRequestAndReply(t *testing.T) {
	const message = "private user text"
	const prompt = "another private prompt"
	const gatewayKey = "sk-gateway-secret"
	const upstreamKey = "sk-upstream-secret"
	const replyTail = "far beyond the relay's old 16 KiB cutoff"
	for _, tc := range []struct {
		name                          string
		upstreamStatus, gatewayStatus int
		replyType                     string
		reply                         string
	}{
		{"nonretryable JSON", 400, 400, "application/json", `{"error":{"type":"invalid_request_error","message":"private \u0075ser text sk-upstream-secret"},"prompt":"` + prompt + `","diagnostic":"` + strings.Repeat("X", 20000) + replyTail + `"}`},
		{"retryable JSON", 503, 502, "application/json", `{"error":{"type":"provider_overloaded","message":"unavailable"}}`},
		{"protocol JSON", 200, 502, "application/json", `{"error":{"code":"invalid_model","message":"unavailable"}}`},
		{"protocol SSE", 200, 502, "text/event-stream", "data: {\"error\":{\"type\":\"overloaded\",\"message\":\"unavailable\"}}\n\n"},
		{"success", 200, 200, "application/json", `{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelsTestDB(t)
			s := testFailureLogs(t, 64<<10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.replyType)
				w.WriteHeader(tc.upstreamStatus)
				fmt.Fprint(w, tc.reply)
			}))
			defer server.Close()
			up := &Upstream{Name: "diagnostic provider", Type: "openai", BaseURL: server.URL, APIKey: upstreamKey, Models: "alias", Enabled: true, Weight: 1}
			if err := dbInsertUpstream(up); err != nil {
				t.Fatal(err)
			}
			key := ApiKey{Models: "alias", Name: "diagnostic", Key: gatewayKey, Enabled: true}
			if err := dbInsertKey(&key); err != nil {
				t.Fatal(err)
			}
			r := gin.New()
			r.POST("/v1/chat/completions", keyAuth(), relayHandler(FormatOpenAIChat, false))
			body := `{"model":"alias","messages":[{"role":"user","content":"` + message + `"}],"prompt":"` + prompt + `","temperature":0.6,"unknown":"private unknown","stream":` + fmt.Sprint(tc.replyType == "text/event-stream") + `}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?secret=private-query", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+gatewayKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Custom", "private header value")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.gatewayStatus {
				t.Fatalf("gateway status = %d, want %d", rec.Code, tc.gatewayStatus)
			}
			if tc.gatewayStatus == 200 {
				if files, err := os.ReadDir(s.dir); err != nil || len(files) != 0 {
					t.Fatalf("success created failure files: %v (%v)", files, err)
				}
				return
			}
			entry, text := readTestFailureLog(t, s)
			for _, secret := range []string{message, prompt, gatewayKey, upstreamKey, "private unknown", "private-query", "private header value"} {
				if strings.Contains(text, secret) {
					t.Fatalf("leaked %q", secret)
				}
			}
			if entry.RequestID == "" || entry.RequestID != rec.Header().Get(failureLogRequestHeader) || entry.Time.IsZero() || entry.Status != tc.gatewayStatus || entry.Upstream.Status != tc.upstreamStatus {
				t.Fatalf("wrong correlation/status: %+v", entry)
			}
			logs, err := dbListLogs(10, 0)
			if err != nil || len(logs) != 1 || logs[0].TraceID != entry.RequestID || strings.Contains(logs[0].Error, message) {
				t.Fatalf("wrong call log: %+v / %v", logs, err)
			}
			if entry.Request.Route != "/v1/chat/completions" || entry.Request.Model != "alias" || entry.Request.Bytes != len(body) || entry.Request.Headers["authorization"] != failureRedacted || entry.Request.Body["temperature"] != 0.6 || !strings.Contains(text, `"role":"user","text_chars":17`) {
				t.Fatalf("request metadata missing: %+v", entry.Request)
			}
			if tc.upstreamStatus == 400 && (!strings.Contains(entry.Upstream.Body, replyTail) || entry.Upstream.Truncated || !entry.Upstream.Redacted || entry.Upstream.ErrorType != "invalid_request_error" || rec.Body.Len() != 16<<10) {
				t.Fatalf("expected extended, redacted diagnostic and unchanged HTTP passthrough (upstream body length %d, downstream length %d)", len(entry.Upstream.Body), rec.Body.Len())
			}
			if entry.Upstream.Channel != up.Name || entry.Upstream.Model != "alias" || entry.Upstream.Body == "" || entry.Upstream.Summary == "" {
				t.Fatalf("missing upstream metadata: %+v", entry.Upstream)
			}
		})
	}
}

func TestFailureLogEntryClipping(t *testing.T) {
	s := testFailureLogs(t, 1024)
	_, reply := readFailureReply(strings.NewReader(`{"error":{"message":"`+strings.Repeat("界\\t<&", 2000)+`"}}`), 16<<10, func() {})
	entry := failureLogEntry{Time: time.Now().UTC(), RequestID: randHex(16), Request: failureRequest{Model: "test"}, Upstream: failureResponse{Status: 400}}
	entry.Upstream.setBody(reply, nil, nil)
	if err := s.write(entry); err != nil {
		t.Fatal(err)
	}
	got, text := readTestFailureLog(t, s)
	if len(text) > 1024 || !got.Truncated || !got.Upstream.Truncated || got.Upstream.Body == "" || got.Request.Model != "test" {
		t.Fatalf("wrong clipped entry: size %d, record %+v", len(text), got)
	}
}

func TestFailureLogDiagnosticReadDoesNotWaitForUpstreamEOF(t *testing.T) {
	testFailureLogs(t, 64<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, strings.Repeat("x", 16<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	original, reply := readFailureReply(resp.Body, 16<<10, cancel)
	if time.Since(start) > 3*time.Second || len(original) != 16<<10 || len(reply.data) != 16<<10 || !reply.truncated {
		t.Fatalf("diagnostic read changed relay prefix or waited too long; elapsed=%s original=%d captured=%d truncated=%v", time.Since(start), len(original), len(reply.data), reply.truncated)
	}
}

func TestFailureLogEvictionAndRestart(t *testing.T) {
	dir := t.TempDir()
	entry := failureLogEntry{Time: time.Now().UTC(), RequestID: randHex(16), Upstream: failureResponse{Body: strings.Repeat("x", 500)}}
	data, err := entry.marshal(1024)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	s, err := openFailureLogStore(dir, 2*size, 1024)
	if err != nil {
		t.Fatal(err)
	}
	oldest := entry.RequestID
	for range 3 {
		if err := s.write(entry); err != nil {
			t.Fatal(err)
		}
		entry.RequestID = randHex(16)
	}
	if len(s.files) != 2 || s.used != 2*size || strings.Contains(s.files[0].name, oldest) {
		t.Fatalf("oldest entry not evicted: %+v (%d bytes)", s.files, s.used)
	}
	newest := s.files[1].name
	for _, name := range []string{"keep-me.json", ".apishare-failure-crashed.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err = openFailureLogStore(dir, size, 1024)
	if err != nil || len(s.files) != 1 || s.files[0].name != newest || s.used != size {
		t.Fatalf("restart failed to apply lower cap: %+v / %v", s, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep-me.json")); err != nil {
		t.Fatal("unrelated file removed:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".apishare-failure-crashed.tmp")); !os.IsNotExist(err) {
		t.Fatal("crashed write was not removed:", err)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			copy := entry
			copy.RequestID = randHex(16)
			if err := s.write(copy); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(s.files) != 1 || s.used != size {
		t.Fatalf("concurrent writers exceeded cap: %+v (%d bytes)", s.files, s.used)
	}
}

func TestFailureLogSettingsNormalized(t *testing.T) {
	// Console-provided settings must be clamped instead of silently accepted:
	// a zero or absurd size would otherwise disable or exhaust the disk.
	if got := (failureLogSettings{}).normalized(); got.MaxMB != failureLogDefaultMaxMB || got.EntryKiB != failureLogDefaultEntryKiB {
		t.Fatalf("zero values not defaulted: %+v", got)
	}
	if got := (failureLogSettings{MaxMB: -1, EntryKiB: 0}).normalized(); got.MaxMB != failureLogDefaultMaxMB || got.EntryKiB != failureLogDefaultEntryKiB {
		t.Fatalf("negative values not defaulted: %+v", got)
	}
	if got := (failureLogSettings{MaxMB: failureLogMaxMB + 1}).normalized(); got.MaxMB != failureLogDefaultMaxMB {
		t.Fatalf("oversized max_mb not clamped: %+v", got)
	}
	if got := (failureLogSettings{MaxMB: 8, EntryKiB: 32, FullContent: true}).normalized(); got.MaxMB != 8 || got.EntryKiB != 32 || !got.FullContent {
		t.Fatalf("valid settings changed: %+v", got)
	}
}

func TestFailureLogDirFollowsDatabase(t *testing.T) {
	previous := failureLogDBPath
	t.Cleanup(func() { failureLogDBPath = previous })
	failureLogDBPath = filepath.Join("var", "gateway.db")
	if got := failureLogDir(); got != filepath.Join("var", "gateway.db.failure-logs") {
		t.Fatalf("dir = %q", got)
	}
}

func TestFailureLogStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := openFailureLogStore(dir, 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		entry := failureLogEntry{Time: time.Unix(int64(1700000000+i), 0).UTC(), RequestID: fmt.Sprintf("%08x%024x", i, i),
			Status: 502, Duration: 42, Request: failureRequest{Method: "POST", Route: "/v1/messages", Model: "gpt-test"},
			Upstream: failureResponse{Channel: "up", Kind: "upstream_error", Summary: "boom"}}
		if err := s.write(entry); err != nil {
			t.Fatal(err)
		}
	}
	items, total := s.list(0, 10)
	if total != 3 || len(items) != 3 {
		t.Fatalf("list = %d/%d", len(items), total)
	}
	if items[0].RequestID != fmt.Sprintf("%08x%024x", 2, 2) {
		t.Fatalf("newest first violated: %+v", items[0])
	}
	if items[0].Model != "gpt-test" || items[0].Kind != "upstream_error" {
		t.Fatalf("list row missing detail: %+v", items[0])
	}
	full, err := s.entry(items[0].Name)
	if err != nil || full.Request.Route != "/v1/messages" {
		t.Fatalf("entry = %+v, %v", full, err)
	}
	// Path traversal must never resolve through the store.
	if _, err := s.entry("../../etc/passwd"); err == nil {
		t.Fatal("unsafe name accepted")
	}
	if err := s.remove(items[0].Name); err != nil {
		t.Fatal(err)
	}
	if items, total := s.list(0, 10); total != 2 || len(items) != 2 {
		t.Fatalf("after remove = %d/%d", len(items), total)
	}
	if removed := s.clear(); removed != 2 {
		t.Fatalf("clear removed %d", removed)
	}
	if items, total := s.list(0, 10); total != 0 || len(items) != 0 {
		t.Fatalf("after clear = %d/%d", len(items), total)
	}
}

func TestFailureLogFullContentKeepsRawBody(t *testing.T) {
	dir := t.TempDir()
	s, err := openFailureLogStore(dir, 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	s.fullContent = true
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"secret prompt"}],"api_key":"sk-leak"}`)
	headers := http.Header{"Authorization": {"Bearer sk-leak"}}
	root, raw := failureRawBody(body, 4096)
	if raw != "" || root["model"] != "m" {
		t.Fatalf("raw body not decoded: %v %q", root, raw)
	}
	if got := failureRawHeaders(headers)["authorization"]; got != "Bearer sk-leak" {
		t.Fatalf("authorization = %q", got)
	}
	// Redacted mode must keep dropping the same material.
	redacted := failureRequestHeaders(headers)
	if redacted["authorization"] == "Bearer sk-leak" {
		t.Fatalf("redacted mode leaked authorization")
	}
}

func TestFailureLogRecordsRemainReadableWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	previous := failureLogDBPath
	failureLogDBPath = filepath.Join(dir, "gateway.db")
	t.Cleanup(func() { failureLogDBPath = previous })

	s, err := openFailureLogStore(failureLogDir(), 1<<20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	entry := failureLogEntry{Time: time.Now().UTC(), RequestID: strings.Repeat("ab", 16), Status: 500,
		Request:  failureRequest{Method: "POST", Route: "/v1/messages", Model: "m"},
		Upstream: failureResponse{Kind: "upstream_http_error", Summary: "boom"}}
	if err := s.write(entry); err != nil {
		t.Fatal(err)
	}

	// Disabling recording must not hide what was already saved.
	if err := applyFailureLogSettings(failureLogSettings{Enabled: false, MaxMB: 32, EntryKiB: 64}); err != nil {
		t.Fatal(err)
	}
	if activeFailureLogs() != nil {
		t.Fatal("disabled store still present")
	}
	reader := failureLogStoreForReading()
	if reader == nil {
		t.Fatal("no store for reading after disabling")
	}
	items, total := reader.list(0, 10)
	if total != 1 || len(items) != 1 || items[0].Model != "m" {
		t.Fatalf("records not readable while disabled: %d/%d %+v", len(items), total, items)
	}
	if _, err := reader.entry(items[0].Name); err != nil {
		t.Fatalf("detail unreadable while disabled: %v", err)
	}
	if err := reader.remove(items[0].Name); err != nil {
		t.Fatalf("delete failed while disabled: %v", err)
	}
}

func TestFailureLogConcurrentSettingsSwap(t *testing.T) {
	dir := t.TempDir()
	previous := failureLogDBPath
	failureLogDBPath = filepath.Join(dir, "gateway.db")
	t.Cleanup(func() { failureLogDBPath = previous })

	// Readers and a settings writer run together: this is the race that used to
	// panic when the handle was swapped to nil mid-read.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			settings := failureLogSettings{Enabled: i%2 == 0, MaxMB: 2, EntryKiB: 16}
			if err := applyFailureLogSettings(settings); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		_ = failureLogsEnabled()
		_ = failureCaptureLimit()
		_ = activeFailureLogs()
	}
	<-done
}

func TestFailureLogLargeRawPayloadKeepsRecord(t *testing.T) {
	// A prompt far larger than the per-entry limit used to make marshal fail,
	// which dropped the whole record — the exact case worth diagnosing.
	payload := strings.Repeat("prompt-", 20000)
	entry := failureLogEntry{
		Time: time.Now().UTC(), RequestID: strings.Repeat("cd", 16), Status: 502,
		Request:  failureRequest{Method: "POST", Route: "/v1/messages", Model: "claude", Raw: payload, Bytes: len(payload)},
		Upstream: failureResponse{Kind: "upstream_http_error", Summary: "upstream returned 502", Body: strings.Repeat("upstream-", 8000)},
	}
	for _, limit := range []int{16 << 10, 64 << 10, 256 << 10} {
		b, err := entry.marshal(limit)
		if err != nil {
			t.Fatalf("limit %d dropped the record: %v", limit, err)
		}
		if len(b) > limit {
			t.Fatalf("limit %d produced %d bytes", limit, len(b))
		}
		var got failureLogEntry
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		// Small limits shrink the record; the largest one may hold everything,
		// in which case the marker must stay absent.
		fits := len(payload)+len(entry.Upstream.Body) < limit
		if fits {
			if got.TruncatedReason != "" {
				t.Fatalf("limit %d marked a record that fits: %q", limit, got.TruncatedReason)
			}
		} else if got.TruncatedReason != failureTruncatedEntryLimit || !got.Truncated {
			t.Fatalf("limit %d missing truncation marker: %+v", limit, got.TruncatedReason)
		}
		// Request side survives: it is only recorded here, so it gets priority.
		// It is shrunk only when the limit cannot hold it whole.
		if got.Request.Raw == "" {
			t.Fatalf("limit %d lost request body", limit)
		}
		if got.Request.RawTruncated != (len(got.Request.Raw) < len(payload)) {
			t.Fatalf("limit %d wrong request truncation flag: %d of %d bytes, truncated=%v",
				limit, len(got.Request.Raw), len(payload), got.Request.RawTruncated)
		}
		if !strings.HasPrefix(payload, got.Request.Raw) {
			t.Fatalf("limit %d kept a non-prefix slice", limit)
		}
		// The request share takes priority, so a tight limit may leave no room
		// for the reply; that must be reported rather than silently empty.
		if got.Upstream.Truncated != (len(got.Upstream.Body) < len(entry.Upstream.Body)) {
			t.Fatalf("limit %d wrong upstream truncation flag: %d bytes, truncated=%v",
				limit, len(got.Upstream.Body), got.Upstream.Truncated)
		}
	}
}

func TestFailureLogEntryBudgetPrefersRequest(t *testing.T) {
	// Both sides are oversized; the request must keep the larger share.
	entry := failureLogEntry{
		Time: time.Now().UTC(), RequestID: strings.Repeat("ef", 16), Status: 500,
		Request:  failureRequest{Method: "POST", Model: "m", Raw: strings.Repeat("R", 100000)},
		Upstream: failureResponse{Kind: "upstream_http_error", Summary: "s", Body: strings.Repeat("U", 100000)},
	}
	b, err := entry.marshal(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	var got failureLogEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Request.Raw) <= len(got.Upstream.Body) {
		t.Fatalf("request share not larger: request=%d upstream=%d", len(got.Request.Raw), len(got.Upstream.Body))
	}
}

func TestFailureLogFullContentRaisesEntryDefault(t *testing.T) {
	redacted := failureLogSettings{Enabled: true, MaxMB: 32}
	full := failureLogSettings{Enabled: true, MaxMB: 32, FullContent: true}
	if got := redacted.normalized().EntryKiB; got != failureLogDefaultEntryKiB {
		t.Fatalf("redacted default = %d", got)
	}
	if got := full.normalized().EntryKiB; got != failureLogFullContentEntryKiB {
		t.Fatalf("full content default = %d", got)
	}
	if got := failureLogFullContentEntryKiB; got <= failureLogDefaultEntryKiB {
		t.Fatalf("full content default should exceed redaction default")
	}
}

func TestFailureLogKeepsNestedBodyPrefix(t *testing.T) {
	// Chat payloads nest their text under messages[].content; a flat top-level
	// scan used to keep nothing and store an empty body.
	content := strings.Repeat("ABCDEFGH", 10000)
	entry := failureLogEntry{
		Time: time.Now().UTC(), RequestID: strings.Repeat("12", 16), Status: 502,
		Request: failureRequest{Method: "POST", Route: "/v1/messages", Model: "m",
			Body: map[string]any{"model": "m", "max_tokens": float64(8),
				"messages": []any{map[string]any{"role": "user", "content": content}}}},
		Upstream: failureResponse{Kind: "upstream_http_error", Summary: "s"},
	}
	b, err := entry.marshal(16 << 10)
	if err != nil {
		t.Fatal(err)
	}
	var got failureLogEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	messages, _ := got.Request.Body["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("nested body dropped entirely: %+v", got.Request.Body)
	}
	saved, _ := messages[0].(map[string]any)["content"].(string)
	if saved == "" {
		t.Fatal("nested content kept no prefix")
	}
	if !strings.HasPrefix(content, saved) {
		t.Fatalf("kept a non-prefix slice of content")
	}
	// Shape is preserved: sibling fields survive alongside the shortened text.
	if got.Request.Body["max_tokens"] != float64(8) || got.Request.Body["model"] != "m" {
		t.Fatalf("sibling fields lost: %+v", got.Request.Body)
	}
}
