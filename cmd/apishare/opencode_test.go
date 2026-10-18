package main

import (
	"context"
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

func TestOpenCodeConversationSessions(t *testing.T) {
	modelsTestDB(t)
	makeTask := func(key int64, history string) *relayTask {
		return &relayTask{apiKeyID: key, model: "alias", canon: &canonReq{Model: "alias", InputText: history}}
	}
	headers := http.Header{"X-Session-Id": {"foreign-conversation"}}
	first, err := openCodeSessionForTask(makeTask(1, "first turn"), headers)
	if err != nil || !openCodeSessionPattern.MatchString(first) || first[13:16] != "ffe" {
		t.Fatalf("invalid official session layout: %q %v", first, err)
	}
	for _, key := range []int64{1, 2} {
		session, err := openCodeSessionForTask(makeTask(key, "different follow-up"), headers)
		if err != nil || (session == first) != (key == 1) {
			t.Fatalf("session lost stability or key isolation: key=%d session=%q err=%v", key, session, err)
		}
	}
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			session, err := openCodeSessionForTask(makeTask(1, "concurrent turn"), headers)
			if err != nil || session != first {
				t.Errorf("concurrent mapping changed: %q %v", session, err)
			}
		})
	}
	workers.Wait()
	// Clients without session headers keep the first user turn when history grows.
	var inferred string
	for _, body := range []string{
		`{"model":"alias","messages":[{"role":"user","content":"opening"}]}`,
		`{"model":"alias","messages":[{"role":"user","content":"opening"},{"role":"assistant","content":"reply"},{"role":"user","content":"follow-up"}]}`,
	} {
		cr, err := parseClientRequest(FormatOpenAIChat, []byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		session, err := openCodeSessionForTask(&relayTask{apiKeyID: 1, canon: cr}, nil)
		if err != nil || (inferred != "" && inferred != session) {
			t.Fatalf("history changed inferred session: %q %v", session, err)
		}
		inferred = session
	}
	// A new task reads the stored mapping, without retaining the previous task.
	var stored string
	if err := db.QueryRow(`SELECT session_id FROM opencode_sessions WHERE session_id=?`, first).Scan(&stored); err != nil || stored != first {
		t.Fatalf("mapping was not persisted: %q %v", stored, err)
	}
	canonical := http.Header{}
	canonical.Set("x-opencode-session", first)
	if session, err := openCodeSessionForTask(makeTask(1, "changed"), canonical); err != nil || session != first {
		t.Fatalf("official client's session was replaced: %q %v", session, err)
	}
	for _, name := range []string{"x-opencode-session-id", "x-session-affinity", "conversation-id"} {
		header := http.Header{}
		header.Set(name, "foreign-conversation")
		if session, err := openCodeSessionForTask(makeTask(1, "another turn"), header); err != nil || session != first {
			t.Errorf("session alias %s was ignored: %q %v", name, session, err)
		}
	}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic} {
		body := strings.TrimSuffix(protocolRequests[format], "}") + `,"metadata":{"session_id":"foreign-conversation"}}`
		cr, err := parseClientRequest(format, []byte(body), "")
		if err != nil || cr.SessionID != "foreign-conversation" {
			t.Fatalf("metadata session missing: %+v %v", cr, err)
		}
		if key := sessionKeyFromCanon("/different-format", nil, cr); key != "sid:foreign-conversation" {
			t.Errorf("affinity did not use metadata: %s", key)
		}
		if session, err := openCodeSessionForTask(&relayTask{apiKeyID: 1, canon: cr}, nil); err != nil || session != first {
			t.Errorf("protocol conversion changed session: %s %v", session, err)
		}
	}
	if _, err := db.Exec(`UPDATE opencode_sessions SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	if session, err := openCodeSessionForTask(makeTask(1, "next"), headers); err != nil || session == first {
		t.Fatalf("expired session mapping was retained: %q %v", session, err)
	}
}

func TestOpenCodeRelayAndDiagnosticsHeaders(t *testing.T) {
	modelsTestDB(t)
	versions := newClientVersionService(http.DefaultClient, officialClientVersionSources, nil)
	raw, _ := json.Marshal([]clientVersion{{ID: "opencode", Version: "2.0.30", CheckedAt: time.Now().Add(-time.Minute)}})
	if err := versions.restore(string(raw)); err != nil {
		t.Fatal(err)
	}
	useTestClientVersions(t, versions)
	for _, tc := range []struct {
		protocol string
		format   clientFormat
	}{
		{"chat", FormatOpenAIChat}, {"responses", FormatOpenAIResponse}, {"anthropic", FormatAnthropic},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			count, relaySession := 0, ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				if r.UserAgent() != "opencode/latest/2.0.30/cli" || r.Header.Get("x-opencode-client") != "cli" || r.Header.Get("x-opencode-request") != "" {
					t.Errorf("incorrect OpenCode client headers: %v", r.Header)
				}
				if project := r.Header.Get("x-opencode-project"); project != "global" && !openCodeProjectPattern.MatchString(project) {
					t.Errorf("invalid project: %q", project)
				}
				if r.URL.Path == "/zen/go/v1/models" {
					if r.Header.Get("Authorization") != "Bearer channel-key" {
						t.Error("model discovery lost credentials")
					}
					if !openCodeSessionPattern.MatchString(r.Header.Get("x-opencode-session")) {
						t.Error("model discovery omitted OpenCode session identity")
					}
					fmt.Fprint(w, `{"data":[{"id":"mapped"}]}`)
					return
				}
				if r.URL.Path != "/zen/go"+nativeEndpoint(tc.format, &canonReq{}) {
					t.Errorf("wrong OpenCode endpoint: %s", r.URL.Path)
				}
				session := r.Header.Get("x-opencode-session")
				if !openCodeSessionPattern.MatchString(session) || r.Header.Get("x-opencode-session-id") != session || r.Header.Get("x-session-affinity") != session || r.Header.Get("X-Session-Id") != session {
					t.Errorf("missing or inconsistent sessions: %v", r.Header)
				}
				if count <= 2 {
					if relaySession != "" && relaySession != session {
						t.Error("retry changed the upstream session")
					}
					relaySession = session
				}
				if tc.format == FormatAnthropic {
					if r.Header.Get("x-api-key") != "channel-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") == "" || r.Header.Get("anthropic-beta") == "" {
						t.Error("Messages used incorrect authentication or version headers")
					}
				} else if r.Header.Get("Authorization") != "Bearer channel-key" || r.Header.Get("x-api-key") != "" {
					t.Error("Chat/Responses used incorrect authentication")
				}
				body, _ := io.ReadAll(r.Body)
				var payload map[string]any
				if json.Unmarshal(body, &payload) != nil || payload["model"] != "mapped" {
					t.Errorf("mapped request missing: %s", body)
				}
				if count == 3 && payload["tools"] != nil {
					t.Error("model testing injected tools the client did not declare")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, fixtureSSE(tc.format))
			}))
			defer server.Close()
			up := &Upstream{Type: "opencode", OpenAIProtocol: tc.protocol, APIKey: "channel-key", BaseURL: server.URL + "/zen/go/v1", ModelMap: `{"alias":"mapped"}`, HeaderOverrides: `{"*":true,"x-opencode-request":"legacy-correlation"}`}
			task := fixtureTask(t, FormatOpenAIChat, false)
			task.apiKeyID = 7
			for range 2 {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
				c.Request.Header.Set("x-opencode-session", "foreign-session")
				c.Request.Header.Set("x-opencode-request", "incoming-legacy")
				c.Request.Header.Set("x-opencode-project", "prj_foreign")
				c.Request.Header.Set("User-Agent", "foreign-sdk")
				if _, wrote, err := doRelay(c, up, task); err != nil || !wrote {
					t.Fatalf("OpenCode relay failed: wrote=%v err=%v", wrote, err)
				}
			}
			if _, _, err := testUpstreamModel(context.Background(), up, "alias", "hi", 128, true); err != nil {
				t.Fatal(err)
			}
			if _, err := probeUpstreamModelsWithProxy(context.Background(), up.Type, up.BaseURL, up.APIKey, 0, up.HeaderOverrides); err != nil || count != 4 {
				t.Fatalf("OpenCode discovery failed: count=%d err=%v", count, err)
			}
		})
	}
}

func TestOpenCodeExplicitOverridesAndChildIdentity(t *testing.T) {
	session, _ := newOpenCodeSessionID()
	child, _ := newOpenCodeSessionID()
	incoming := http.Header{}
	incoming.Set("x-opencode-session-id", child)
	up := &Upstream{Type: "opencode", APIKey: "key", HeaderOverrides: `{"User-Agent":"custom-opencode","x-opencode-project":"global"}`}
	req := httptest.NewRequest("POST", "http://upstream.invalid/v1/responses", nil)
	setOpenCodeHeaders(req, up.APIKey, FormatOpenAIResponse, session, incoming)
	if err := up.applyHeaders(req, incoming); err != nil || req.UserAgent() != "custom-opencode" || req.Header.Get("x-opencode-session") != session || req.Header.Get("x-opencode-session-id") != child {
		t.Fatalf("explicit override or child identity was lost: %v %v", req.Header, err)
	}
}
