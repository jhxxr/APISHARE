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
	if _, exists := view["pairs"]; exists {
		t.Fatal("personal view exposes administrator channel selections")
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
	call := func(ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"system","content":"original"},{"role":"user","content":"same conversation"}]}`))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		req.Header.Set("User-Agent", ua)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := call("codex_cli_rs/1.2.3"); w.Code != http.StatusOK || rawCalls.Load() != 1 {
		t.Fatalf("initial normal request = %d %s", w.Code, w.Body.String())
	}
	// Simulate a pre-upgrade config whose user selection excludes the only
	// channel now authorized by the administrator. It must not suppress routing.
	legacy, _ := json.Marshal(userAgentPurgeConfig{Enabled: true, Mode: "strict", PersonaID: "neutral", Clients: []string{"codex"},
		Pairs: []agentPurgePair{{Model: "alias", UpstreamID: primary.ID}}})
	if _, err := db.Exec(`INSERT INTO user_agent_purge_configs(sub,config_json) VALUES(?,?)`, "route-user", string(legacy)); err != nil {
		t.Fatal(err)
	}
	if w := call("codex_cli_rs/1.2.3"); w.Code != http.StatusOK || personaCalls.Load() != 1 || rawCalls.Load() != 1 {
		t.Fatalf("persona did not override unauthorized affinity: %d raw=%d persona=%d %s", w.Code, rawCalls.Load(), personaCalls.Load(), w.Body.String())
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 2 || logs[0].UpstreamID != approved.ID || logs[0].Billing == nil || logs[0].Billing.AgentPurge == nil || logs[1].Billing.AgentPurge != nil {
		t.Fatalf("history does not distinguish persona application: %+v, %v", logs, err)
	}
	if got := logs[0].Billing.AgentPurge; got.Persona != "neutral" || got.Mode != "strict" || got.SHA256 == "" {
		t.Fatalf("incomplete persona receipt: %+v", got)
	}
	approved.Enabled = false
	if err := dbUpdateUpstream(approved); err != nil {
		t.Fatal(err)
	}
	if w := call("codex_cli_rs/1.2.3"); w.Code != http.StatusBadGateway || rawCalls.Load() != 1 || personaCalls.Load() != 1 {
		t.Fatalf("unavailable authorized channel escaped to ordinary routing: %d %s", w.Code, w.Body.String())
	}
	if w := call("Cline/1.2.3"); w.Code != http.StatusOK || rawCalls.Load() != 2 {
		t.Fatalf("unselected client was restricted: %d %s", w.Code, w.Body.String())
	}
	cfg := loadUserAgentPurgeConfig("route-user")
	cfg.Enabled = false
	if err := saveUserAgentPurgeConfig("route-user", cfg); err != nil {
		t.Fatal(err)
	}
	if len(loadUserAgentPurgeConfig("route-user").Pairs) != 0 {
		t.Fatal("saving retained obsolete user channel selections")
	}
	if w := call("codex_cli_rs/1.2.3"); w.Code != http.StatusOK || rawCalls.Load() != 3 {
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
		user:   userAgentPurgeConfig{Enabled: true, Mode: "append", Clients: []string{"codex"}},
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
				userSub: "candidate-user", agentPurgeState: state, nativeOnly: tc.nativeOnly,
				client: &requestClientState{identity: detectRequestClient(http.Header{"User-Agent": {"codex_cli_rs/1.2.3"}})}}
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
	if err := saveUserAgentPurgeConfig("policy-ws-user", userAgentPurgeConfig{Enabled: true, Mode: "strict", Clients: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	ctx, conn := responsesWSDial(t, gateway, key, nil)
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
	ctx2, conn2 := responsesWSDial(t, gateway, key, nil)
	responsesWSWrite(t, ctx2, conn2, event)
	responsesWSRead(t, ctx2, conn2)
	if got := <-received; got != "preferred" {
		t.Fatalf("reconnected WebSocket did not follow new policy: %s", got)
	}
}
