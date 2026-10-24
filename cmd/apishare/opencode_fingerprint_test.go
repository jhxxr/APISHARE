package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apishare/internal/bunhttp"

	utls "github.com/refraction-networking/utls"
)

func TestOpenCodeLaneHeaderShapes(t *testing.T) {
	session := "ses_0123456789abCDEFGHIJKLMNOP"
	incoming := http.Header{}
	incoming.Set("x-opencode-request", "incoming-legacy")
	incoming.Set("x-opencode-project", "prj_foreign")
	incoming.Set("User-Agent", "foreign-sdk")
	incoming.Set("Accept", "text/event-stream")
	task := &relayTask{canon: &canonReq{Model: "free", Messages: []canonMsg{{Role: "user", Text: "hello"}}}}
	body := []byte(`{"model":"free","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	v1 := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	setOpenCodeHeaders(v1, "public", FormatOpenAIChat, session, incoming, task, body)
	if got, want := v1.Header.Get("User-Agent"), "opencode/1.19.3 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"; got != want {
		t.Errorf("v1 User-Agent = %q, want %q", got, want)
	}
	if v1.Header.Get("Accept") != "*/*" || v1.Header.Get("x-opencode-client") != "cli" {
		t.Errorf("v1 header shape: %v", v1.Header)
	}
	if !openCodeMessagePattern.MatchString(v1.Header.Get("x-opencode-request")) || v1.Header.Get("x-opencode-request") == "incoming-legacy" {
		t.Errorf("v1 request id was not canonicalized: %q", v1.Header.Get("x-opencode-request"))
	}
	if v1.Header.Get("x-session-affinity") != "" || v1.Header.Get("X-Session-Id") != "" {
		t.Errorf("v1 lane sent v2-only session headers: %v", v1.Header)
	}
	if v1.Header.Get("x-opencode-session") != session || v1.Header.Get("x-opencode-session-id") != session {
		t.Errorf("v1 session headers: %v", v1.Header)
	}
	if project := v1.Header.Get("x-opencode-project"); project != "global" && !openCodeProjectPattern.MatchString(project) {
		t.Errorf("v1 project shape: %q", project)
	}

	for _, target := range []string{"https://opencode.ai/zen/go/v1/chat/completions", "https://opencode.ai/inference/v1/chat/completions"} {
		v2 := httptest.NewRequest(http.MethodPost, target, nil)
		v2.Header.Set("Accept", "text/event-stream")
		setOpenCodeHeaders(v2, "public", FormatOpenAIChat, session, incoming, task, body)
		if v2.Header.Get("Accept") != "" {
			t.Errorf("%s did not leave Accept to the runtime: %v", target, v2.Header)
		}
		up := &Upstream{Type: openCodeFreeType, HeaderOverrides: `{"*":true}`}
		if err := up.applyHeaders(v2, incoming); err != nil || v2.Header.Get("Accept") != "" {
			t.Errorf("%s broad passthrough changed the runtime Accept: %v %v", target, v2.Header, err)
		}
		up.HeaderOverrides = `{"Accept":"text/event-stream"}`
		if err := up.applyHeaders(v2, incoming); err != nil || v2.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("%s explicit Accept override was lost: %v %v", target, v2.Header, err)
		}
		if got := v2.Header.Get("User-Agent"); got != openCodeV2UserAgent() {
			t.Errorf("%s User-Agent = %q, want %q", target, got, openCodeV2UserAgent())
		}
		if v2.Header.Get("x-opencode-request") != "" {
			t.Errorf("%s sent x-opencode-request: %v", target, v2.Header)
		}
		if v2.Header.Get("x-session-affinity") != session || v2.Header.Get("X-Session-Id") != session {
			t.Errorf("%s missing v2 session aliases: %v", target, v2.Header)
		}
		if !openCodeProjectPattern.MatchString(v2.Header.Get("x-opencode-project")) {
			t.Errorf("%s project shape: %q", target, v2.Header.Get("x-opencode-project"))
		}
	}
	if got := bunhttp.OpenCodeV2UserAgent(); got != "opencode/latest/2.1.1/cli" {
		t.Errorf("fallback User-Agent = %q", got)
	}

	anthropicBody := []byte(`{"model":"free","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"mcp_servers":[{"name":"x","url":"https://example.invalid"}]}`)
	v1Anthropic := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/messages", nil)
	setOpenCodeHeaders(v1Anthropic, "public", FormatAnthropic, session, nil, nil, anthropicBody)
	if got := v1Anthropic.Header.Get("x-api-key"); got != "public" {
		t.Errorf("v1 Messages x-api-key = %q", got)
	}
	if v1Anthropic.Header.Get("Authorization") != "" {
		t.Error("v1 Messages sent bearer credentials")
	}
	if betas := v1Anthropic.Header.Get("anthropic-beta"); !strings.Contains(betas, "mcp-client-2025-04-04") {
		t.Errorf("v1 Messages beta flags = %q", betas)
	}
	v2Anthropic := httptest.NewRequest(http.MethodPost, "https://opencode.ai/inference/anthropic/v1/messages?beta=true", nil)
	setOpenCodeHeaders(v2Anthropic, "public", FormatAnthropic, session, nil, nil, anthropicBody)
	if got := v2Anthropic.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("v2 Messages beta flags = %q", got)
	}
}

func TestOpenCodeFreeFingerprintSelection(t *testing.T) {
	free := &Upstream{Type: openCodeFreeType, BaseURL: "https://opencode.ai/zen", APIKey: "public"}
	paid := &Upstream{Type: "opencode", BaseURL: "https://opencode.ai/zen", APIKey: "zen-key", OpenCodeTier: "zen"}
	newRequest := func(target string) *http.Request {
		return httptest.NewRequest(http.MethodPost, target, nil)
	}
	for _, tc := range []struct {
		up     *Upstream
		target string
		model  string
		want   bool
	}{
		{free, "https://opencode.ai/zen/v1/chat/completions", "space-bunny-free", true},
		{free, "https://opencode.ai/inference/v1/responses", "space-bunny-free", true},
		{free, "https://opencode.ai/zen/go/v1/chat/completions", "space-bunny-free", false},
		// The anonymous free channel is free-only, so its credential is eligible
		// for every request it can make; upstream still rejects paid models.
		{free, "https://opencode.ai/zen/v1/chat/completions", "paid-model", true},
		{free, "https://example.com/zen/v1/chat/completions", "space-bunny-free", false},
		{paid, "https://opencode.ai/zen/v1/chat/completions", "paid-model", false},
	} {
		if got := openCodeFreeFingerprintRequest(tc.up, newRequest(tc.target), tc.model); got != tc.want {
			t.Errorf("fingerprint selection for %s/%s = %v, want %v", tc.target, tc.model, got, tc.want)
		}
	}
}

func TestBunHTTPHeadOrderAndDefaults(t *testing.T) {
	body := []byte(`{"model":"free"}`)
	req := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/messages?beta=1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.1.1/cli")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("x-api-key", "public")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-opencode-session", "ses_abc")
	head, decompress, err := bunHTTPHead(req, body)
	if err != nil {
		t.Fatal(err)
	}
	want := "POST /zen/v1/messages?beta=1 HTTP/1.1\r\n" +
		"Authorization: Bearer public\r\n" +
		"Content-Length: 16\r\n" +
		"Content-Type: application/json\r\n" +
		"User-Agent: opencode/latest/2.1.1/cli\r\n" +
		"anthropic-version: 2023-06-01\r\n" +
		"x-api-key: public\r\n" +
		"x-opencode-session: ses_abc\r\n" +
		"Connection: keep-alive\r\n" +
		"Accept: */*\r\n" +
		"Host: opencode.ai\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
		"\r\n"
	if string(head) != want {
		t.Fatalf("unexpected Bun head:\n%q\nwant:\n%q", head, want)
	}
	if !decompress {
		t.Error("transport-added Accept-Encoding did not enable decompression")
	}
	// Caller-supplied lengths cannot add a duplicate or misplace the computed
	// value; nil and empty bodies retain their distinct fetch layouts.
	req.Header.Set("Content-Length", "999")
	req.Header.Set("Transfer-Encoding", "chunked")
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"absent", nil, ""},
		{"empty", []byte{}, "Content-Length: 0\r\n"},
		{"nonempty", body, "Content-Length: 16\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, _, err := bunHTTPHead(req, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			got := string(head)
			wantCount := 0
			if tc.want != "" {
				wantCount = 1
				if !strings.Contains(got, tc.want+"Content-Type:") {
					t.Errorf("computed length is outside common headers: %q", got)
				}
			}
			if strings.Count(got, "Content-Length:") != wantCount || strings.Contains(got, "999") || strings.Contains(got, "Transfer-Encoding:") {
				t.Errorf("incorrect transport-owned headers: %q", got)
			}
		})
	}

	identity := httptest.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/messages", nil)
	identity.Header.Set("Accept-Encoding", "identity")
	identity.Header.Set("Accept", "text/event-stream")
	head, decompress, err = bunHTTPHead(identity, nil)
	if err != nil || decompress || !strings.Contains(string(head), "Accept-Encoding: identity\r\n") || !strings.Contains(string(head), "Accept: text/event-stream\r\n") {
		t.Fatalf("caller compression override: %q %v %v", head, decompress, err)
	}
}

