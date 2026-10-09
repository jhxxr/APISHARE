package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func testSavedProxy(t *testing.T, endpoint, username, password string) *OutboundProxy {
	t.Helper()
	p := &OutboundProxy{Name: "test proxy", URL: endpoint, Username: username, Password: password, Enabled: true}
	if err := normalizeOutboundProxy(p); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO outbound_proxies(name,url,username,password,enabled,created_at) VALUES(?,?,?,?,1,?)`, p.Name, p.URL, username, password, nowStr())
	if err != nil {
		t.Fatal(err)
	}
	p.ID, _ = res.LastInsertId()
	t.Cleanup(func() { closeProxyTransport(p.ID) })
	return p
}

func TestOutboundProxyValidation(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:80", "https://proxy.example:443/", "socks5://proxy.example:1080", "socks5h://[::1]:1080"} {
		p := &OutboundProxy{Name: "name", URL: endpoint}
		if err := normalizeOutboundProxy(p); err != nil {
			t.Errorf("rejected %s: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"ftp://proxy.example:21", "http://proxy.example", "http://proxy.example:0", "http://proxy.example:65536", "http://user:secret@proxy.example:80", "http://proxy.example:80/path", "http://proxy.example:80?secret=yes", "http://proxy.example:80#secret", "http://proxy.example:80?", "socks5://:1080", "http://proxy.example:not-a-port"} {
		p := &OutboundProxy{Name: "name", URL: endpoint}
		if err := normalizeOutboundProxy(p); err == nil {
			t.Errorf("accepted invalid %s", endpoint)
		} else if strings.Contains(err.Error(), "secret") {
			t.Error("validation exposed credentials")
		}
	}
	p := &OutboundProxy{Name: "name", URL: "socks5://proxy.example:1080", Username: strings.Repeat("a", 256)}
	if normalizeOutboundProxy(p) == nil {
		t.Fatal("accepted oversized SOCKS authentication")
	}
}

func TestProxyMigrationPreservesExistingUpstreams(t *testing.T) {
	modelsTestDB(t)
	u := &Upstream{Name: "legacy", Type: "openai", BaseURL: "https://example.invalid", APIKey: "saved-key", Enabled: true, Weight: 1}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE upstreams DROP COLUMN proxy_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE outbound_proxies`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := dbGetUpstream(u.ID)
	if err != nil || got.ProxyID != 0 || got.APIKey != u.APIKey || got.Name != u.Name {
		t.Fatalf("migration lost legacy settings: %v %v", got, err)
	}
}

func testProxyAdminRouter(t *testing.T) *gin.Engine {
	t.Helper()
	modelsTestDB(t)
	if err := settingsSet("session_secret", "proxy-test-secret"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerAdmin(r)
	return r
}

func proxyAdminRequest(r *gin.Engine, method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, adminPath+"/api/"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+makeAdminToken())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestProxyAdminCRUDAndBindings(t *testing.T) {
	r := testProxyAdminRouter(t)
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		path := "proxies"
		if method == "PATCH" || method == "DELETE" {
			path += "/1"
		}
		if w := proxyAdminRequest(r, method, path, `{}`, false); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", method, w.Code)
		}
	}
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := proxyAdminRequest(r, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	request("POST", "proxies", `{"name":"bad","url":"file:///secret"}`, 400)
	w := request("POST", "proxies", `{"name":"edge","url":"socks5://127.0.0.1:1080","username":"alice","password":"secret-password"}`, 200)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("proxies/%d", created.ID)
	w = request("GET", "proxies", "", 200)
	if strings.Contains(w.Body.String(), "secret-password") || strings.Contains(w.Body.String(), `"password":`) || !strings.Contains(w.Body.String(), `"has_password":true`) {
		t.Fatal("proxy list leaked or lost password state")
	}
	request("PATCH", path, `{"name":"updated","password":""}`, 200)
	p, _ := dbGetOutboundProxy(created.ID)
	if p.Password != "secret-password" {
		t.Fatal("blank edit removed saved password")
	}
	request("PATCH", path, `{"clear_password":true}`, 200)
	p, _ = dbGetOutboundProxy(created.ID)
	if p.Password != "" {
		t.Fatal("password not cleared")
	}
	request("PATCH", path, `{"password":"replacement"}`, 200)
	request("PATCH", path, `{"username":""}`, 200)
	p, _ = dbGetOutboundProxy(created.ID)
	if p.Password != "" || p.Username != "" {
		t.Fatal("authentication not removed")
	}
	request("POST", "upstreams", `{"type":"openai","base_url":"https://example.invalid","api_key":"key","proxy_id":999}`, 400)
	request("POST", "upstreams", fmt.Sprintf(`{"name":"up","type":"openai","base_url":"https://example.invalid","api_key":"key","proxy_id":%d}`, created.ID), 200)
	ups, _ := dbListUpstreams()
	if len(ups) != 1 || ups[0].ProxyID != created.ID {
		t.Fatal("upstream binding not persisted")
	}
	upPath := fmt.Sprintf("upstreams/%d", ups[0].ID)
	request("DELETE", path, "", 409)
	request("PATCH", path, `{"enabled":false}`, 200)
	if _, err := clientForUpstream(upstreamHTTPClient, created.ID); err == nil {
		t.Fatal("disabled proxy fell back to direct")
	}
	request("POST", "upstreams", fmt.Sprintf(`{"type":"openai","base_url":"https://example.invalid","api_key":"key","proxy_id":%d}`, created.ID), 400)
	request("PATCH", upPath, fmt.Sprintf(`{"name":"renamed","proxy_id":%d}`, created.ID), 200)
	request("PATCH", upPath, `{"proxy_id":-1}`, 400)
	request("PATCH", upPath, `{"proxy_id":0}`, 200)
	u, _ := dbGetUpstream(ups[0].ID)
	if u.ProxyID != 0 {
		t.Fatal("default connection selection not persisted")
	}
	request("DELETE", path, "", 200)
	if _, err := clientForUpstream(upstreamHTTPClient, created.ID); err == nil {
		t.Fatal("missing proxy fell back to direct")
	}
	request("PATCH", path, `{}`, 404)
}

