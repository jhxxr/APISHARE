package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"apishare/internal/agentpurge"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func TestAgentPurgeEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, ua, client, model string
		upstream                int64
		policy, enabled, want   bool
	}{
		{"selected", "codex_cli_rs/1.2.3", "codex", "alias", 7, true, true, true},
		{"different client", "Cline/1.2.3", "codex", "alias", 7, true, true, false},
		{"different upstream", "codex_cli_rs/1.2.3", "codex", "alias", 8, true, true, false},
		{"different model", "codex_cli_rs/1.2.3", "codex", "other", 7, true, true, false},
		{"case-sensitive model", "codex_cli_rs/1.2.3", "codex", "ALIAS", 7, true, true, false},
		{"thinking variant", "codex_cli_rs/1.2.3", "codex", "alias(high)", 7, true, true, true},
		{"non-site upstream", "codex_cli_rs/1.2.3", "codex", "alias", -7, true, true, false},
		{"admin off", "codex_cli_rs/1.2.3", "codex", "alias", 7, false, true, false},
		{"user off", "codex_cli_rs/1.2.3", "codex", "alias", 7, true, false, false},
		{"unknown", "Mozilla/5.0", "codex", "alias", 7, true, true, false},
		{"unknown SDK selected", "OpenAI/Python 2.1.0", "sdk:openai_python", "alias", 7, true, true, true},
		{"SDK is not Codex", "OpenAI/Python 2.1.0", "codex", "alias", 7, true, true, false},
		{"declared Claude family", "claude-cli/2.1.20 (external, cli)", "claude_code", "alias", 7, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs := []agentPurgePair{{Model: "alias", UpstreamID: 7}}
			state := &agentPurgeRequestState{
				policy: agentPurgePolicy{Enabled: tc.policy, Pairs: pairs},
				user:   userAgentPurgeConfig{Enabled: tc.enabled, Mode: "append", Clients: []string{tc.client}},
			}
			task := &relayTask{model: tc.model, client: &requestClientState{identity: detectRequestClient(http.Header{"User-Agent": {tc.ua}})}}
			if got := shouldApplyAgentPurge(state, "personal-user", &Upstream{ID: tc.upstream}, task); got != tc.want {
				t.Fatalf("eligibility = %t, want %t", got, tc.want)
			}
			task.client.identity.Conflict = true
			if shouldApplyAgentPurge(state, "personal-user", &Upstream{ID: tc.upstream}, task) {
				t.Fatal("conflicting identity matched")
			}
			task.client.identity.Conflict = false
			task.nativeOnly = true
			if shouldApplyAgentPurge(state, "personal-user", &Upstream{ID: tc.upstream}, task) {
				t.Fatal("native-only operation such as embeddings matched")
			}
			if shouldApplyAgentPurge(state, "", &Upstream{ID: tc.upstream}, task) {
				t.Fatal("ordinary key matched personal preferences")
			}
		})
	}
}

func TestAgentPurgeRelayAllProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, in := range protocolFormats {
		for _, out := range protocolFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", in, out, stream), func(t *testing.T) {
					personaID := agentpurge.PersonaID
					if out == FormatOpenAIChat {
						personaID = "eni-deepseek-flash"
					}
					persona, _ := agentpurge.Lookup(personaID)
					var received []byte
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						received, _ = io.ReadAll(r.Body)
						var fields map[string]json.RawMessage
						_ = json.Unmarshal(received, &fields)
						parsed, err := parseClientRequest(out, received, "mapped")
						if err != nil || parsed == nil || strings.Count(parsed.System, persona.Text) != 1 {
							t.Errorf("missing, wrong or repeated selected persona %s", personaID)
						}
						if len(fields["tools"]) == 0 || !bytes.Contains(received, []byte("weather")) || !bytes.Contains(received, []byte("Paris")) {
							t.Error("tools or conversation history were removed")
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, fixtureSSE(out))
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, protocolResponses[out])
						}
					}))
					defer server.Close()
					up := fixtureUpstream(out, server.URL)
					up.ID, up.Enabled, up.Weight = 7, true, 1
					task := fixtureTask(t, in, stream)
					task.userSub = "personal-user"
					task.client = &requestClientState{identity: detectRequestClient(http.Header{"User-Agent": {"codex_cli_rs/1.2.3"}})}
					pair := []agentPurgePair{{Model: "alias", UpstreamID: 7}}
					task.agentPurgeState = &agentPurgeRequestState{
						policy: agentPurgePolicy{Enabled: true, Pairs: pair},
						user:   userAgentPurgeConfig{Enabled: true, Mode: "append", PersonaID: personaID, Clients: []string{"codex"}},
					}
					original := string(task.body)
					for range 2 { // The same task retried must not accumulate prompt text.
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest("POST", nativeEndpoint(in, task.canon), nil)
						info, wrote, err := doRelay(c, up, task)
						if err != nil || !wrote || info.Status != 200 || info.agentPurge == nil || len(received) == 0 {
							t.Fatalf("relay: wrote=%t, info=%+v err=%v body=%s", wrote, info, err, rec.Body.String())
						}
						if info.agentPurge.Persona != personaID || info.agentPurge.SHA256 != persona.SHA256 {
							t.Fatal("receipt attributes the default instead of the selected template")
						}
					}
					if string(task.body) != original || strings.Contains(task.canon.System, persona.Text) {
						t.Fatal("rewriting mutated original request or routing state")
					}
				})
			}
		}
	}
}

