package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminModelMappingValidationAndPersistence(t *testing.T) {
	r := testProxyAdminRouter(t)
	request := func(method, path, raw string, status int) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"type": "openai", "base_url": "https://upstream.invalid", "api_key": "fixture", "models": "real", "model_map": raw})
		w := proxyAdminRequest(r, method, path, string(body), true)
		if w.Code != status {
			t.Fatalf("%s %s map=%s: %d %s", method, path, raw, w.Code, w.Body.String())
		}
		return w
	}
	invalid := []string{`[]`, `null`, `"text"`, `{"alias":null}`, `{"alias":123}`, `{"alias":" "}`, `{" ":"real"}`, `{"alias":"real",}`, `{"alias":"a","alias":"b"}`, `{"alias":"a"," alias ":"b"}`, `{} {}`}
	for _, raw := range invalid {
		request("POST", "upstreams", raw, 400)
	}
	w := request("POST", "upstreams", `{" alias ":" real "}`, 200)
	var created struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("upstreams/%d", created.ID)
	up, err := dbGetUpstream(created.ID)
	if err != nil || up.ModelMap != `{"alias":"real"}` {
		t.Fatalf("normalized map not saved: %+v %v", up, err)
	}
	if selected, err := pickUpstream(FormatOpenAIChat, "alias", nil, false); err != nil || selected.ID != created.ID {
		t.Fatalf("alias did not route after creation: %+v %v", selected, err)
	}
	for _, raw := range invalid {
		request("PATCH", path, raw, 400)
	}
	up, _ = dbGetUpstream(created.ID)
	if up.ModelMap != `{"alias":"real"}` {
		t.Fatal("invalid edit overwrote saved mapping")
	}
	request("PATCH", path, `{"next":"other-real"}`, 200)
	if _, err := pickUpstream(FormatOpenAIChat, "alias", nil, false); err == nil {
		t.Fatal("removed alias remained in routing cache")
	}
	if selected, err := pickUpstream(FormatOpenAIChat, "next", nil, false); err != nil || selected.mapModel("next") != "other-real" {
		t.Fatalf("mapping update did not take effect: %+v %v", selected, err)
	}
	listed := proxyAdminRequest(r, "GET", "upstreams", "", true)
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), `\"next\":\"other-real\"`) {
		t.Fatalf("saved editor mapping not returned: %s", listed.Body.String())
	}
	request("PATCH", path, "", 200)
	if _, err := pickUpstream(FormatOpenAIChat, "next", nil, false); err == nil {
		t.Fatal("cleared mapping still routes")
	}
}

func TestMappedAliasesAreExplicitAndOneHop(t *testing.T) {
	up := &Upstream{Models: "real", ModelMap: `{"alias":"real","real":"final","identity":"identity"}`}
	for _, compiled := range []bool{false, true} {
		if compiled {
			compileUpstreamConfig(up)
		}
		for _, name := range []string{"alias", "identity", "real"} {
			if serves, explicit := up.servesModelExplicit(name); !serves || !explicit {
				t.Fatalf("compiled=%t alias %s excluded", compiled, name)
			}
		}
		if up.mapModel("alias") != "real" || up.servesModel("unknown") {
			t.Fatal("mapping recursed or allowed an unknown model")
		}
	}
	up.Models = ""
	if serves, explicit := up.servesModelExplicit("alias"); !serves || !explicit {
		t.Fatal("mapping on a wildcard upstream must remain explicit")
	}
	if serves, explicit := up.servesModelExplicit("unknown"); !serves || explicit {
		t.Fatal("empty allowlist must retain wildcard behavior")
	}
}

func TestMappedAliasRoutingPriorityAndFailover(t *testing.T) {
	modelsTestDB(t)
	alias := &Upstream{Type: "openai", OpenAIProtocol: "chat", Models: "real", ModelMap: `{"alias":"real"}`, Enabled: true, Weight: 1}
	wildcard := &Upstream{Type: "openai", OpenAIProtocol: "chat", Enabled: true, Weight: 1000000}
	for _, up := range []*Upstream{alias, wildcard} {
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	for _, models := range []string{"real", ""} {
		alias.Models = models
		if err := dbUpdateUpstream(alias); err != nil {
			t.Fatal(err)
		}
		picked, err := pickUpstream(FormatOpenAIChat, "alias", nil, false)
		if err != nil || picked.ID != alias.ID {
			t.Fatalf("mapped alias lost priority to wildcard: %+v %v", picked, err)
		}
		picked, err = pickUpstream(FormatOpenAIChat, "alias", map[int64]bool{alias.ID: true}, false)
		if err != nil || picked.ID != wildcard.ID {
			t.Fatalf("alias failover unavailable: %+v %v", picked, err)
		}
	}
}

func TestMappedAliasAdminToRelayMatrix(t *testing.T) {
	for _, in := range protocolFormats {
		for _, out := range protocolFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", in, out, stream), func(t *testing.T) {
					r := testProxyAdminRouter(t)
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						calls++
						if req.URL.RequestURI() != nativeEndpoint(out, &canonReq{Model: "mapped", Stream: stream}) {
							t.Errorf("mapped endpoint: %s", req.URL.Path)
						}
						if out != FormatGemini {
							var body map[string]any
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["model"] != "mapped" {
								t.Errorf("upstream did not receive mapped model: %v %v", body, err)
							}
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
					up.Models, up.Weight = "mapped", 1 // Alias intentionally absent from allowlist.
					body, _ := json.Marshal(up)
					if w := proxyAdminRequest(r, "POST", "upstreams", string(body), true); w.Code != 200 {
						t.Fatalf("create: %d %s", w.Code, w.Body.String())
					}
					key := &ApiKey{Name: "mapping", Key: "sk-mapping", Models: "alias", Enabled: true, QPS: 100, Concurrency: 10}
					if err := dbInsertKey(key); err != nil {
						t.Fatal(err)
					}
					v1 := r.Group("/v1", keyAuth(), keyRateLimit())
					registerModelRoutes(v1)
					v1.POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
					v1.POST("/responses", relayHandler(FormatOpenAIResponse, false))
					v1.POST("/messages", relayHandler(FormatAnthropic, false))
					r.POST("/v1beta/models/*action", keyAuth(), keyRateLimit(), relayHandler(FormatGemini, false))
					request := func(method, path, body string) *httptest.ResponseRecorder {
						req := httptest.NewRequest(method, path, strings.NewReader(body))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Authorization", "Bearer "+key.Key)
						w := httptest.NewRecorder()
						r.ServeHTTP(w, req)
						return w
					}
					catalog := request("GET", "/v1/models", "")
					if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), `"id":"alias"`) || strings.Contains(catalog.Body.String(), `"id":"mapped"`) {
						t.Fatalf("alias discovery/key filtering: %d %s", catalog.Code, catalog.Body.String())
					}
					denied := request("POST", "/v1/chat/completions", `{"model":"mapped","messages":[{"role":"user","content":"hi"}]}`)
					if denied.Code != 403 || calls != 0 {
						t.Fatal("mapping bypassed key model permissions")
					}
					task := fixtureTask(t, in, stream)
					w := request("POST", task.endpoint, string(task.body))
					if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "Hello") {
						t.Fatalf("relay: %d calls=%d %s", w.Code, calls, w.Body.String())
					}
				})
			}
		}
	}
}
