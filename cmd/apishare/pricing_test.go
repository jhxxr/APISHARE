package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func preservePricingLimits(t *testing.T) {
	t.Helper()
	autoModelLimits.RLock()
	previous := autoModelLimits.data
	autoModelLimits.RUnlock()
	t.Cleanup(func() {
		autoModelLimits.Lock()
		autoModelLimits.data = previous
		autoModelLimits.Unlock()
	})
}

func TestPricingSyncSkipsSchemaAndMalformedEntries(t *testing.T) {
	publicTestDB(t)
	preservePricingLimits(t)
	auditPricingState(t, map[string]Price{"default": {}}, nil, true)
	body := `{
		"sample_spec":{"max_input_tokens":"max input tokens, if the provider specifies it"},
		"metadata":"catalog description",
		"openai/test-model":{"input_cost_per_token":"0.0000025","output_cost_per_token":0.00001,"cache_read_input_token_cost":"0","max_input_tokens":"128000","max_output_tokens":"4.096e3"},
		"free-model":{"input_cost_per_token":0,"output_cost_per_token":0,"max_input_tokens":8192},
		"invalid-number":{"input_cost_per_token":"not a number"},
		"invalid-limit":{"input_cost_per_token":0.001,"max_input_tokens":4.5},
		"invalid-infinity":{"input_cost_per_token":"Inf"},
		"invalid-negative":{"input_cost_per_token":-1},
		"null-entry":null
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
	defer server.Close()
	settingsMu.Lock()
	settingsCache.AutoPricingURL = server.URL
	settingsMu.Unlock()
	n, err := refreshAutoPricing()
	if err != nil || n != 2 {
		t.Fatalf("sync should keep the two valid models: count=%d err=%v", n, err)
	}
	price, source := lookupPrice("test-model")
	if price.In != 2.5 || math.Abs(price.Out-10) > 1e-12 || price.CacheRead == nil || *price.CacheRead != 0 || source != "official" {
		t.Fatalf("numeric/string rates were not preserved: %+v %s", price, source)
	}
	if limit, ok := lookupAutoModelLimits("test-model"); !ok || limit.ContextLimit != 128000 || limit.MaxOutput != 4096 {
		t.Fatalf("string token limits were not parsed: %+v %v", limit, ok)
	}
	autoPricing.RLock()
	if count := autoPricingModelCount(autoPricing.data); count != n {
		t.Errorf("model count includes internal aliases: %d != %d", count, n)
	}
	at := autoPricing.fetchedAt
	autoPricing.RUnlock()
	// A broken/empty download must preserve the last working snapshot.
	for _, invalid := range []string{`{"sample_spec":{"max_input_tokens":"description"}}`, `{"broken":`} {
		body = invalid
		if _, err := refreshAutoPricing(); err == nil {
			t.Fatal("invalid/empty catalog was accepted")
		}
		if got, _ := lookupPrice("test-model"); got.In != 2.5 {
			t.Fatal("failed refresh discarded the previous prices")
		}
	}
	autoPricing.Lock()
	autoPricing.data, autoPricing.fetchedAt = nil, ""
	autoPricing.Unlock()
	autoModelLimits.Lock()
	autoModelLimits.data = nil
	autoModelLimits.Unlock()
	loadAutoPricingCache()
	if got, source := lookupPrice("test-model"); source != "official" || got.In != 2.5 {
		t.Fatalf("restart lost normalized price indexes: %+v %s", got, source)
	}
	autoPricing.RLock()
	if autoPricing.fetchedAt != at {
		t.Error("restart lost the last successful update time")
	}
	autoPricing.RUnlock()
	if limit, ok := lookupAutoModelLimits("test-model"); !ok || limit.MaxOutput != 4096 {
		t.Fatalf("restart lost token limits: %+v %v", limit, ok)
	}
}

func TestPricingAdminEditResetAndCatalog(t *testing.T) {
	publicTestDB(t)
	auditPricingState(t, map[string]Price{"default": {}, "keep-model": {In: 7, Out: 8}}, indexAutoPrices(map[string]Price{"openai/test-model": {In: 2, Out: 9}}), true)
	if err := settingsSet("session_secret", "pricing-test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := dbInsertUpstream(&Upstream{Name: "pricing-test", Type: "openai", Enabled: true, Weight: 1, Models: "test-model,alias-one,alias-multiple", ModelMap: `{"alias-one":"test-model","alias-multiple":"test-model"}`}); err != nil {
		t.Fatal(err)
	}
	if err := dbInsertUpstream(&Upstream{Name: "pricing-second", Type: "openai", Enabled: true, Weight: 1, Models: "alias-multiple", ModelMap: `{"alias-multiple":"keep-model"}`}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerPricingAdminRoutes(r.Group("/api", adminAuth()))
	token := makeAdminToken()
	if got := deploymentRequest(r, "PUT", "/api/settings/pricing/model", "", `{"model":"test-model","price":{"in":0,"out":0}}`); got.Code != 401 {
		t.Fatal("pricing edit must require administrator authentication")
	}
	for _, payload := range []string{
		`{"model":"test-model","price":{"in":0,"out":0,"cache_read":0}}`,
		`{"model":"vendor/deepseek-flash-fast","price":{"in":0.3,"out":1.2}}`,
	} {
		deploymentJSON(t, deploymentRequest(r, "PUT", "/api/settings/pricing/model", token, payload), 200)
	}
	price, source := lookupPrice("test-model")
	if source != "override" || price.In != 0 || price.Out != 0 || price.CacheRead == nil || *price.CacheRead != 0 {
		t.Fatalf("explicit free override lost: %+v %s", price, source)
	}
	s := getSettings()
	if s.Pricing["keep-model"].In != 7 || s.Pricing["deepseek-flash"].Out != 1.2 {
		t.Fatal("row save overwrote another model or missed canonical Flash pricing")
	}
	blob, _ := settingsGet("pricing")
	var persisted map[string]Price
	if err := json.Unmarshal([]byte(blob), &persisted); err != nil || persisted["keep-model"].In != 7 {
		t.Fatalf("pricing was not persisted: %s %v", blob, err)
	}
	for _, payload := range []string{
		`{"model":"","price":{"in":1,"out":2}}`,
		`{"model":"test-model","price":{"in":-1,"out":2}}`,
		`{"model":"test-model","price":{"in":1}}`,
		`{"model":"test-model","price":{"in":1,"out":2,"unknown":3}}`,
		`{"model":"test-model","price":null}`,
	} {
		deploymentJSON(t, deploymentRequest(r, "PUT", "/api/settings/pricing/model", token, payload), 400)
	}
	result := deploymentJSON(t, deploymentRequest(r, "GET", "/api/settings/pricing", token, ""), 200)
	if result["auto_pricing"].(map[string]any)["model_count"] != float64(1) {
		t.Fatal("catalog count includes normalized aliases")
	}
	found, aliasFound, variantsFound := false, false, false
	for _, entry := range result["models"].([]any) {
		row := entry.(map[string]any)
		if strings.HasPrefix(row["model"].(string), "bare:") {
			t.Fatal("internal alias leaked into the table")
		}
		if row["model"] == "test-model" {
			found = row["connected"] == true && row["source"] == "override" && row["override"] == true
		}
		if row["model"] == "alias-one" {
			aliasFound = row["pricing_model"] == "test-model" && row["source"] == "override" && row["override"] == false
		}
		if row["model"] == "alias-multiple" {
			variantsFound = row["source"] == "upstream" && len(row["variants"].([]any)) == 2
		}
	}
	if !found {
		t.Fatal("connected model or effective override is missing from catalog")
	}
	if !aliasFound || !variantsFound {
		t.Fatal("catalog misrepresents upstream fallback billing for mapped client aliases")
	}
	deploymentJSON(t, deploymentRequest(r, "DELETE", "/api/settings/pricing/model?model=default", token, ""), 400)
	deploymentJSON(t, deploymentRequest(r, "DELETE", "/api/settings/pricing/model?model=test-model", token, ""), 200)
	if price, source := lookupPrice("test-model"); source != "official" || price.In != 2 {
		t.Fatalf("reset failed to restore official pricing: %+v %s", price, source)
	}
}
