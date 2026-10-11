package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestKeyQuotasAdminPreservesSpendAndValidatesBudgets(t *testing.T) {
	modelsTestDB(t)
	if err := settingsSet("session_secret", "quota-test-secret"); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerAdmin(r)
	token := makeAdminToken()
	path := currentAdminPath() + "/api/keys"
	result := deploymentJSON(t, deploymentRequest(r, "POST", path, token,
		`{"name":"mixed","models":"a,b,c","quota_usd":10,"model_quotas":{"b":{"quota_usd":5,"used_usd":999}}}`), 200)
	id := int64(result["id"].(float64))
	path = fmt.Sprintf("%s/%d", path, id)
	read := func() *ApiKey {
		t.Helper()
		ak, err := dbGetKeyByID(id)
		if err != nil {
			t.Fatal(err)
		}
		return ak
	}
	if read().ModelQuotas["b"].UsedUSD != 0 {
		t.Fatal("client can override usage")
	}
	if err := dbChargeKey(id, "b", 2, true); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(id, "a", 3, false); err != nil {
		t.Fatal(err)
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"enabled":false,"show_on_home":true}`), 200)
	ak := read()
	if ak.UsedUSD != 5 || ak.SharedUsedUSD != 3 || ak.ModelQuotas["b"].UsedUSD != 2 || ak.Enabled || !ak.ShowOnHome {
		t.Fatalf("partial edit changed balances: %+v", ak)
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"quota_usd":12,"model_quotas":{"b":{"quota_usd":8,"used_usd":0}}}`), 200)
	if ak = read(); ak.ModelQuotas["b"].UsedUSD != 2 || ak.SharedUsedUSD != 3 || ak.ModelQuotas["b"].QuotaUSD != 8 {
		t.Fatalf("budget edit reset spending: %+v", ak)
	}
	// Saving an old editor snapshot after a call completes must not erase the charge.
	if err := dbChargeKey(id, "b", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	if read().ModelQuotas["b"].UsedUSD != 3 {
		t.Fatal("stale edit overwrote billing")
	}
	for _, body := range []string{
		`{"quota_usd":-1}`, `{"model_quotas":{"b":{"quota_usd":-1}}}`,
		`{"model_quotas":{"unbound":{"quota_usd":5}}}`, `{"model_quotas":{" b ":{"quota_usd":5}}}`,
	} {
		deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, body), 400)
		deploymentJSON(t, deploymentRequest(r, "POST", currentAdminPath()+"/api/keys", token, body), 400)
	}
	if read().QuotaUSD != 12 || read().ModelQuotas["b"].QuotaUSD != 8 {
		t.Fatal("invalid update partially changed settings")
	}
	// Mode changes retain independent history; in-flight calls bill their original pool.
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"model_quotas":{}}`), 200)
	if len(read().ModelQuotas) != 0 {
		t.Fatal("clearing budgets did not enable sharing")
	}
	if err := dbChargeKey(id, "b", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(id, "b", 2, false); err != nil {
		t.Fatal(err)
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"model_quotas":{"b":{"quota_usd":0}}}`), 200)
	ak = read()
	if ak.UsedUSD != 9 || ak.SharedUsedUSD != 5 || ak.ModelQuotas["b"].UsedUSD != 4 || ak.quotaExhausted("b") {
		t.Fatalf("mode change lost history or affected wrong pool: %+v", ak)
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"models":"a,c"}`), 200)
	if read().allowsModel("b") || len(read().ModelQuotas) != 0 {
		t.Fatal("unbinding kept model access or active budget")
	}
	deploymentJSON(t, deploymentRequest(r, "PATCH", path, token, `{"models":"a,b,c","model_quotas":{"b":{"quota_usd":8}}}`), 200)
	if read().ModelQuotas["b"].UsedUSD != 4 {
		t.Fatal("rebinding reset historical independent spending")
	}
	byKey, err := dbGetKeyByKey(result["key"].(string))
	keys, listErr := dbListKeys()
	if err != nil || listErr != nil || byKey.ModelQuotas["b"].UsedUSD != 4 || len(keys) != 1 || keys[0].SharedUsedUSD != 5 {
		t.Fatal("authentication/listing lost budget data")
	}
	deploymentJSON(t, deploymentRequest(r, "DELETE", path, token, ""), 200)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_key_model_quotas WHERE key_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleting key left orphaned budgets")
	}
}

func TestKeyQuotasIsolatePoolsAcrossProtocols(t *testing.T) {
	for _, format := range protocolFormats {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", format, stream), func(t *testing.T) {
				modelsTestDB(t)
				previous := getSettings()
				settingsMu.Lock()
				settingsCache.Pricing = map[string]Price{"alias": {In: 1e6, Out: 1e6}, "shared-a": {In: 1e6}, "shared-b": {In: 1e6}}
				settingsMu.Unlock()
				t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
				ak := &ApiKey{Key: "sk-quota", Enabled: true, QPS: 1000, Concurrency: 10, Models: "alias,shared-a,shared-b", QuotaUSD: 17,
					ModelQuotas: map[string]ModelQuota{"alias": {QuotaUSD: 17}}}
				if err := dbInsertKey(ak); err != nil {
					t.Fatal(err)
				}
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["model"] != "real-model" {
						t.Errorf("upstream mapping: %v %v", body, err)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, fixtureSSE(FormatOpenAIChat))
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, protocolResponses[FormatOpenAIChat])
					}
				}))
				defer upstream.Close()
				up := &Upstream{Type: "openai", OpenAIProtocol: "chat", BaseURL: upstream.URL, APIKey: "mock", Enabled: true, Weight: 1,
					Models: "alias,shared-a,shared-b", ModelMap: `{"alias":"real-model","shared-a":"real-model","shared-b":"real-model"}`}
				if err := dbInsertUpstream(up); err != nil {
					t.Fatal(err)
				}
				r := gin.New()
				v1 := r.Group("/v1", keyAuth(), keyRateLimit())
				v1.POST("/chat/completions", relayHandler(FormatOpenAIChat, false))
				v1.POST("/responses", relayHandler(FormatOpenAIResponse, false))
				v1.POST("/messages", relayHandler(FormatAnthropic, false))
				r.Group("/v1beta", keyAuth(), keyRateLimit()).POST("/models/*action", relayHandler(FormatGemini, false))
				registerModelRoutes(v1)
				request := func(model string, status int) {
					t.Helper()
					body := strings.ReplaceAll(protocolRequests[format], `"alias"`, fmt.Sprintf("%q", model))
					if stream && format != FormatGemini {
						body = `{"stream":true,` + body[1:]
					}
					path := nativeEndpoint(format, &canonReq{Model: model, Stream: stream})
					w := deploymentRequest(r, "POST", path, ak.Key, body)
					if w.Code != status {
						t.Fatalf("%s: %d %s", model, w.Code, w.Body.String())
					}
					if status == 200 && stream {
						checkClientStream(t, format, w.Body.String())
					}
				}
				request("alias", 200)
				request("alias", 402) // Never falls back to the untouched shared pool.
				stored, _ := dbGetKeyByID(ak.ID)
				if stored.SharedUsedUSD != 0 || stored.ModelQuotas["alias"].UsedUSD != 17 || stored.UsedUSD != 17 {
					t.Fatalf("independent request affected another pool: %+v", stored)
				}
				request("shared-a", 200)
				request("shared-b", 402) // Two models consume the same shared pool.
				stored, _ = dbGetKeyByID(ak.ID)
				if stored.SharedUsedUSD != 17 || stored.ModelQuotas["alias"].UsedUSD != 17 || stored.UsedUSD != 34 {
					t.Fatalf("shared request affected independent quota: %+v", stored)
				}
				stored.ModelQuotas["alias"] = ModelQuota{QuotaUSD: 34}
				if err := dbUpdateKey(stored); err != nil {
					t.Fatal(err)
				}
				request("alias", 200) // Shared exhaustion must not block an independent model.
				request("alias", 402)
				stored, _ = dbGetKeyByID(ak.ID)
				if stored.SharedUsedUSD != 17 || stored.ModelQuotas["alias"].UsedUSD != 34 || stored.UsedUSD != 51 {
					t.Fatalf("independent top-up accounting: %+v", stored)
				}
				request("real-model", 403)
				stored.Models = ""
				if err := dbUpdateKey(stored); err != nil {
					t.Fatal(err)
				}
				request("alias", 403) // Unbound old keys also fail closed.
				catalog := deploymentJSON(t, deploymentRequest(r, "GET", "/v1/models", ak.Key, ""), 200)
				logs, _ := dbListLogs(20, 0)
				if calls.Load() != 3 || len(logs) != 3 || len(catalog["data"].([]any)) != 0 {
					t.Fatalf("rejected calls reached upstream/billing: calls=%d logs=%d catalog=%v", calls.Load(), len(logs), catalog)
				}
			})
		}
	}
}

func TestKeyQuotasEmbeddingsAndConcurrentBilling(t *testing.T) {
	modelsTestDB(t)
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Pricing = map[string]Price{"embedding": {In: 1e6}}
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	ak := &ApiKey{Key: "sk-embedding-quota", Enabled: true, QPS: 1000, Concurrency: 10, Models: "embedding,shared", QuotaUSD: 1,
		ModelQuotas: map[string]ModelQuota{"embedding": {QuotaUSD: 4}}}
	if err := dbInsertKey(ak); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(ak.ID, "shared", 1, false); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("wrong embeddings path: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
	}))
	defer upstream.Close()
	if err := dbInsertUpstream(&Upstream{Type: "openai", OpenAIProtocol: "responses", BaseURL: upstream.URL, Enabled: true, Weight: 1, Models: "embedding"}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Group("/v1", keyAuth(), keyRateLimit()).POST("/embeddings", relayHandler(FormatOpenAIChat, true))
	deploymentJSON(t, deploymentRequest(r, "POST", "/v1/embeddings", ak.Key, `{"model":"embedding","input":"hi"}`), 200)
	deploymentJSON(t, deploymentRequest(r, "POST", "/v1/embeddings", ak.Key, `{"model":"embedding","input":"hi"}`), 402)
	stored, _ := dbGetKeyByID(ak.ID)
	if calls.Load() != 1 || stored.SharedUsedUSD != 1 || stored.ModelQuotas["embedding"].UsedUSD != 4 {
		t.Fatalf("embedding quota isolation: %+v calls=%d", stored, calls.Load())
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := dbChargeKey(ak.ID, "embedding", 0.25, true); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if err := dbChargeKey(ak.ID, "shared", 0.25, false); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	stored, _ = dbGetKeyByID(ak.ID)
	if stored.UsedUSD != 15 || stored.SharedUsedUSD != 6 || stored.ModelQuotas["embedding"].UsedUSD != 9 {
		t.Fatalf("concurrent billing lost charges: %+v", stored)
	}
}

func TestKeyQuotasPublicOverviewCountsEachActivePoolOnce(t *testing.T) {
	modelsTestDB(t)
	ak := &ApiKey{Key: "sk-public-quota", Enabled: true, ShowOnHome: true, Models: "a,b,c", QuotaUSD: 10,
		ModelQuotas: map[string]ModelQuota{"c": {QuotaUSD: 20}}}
	if err := dbInsertKey(ak); err != nil {
		t.Fatal(err)
	}
	for _, charge := range []struct {
		model       string
		cost        float64
		independent bool
	}{{"a", 3, false}, {"b", 2, false}, {"c", 7, true}} {
		if err := dbChargeKey(ak.ID, charge.model, charge.cost, charge.independent); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(quota, used, remaining float64, unlimited bool) {
		t.Helper()
		got, err := buildPublicOverview(time.Now(), "hour")
		if err != nil || got.Summary.QuotaUSD != quota || got.Summary.UsedUSD != used || got.Summary.Unlimited != unlimited {
			t.Fatalf("summary: %+v %v", got, err)
		}
		if !unlimited && (got.Summary.RemainingUSD == nil || *got.Summary.RemainingUSD != remaining) {
			t.Fatalf("remaining: %+v", got.Summary)
		}
		if unlimited && got.Summary.RemainingUSD != nil {
			t.Fatal("unlimited summary showed finite remaining")
		}
	}
	assert(30, 12, 18, false)
	ak.Models = "c" // The unused shared pool is no longer counted.
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	assert(20, 12, 13, false)
	ak.ModelQuotas["c"] = ModelQuota{QuotaUSD: 0}
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	assert(0, 12, 0, true)
	ak.Models = "" // No bound models means no usable remaining quota.
	if err := dbUpdateKey(ak); err != nil {
		t.Fatal(err)
	}
	assert(0, 12, 0, false)
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		if validQuota(value) {
			t.Fatalf("accepted invalid amount %v", value)
		}
	}
}

func TestKeyQuotasBillingRollsBackOnError(t *testing.T) {
	modelsTestDB(t)
	ak := &ApiKey{Key: "sk-atomic-quota", Models: "model", ModelQuotas: map[string]ModelQuota{"model": {QuotaUSD: 10}}}
	if err := dbInsertKey(ak); err != nil {
		t.Fatal(err)
	}
	// Simulate a storage failure in the second write of the billing transaction.
	if _, err := db.Exec(`CREATE TRIGGER reject_quota_charge BEFORE UPDATE OF used_usd ON api_key_model_quotas
		BEGIN SELECT RAISE(ABORT,'simulated billing error'); END`); err != nil {
		t.Fatal(err)
	}
	if err := dbChargeKey(ak.ID, "model", 2, true); err == nil {
		t.Fatal("simulated storage failure was ignored")
	}
	stored, err := dbGetKeyByID(ak.ID)
	if err != nil || stored.UsedUSD != 0 || stored.SharedUsedUSD != 0 || stored.ModelQuotas["model"].UsedUSD != 0 {
		t.Fatalf("failed transaction charged a partial balance: %+v %v", stored, err)
	}
}