func TestBunTransportRoundTripAndCompression(t *testing.T) {
	var remoteAddrs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAddrs = append(remoteAddrs, r.RemoteAddr)
		if r.ProtoMajor != 1 || r.ProtoMinor != 1 {
			t.Errorf("Bun transport used %s", r.Proto)
		}
		if r.Header.Get("User-Agent") != "Bun/1.3.14" {
			t.Errorf("default User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Accept") != "*/*" {
			t.Errorf("default Accept = %q", r.Header.Get("Accept"))
		}
		if r.Header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
			t.Errorf("default Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		payload, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		fmt.Fprintf(writer, "reply:%s", payload)
		writer.Close()
	}))
	defer server.Close()

	transport, err := newBunFingerprintTransport(relayNetwork, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/zen/v1/chat/completions", strings.NewReader("hello"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(got) != "reply:hello" {
			t.Fatalf("round trip %d: %q %v", i, got, err)
		}
		if resp.Header.Get("Content-Encoding") != "" || !resp.Uncompressed {
			t.Errorf("response was not decompressed: %v", resp.Header)
		}
	}
	if len(remoteAddrs) != 2 || remoteAddrs[0] != remoteAddrs[1] {
		t.Errorf("connection was not reused: %v", remoteAddrs)
	}
}

func TestBunTransportTLSALPN(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.NegotiatedProtocol != "http/1.1" {
			t.Errorf("negotiated ALPN = %q", r.TLS.NegotiatedProtocol)
		}
		if r.ProtoMajor != 1 {
			t.Errorf("TLS request protocol = %s", r.Proto)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()
	transport, err := newBunFingerprintTransport(relayNetwork, nil, &utls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Get(server.URL + "/inference/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("TLS body = %q", body)
	}
}

func TestOpenCodeSessionIdentitySources(t *testing.T) {
	header := http.Header{}
	header.Set("session_id", "codex-conversation")
	if got := explicitSessionID(header, ""); got != "codex-conversation" {
		t.Errorf("Codex session_id = %q", got)
	}
	header = http.Header{}
	header.Set("conversation_id", "codex-conversation-2")
	if got := explicitSessionID(header, ""); got != "codex-conversation-2" {
		t.Errorf("Codex conversation_id = %q", got)
	}
	claude := `{"model":"claude","metadata":{"user_id":"user_abc_account_123_session_01234567-89ab-cdef-0123-456789abcdef"},"messages":[{"role":"user","content":"hi"}]}`
	cr, err := parseClientRequest(FormatAnthropic, []byte(claude), "")
	if err != nil || cr.SessionID != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("Claude Code session = %q %v", cr.SessionID, err)
	}
	codex := `{"model":"gpt","input":"hi","conversation_id":"body-conversation"}`
	cr, err = parseClientRequest(FormatOpenAIResponse, []byte(codex), "")
	if err != nil || cr.SessionID != "body-conversation" {
		t.Fatalf("body conversation_id = %q %v", cr.SessionID, err)
	}
}

func TestOpenCodeMessagePinningPerTurn(t *testing.T) {
	session := "ses_0123456789abCDEFGHIJKLMNOP"
	taskFor := func(messages ...canonMsg) *relayTask {
		return &relayTask{canon: &canonReq{Model: "free", Messages: messages}}
	}
	first := taskFor(canonMsg{Role: "user", Text: "hello"})
	firstID := openCodeMessageForTask(first, session, nil)
	if !openCodeMessagePattern.MatchString(firstID) {
		t.Fatalf("invalid message id: %q", firstID)
	}
	second := taskFor(canonMsg{Role: "user", Text: "hello"})
	if got := openCodeMessageForTask(second, session, nil); got != firstID {
		t.Fatalf("same turn changed message id: %q != %q", got, firstID)
	}
	// Tool results arrive as user messages but belong to the same user turn.
	withTool := taskFor(
		canonMsg{Role: "user", Text: "hello"},
		canonMsg{Role: "assistant", Calls: []canonCall{{ID: "call_1", Name: "read"}}},
		canonMsg{Role: "user", ToolID: "call_1", ToolName: "read", Text: "result"},
	)
	if got := openCodeMessageForTask(withTool, session, nil); got != firstID {
		t.Fatalf("tool result started a new user turn: %q != %q", got, firstID)
	}
	next := taskFor(
		canonMsg{Role: "user", Text: "hello"},
		canonMsg{Role: "assistant", Text: "reply"},
		canonMsg{Role: "user", Text: "next"},
	)
	if got := openCodeMessageForTask(next, session, nil); got == firstID || !openCodeMessagePattern.MatchString(got) {
		t.Fatalf("new user turn reused the previous id: %q", got)
	}
	incoming := http.Header{}
	incoming.Set("x-opencode-request", "msg_0123456789abABCDEFGHIJKLMN")
	official := taskFor(canonMsg{Role: "user", Text: "official"})
	if got := openCodeMessageForTask(official, session, incoming); got != incoming.Get("x-opencode-request") {
		t.Fatalf("official client message id was replaced: %q", got)
	}
}

func TestOpenCodeShellDefinitionUsesLinuxBash(t *testing.T) {
	definitions, err := openCodeToolDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	description := definitions["shell"].Description
	if !strings.Contains(description, "Commands run on Linux using bash") || strings.Contains(description, "PowerShell") {
		t.Fatalf("shell description is not the official Linux/bash one: %q", description)
	}
}