func TestHTTPProxyRelayProbeAndCacheUpdates(t *testing.T) {
	r := testProxyAdminRouter(t)
	var callsA, callsB atomic.Int32
	proxyServer := func(label string, counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			counter.Add(1)
			if req.URL.Host != "proxy-only.invalid" {
				t.Errorf("destination = %s", req.URL.Host)
			}
			if req.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")) {
				w.WriteHeader(407)
				return
			}
			if req.Header.Get("Authorization") != "Bearer saved-key" {
				t.Error("upstream authentication missing")
			}
			if req.URL.Path == "/v1/models" {
				fmt.Fprintf(w, `{"data":[{"id":%q}]}`, label)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"through proxy\"}}]}\n\ndata: [DONE]\n\n")
		}))
	}
	a, b := proxyServer("model-a", &callsA), proxyServer("model-b", &callsB)
	defer a.Close()
	defer b.Close()
	p := testSavedProxy(t, a.URL, "alice", "secret")
	u := &Upstream{Name: "proxied", Type: "openai", BaseURL: "http://proxy-only.invalid", APIKey: "saved-key", Enabled: true, Weight: 1, ProxyID: p.ID}
	if err := dbInsertUpstream(u); err != nil {
		t.Fatal(err)
	}
	ids, err := cachedUpstreamModels(context.Background(), u)
	if err != nil || len(ids) != 1 || ids[0] != "model-a" {
		t.Fatalf("proxy discovery: %v %v", ids, err)
	}
	oldKey := upstreamModelCacheKey(u)
	for _, suffix := range []string{"test", "probe"} {
		w := proxyAdminRequest(r, "POST", fmt.Sprintf("upstreams/%d/%s", u.ID, suffix), "", true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("proxy %s: %s", suffix, w.Body.String())
		}
	}
	w := proxyAdminRequest(r, "POST", "upstream-probe", fmt.Sprintf(`{"type":"openai","base_url":"http://proxy-only.invalid","api_key":"saved-key","proxy_id":%d}`, p.ID), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "model-a") {
		t.Fatal("unsaved proxy selection ignored")
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[{"id":"direct-model"}]}`) }))
	defer direct.Close()
	w = proxyAdminRequest(r, "POST", fmt.Sprintf("upstreams/%d/probe", u.ID), fmt.Sprintf(`{"proxy_id":0,"base_url":%q}`, direct.URL), true)
	if !strings.Contains(w.Body.String(), "direct-model") {
		t.Fatal("explicit zero did not override saved proxy")
	}
	output := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(output)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	info, wrote, err := doRelay(c, u, &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "model-a", body: []byte(`{"model":"model-a","stream":true}`), stream: true})
	if err != nil || !wrote || info.Status != 200 || !strings.Contains(output.Body.String(), "through proxy") {
		t.Fatalf("SSE relay: %+v %v %v", info, wrote, err)
	}
	w = proxyAdminRequest(r, "PATCH", fmt.Sprintf("proxies/%d", p.ID), fmt.Sprintf(`{"url":%q}`, b.URL), true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if oldKey == upstreamModelCacheKey(u) {
		t.Fatal("proxy update did not change discovery cache key")
	}
	ids, err = cachedUpstreamModels(context.Background(), u)
	if err != nil || len(ids) != 1 || ids[0] != "model-b" || callsB.Load() != 1 {
		t.Fatalf("stale proxy cache: %v %v", ids, err)
	}
	if callsA.Load() != 5 {
		t.Fatalf("proxy A requests = %d", callsA.Load())
	}
}

// Minimal local SOCKS5 server: verify real negotiation, username/password and
// domain forwarding, then tunnel bytes to the test upstream without external I/O.
func testSOCKSProxy(t *testing.T, target string, observed chan<- string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				defer func() { mu.Lock(); delete(connections, conn); mu.Unlock() }()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				header := make([]byte, 2)
				if _, err := io.ReadFull(r, header); err != nil || header[0] != 5 {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err := io.ReadFull(r, methods); err != nil {
					return
				}
				conn.Write([]byte{5, 2})
				if _, err := io.ReadFull(r, header); err != nil || header[0] != 1 {
					return
				}
				user := make([]byte, int(header[1]))
				if _, err := io.ReadFull(r, user); err != nil {
					return
				}
				n, err := r.ReadByte()
				if err != nil {
					return
				}
				pass := make([]byte, int(n))
				if _, err := io.ReadFull(r, pass); err != nil {
					return
				}
				if string(user) != "alice" || string(pass) != "secret" {
					conn.Write([]byte{1, 1})
					return
				}
				conn.Write([]byte{1, 0})
				request := make([]byte, 4)
				if _, err := io.ReadFull(r, request); err != nil || request[0] != 5 || request[1] != 1 || request[3] != 3 {
					return
				}
				n, err = r.ReadByte()
				if err != nil {
					return
				}
				host := make([]byte, int(n))
				if _, err := io.ReadFull(r, host); err != nil {
					return
				}
				portBytes := make([]byte, 2)
				if _, err := io.ReadFull(r, portBytes); err != nil {
					return
				}
				observed <- fmt.Sprintf("%s:%d", host, binary.BigEndian.Uint16(portBytes))
				up, err := net.DialTimeout("tcp", target, 5*time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				conn.SetDeadline(time.Time{})
				done := make(chan struct{})
				go func() { io.Copy(up, r); up.Close(); close(done) }()
				io.Copy(conn, up)
				conn.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return listener.Addr().String()
}

func TestSOCKSProxiesRemoteDNSAuthenticationAndFormats(t *testing.T) {
	modelsTestDB(t)
	for _, scheme := range []string{"socks5", "socks5h"} {
		for _, typ := range []string{"openai", "anthropic", "gemini"} {
			t.Run(scheme+"/"+typ, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch typ {
					case "anthropic":
						if r.Header.Get("x-api-key") != "key" {
							t.Error("Claude auth missing")
						}
					case "gemini":
						if r.Header.Get("x-goog-api-key") != "key" {
							t.Error("Gemini auth missing")
						}
					default:
						if r.Header.Get("Authorization") != "Bearer key" {
							t.Error("OpenAI auth missing")
						}
					}
					if r.Header.Get("Proxy-Authorization") != "" {
						t.Error("proxy authentication leaked to upstream")
					}
					if r.URL.Path == "/v1/chat/completions" {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"SOCKS stream\"}}]}\n\ndata: [DONE]\n\n")
						return
					}
					if typ == "gemini" {
						fmt.Fprint(w, `{"models":[{"name":"models/proxied-model"}]}`)
					} else {
						fmt.Fprint(w, `{"data":[{"id":"proxied-model"}]}`)
					}
				}))
				defer upstream.Close()
				observed := make(chan string, 8)
				endpoint := testSOCKSProxy(t, strings.TrimPrefix(upstream.URL, "http://"), observed)
				p := testSavedProxy(t, scheme+"://"+endpoint, "alice", "secret")
				ids, err := probeUpstreamModelsWithProxy(context.Background(), typ, "http://proxy-only.invalid:4321", "key", p.ID)
				if err != nil || len(ids) != 1 || ids[0] != "proxied-model" {
					t.Fatalf("SOCKS probe: %v %v", ids, err)
				}
				select {
				case destination := <-observed:
					if destination != "proxy-only.invalid:4321" {
						t.Fatal(destination)
					}
				default:
					t.Fatal("domain was not sent to SOCKS proxy")
				}
				if typ == "openai" {
					output := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(output)
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
					up := &Upstream{Type: typ, BaseURL: "http://proxy-only.invalid:4321", APIKey: "key", ProxyID: p.ID}
					_, wrote, err := doRelay(c, up, &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: "proxied-model", stream: true, body: []byte(`{"model":"proxied-model","stream":true}`)})
					if err != nil || !wrote || !strings.Contains(output.Body.String(), "SOCKS stream") {
						t.Fatalf("SOCKS streaming relay: wrote=%v err=%v body=%s", wrote, err, output.Body.String())
					}
				}
				if _, err := db.Exec(`UPDATE outbound_proxies SET password='wrong-secret' WHERE id=?`, p.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := probeUpstreamModelsWithProxy(context.Background(), typ, "http://proxy-only.invalid:4321", "key", p.ID); err == nil || strings.Contains(err.Error(), "wrong-secret") {
					t.Fatalf("bad SOCKS auth accepted/leaked: %v", err)
				}
			})
		}
	}
}

func TestHTTPSProxyAndCONNECT(t *testing.T) {
	modelsTestDB(t)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy auth reached upstream")
		}
		fmt.Fprint(w, `{"data":[{"id":"tls-model"}]}`)
	}))
	defer upstream.Close()
	// Trust the local self-signed test endpoints only in this test client.
	original := upstreamHTTPClient
	transport := original.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	upstreamHTTPClient = &http.Client{Transport: transport}
	defer func() { upstreamHTTPClient = original; transport.CloseIdleConnections() }()
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS-proxy=%v", secure), func(t *testing.T) {
			var connects atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("alice:secret")) {
					w.WriteHeader(407)
					return
				}
				connects.Add(1)
				up, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "https://"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(502)
					return
				}
				conn, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					up.Close()
					return
				}
				defer conn.Close()
				defer up.Close()
				fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
				buffered.Flush()
				done := make(chan struct{})
				go func() { io.Copy(up, buffered); up.Close(); close(done) }()
				io.Copy(conn, up)
				conn.Close()
				<-done
			}))
			if secure {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			p := testSavedProxy(t, server.URL, "alice", "secret")
			ids, err := probeUpstreamModelsWithProxy(context.Background(), "openai", "https://proxy-only.invalid", "key", p.ID)
			closeProxyTransport(p.ID)
			if err != nil || len(ids) != 1 || ids[0] != "tls-model" || connects.Load() != 1 {
				t.Fatalf("CONNECT: %v %v, count=%d", ids, err, connects.Load())
			}
		})
	}
}

func TestProxyFailureFailsOverWithoutDirectRequest(t *testing.T) {
	modelsTestDB(t)
	settingsMu.Lock()
	originalSettings := settingsCache
	settingsCache.Retry = 1
	settingsMu.Unlock()
	defer func() { settingsMu.Lock(); settingsCache = originalSettings; settingsMu.Unlock() }()
	var directCalls, proxyCalls atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directCalls.Add(1)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer direct.Close()
	goodProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"fallback through proxy"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer goodProxy.Close()
	badProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer badProxy.Close()
	bad := testSavedProxy(t, badProxy.URL, "alice", "hidden-password")
	good := testSavedProxy(t, goodProxy.URL, "", "")
	first := &Upstream{Name: "bad", Type: "openai", BaseURL: direct.URL, APIKey: "key", Models: "proxy-model", Weight: 1, Enabled: true, ProxyID: bad.ID}
	second := &Upstream{Name: "good", Type: "openai", BaseURL: direct.URL, APIKey: "key", Models: "proxy-model", Weight: 1, Enabled: true, ProxyID: good.ID}
	for _, up := range []*Upstream{first, second} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	key := publicTestKey(t, "proxy-failover", 0, 0, false)
	affinitySet("sid:proxy-failover", first.ID)
	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("apiKey", *key) }, relayHandler(FormatOpenAIChat, false))
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"proxy-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("X-Session-Id", "proxy-failover")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fallback through proxy") || directCalls.Load() != 0 || proxyCalls.Load() != 1 {
		t.Fatalf("proxy failover: status=%d direct=%d proxied=%d body=%s", w.Code, directCalls.Load(), proxyCalls.Load(), w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hidden-password") {
		t.Fatal("proxy password exposed")
	}
	logs, err := dbListLogs(10, 0)
	if err != nil || len(logs) != 1 || logs[0].UpstreamID != second.ID {
		t.Fatalf("incorrect final upstream log: %v %v", logs, err)
	}
}
