package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"apishare/internal/agentpurge"
	"github.com/coder/websocket"
)

func TestAgentPurgeUserViewListsModelsWithoutChannelSelection(t *testing.T) {
	modelsTestDB(t)
	first := &Upstream{Name: "private first", Type: "openai", OpenAIProtocol: "chat", Models: "alias,denied", Enabled: true, Weight: 1}
	second := &Upstream{Name: "private second", Type: "openai", OpenAIProtocol: "chat", Models: "alias,disabled", Enabled: true, Weight: 1}
	disabled := &Upstream{Name: "private disabled", Type: "openai", Models: "disabled", Enabled: false, Weight: 1}
	for _, up := range []*Upstream{first, second, disabled} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	policy := agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{
		{Model: "alias", UpstreamID: first.ID}, {Model: "alias", UpstreamID: second.ID},
		{Model: "denied", UpstreamID: first.ID}, {Model: "disabled", UpstreamID: disabled.ID},
		{Model: "removed", UpstreamID: first.ID}, {Model: "removed-channel", UpstreamID: 99999},
	}}
	if err := saveAgentPurgePolicy(policy); err != nil {
		t.Fatal(err)
	}
	key := ApiKey{Models: "alias,disabled,removed,removed-channel"}
	view, err := buildUserAgentPurgeView("view-user", key)
	if err != nil || !reflect.DeepEqual(view["models"], []string{"alias"}) {
		t.Fatalf("usable model view = %v, err=%v", view["models"], err)
	}
	for _, field := range []string{"pairs", "clients", "selected_clients"} {
		if _, exists := view[field]; exists {
			t.Fatalf("personal view exposes obsolete selection field %s", field)
		}
	}
	key.Models = "denied"
	view, err = buildUserAgentPurgeView("view-user", key)
	if err != nil || !reflect.DeepEqual(view["models"], []string{"denied"}) {
		t.Fatalf("key permission refresh = %v, err=%v", view["models"], err)
	}
	policy.Enabled = false
	if err := saveAgentPurgePolicy(policy); err != nil {
		t.Fatal(err)
	}
	view, err = buildUserAgentPurgeView("view-user", key)
	if err != nil || !reflect.DeepEqual(view["models"], []string{}) {
		t.Fatalf("disabled policy model view = %v, err=%v", view["models"], err)
	}
}

