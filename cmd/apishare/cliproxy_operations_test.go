package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"apishare/internal/cliproxy"
)

func TestPoolAccountOperationsKeepMembershipAndSettings(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	create := func(name string) int64 {
		t.Helper()
		result := proxyAdminRequest(router, "POST", "upstreams", fmt.Sprintf(`{"name":%q,"type":"cliproxy"}`, name), true)
		var payload struct{ ID int64 }
		if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &payload) != nil || payload.ID == 0 {
			t.Fatalf("create: %d %s", result.Code, result.Body.String())
		}
		return payload.ID
	}
	a, b := create("source"), create("destination")
	path := func(id int64, suffix string) string { return fmt.Sprintf("upstreams/%d/cliproxy/%s", id, suffix) }
	importFile := func(id int64, name, email string) {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"proxy_id":0,"credential":{"type":"codex","email":%q,"access_token":"PRIVATE-TOKEN","disabled":true,"priority":4,"weight":2,"label":"operator label","use_siwc":true,"siwc":{"access_token":"PRIVATE-SIWC","expires_at":"2099-01-01T00:00:00Z"}}}`, name, email)
		if result := proxyAdminRequest(router, "POST", path(id, "credentials/import"), body, true); result.Code != 200 {
			t.Fatalf("import: %d %s", result.Code, result.Body.String())
		}
	}
	importFile(a, "first.json", "first@example.invalid")
	importFile(a, "conflict.json", "conflict@example.invalid")
	importFile(b, "existing.json", "conflict@example.invalid")
	var managerA, managerB *cliproxy.Manager
	managerA, _ = cliProxyPool(a)
	managerB, _ = cliProxyPool(b)
	batch := func(action, names, extra string) []poolAccountResult {
		t.Helper()
		body := `{"action":"` + action + `","names":` + names + extra + `}`
		result := proxyAdminRequest(router, "POST", path(a, "credentials/batch"), body, true)
		var payload struct {
			Results []poolAccountResult `json:"results"`
		}
		if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &payload) != nil {
			t.Fatalf("batch: %d %s", result.Code, result.Body.String())
		}
		return payload.Results
	}
	results := batch("disable", `["first.json","missing.json","first.json"]`, "")
	if len(results) != 2 || !results[0].OK || results[1].OK {
		t.Fatalf("partial result/dedup: %+v", results)
	}
	for _, body := range []string{`{"name":"first.json","weight":0}`, `{"name":"first.json","priority":-1}`, `{"name":"first.json","location":"us-east5"}`} {
		if result := proxyAdminRequest(router, "PATCH", path(a, "credentials/settings"), body, true); result.Code != 400 {
			t.Fatalf("invalid settings accepted: %d %s", result.Code, result.Body.String())
		}
	}
	if result := proxyAdminRequest(router, "PATCH", path(a, "credentials/settings"), `{"name":"first.json","priority":30,"weight":5,"websockets":true}`, true); result.Code != 200 {
		t.Fatalf("settings: %d %s", result.Code, result.Body.String())
	}
	proxy := testSavedProxy(t, "http://proxy.invalid:1080", "", "")
	results = batch("proxy", `["first.json"]`, fmt.Sprintf(`,"proxy_id":%d`, proxy.ID))
	if !results[0].OK {
		t.Fatalf("batch proxy: %+v", results)
	}
	results = batch("move", `["first.json","conflict.json","existing.json"]`, fmt.Sprintf(`,"target_pool_id":%d`, b))
	if len(results) != 3 || !results[0].OK || results[1].OK || results[2].OK {
		t.Fatalf("migration/conflict/scope: %+v", results)
	}
	source, _ := managerA.ReadCredentials()
	destination, _ := managerB.ReadCredentials()
	if source["first.json"] != nil || source["conflict.json"] == nil || destination["first.json"] == nil || destination["conflict.json"] != nil {
		t.Fatal("migration lost data or overwrote destination conflict")
	}
	var document map[string]any
	json.Unmarshal(destination["first.json"], &document)
	if document["priority"] != float64(30) || document["weight"] != float64(5) || document["label"] != "operator label" || document["disabled"] != true || document["use_siwc"] != true || document["websockets"] != true ||
		accountProxyID(b, "first.json") != proxy.ID {
		t.Fatalf("migration lost operator settings: %+v", document)
	}
	if err := managerA.Stop(); err != nil {
		t.Fatal(err)
	}
	results = batch("enable", `["conflict.json"]`, "")
	if !results[0].OK {
		t.Fatalf("stopped enable: %+v", results)
	}
	results = batch("disable", `["conflict.json"]`, "")
	if !results[0].OK {
		t.Fatalf("stopped disable: %+v", results)
	}
	results = batch("refresh", `["conflict.json"]`, "")
	if results[0].OK || strings.Contains(results[0].Message, "PRIVATE") {
		t.Fatalf("stopped refresh: %+v", results)
	}
	results = batch("move", `["conflict.json"]`, fmt.Sprintf(`,"target_pool_id":%d`, b))
	if results[0].OK {
		t.Fatal("stopped migration accepted")
	}
}

func TestPoolVertexRegionAndAccountLogQueries(t *testing.T) {
	router := testProxyAdminRouter(t)
	withTestCLIProxyPools(t)
	response := proxyAdminRequest(router, "POST", "upstreams", `{"name":"vertex settings","type":"cliproxy"}`, true)
	var pool struct{ ID int64 }
	json.Unmarshal(response.Body.Bytes(), &pool)
	if response.Code != 200 || pool.ID == 0 {
		t.Fatal(response.Body.String())
	}
	path := fmt.Sprintf("upstreams/%d/cliproxy/", pool.ID)
	credential := `{"name":"vertex.json","credential":{"type":"vertex","disabled":true,"email":"vertex@example.invalid","project_id":"project","location":"europe-west4","service_account":{"type":"service_account","project_id":"project","client_email":"vertex@example.invalid","private_key":"PRIVATE-KEY"}}}`
	if result := proxyAdminRequest(router, "POST", path+"credentials/import", credential, true); result.Code != 200 {
		t.Fatalf("region import: %d %s", result.Code, result.Body.String())
	}
	for _, region := range []string{"asia-northeast1", "global"} {
		body := fmt.Sprintf(`{"name":"vertex.json","location":%q,"weight":2,"priority":1}`, region)
		if result := proxyAdminRequest(router, "PATCH", path+"credentials/settings", body, true); result.Code != 200 {
			t.Fatalf("region patch: %d %s", result.Code, result.Body.String())
		}
	}
	bad := strings.Replace(credential, "europe-west4", "https://invalid", 1)
	if result := proxyAdminRequest(router, "POST", path+"credentials/import", bad, true); result.Code != 400 {
		t.Fatal("unsafe region accepted")
	}
	manager, _ := cliProxyPool(pool.ID)
	manager.Stop()
	result := proxyAdminRequest(router, "GET", path+"credentials", "", true)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"location":"global"`) || strings.Contains(result.Body.String(), "PRIVATE") {
		t.Fatalf("offline region and secret projection: %d %s", result.Code, result.Body.String())
	}
	if result := proxyAdminRequest(router, "GET", path+"attempts?account=vertex.json", "", false); result.Code != 401 {
		t.Fatal("account logs exposed without admin authentication")
	}
	result = proxyAdminRequest(router, "GET", path+"attempts?account=vertex.json", "", true)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"items":[]`) {
		t.Fatalf("offline logs: %d %s", result.Code, result.Body.String())
	}
	trace := "1123456789abcdef0123456789abcdef"
	_, err := db.Exec(`INSERT INTO call_logs(model,status,trace_id,created_at) VALUES('vertex-test',200,?,?)`, trace, time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	query, _ := parseLogQuery(nil)
	logs, err := queryLogs(context.Background(), query)
	if err != nil || len(logs.Items) != 1 || logs.Items[0].TraceID != trace {
		t.Fatalf("gateway trace roundtrip: %+v %v", logs, err)
	}
}