func agentPurgeTestUser(t *testing.T, key *ApiKey, sub string) string {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO xauth_users(sub,name,key_id,gift_claimed) VALUES(?,?,?,1)`, sub, sub, key.ID); err != nil {
		t.Fatal(err)
	}
	session, err := createXAuthUserSession(sub, key.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestAgentPurgeRoutesPermissionsAndPersistence(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "agent-purge-test-secret"); err != nil {
		t.Fatal(err)
	}
	up := &Upstream{Name: "private upstream", Type: "openai", OpenAIProtocol: "chat", Models: "alias,other", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	key := &ApiKey{Name: "personal", Key: "sk-purge-personal", Models: "alias", Enabled: true, QPS: 100, Concurrency: 5}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	session := agentPurgeTestUser(t, key, "purge-user")
	router := newRouter()
	requestUser := func(method, body string, withSession, csrf bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/user/agent-purge", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withSession {
			req.AddCookie(&http.Cookie{Name: xauthUserSessionCookie, Value: session})
		}
		if csrf {
			req.Header.Set(xauthUserRequestHeader, "user-console")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct{ method, path string }{{"GET", "agent-purge/policy"}, {"PUT", "agent-purge/policy"}, {"GET", "agent-purge/license"}} {
		if w := proxyAdminRequest(router, tc.method, tc.path, `{}`, false); w.Code != 401 {
			t.Fatalf("admin authentication: %s %d", tc.path, w.Code)
		}
	}
	if w := requestUser("GET", "", false, false); w.Code != 401 {
		t.Fatalf("anonymous read = %d", w.Code)
	}
	if w := requestUser("PUT", `{}`, true, false); w.Code != 403 {
		t.Fatalf("missing CSRF marker = %d", w.Code)
	}
	if w := requestUser("GET", "", true, false); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("default unexpectedly enabled: %s", w.Body.String())
	} else {
		var view struct {
			PersonaID  string `json:"persona_id"`
			PromptName string `json:"prompt_name"`
			Prompt     string `json:"prompt"`
			Source     struct {
				Repository string `json:"repository"`
				Revision   string `json:"revision"`
			} `json:"prompt_source"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.PersonaID != agentpurge.PersonaID || view.PromptName != agentpurge.PersonaName ||
			view.Prompt != agentpurge.Prompt() || view.Source.Repository != agentpurge.SourceRepository ||
			view.Source.Revision != agentpurge.SourceRevision {
			t.Fatal("personal console does not expose the original bundled project template and provenance")
		}
	}
	body := fmt.Sprintf(`{"enabled":true,"revision":0,"pairs":[{"model":"alias","upstream_id":%d}]}`, up.ID)
	if w := proxyAdminRequest(router, "PUT", "agent-purge/policy", body, true); w.Code != 200 {
		t.Fatalf("save policy = %d %s", w.Code, w.Body.String())
	}
	if w := proxyAdminRequest(router, "PUT", "agent-purge/policy", body, true); w.Code != 409 {
		t.Fatalf("stale admin save = %d", w.Code)
	}
	body = fmt.Sprintf(`{"enabled":true,"mode":"strict","revision":1,"clients":["codex"],"pairs":[{"model":"other","upstream_id":%d}]}`, up.ID)
	if w := requestUser("PUT", body, true, true); w.Code != 403 {
		t.Fatalf("forged pair or model accepted: %d %s", w.Code, w.Body.String())
	}
	body = `{"enabled":true,"mode":"strict","revision":1,"clients":["codex"]}`
	if w := requestUser("PUT", body, true, true); w.Code != 200 {
		t.Fatalf("save user = %d %s", w.Code, w.Body.String())
	}
	stored := loadUserAgentPurgeConfig("purge-user")
	if !stored.Enabled || stored.Mode != "strict" || len(stored.Pairs) != 0 || len(stored.Clients) != 1 {
		t.Fatalf("user preferences not persisted: %+v", stored)
	}
	selectedBody := strings.TrimSuffix(body, "}") + `,"persona_id":"eni-deepseek-flash"}`
	if w := requestUser("PUT", selectedBody, true, true); w.Code != 200 {
		t.Fatalf("template selection failed: %d %s", w.Code, w.Body.String())
	}
	if selected := loadUserAgentPurgeConfig("purge-user"); selected.PersonaID != "eni-deepseek-flash" {
		t.Fatal("selected original template was not persisted")
	}
	if w := requestUser("PUT", strings.Replace(selectedBody, "eni-deepseek-flash", "../unknown", 1), true, true); w.Code != 400 {
		t.Fatalf("unknown/path-like template accepted: %d", w.Code)
	}
	if w := requestUser("GET", "", true, false); w.Code != 200 {
		t.Fatalf("template catalog read failed: %d", w.Code)
	} else {
		var view struct {
			PersonaID string               `json:"persona_id"`
			Prompt    string               `json:"prompt"`
			Personas  []agentpurge.Persona `json:"personas"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		selected, _ := agentpurge.Lookup("eni-deepseek-flash")
		if len(view.Personas) != 21 || view.PersonaID != selected.ID || view.Prompt != selected.Text {
			t.Fatal("user settings did not expose all original templates and the selected body")
		}
	}
	other := loadUserAgentPurgeConfig("different-user")
	if other.Enabled || len(other.Pairs) != 0 || len(other.Clients) != 0 {
		t.Fatal("user preference leaked across identities")
	}
	if w := requestUser("PUT", strings.Replace(body, `"codex"`, `"unknown"`, 1), true, true); w.Code != 400 {
		t.Fatalf("unsupported client = %d", w.Code)
	}
	if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{}}); err != nil {
		t.Fatal(err)
	}
	if w := requestUser("PUT", body, true, true); w.Code != 409 {
		t.Fatalf("stale user authorization = %d", w.Code)
	}
	if w := requestUser("GET", "", true, false); strings.Contains(w.Body.String(), "private upstream") {
		t.Fatal("revoked upstream still exposed in user settings")
	}
}

func TestAgentPurgeResponsesWSRefreshesPreferences(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	agentPurgeTestUser(t, key, "purge-ws-user")
	received := make(chan []byte, 3)
	fixture := responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		for i := range 3 {
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			received <- body
			responsesWSWrite(t, ctx, conn, responsesWSTerminal(fmt.Sprintf("purge-%d", i), "", 10, 3))
		}
		_, _, _ = conn.Read(ctx)
	})
	up := responsesWSTestUpstream(t, fixture.URL, "openai", 0)
	pairs := []agentPurgePair{{Model: "ws-alias", UpstreamID: up.ID}}
	if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: pairs}); err != nil {
		t.Fatal(err)
	}
	cfg := userAgentPurgeConfig{Enabled: true, Mode: "append", Pairs: pairs, Clients: []string{"codex"}}
	if err := saveUserAgentPurgeConfig("purge-ws-user", cfg); err != nil {
		t.Fatal(err)
	}
	ctx, conn := responsesWSDial(t, gateway, key, nil)
	for i := range 3 {
		responsesWSWrite(t, ctx, conn, `{"type":"response.create","model":"ws-alias","instructions":"original","input":"hello"}`)
		responsesWSRead(t, ctx, conn)
		body := <-received
		if bytes.Contains(body, []byte("系统提示词 · 起步模板")) != (i == 0) {
			t.Fatalf("WebSocket preferences not refreshed for turn %d: %s", i, body)
		}
		var forwarded map[string]json.RawMessage
		_ = json.Unmarshal(body, &forwarded)
		var instructions string
		_ = json.Unmarshal(forwarded["instructions"], &instructions)
		selected, _ := agentpurge.Lookup("eni-deepseek-flash")
		if strings.Contains(instructions, selected.Text) != (i == 1) {
			t.Fatalf("WebSocket did not refresh selected template for turn %d", i)
		}
		if i == 0 {
			cfg.PersonaID = "eni-deepseek-flash"
		} else {
			cfg.Enabled = false
		}
		if err := saveUserAgentPurgeConfig("purge-ws-user", cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentPurgeFailoverAndEstimatedBilling(t *testing.T) {
	modelsTestDB(t)
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Retry = 1
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	key := &ApiKey{Name: "personal retry", Key: "sk-purge-retry", Models: "alias", Enabled: true, QPS: 1000, Concurrency: 5}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	agentPurgeTestUser(t, key, "purge-retry-user")
	var first, second []byte
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, _ = io.ReadAll(r.Body)
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"message":"retry"}}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer b.Close()
	ups := []*Upstream{
		{Name: "primary", Type: "openai", OpenAIProtocol: "chat", BaseURL: a.URL, Models: "alias", Enabled: true, Weight: 1},
		{Name: "fallback", Type: "openai", OpenAIProtocol: "chat", BaseURL: b.URL, Models: "alias", Enabled: true, Weight: 1, Priority: 1},
	}
	for _, up := range ups {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	pairs := []agentPurgePair{{Model: "alias", UpstreamID: ups[0].ID}}
	router := newRouter()
	for iteration := range 2 {
		resetUpstreamHealthForTest()
		first, second = nil, nil
		if iteration == 1 {
			pairs = append(pairs, agentPurgePair{Model: "alias", UpstreamID: ups[1].ID})
		}
		if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: pairs}); err != nil {
			t.Fatal(err)
		}
		if err := saveUserAgentPurgeConfig("purge-retry-user", userAgentPurgeConfig{Enabled: true, Mode: "append", Clients: []string{"codex"}}); err != nil {
			t.Fatal(err)
		}
		body := `{"model":"alias","messages":[{"role":"system","content":"original"},{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("User-Agent", "codex_cli_rs/1.2.3")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		wantStatus, wantUpstream := http.StatusBadGateway, ups[0].ID
		if iteration == 1 {
			wantStatus, wantUpstream = http.StatusOK, ups[1].ID
		}
		if response.Code != wantStatus {
			t.Fatalf("production failover = %d %s", response.Code, response.Body.String())
		}
		if bytes.Count(first, []byte("系统提示词 · 起步模板")) != 1 || bytes.Contains(second, []byte("系统提示词 · 起步模板")) != (iteration == 1) {
			t.Fatalf("rewrite crossed failover boundaries: first=%s second=%s", first, second)
		}
		logs, err := dbListLogs(10, 0)
		if err != nil || len(logs) != iteration+1 || logs[0].UpstreamID != wantUpstream || logs[0].Billing == nil {
			t.Fatalf("incorrect final receipt: %+v %v", logs, err)
		}
		if logs[0].Billing.AgentPurge == nil || logs[0].Status != wantStatus {
			t.Fatal("receipt does not match the rewritten final attempt and its outcome")
		}
		if iteration == 0 && len(second) != 0 {
			t.Fatal("retry reached an unauthorized fallback")
		}
		if iteration == 1 {
			original, _ := parseClientRequest(FormatOpenAIChat, []byte(body), "")
			if logs[0].PromptTokens <= estimatePromptTokens(&relayTask{model: "alias", canon: original}) {
				t.Fatal("fallback token estimate excluded the injected persona")
			}
		}
	}
}
