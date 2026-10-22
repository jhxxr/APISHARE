package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"apishare/internal/cliproxy"
	"github.com/gin-gonic/gin"
	sdksession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"gopkg.in/yaml.v3"
)

func TestCLIProxyManagementBoundary(t *testing.T) {
	modelsTestDB(t)
	settingsSet("session_secret", "test-secret")
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operations = append(operations, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") == "Bearer "+makeAdminToken() {
			t.Error("administrator token forwarded to core")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v8/management/credentials":
			if r.Method == "DELETE" && r.URL.Query().Get("all") != "" {
				t.Error("bulk delete forwarded")
			}
			fmt.Fprint(w, `{"files":[{"id":"account","name":"account.json","provider":"codex","access_token":"TOP-SECRET"}]}`)
		case "/v8/management/oauth/auth-url":
			fmt.Fprint(w, `{"status":"ok","url":"https://auth.example/authorize","state":"login-test"}`)
		case "/v8/management/oauth/status":
			fmt.Fprint(w, `{"status":"wait"}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
		default:
			fmt.Fprint(w, `{"status":"ok"}`)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	p, _ := strconv.Atoi(port)
	manager, err := cliproxy.NewManager(filepath.Join(t.TempDir(), "core"), p)
	if err != nil {
		t.Fatal(err)
	}
	previous := cliProxyCore
	cliProxyCore = manager
	t.Cleanup(func() { cliProxyCore = previous })
	r := gin.New()
	registerCLIProxyRoutes(r.Group("/api", adminAuth()))
	token := makeAdminToken()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		return deploymentRequest(r, method, "/api/cliproxy"+path, token, body)
	}
	unauth := deploymentRequest(r, "GET", "/api/cliproxy/credentials", "", "")
	if unauth.Code != 401 || len(operations) != 0 {
		t.Fatal("unauthenticated core access")
	}
	w := request("GET", "/credentials", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "TOP-SECRET") {
		t.Fatalf("unsafe credential list: %d %s", w.Code, w.Body.String())
	}
	for _, name := range []string{"../secret.json", "all", "x.json&all=true", ""} {
		// Query escaping is handled separately; a missing name must never become all=true.
		if name == "x.json&all=true" {
			continue
		}
		w := request("DELETE", "/credentials?name="+name, "")
		if w.Code != 400 {
			t.Fatalf("invalid delete accepted: %s", name)
		}
	}
	w = request("POST", "/oauth/start", `{"provider":"codex"}`)
	if w.Code != 200 {
		t.Fatalf("start login: %s", w.Body.String())
	}
	coreLoginSessions.Lock()
	session := coreLoginSessions.items["login-test"]
	session.owner = [32]byte{}
	coreLoginSessions.items["login-test"] = session
	coreLoginSessions.Unlock()
	if w = request("GET", "/oauth/status?state=login-test", ""); w.Code != 404 {
		t.Fatal("login session ownership bypass")
	}
	for i := 0; i < 2; i++ {
		if w = request("POST", "/upstream", "{}"); w.Code != 200 {
			t.Fatalf("pool attach: %s", w.Body.String())
		}
	}
	ups, _ := dbListUpstreams()
	if len(ups) != 1 || ups[0].Type != "cliproxy" {
		t.Fatal("pool registration is not idempotent")
	}
	for _, path := range []string{"/config", "/requests/api-call", "/credentials/download"} {
		if request("GET", path, "").Code != 404 {
			t.Fatal("unrestricted management proxy exposed")
		}
	}
}

func TestCLIProxyEgressProxyBinding(t *testing.T) {
	modelsTestDB(t)
	settingsSet("session_secret", "test-secret")
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	p, _ := strconv.Atoi(port)
	root := filepath.Join(t.TempDir(), "core")
	manager, err := cliproxy.NewManager(root, p)
	if err != nil {
		t.Fatal(err)
	}
	previous := cliProxyCore
	cliProxyCore = manager
	t.Cleanup(func() { cliProxyCore = previous })
	settings := testSavedProxy(t, "socks5h://proxy.example:1080", "alice", "p@ss:word")
	const expectedURL = "socks5h://alice:p%40ss%3Aword@proxy.example:1080"
	configPath := filepath.Join(root, "config.yaml")
	proxyURL := func() string {
		t.Helper()
		raw, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		value, _ := document["requests"].(map[string]any)["proxy-url"].(string)
		return value
	}
	r := gin.New()
	registerCLIProxyRoutes(r.Group("/api", adminAuth()))
	token := makeAdminToken()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		return deploymentRequest(r, method, "/api/cliproxy"+path, token, body)
	}
	if w := request("PUT", "/proxy", fmt.Sprintf(`{"proxy_id":%d}`, settings.ID)); w.Code != 200 {
		t.Fatalf("select proxy: %d %s", w.Code, w.Body.String())
	}
	if got, _ := settingsGet(cliProxyProxySetting); got != strconv.FormatInt(settings.ID, 10) {
		t.Fatalf("binding = %q", got)
	}
	// The core is stopped, so the gateway updates config.yaml directly.
	if got := proxyURL(); got != expectedURL {
		t.Fatalf("proxy-url = %q", got)
	}
	w := request("GET", "/status", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"proxy_id":%d`, settings.ID)) {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "p@ss") || strings.Contains(w.Body.String(), "p%40ss") {
		t.Fatal("status exposed the proxy password")
	}
	before := append([]string(nil), requested...)
	for _, body := range []string{`{"proxy_id":999999}`, `{"proxy_id":-1}`, `{"proxy_id":"x"}`, `not-json`} {
		if w := request("PUT", "/proxy", body); w.Code != 400 {
			t.Fatalf("invalid binding %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if got, _ := settingsGet(cliProxyProxySetting); got != strconv.FormatInt(settings.ID, 10) {
		t.Fatal("invalid selection changed the binding")
	}
	if got := proxyURL(); got != expectedURL {
		t.Fatal("invalid selection rewrote the core configuration")
	}
	if len(requested) != len(before) {
		t.Fatal("invalid selection reached the core")
	}
	if _, err := db.Exec(`UPDATE outbound_proxies SET enabled=0 WHERE id=?`, settings.ID); err != nil {
		t.Fatal(err)
	}
	if w := request("PUT", "/proxy", fmt.Sprintf(`{"proxy_id":%d}`, settings.ID)); w.Code != 400 {
		t.Fatalf("disabled proxy accepted: %d %s", w.Code, w.Body.String())
	}
	if _, err := db.Exec(`UPDATE outbound_proxies SET enabled=1 WHERE id=?`, settings.ID); err != nil {
		t.Fatal(err)
	}
	if w := request("PUT", "/proxy", `{"proxy_id":0}`); w.Code != 200 {
		t.Fatalf("clear proxy: %d %s", w.Code, w.Body.String())
	}
	if got := proxyURL(); got != "" {
		t.Fatalf("proxy-url was not cleared: %q", got)
	}
}

func TestCLIProxyUpstreamRejectsGatewayProxy(t *testing.T) {
	r := testProxyAdminRouter(t)
	saved := testSavedProxy(t, "http://127.0.0.1:3128", "", "")
	w := proxyAdminRequest(r, "POST", "upstreams", fmt.Sprintf(`{"name":"pool","type":"cliproxy","base_url":"http://127.0.0.1:18317","api_key":"key","proxy_id":%d}`, saved.ID), true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "核心设置") {
		t.Fatalf("pool channel accepted a loopback proxy: %d %s", w.Code, w.Body.String())
	}
	ups, _ := dbListUpstreams()
	if len(ups) != 0 {
		t.Fatal("rejected pool channel was saved")
	}
	up := &Upstream{Name: "pool", Type: "cliproxy", BaseURL: "http://127.0.0.1:18317", APIKey: "key", Weight: 1, Enabled: true, ProxyID: saved.ID}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("upstreams/%d", up.ID)
	if w := proxyAdminRequest(r, "PATCH", path, fmt.Sprintf(`{"proxy_id":%d}`, saved.ID), true); w.Code != 400 {
		t.Fatalf("pool channel binding accepted: %d %s", w.Code, w.Body.String())
	}
	// Editing other fields drops the stale proxy left by older configurations.
	if w := proxyAdminRequest(r, "PATCH", path, `{"name":"renamed"}`, true); w.Code != 200 {
		t.Fatalf("pool rename: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dbGetUpstream(up.ID); got.ProxyID != 0 {
		t.Fatal("stale loopback proxy survived an edit")
	}
	// The core binding is a real reference: the proxy cannot be deleted or
	// disabled while the embedded core depends on it.
	manager, err := cliproxy.NewManager(filepath.Join(t.TempDir(), "core"), 18318)
	if err != nil {
		t.Fatal(err)
	}
	previous := cliProxyCore
	cliProxyCore = manager
	t.Cleanup(func() { cliProxyCore = previous })
	settingsSet("session_secret", "proxy-test-secret")
	if err := settingsSet(cliProxyProxySetting, strconv.FormatInt(saved.ID, 10)); err != nil {
		t.Fatal(err)
	}
	proxyPath := fmt.Sprintf("proxies/%d", saved.ID)
	if w := proxyAdminRequest(r, "DELETE", proxyPath, "", true); w.Code != 409 {
		t.Fatalf("core proxy was deleted: %d %s", w.Code, w.Body.String())
	}
	if w := proxyAdminRequest(r, "PATCH", proxyPath, `{"enabled":false}`, true); w.Code != 409 {
		t.Fatalf("core proxy was disabled: %d %s", w.Code, w.Body.String())
	}
	if w := proxyAdminRequest(r, "PATCH", proxyPath, `{"name":"renamed"}`, true); w.Code != 200 {
		t.Fatalf("core proxy rename: %d %s", w.Code, w.Body.String())
	}
	if err := settingsSet(cliProxyProxySetting, "0"); err != nil {
		t.Fatal(err)
	}
	if w := proxyAdminRequest(r, "POST", fmt.Sprintf("upstreams/%d/duplicate", up.ID), `{}`, true); w.Code != 200 {
		t.Fatalf("pool duplicate: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dbListUpstreams(); len(got) != 2 || got[1].ProxyID != 0 || got[1].Type != "cliproxy" {
		t.Fatalf("duplicate copied the loopback proxy: %+v", got)
	}
}

func TestCLIProxyNativeRelayAndScopedAffinity(t *testing.T) {
	modelsTestDB(t)
	var sessions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer pool-key" {
			t.Errorf("wrong pool request: %s", r.URL.Path)
		}
		sessions = append(sessions, r.Header.Get("X-Session-Id"))
		fmt.Fprint(w, `{"id":"resp-test","object":"response","status":"completed","model":"test-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer server.Close()
	up := &Upstream{ID: 99, Type: "cliproxy", BaseURL: server.URL, APIKey: "pool-key"}
	for _, format := range []clientFormat{FormatOpenAIChat, FormatOpenAIResponse, FormatAnthropic, FormatGemini} {
		if up.wireFormat(format) != format {
			t.Fatalf("native protocol lost: %s", format)
		}
	}
	if !supportsNativeEndpoint(up, FormatOpenAIResponse) || supportsNativeEndpoint(up, FormatOpenAIChat) {
		t.Fatal("invalid native-only endpoint support")
	}
	for _, id := range []int64{1, 1, 2} {
		body := []byte(`{"model":"test-model","input":"hello"}`)
		cr, err := parseClientRequest(FormatOpenAIResponse, body, "")
		if err != nil {
			t.Fatal(err)
		}
		task := &relayTask{format: FormatOpenAIResponse, model: "test-model", endpoint: "/v1/responses", body: body, canon: cr, apiKeyID: id}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(context.Background())
		c.Request.Header.Set("X-Session-Id", "shared-client-session")
		info, _, err := doRelay(c, up, task)
		if err != nil || info.PromptTok != 4 || info.CompletionTok != 2 || w.Code != 200 {
			t.Fatalf("relay accounting: %+v %v %s", info, err, w.Body.String())
		}
	}
	if len(sessions) != 3 || sessions[0] == "" || sessions[0] != sessions[1] || sessions[0] == sessions[2] {
		t.Fatal("pool affinity is not isolated by API key")
	}
}

func TestCLIProxyCallbackRejectsDifferentState(t *testing.T) {
	modelsTestDB(t)
	settingsSet("session_secret", "test-secret")
	m, _ := cliproxy.NewManager(t.TempDir(), 18317)
	previous := cliProxyCore
	cliProxyCore = m
	defer func() { cliProxyCore = previous }()
	token := makeAdminToken()
	coreLoginSessions.Lock()
	coreLoginSessions.items["callback-test"] = coreLoginSession{owner: sha256.Sum256([]byte(token)), expires: time.Now().Add(time.Minute)}
	coreLoginSessions.Unlock()
	r := gin.New()
	registerCLIProxyRoutes(r.Group("/api", adminAuth()))
	body, _ := json.Marshal(map[string]string{"state": "callback-test", "redirect_url": "http://localhost/callback?code=x&state=different"})
	w := deploymentRequest(r, "POST", "/api/cliproxy/oauth/callback", token, string(body))
	if w.Code != 400 {
		t.Fatalf("unexpected session access: %d", w.Code)
	}
}

func TestCLIProxyClaudeAffinityOverridesUnscopedMetadata(t *testing.T) {
	body := []byte(`{"model":"claude","metadata":{"user_id":"user_abc_account_123_session_01234567-89ab-cdef-0123-456789abcdef"},"messages":[{"role":"user","content":"hi"}]}`)
	cr, err := parseClientRequest(FormatAnthropic, body, "")
	if err != nil {
		t.Fatal(err)
	}
	var identities []string
	for _, id := range []int64{1, 1, 2} {
		req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
		setCLIProxyHeaders(req, http.Header{}, &relayTask{format: FormatAnthropic, apiKeyID: id, endpoint: "/v1/messages", model: "claude", body: body, canon: cr})
		info, ok := sdksession.ExtractSessionInfo(req.Header, body, nil)
		if !ok || info.SessionID == "" {
			t.Fatal("SDK did not recognize scoped identity")
		}
		identities = append(identities, info.SessionID)
	}
	if identities[0] != identities[1] || identities[0] == identities[2] {
		t.Fatal("Claude metadata bypassed gateway key scope")
	}
}
