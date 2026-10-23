package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPoolQuotaRoutesKeepScopeAndStoppedPreference(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	a, manager := jobTestPool(t, router, "quota source")
	b, _ := jobTestPool(t, router, "quota other")
	if err := manager.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SeedCredentials(map[string]json.RawMessage{"quota.json": json.RawMessage(`{"type":"codex","email":"one@example.invalid","access_token":"PRIVATE-TOKEN"}`)}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("upstreams/%d/cliproxy/credentials/quota?name=quota.json", a)
	if response := proxyAdminRequest(router, "GET", path, "", false); response.Code != 401 {
		t.Fatal("quota exposed without admin authentication")
	}
	if response := proxyAdminRequest(router, "GET", fmt.Sprintf("upstreams/%d/cliproxy/credentials/quota?name=quota.json", b), "", true); response.Code != 404 {
		t.Fatal("quota lookup crossed pools")
	}
	response := proxyAdminRequest(router, "GET", path+"&refresh=1&url=https://arbitrary.invalid", "", true)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"error":"pool_stopped"`) || strings.Contains(response.Body.String(), "PRIVATE") {
		t.Fatalf("stopped quota view: %d %s", response.Code, response.Body.String())
	}
	if manager.Running() || manager.Status().AutoStart {
		t.Fatal("querying quota restarted a manually stopped pool")
	}
}
