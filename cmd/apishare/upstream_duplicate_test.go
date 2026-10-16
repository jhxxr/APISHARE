package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamDuplicateCopiesFullConfiguration(t *testing.T) {
	r := testProxyAdminRouter(t)
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := proxyAdminRequest(r, method, path, body, true)
		if w.Code != status {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	duplicate := func(id int64, body string) (int64, string) {
		t.Helper()
		w := request("POST", fmt.Sprintf("upstreams/%d/duplicate", id), body, 200)
		var result struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.ID == 0 || result.ID == id {
			t.Fatalf("duplicate id = %d (source %d)", result.ID, id)
		}
		return result.ID, result.Name
	}

	var created struct{ ID int64 }
	body := `{"name":"主账号","type":"openai","openai_protocol":"chat","base_url":"https://example.invalid/v1","api_key":"secret-key","weight":7,"models":"model-a,model-b","model_map":"{\"alias\":\"real\"}","header_overrides":"{\"X-Test\":\"1\"}","structured_output":"json_object"}`
	if err := json.Unmarshal(request("POST", "upstreams", body, 200).Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	src, _ := dbGetUpstream(created.ID)

	dupID, dupName := duplicate(created.ID, `{"name":"备用账号"}`)
	if dupName != "备用账号" {
		t.Fatalf("duplicate name = %q", dupName)
	}
	copyUp, err := dbGetUpstream(dupID)
	if err != nil {
		t.Fatal(err)
	}
	if copyUp.CreatedAt == "" {
		t.Fatal("copy has no creation time")
	}
	if copyUp.Type != src.Type || copyUp.BaseURL != src.BaseURL || copyUp.APIKey != src.APIKey ||
		copyUp.Weight != src.Weight || copyUp.Models != src.Models || copyUp.ModelMap != src.ModelMap ||
		copyUp.HeaderOverrides != src.HeaderOverrides || copyUp.OpenAIProtocol != src.OpenAIProtocol ||
		copyUp.StructuredOutput != src.StructuredOutput || copyUp.ProxyID != src.ProxyID || copyUp.Enabled != src.Enabled {
		t.Fatalf("copy lost configuration: %+v vs %+v", copyUp, src)
	}
	if copyUp.APIKey != "secret-key" || !copyUp.Enabled {
		t.Fatalf("copy key/enabled = %q %v", copyUp.APIKey, copyUp.Enabled)
	}
	// The new channel must be routable immediately (snapshot invalidation).
	selected, err := pickUpstream(FormatOpenAIChat, "model-a", map[int64]bool{src.ID: true}, false)
	if err != nil || selected.ID != dupID || selected.mapModel("alias") != "real" {
		t.Fatalf("duplicate did not join routing: %+v %v", selected, err)
	}
	// The list must expose the copy with its masked key and no leaked secret.
	listed := request("GET", "upstreams", "", 200)
	if !strings.Contains(listed.Body.String(), "备用账号") || strings.Contains(listed.Body.String(), "secret-key") {
		t.Fatalf("upstream list lost the copy or leaked the key: %s", listed.Body.String())
	}
	// An empty name falls back to the source name plus a suffix.
	if _, name := duplicate(created.ID, `{}`); name != "主账号 副本" {
		t.Fatalf("default duplicate name = %q", name)
	}
	// A disabled source stays disabled after duplication.
	request("PATCH", fmt.Sprintf("upstreams/%d", created.ID), `{"enabled":false}`, 200)
	disabledID, _ := duplicate(created.ID, `{"name":"停用副本"}`)
	if copyUp, err := dbGetUpstream(disabledID); err != nil || copyUp.Enabled {
		t.Fatalf("disabled source duplicate = %+v %v", copyUp, err)
	}
	request("PATCH", fmt.Sprintf("upstreams/%d", created.ID), `{"enabled":true}`, 200)
	// Deleting the source leaves the copy intact.
	request("DELETE", fmt.Sprintf("upstreams/%d", created.ID), "", 200)
	if _, err := dbGetUpstream(dupID); err != nil {
		t.Fatalf("copy disappeared with its source: %v", err)
	}
	// Unnamed sources still get a usable name.
	var unnamed struct{ ID int64 }
	if err := json.Unmarshal(request("POST", "upstreams", `{"type":"gemini","base_url":"https://gemini.invalid","api_key":"k"}`, 200).Body.Bytes(), &unnamed); err != nil {
		t.Fatal(err)
	}
	if _, name := duplicate(unnamed.ID, `{"name":"   "}`); name != "未命名上游 副本" {
		t.Fatalf("unnamed source duplicate name = %q", name)
	}
	// Unknown sources and unauthenticated callers are rejected.
	if w := proxyAdminRequest(r, "POST", "upstreams/999999/duplicate", `{}`, true); w.Code != http.StatusNotFound {
		t.Fatalf("unknown source: %d", w.Code)
	}
	if w := proxyAdminRequest(r, "POST", fmt.Sprintf("upstreams/%d/duplicate", unnamed.ID), `{}`, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated duplicate: %d", w.Code)
	}
	if w := proxyAdminRequest(r, "POST", fmt.Sprintf("upstreams/%d/duplicate", unnamed.ID), `{"name":123}`, true); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid name payload: %d", w.Code)
	}
}