func TestAgentPurgeRoutesPastUnauthorizedAffinityAndIgnoresLegacySelection(t *testing.T) {
	modelsTestDB(t)
	key := &ApiKey{Name: "persona routing", Key: "sk-persona-route", Models: "alias", Enabled: true, QPS: 1000, Concurrency: 5}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	agentPurgeTestUser(t, key, "route-user")
	var rawCalls, personaCalls atomic.Int32
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(agentpurge.Prompt())) {
			t.Error("unauthorized channel received the persona")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
	}))
	defer raw.Close()
	persona := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		personaCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		parsed, err := parseClientRequest(FormatOpenAIChat, body, "")
		if err != nil || parsed.Model != "mapped-persona" || parsed.System != agentpurge.Prompt() {
			t.Errorf("authorized channel did not receive the mapped model and strict persona: %s, %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
	}))
	defer persona.Close()
	primary := &Upstream{Name: "unapproved preferred", Type: "openai", OpenAIProtocol: "chat", BaseURL: raw.URL, Models: "alias", Enabled: true, Weight: 1}
	approved := &Upstream{Name: "approved", Type: "openai", OpenAIProtocol: "chat", BaseURL: persona.URL, Models: "alias", ModelMap: `{"alias":"mapped-persona"}`, Enabled: true, Weight: 1, Priority: 1}
	for _, up := range []*Upstream{primary, approved} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{{Model: "alias", UpstreamID: approved.ID}}}); err != nil {
		t.Fatal(err)
	}
	router := newRouter()
	call := func(ua string, extraHeaders ...http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"system","content":"original"},{"role":"user","content":"same conversation"}]}`))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		for _, headers := range extraHeaders {
			for name, values := range headers {
				req.Header[name] = values
			}
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call("codex_cli_rs/1.2.3"); w.Code != http.StatusOK || rawCalls.Load() != 1 {
		t.Fatalf("initial normal request = %d %s", w.Code, w.Body.String())
	}
	// Simulate a pre-upgrade config whose user selection excludes the only
	// channel now authorized by the administrator and only enables Codex. Neither
	// the channel selection nor the client selection may suppress the role.
	legacy, _ := json.Marshal(userAgentPurgeConfig{Enabled: true, Mode: "strict", PersonaID: "neutral", Clients: []string{"codex"},
		Pairs: []agentPurgePair{{Model: "alias", UpstreamID: primary.ID}}})
	if _, err := db.Exec(`INSERT INTO user_agent_purge_configs(sub,config_json) VALUES(?,?)`, "route-user", string(legacy)); err != nil {
		t.Fatal(err)
	}
	userAgents := []string{"claude-cli/2.1.273 (external, cli)", "", "custom-application/1.0", "Cline/1.2.3", "OpenAI/Python 2.1.0"}
	for i, ua := range userAgents {
		if w := call(ua); w.Code != http.StatusOK || personaCalls.Load() != int32(i+1) || rawCalls.Load() != 1 {
			t.Fatalf("User-Agent %q changed persona routing: %d raw=%d persona=%d %s", ua, w.Code, rawCalls.Load(), personaCalls.Load(), w.Body.String())
		}
	}
	if w := call("codex_cli_rs/1.2.3", http.Header{"X-Title": {"Cline"}, "HTTP-Referer": {"https://cline.bot"}}); w.Code != http.StatusOK {
		t.Fatalf("conflicting client identity prevented persona routing: %d %s", w.Code, w.Body.String())
	}
	wantPersonaCalls := int32(len(userAgents) + 1)
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != int(wantPersonaCalls)+1 || personaCalls.Load() != wantPersonaCalls || rawCalls.Load() != 1 {
		t.Fatalf("history does not distinguish persona application: %+v, %v", logs, err)
	}
	for _, entry := range logs[:len(logs)-1] {
		if entry.UpstreamID != approved.ID || entry.Billing == nil || entry.Billing.AgentPurge == nil {
			t.Fatalf("missing authorized upstream and persona receipt: %+v", entry)
		}
		if got := entry.Billing.AgentPurge; got.Persona != "neutral" || got.Mode != "strict" || got.SHA256 == "" {
			t.Fatalf("incomplete persona receipt: %+v", got)
		}
	}
	if logs[len(logs)-1].Billing.AgentPurge != nil {
		t.Fatal("ordinary request received a persona receipt")
	}
	key.Models = "other"
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	if w := call("claude-cli/2.1.273 (external, cli)"); w.Code != http.StatusForbidden {
		t.Fatalf("enabled role bypassed key model permissions: %d %s", w.Code, w.Body.String())
	}
	key.Models = "alias"
	if err := dbUpdateKey(key); err != nil {
		t.Fatal(err)
	}
	approved.Enabled = false
	if err := dbUpdateUpstream(approved); err != nil {
		t.Fatal(err)
	}
	for _, ua := range userAgents {
		if w := call(ua); w.Code != http.StatusBadGateway || rawCalls.Load() != 1 || personaCalls.Load() != wantPersonaCalls {
			t.Fatalf("User-Agent %q escaped unavailable authorized channels: %d %s", ua, w.Code, w.Body.String())
		}
	}
	cfg := loadUserAgentPurgeConfig("route-user")
	cfg.Enabled = false
	if err := saveUserAgentPurgeConfig("route-user", cfg); err != nil {
		t.Fatal(err)
	}
	if cfg := loadUserAgentPurgeConfig("route-user"); len(cfg.Pairs) != 0 || len(cfg.Clients) != 0 {
		t.Fatal("saving retained obsolete user channel or client selections")
	}
	if w := call("claude-cli/2.1.273 (external, cli)"); w.Code != http.StatusOK || rawCalls.Load() != 2 {
		t.Fatalf("disabled persona was restricted: %d %s", w.Code, w.Body.String())
	}
}

func TestAgentPurgeCandidateRoutingAndSitePermissions(t *testing.T) {
	modelsTestDB(t)
	preferred := &Upstream{Name: "preferred", Type: "openai", Models: "alias,other", Enabled: true, Weight: 1}
	approved := &Upstream{Name: "approved", Type: "openai", Models: "alias", Enabled: true, Weight: 1, Priority: 1}
	for _, up := range []*Upstream{preferred, approved} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	state := &agentPurgeRequestState{
		policy: agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{{Model: "alias", UpstreamID: approved.ID}}},
		user:   userAgentPurgeConfig{Enabled: true, Mode: "append"},
	}
	for _, tc := range []struct {
		name, requested, candidate string
		nativeOnly                 bool
		want                       int64
	}{
		{"direct", "alias", "", false, approved.ID},
		{"explicit model options", "alias", "alias(high)", false, approved.ID},
		{"unlisted model", "other", "", false, preferred.ID},
		{"native-only endpoint", "alias", "", true, preferred.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &relayTask{model: tc.requested, requestedModel: tc.candidate, format: FormatOpenAIChat,
				userSub: "candidate-user", agentPurgeState: state, nativeOnly: tc.nativeOnly}
			up, err := task.pickUpstream("", map[int64]bool{}, map[string]bool{})
			if err != nil || up == nil || up.ID != tc.want {
				t.Fatalf("selected = %+v, err=%v; want upstream %d", up, err, tc.want)
			}

		})
	}
}

func TestAgentPurgeResponsesWSRestrictsRoutingAndRechecksPolicy(t *testing.T) {
	key, _, gateway := responsesWSTestGateway(t)
	agentPurgeTestUser(t, key, "policy-ws-user")
	received := make(chan string, 4)
	fixture := func(name string) *httptest.Server {
		return responsesWSFixture(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
			for i := 0; ; i++ {
				_, body, err := conn.Read(ctx)
				if err != nil {
					return
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(body, &fields)
				var instructions string
				_ = json.Unmarshal(fields["instructions"], &instructions)
				if !strings.Contains(instructions, agentpurge.Prompt()) {
					t.Error("WebSocket forwarded without the selected persona")
				}
				received <- name
				responsesWSWrite(t, ctx, conn, responsesWSTerminal(fmt.Sprintf("%s-%d", name, i), "", 10, 3))
			}
		})
	}
	preferred := responsesWSTestUpstream(t, fixture("preferred").URL, "openai", 0)
	approved := responsesWSTestUpstream(t, fixture("approved").URL, "openai", 1)
	if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{{Model: "ws-alias", UpstreamID: approved.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := saveUserAgentPurgeConfig("policy-ws-user", userAgentPurgeConfig{Enabled: true, Mode: "strict"}); err != nil {
		t.Fatal(err)
	}
	dial := func(ua string) (context.Context, *websocket.Conn) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		t.Cleanup(cancel)
		headers := http.Header{"Authorization": {"Bearer " + key.Key}, "User-Agent": {ua}}
		conn, _, err := websocket.Dial(ctx, gateway.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		return ctx, conn
	}
	ctx, conn := dial("claude-cli/2.1.273 (external, cli)")
	const event = `{"type":"response.create","model":"ws-alias","input":"hello"}`
	responsesWSWrite(t, ctx, conn, event)
	responsesWSRead(t, ctx, conn)
	if got := <-received; got != "approved" {
		t.Fatalf("WebSocket selected unauthorized upstream %s", got)
	}
	if err := saveAgentPurgePolicy(agentPurgePolicy{Enabled: true, Pairs: []agentPurgePair{{Model: "ws-alias", UpstreamID: preferred.ID}}}); err != nil {
		t.Fatal(err)
	}
	responsesWSWrite(t, ctx, conn, event)
	var denied struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(responsesWSRead(t, ctx, conn), &denied); err != nil || denied.Type != "error" || denied.Status != http.StatusServiceUnavailable {
		t.Fatalf("revoked connected channel did not request reconnect: %+v %v", denied, err)
	}
	_ = conn.CloseNow()
	ctx2, conn2 := dial("custom-application/1.0")
	responsesWSWrite(t, ctx2, conn2, event)
	responsesWSRead(t, ctx2, conn2)
	if got := <-received; got != "preferred" {
		t.Fatalf("reconnected WebSocket did not follow new policy: %s", got)
	}
}
