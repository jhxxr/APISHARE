package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestClientUsesV8AndProtectsSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ManagementPath+"/credentials" || r.Header.Get("Authorization") != "Bearer management-secret" {
			t.Errorf("incorrect management request")
		}
		if r.Method == "POST" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"management-secret access_token=private"}`)
			return
		}
		fmt.Fprint(w, `{"files":[{"id":"account","name":"one.json","provider":"codex","access_token":"private"}]}`)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Secret: "management-secret"}
	var result struct {
		Files []Credential `json:"files"`
	}
	if err := client.Do(context.Background(), "GET", "/credentials", nil, &result); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), "private") || len(result.Files) != 1 {
		t.Fatal("unsafe credential projection")
	}
	if err := client.Do(context.Background(), "POST", "/credentials", map[string]string{"a": "b"}, nil); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("upstream error exposed secrets")
	}
}

func TestEmbeddedStateSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, 18317)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Status().AutoStart || !m.Status().Embedded {
		t.Fatal("core must be embedded and enabled by default")
	}
	if err := m.ensureConfig(); err != nil {
		t.Fatal(err)
	}
	base, key := m.Connection()
	next, err := NewManager(root, 18318)
	if err != nil {
		t.Fatal(err)
	}
	nextBase, nextKey := next.Connection()
	if base != nextBase || key != nextKey || key == "" {
		t.Fatal("connection changed on restart")
	}
	b, _ := json.Marshal(next.Status())
	if strings.Contains(string(b), key) || strings.Contains(string(b), next.snapshot().ManagementKey) {
		t.Fatal("status exposes credentials")
	}
	if err := next.Stop(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(root, 18317)
	if err != nil || restarted.Status().AutoStart {
		t.Fatal("explicit stop did not persist")
	}
}

func TestSetProxyURLWhileStopped(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, 18319)
	if err != nil {
		t.Fatal(err)
	}
	const proxyURL = "socks5h://alice:secret%40word@proxy.example:1080"
	if err := m.SetProxyURL(context.Background(), "  "+proxyURL+"  "); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	read := func() map[string]any {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := yaml.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	document := read()
	requests, _ := document["requests"].(map[string]any)
	if requests["proxy-url"] != proxyURL {
		t.Fatalf("proxy-url = %v", requests["proxy-url"])
	}
	management, _ := document["management"].(map[string]any)
	if management["secret-key"] != m.snapshot().ManagementKey {
		t.Fatal("saving the proxy re-hashed the management secret")
	}
	if err := m.SetProxyURL(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	document = read()
	requests, _ = document["requests"].(map[string]any)
	if requests["proxy-url"] != "" {
		t.Fatalf("proxy-url was not cleared: %v", requests["proxy-url"])
	}
	if management, _ = document["management"].(map[string]any); management["secret-key"] != m.snapshot().ManagementKey {
		t.Fatal("clearing the proxy re-hashed the management secret")
	}
	// The SDK must read back exactly what the gateway wrote.
	loaded, err := sdkconfig.LoadConfig(path)
	if err != nil || loaded.ProxyURL != "" {
		t.Fatalf("SDK load: %v %q", err, loaded.ProxyURL)
	}
	if err := m.SetProxyURL(context.Background(), proxyURL); err != nil {
		t.Fatal(err)
	}
	if loaded, err = sdkconfig.LoadConfig(path); err != nil || loaded.ProxyURL != proxyURL {
		t.Fatalf("SDK proxy read-back: %v %q", err, loaded.ProxyURL)
	}
}

// TestEmbeddedCoreUsesConfiguredProxy proves the gateway-selected proxy is the
// core's real egress: the provider host does not exist, so only a request sent
// through the local HTTP proxy can succeed.
func TestEmbeddedCoreUsesConfiguredProxy(t *testing.T) {
	var proxied int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The core's own background version checks share this egress; only the
		// inference request matters here.
		if r.URL.Host != "upstream.invalid" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("proxied path = %s", r.URL.Path)
		}
		atomic.AddInt32(&proxied, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"proxied","object":"chat.completion","model":"local-model","choices":[{"index":0,"message":{"role":"assistant","content":"through core proxy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer proxy.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	p, _ := strconv.Atoi(port)
	root := t.TempDir()
	m, err := NewManager(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.ensureConfig(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	raw, _ := os.ReadFile(configPath)
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["api-keys"] = map[string]any{"openai-compatibility": []any{map[string]any{"name": "proxied", "base-url": "http://upstream.invalid/v1", "keys": []any{map[string]any{"api-key": "local-test-key"}}, "models": []any{map[string]any{"name": "local-model", "alias": "local-model"}}}}}
	raw, _ = json.Marshal(document)
	if err := writePrivate(configPath, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := m.SetProxyURL(ctx, proxy.URL); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	base, key := m.Connection()
	request, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewBufferString(`{"model":"local-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(result, []byte("through core proxy")) {
		t.Fatalf("proxied inference: %d %s", response.StatusCode, result)
	}
	if atomic.LoadInt32(&proxied) != 1 {
		t.Fatalf("core bypassed the configured proxy: %d", proxied)
	}
}

