package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"apishare/internal/cliproxy"
)

func TestPoolDuplicateImportPreservesAccountSettings(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	w := proxyAdminRequest(router, "POST", "upstreams", `{"name":"pool","type":"cliproxy"}`, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &saved)
	path := fmt.Sprintf("upstreams/%d/cliproxy/credentials/import", saved.ID)
	original := `{"name":"original.json","credential":{"type":"codex","email":"same@example.invalid","access_token":"old","disabled":true,"label":"Operator label","weight":4}}`
	if w = proxyAdminRequest(router, "POST", path, original, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	proxy := testSavedProxy(t, "http://127.0.0.1:13218", "", "")
	if w = proxyAdminRequest(router, "PATCH", fmt.Sprintf("upstreams/%d/cliproxy/credentials/proxy", saved.ID), fmt.Sprintf(`{"name":"original.json","proxy_id":%d}`, proxy.ID), true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	payload := `{"name":"new-name.json","credential":{"type":"codex","email":"same@example.invalid","access_token":"new","disabled":false,"label":"Incoming label","weight":1},"proxy_id":0}`
	if w = proxyAdminRequest(router, "POST", path, payload, true); w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"credential_exists"`) {
		t.Fatalf("no duplicate confirmation: %d %s", w.Code, w.Body.String())
	}
	manager, _ := cliProxyPool(saved.ID)
	files, _ := manager.ReadCredentials()
	if len(files) != 1 || !strings.Contains(string(files["original.json"]), `"old"`) {
		t.Fatal("unconfirmed import overwrote credentials")
	}
	payload = strings.Replace(payload, `"name":"new-name.json"`, `"replace":true,"name":"new-name.json"`, 1)
	if w = proxyAdminRequest(router, "POST", path, payload, true); w.Code != 200 {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body.String())
	}
	files, _ = manager.ReadCredentials()
	var doc map[string]any
	json.Unmarshal(files["original.json"], &doc)
	if len(files) != 1 || doc["access_token"] != "new" || doc["disabled"] != true || doc["label"] != "Operator label" || doc["weight"] != float64(4) || doc["proxy_url"] != "http://127.0.0.1:13218" || accountProxyID(saved.ID, "original.json") != proxy.ID {
		t.Fatalf("replacement lost original operator settings: %+v", doc)
	}
	if w = proxyAdminRequest(router, "POST", path, `{"name":"original.json","replace":true,"credential":{"type":"codex","email":"another@example.invalid","access_token":"other"}}`, true); w.Code != 400 {
		t.Fatal("different account overwrote the same filename")
	}
}

func TestOAuthFinishesInBackgroundAndCanBeRestored(t *testing.T) {
	router := testProxyAdminRouter(t)
	token := makeAdminToken()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, adminPath+"/api/"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	withTestCLIProxyPools(t)
	w := request("POST", "upstreams", `{"name":"pool","type":"cliproxy"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &saved)
	var authorized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if authorized.Load() {
			fmt.Fprint(w, `{"status":"ok"}`)
		} else {
			fmt.Fprint(w, `{"status":"wait"}`)
		}
	}))
	defer server.Close()
	_, portRaw, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portRaw)
	worker, err := cliproxy.NewManager(filepath.Join(cliProxyPools.root, "login-recovery-fixture"), port)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"type":"codex","email":"recovered@example.invalid","access_token":"PRIVATE-TOKEN","disabled":true}`)
	if err := worker.SeedCredentials(map[string]json.RawMessage{"recovered.json": raw}); err != nil {
		t.Fatal(err)
	}
	const state = "recovery-fixture"
	coreLoginSessions.Lock()
	coreLoginSessions.items[state] = coreLoginSession{
		owner: sha256.Sum256([]byte(token)), provider: "codex", poolID: saved.ID, proxyID: -1,
		expires: time.Now().Add(time.Minute), worker: worker, status: "wait", op: &sync.Mutex{},
		login: cliproxy.Login{State: state, URL: "https://auth.example/authorize", Flow: "device", UserCode: "ABC-123"},
	}
	coreLoginSessions.Unlock()
	t.Cleanup(func() { discardCoreLogin(state) })
	path := fmt.Sprintf("upstreams/%d/cliproxy/", saved.ID)
	if w = request("GET", path+"oauth/sessions", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "ABC-123") || strings.Contains(w.Body.String(), "PRIVATE-TOKEN") {
		t.Fatalf("cannot restore authorization view: %d %s", w.Code, w.Body.String())
	}
	authorized.Store(true)
	go watchCoreLogin(state)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		coreLoginSessions.Lock()
		session := coreLoginSessions.items[state]
		coreLoginSessions.Unlock()
		if session.committed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	coreLoginSessions.Lock()
	session := coreLoginSessions.items[state]
	coreLoginSessions.Unlock()
	if !session.committed {
		t.Fatalf("browser-free authorization did not complete: %+v", session)
	}
	manager, _ := cliProxyPool(saved.ID)
	files, _ := manager.ReadCredentials()
	if !strings.Contains(string(files["recovered.json"]), "PRIVATE-TOKEN") {
		t.Fatal("background authorization did not import the credential")
	}
	if w = request("GET", path+"oauth/sessions", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatal("completed authorization cannot be restored")
	}
	replacement, err := cliproxy.NewManager(filepath.Join(cliProxyPools.root, "login-replacement-fixture"), port)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.SeedCredentials(map[string]json.RawMessage{"renamed.json": json.RawMessage(`{"type":"codex","email":"recovered@example.invalid","access_token":"UPDATED-TOKEN","disabled":false}`)}); err != nil {
		t.Fatal(err)
	}
	const second = "replacement-session-fixture"
	coreLoginSessions.Lock()
	coreLoginSessions.items[second] = coreLoginSession{owner: sha256.Sum256([]byte(token)), provider: "codex", poolID: saved.ID, proxyID: 0,
		expires: time.Now().Add(time.Minute), worker: replacement, status: "wait", op: &sync.Mutex{}, login: cliproxy.Login{State: second, URL: "https://auth.example/authorize"}}
	coreLoginSessions.Unlock()
	t.Cleanup(func() { discardCoreLogin(second) })
	go watchCoreLogin(second)
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		coreLoginSessions.Lock()
		next := coreLoginSessions.items[second]
		coreLoginSessions.Unlock()
		if next.status == "needs_confirmation" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	coreLoginSessions.Lock()
	next := coreLoginSessions.items[second]
	coreLoginSessions.Unlock()
	if next.status != "needs_confirmation" || next.committed {
		t.Fatalf("duplicate OAuth did not wait for confirmation: %+v", next)
	}
	files, _ = manager.ReadCredentials()
	if !strings.Contains(string(files["recovered.json"]), "PRIVATE-TOKEN") {
		t.Fatal("background OAuth overwrote an existing account before confirmation")
	}
	if w = request("POST", path+"oauth/confirm", fmt.Sprintf(`{"state":%q}`, second)); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("confirm OAuth: %d %s", w.Code, w.Body.String())
	}
	files, _ = manager.ReadCredentials()
	var updated map[string]any
	json.Unmarshal(files["recovered.json"], &updated)
	if len(files) != 1 || updated["access_token"] != "UPDATED-TOKEN" || updated["disabled"] != true || accountProxyID(saved.ID, "recovered.json") != -1 {
		t.Fatalf("reauthorization changed account settings or duplicated the credential: %+v", updated)
	}
}

func TestDuplicateAuthorizationWaitsForConfirmation(t *testing.T) {
	root := t.TempDir()
	port := 18317
	manager, err := cliproxy.NewManager(root, port)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SeedCredentials(map[string]json.RawMessage{
		"one.json": json.RawMessage(`{"type":"codex","email":"same@example.invalid","disabled":true,"websockets":true,"access_token":"old"}`),
	}); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"type": "codex", "email": "same@example.invalid", "access_token": "new"}
	_, _, err = prepareCredentialReplacement(manager, "generated-name.json", doc, false)
	var duplicate *duplicatePoolCredential
	if !errors.As(err, &duplicate) || duplicate.Name != "one.json" {
		t.Fatalf("OAuth duplicate was not held for confirmation: %v", err)
	}
	name, replacing, err := prepareCredentialReplacement(manager, "generated-name.json", doc, true)
	if err != nil || !replacing || name != "one.json" || doc["disabled"] != true || doc["websockets"] != true {
		t.Fatalf("OAuth confirmation lost account settings: %s %v %v %+v", name, replacing, err, doc)
	}
}

func TestDuplicateDetectionKeepsDomainsAndOrganizationsSeparate(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		old, new map[string]any
	}{
		{"kimi-sites", map[string]any{"type": "kimi", "domain": "kimi.com", "email": "same@example.invalid"}, map[string]any{"type": "kimi-ai", "domain": "kimi.ai", "email": "same@example.invalid"}},
		{"claude-organizations", map[string]any{"type": "claude", "account_uuid": "same-account", "organization_uuid": "org-a", "email": "same@example.invalid"}, map[string]any{"type": "claude", "account_uuid": "same-account", "organization_uuid": "org-b", "email": "same@example.invalid"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			manager, err := cliproxy.NewManager(t.TempDir(), 18317)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(fixture.old)
			if err := manager.SeedCredentials(map[string]json.RawMessage{"old.json": raw}); err != nil {
				t.Fatal(err)
			}
			name, replacing, err := prepareCredentialReplacement(manager, "new.json", fixture.new, false)
			if err != nil || replacing || name != "new.json" {
				t.Fatalf("distinct accounts were merged: %s %v %v", name, replacing, err)
			}
		})
	}
}