func TestEmbeddedSDKLifecycle(t *testing.T) {
	// Uses the SDK linked into the test binary. No external executable, installation,
	// user credential or provider inference request is needed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	p, _ := strconv.Atoi(port)
	root := t.TempDir()
	m, err := NewManager(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Errorf("incorrect embedded SDK upstream request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chat-test","object":"chat.completion","model":"local-model","choices":[{"index":0,"message":{"role":"assistant","content":"embedded sdk works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	defer upstream.Close()
	if err := m.ensureConfig(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	raw, _ := os.ReadFile(configPath)
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["api-keys"] = map[string]any{"openai-compatibility": []any{map[string]any{"name": "local-test", "base-url": upstream.URL + "/v1", "keys": []any{map[string]any{"api-key": "local-test-key"}}, "models": []any{map[string]any{"name": "local-model", "alias": "local-model"}}}}}
	raw, _ = json.Marshal(document)
	if err := writePrivate(configPath, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var credentials struct {
		Files []Credential `json:"files"`
	}
	if err := m.Client().Do(ctx, "GET", "/credentials", nil, &credentials); err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Disabled bool `json:"disable-control-panel"`
	}
	if err := m.Client().Do(ctx, "GET", "/config/management", nil, &settings); err != nil || !settings.Disabled {
		t.Fatalf("panel: %v %+v", err, settings)
	}
	// The running core applies a proxy change through its management API so the
	// SDK persists config.yaml and reloads without restarting the listener.
	if err := m.SetProxyURL(ctx, "direct"); err != nil {
		t.Fatal(err)
	}
	var requests struct {
		ProxyURL string `json:"proxy-url"`
	}
	if err := m.Client().Do(ctx, "GET", "/config/requests", nil, &requests); err != nil || requests.ProxyURL != "direct" {
		t.Fatalf("running core proxy: %v %+v", err, requests)
	}
	base, key := m.Connection()
	request, _ := http.NewRequest("GET", base+"/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("SDK client authentication failed: %d", response.StatusCode)
	}
	request, _ = http.NewRequestWithContext(ctx, "POST", base+"/v1/messages", bytes.NewBufferString(`{"model":"local-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("anthropic-version", "2023-06-01")
	response, err = (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(result, []byte("embedded sdk works")) || !bytes.Contains(result, []byte(`"type":"message"`)) {
		t.Fatalf("built-in protocol translators were not registered: %d %s", response.StatusCode, result)
	}
	if _, err := os.Stat(filepath.Join(root, "versions")); !os.IsNotExist(err) {
		t.Fatal("external executable installation detected")
	}
	m.Close()
	if m.Status().Running || !m.Status().AutoStart {
		t.Fatal("close must preserve startup preference")
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if m.Status().Running || m.Status().AutoStart {
		t.Fatal("stop did not persist disabled state")
	}
}
