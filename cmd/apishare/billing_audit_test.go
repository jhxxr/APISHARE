package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func auditRate(value float64) *float64 { return &value }

func auditPricingState(t *testing.T, manual map[string]Price, automatic map[string]Price, enabled bool) {
	t.Helper()
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Pricing = manual
	settingsCache.AutoPricingEnabled = enabled
	settingsMu.Unlock()
	autoPricing.Lock()
	oldData, oldAt := autoPricing.data, autoPricing.fetchedAt
	autoPricing.data = automatic
	autoPricing.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsCache = previous
		settingsMu.Unlock()
		autoPricing.Lock()
		autoPricing.data, autoPricing.fetchedAt = oldData, oldAt
		autoPricing.Unlock()
	})
}

func TestBillingAuditDeepSeekFlashCanonicalPrice(t *testing.T) {
	for _, sample := range []struct {
		name                string
		manual              map[string]Price
		automatic           map[string]Price
		enabled             bool
		source              string
		input, output, read float64
	}{
		{"manual", map[string]Price{"deepseek-flash": {In: .5, Out: 2, CacheRead: auditRate(.01)}, "deepseek-v4.1-flash": {In: 99, Out: 99}}, nil, true, "override", .5, 2, .01},
		{"automatic", map[string]Price{"default": {}}, map[string]Price{"deepseek/deepseek-flash": {In: .3, Out: 1.2, CacheRead: auditRate(.006)}, "deepseek-v4.1-flash": {}}, true, "official", .3, 1.2, .006},
		{"offline", map[string]Price{"default": {}}, nil, false, "builtin", .3, 1.2, .006},
	} {
		t.Run(sample.name, func(t *testing.T) {
			auditPricingState(t, sample.manual, sample.automatic, sample.enabled)
			for _, name := range []string{"deepseek-flash", "deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4-flash-vision-exp", "DeepSeek-V4.1-Flash", "deepseek/flash", "provider/deepseek-new-flash"} {
				price, source, model := lookupRelayPrice(name, "mapped-model")
				if model != "deepseek-flash" || source != sample.source || price.In != sample.input || price.Out != sample.output || price.CacheRead == nil || *price.CacheRead != sample.read {
					t.Fatalf("wrong canonical price for %q: %+v / %s / %s", name, price, source, model)
				}
				lookup, lookupSource := lookupPrice(name)
				if lookup.In != price.In || lookupSource != source || priceFor(name, 100, 10) <= 0 {
					t.Fatal("admin lookup or legacy price calculation bypassed canonical billing")
				}
			}
			for _, name := range []string{"deepseek-reasoner", "gemini-flash", "gpt-4.1"} {
				if canonicalPricingModel(name) != name {
					t.Fatal("unrelated model was redirected to Flash pricing")
				}
			}
		})
	}
}

func TestBillingAuditMappedPricePrecedesDefaultButPreservesExplicitFree(t *testing.T) {
	auditPricingState(t, map[string]Price{"default": {}, "deepseek-flash": {In: .5, Out: 2}, "free-alias": {}}, nil, false)
	price, source, model := lookupRelayPrice("custom-alias", "deepseek-v4.1-flash")
	if source != "override" || model != "deepseek-flash" || price.In != .5 {
		t.Fatal("mapped model lost its rate to a zero global default")
	}
	price, source, model = lookupRelayPrice("free-alias", "deepseek-v4.1-flash")
	if source != "override" || model != "free-alias" || price.In != 0 || price.Out != 0 {
		t.Fatal("explicit free client override was ignored")
	}
	price, source, model = lookupRelayPrice("unknown-model", "unknown-upstream")
	if source != "default" || model != "default" || price.In != 0 {
		t.Fatal("unknown models lost configured default behavior")
	}
}

func TestBillingAuditFlashRateChargesOriginalQuotaPool(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "independent"}[independent], func(t *testing.T) {
			publicTestDB(t)
			auditPricingState(t, map[string]Price{"default": {}, "deepseek-flash": {In: .5, Out: 2, CacheRead: auditRate(.01)}}, nil, false)
			model := "deepseek-v4.1-flash"
			key := &ApiKey{Name: "flash", Key: "flash-key", Models: model, QuotaUSD: 1, Enabled: true}
			if independent {
				key.ModelQuotas = map[string]ModelQuota{model: {QuotaUSD: 1}}
			}
			if err := dbInsertKey(key); err != nil {
				t.Fatal(err)
			}
			info := relayInfo{Status: 200, UpstreamModel: "deepseek-v4-flash"}
			info.mergeUsage(upstreamUsage{Source: FormatOpenAIChat, Prompt: 10, Completion: 10, InKnown: true, OutKnown: true, CacheRead: 8, CacheReadKnown: true})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			finalizeCall(c, *key, &relayTask{model: model, format: FormatOpenAIChat}, info, true, nil, -1)
			logs, err := dbListLogs(1, 0)
			expected := 21.08 / 1e6
			pool := map[bool]string{false: "shared", true: "independent"}[independent]
			if err != nil || len(logs) != 1 || logs[0].Billing == nil || math.Abs(logs[0].CostUSD-expected) > 1e-12 || logs[0].Model != model || logs[0].Billing.PricingModel != "deepseek-flash" || logs[0].Billing.QuotaPool != pool || logs[0].Billing.UpstreamModel != info.UpstreamModel {
				t.Fatalf("Flash debit receipt is inconsistent: %+v / %v", logs, err)
			}
			updated, err := dbGetKeyByID(key.ID)
			if err != nil || math.Abs(updated.UsedUSD-expected) > 1e-12 {
				t.Fatal("positive receipt failed to debit total usage")
			}
			if independent {
				budget, exists := updated.ModelQuotas[model]
				if !exists || budget.QuotaUSD != 1 || math.Abs(budget.UsedUSD-expected) > 1e-12 || updated.SharedUsedUSD != 0 {
					t.Fatal("canonical pricing charged the wrong independent pool")
				}
			} else if math.Abs(updated.SharedUsedUSD-expected) > 1e-12 {
				t.Fatal("shared pool missed canonical Flash debit")
			}
			var wrongPool int
			if err := db.QueryRow(`SELECT COUNT(*) FROM api_key_model_quotas WHERE key_id=? AND model='deepseek-flash'`, key.ID).Scan(&wrongPool); err != nil || wrongPool != 0 {
				t.Fatal("canonical price name created a second quota pool")
			}
		})
	}
}

func TestBillingAuditDisjointProviderCounters(t *testing.T) {
	for _, sample := range []struct {
		format                                clientFormat
		body                                  string
		input, output, read, write, reasoning int
	}{
		{FormatAnthropic, `{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}`, 45, 25, 30, 5, 0},
		{FormatAnthropic, `{"input_tokens":173306,"output_tokens":166,"cache_read_input_tokens":173056,"prompt_tokens":173306,"cached_tokens":173056}`, 173306, 166, 173056, 0, 0},
		{FormatAnthropic, `{"input_tokens":1200,"output_tokens":30,"prompt_tokens":1200,"prompt_tokens_details":{"cached_tokens":800}}`, 1200, 30, 800, 0, 0},
		{FormatAnthropic, `{"input_tokens":1200,"output_tokens":30,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}`, 1200, 30, 800, 0, 0},
		{FormatOpenAIChat, `{"prompt_tokens":1200,"completion_tokens":30,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":400}`, 1200, 30, 800, 0, 0},
		{FormatGemini, `{"promptTokenCount":100,"candidatesTokenCount":20,"cachedContentTokenCount":30,"thoughtsTokenCount":5,"toolUsePromptTokenCount":2}`, 102, 25, 30, 0, 5},
		{FormatOpenAIChat, `{"prompt_tokens":32,"completion_tokens":9,"total_tokens":135,"completion_tokens_details":{"reasoning_tokens":94}}`, 32, 103, 0, 0, 94},
		{FormatOpenAIChat, `{"prompt_tokens":32,"completion_tokens":103,"total_tokens":135,"completion_tokens_details":{"reasoning_tokens":94}}`, 32, 103, 0, 0, 94},
		{FormatOpenAIResponse, `{"prompt_tokens":32,"completion_tokens":9,"total_tokens":151,"completion_tokens_details":{"reasoning_tokens":110}}`, 32, 119, 0, 0, 110},
	} {
		body, _ := decodeProtocolObject([]byte(sample.body))
		usage := readUsage(body, sample.format)
		counts := normalizeBillingUsage(usage)
		if counts.inputTotal() != sample.input || counts.outputTotal() != sample.output || counts.CacheRead != sample.read || counts.CacheWrite != sample.write || counts.Reasoning != sample.reasoning {
			t.Fatalf("wrong %s partition: %+v", sample.format, counts)
		}
		response := renderProtocolUsage(FormatOpenAIResponse, usage)
		if response["input_tokens"] != counts.inputTotal() || response["output_tokens"] != counts.outputTotal() {
			t.Fatal("response usage and debit partition disagree")
		}
	}
	// Missing total is not evidence for adding reasoning to an inclusive count.
	counts := normalizeBillingUsage(upstreamUsage{Source: FormatOpenAIChat, Prompt: 32, Completion: 103, Reasoning: 94, InKnown: true, OutKnown: true, ReasoningKnown: true})
	if counts.outputTotal() != 103 {
		t.Fatal("unknown total caused duplicate reasoning charge")
	}
	for _, invalid := range []any{nil, -1, 1.5, "7", math.Inf(1)} {
		if readUsage(map[string]any{"input_tokens": invalid}, FormatAnthropic).InKnown {
			t.Fatal("invalid usage counter masqueraded as genuine zero")
		}
	}
	if !readUsage(map[string]any{"input_tokens": 0}, FormatAnthropic).InKnown {
		t.Fatal("Go integer zero lost presence")
	}
	start := readUsage(map[string]any{"input_tokens": 173306, "prompt_tokens": 173306}, FormatAnthropic)
	start.merge(readUsage(map[string]any{"input_tokens": 0, "cache_read_input_tokens": 173306, "output_tokens": 8}, FormatAnthropic))
	if counts := normalizeBillingUsage(start); counts.Input != 0 || counts.inputTotal() != 173306 {
		t.Fatal("stale inclusive start total doubled the final cached input")
	}
}

func TestBillingAuditCacheTTLBoundsAndPartialUpdates(t *testing.T) {
	body, _ := decodeProtocolObject([]byte(`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":8}}`))
	usage := readUsage(body, FormatAnthropic)
	counts := normalizeBillingUsage(usage)
	if counts.CacheWrite != 10 || counts.CacheWrite5m != 6 || counts.CacheWrite1h != 4 || counts.inputTotal() != 10 {
		t.Fatalf("aggregate counted twice: %+v", counts)
	}
	usage.merge(readUsage(map[string]any{"output_tokens": 0}, FormatAnthropic))
	if normalizeBillingUsage(usage).CacheWrite1h != 4 {
		t.Fatal("partial usage erased cache TTL")
	}
	usage.merge(readUsage(map[string]any{"cache_creation_input_tokens": 0}, FormatAnthropic))
	if counts := normalizeBillingUsage(usage); counts.CacheWrite != 0 || counts.CacheWrite1h != 0 {
		t.Fatal("explicit zero did not own the aggregate")
	}
	missing := readUsage(map[string]any{"input_tokens": 0, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 8}}, FormatAnthropic)
	if counts := normalizeBillingUsage(missing); counts.CacheWrite != 8 || counts.CacheWrite1h != 8 {
		t.Fatal("TTL-only metadata lost")
	}
}

func TestBillingAuditNativeAggregationDoesNotRestoreStaleInclusiveAliases(t *testing.T) {
	frames := auditFormatFrames(
		`{"type":"message_start","message":{"type":"message","content":[],"usage":{"input_tokens":1200,"prompt_tokens":1200}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":30,"cache_read_input_tokens":800}}`,
		`{"type":"message_stop"}`,
	)
	info := relayInfo{}
	body, err := collectNativeProtocolSSE(strings.NewReader(frames), FormatAnthropic, &info)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := decodeProtocolObject(body)
	usage := readUsage(object(root["usage"]), FormatAnthropic)
	if normalizeBillingUsage(usage).inputTotal() != 800 || normalizeBillingUsage(info.protocolUsage()).inputTotal() != 800 {
		t.Fatalf("buffered/native input totals disagree: %s / %+v", body, info.protocolUsage())
	}
}

func TestBillingAuditOptionalZeroRatesAndPriceSnapshot(t *testing.T) {
	usage := billingUsage{Input: 10, Output: 7, CacheRead: 100, CacheWrite: 40, CacheWrite5m: 30, CacheWrite1h: 10, Reasoning: 3}
	price := Price{In: 2, Out: 8, CacheRead: auditRate(.2), CacheWrite: auditRate(2.5), CacheWrite1h: auditRate(4)}
	cost := costForUsage(usage, price)
	if math.Abs(cost.Total-235.0/1e6) > 1e-12 {
		t.Fatalf("TTL/partition rate wrong: %+v", cost)
	}
	price.CacheRead = auditRate(0)
	if cost := costForUsage(usage, price); cost.CacheRead != 0 {
		t.Fatal("explicit free cache rate used fallback")
	}
	price.CacheRead = nil
	if cost := costForUsage(usage, price); math.Abs(cost.CacheRead-200.0/1e6) > 1e-12 {
		t.Fatal("missing cache rate became free")
	}
	if costForUsage(usage, Price{In: -1}).Total != 0 || validPrice(Price{In: math.Inf(1)}) {
		t.Fatal("invalid rates accepted")
	}
	encoded, _ := json.Marshal(Price{In: 2, Out: 8, CacheRead: auditRate(0)})
	var restored Price
	json.Unmarshal(encoded, &restored)
	if restored.CacheRead == nil || *restored.CacheRead != 0 || restored.CacheWrite != nil {
		t.Fatal("persisted optional rates lost zero/absent distinction")
	}
	for _, body := range []string{`null`, `{}`, `{"in":null,"out":8}`, `{"in":2,"out":8,"cash_read":0}`} {
		if json.Unmarshal([]byte(body), &restored) == nil {
			t.Fatal("null or misspelled model pricing accepted")
		}
	}
}

func TestBillingAuditEstimatedCountsDoNotAddKnownSubcategoriesAgain(t *testing.T) {
	usage := upstreamUsage{Source: FormatAnthropic, Prompt: 100, Completion: 20, CacheRead: 30, CacheWrite: 10, CacheReadKnown: true, CacheWriteKnown: true, Reasoning: 5, ReasoningKnown: true}
	counts := normalizeBillingUsage(usage)
	if counts.inputTotal() != 100 || counts.outputTotal() != 20 || counts.Input != 60 || counts.Output != 15 || !counts.EstimatedInput || !counts.EstimatedOutput {
		t.Fatalf("estimated total inflated: %+v", counts)
	}
}

func TestBillingAuditDebitReceiptAndIndependentQuotaCommitTogether(t *testing.T) {
	publicTestDB(t)
	key := &ApiKey{Name: "receipt", Key: "receipt-key", Models: "alias", Enabled: true, ModelQuotas: map[string]ModelQuota{"alias": {QuotaUSD: 1}}}
	if err := dbInsertKey(key); err != nil {
		t.Fatal(err)
	}
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.Pricing = map[string]Price{"alias": {In: 2, Out: 8, CacheRead: auditRate(.2), CacheWrite: auditRate(2.5), CacheWrite1h: auditRate(4)}}
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	usage := upstreamUsage{Source: FormatAnthropic, Prompt: 10, Completion: 10, InKnown: true, OutKnown: true, CacheRead: 100, CacheReadKnown: true, CacheWrite: 40, CacheWriteKnown: true, CacheWrite5m: 30, CacheWrite5mKnown: true, CacheWrite1h: 10, CacheWrite1hKnown: true}
	info := relayInfo{Status: 200}
	info.mergeUsage(usage)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	finalizeCall(c, *key, &relayTask{model: "alias", format: FormatAnthropic}, info, true, nil, -1)
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 || logs[0].Billing == nil || logs[0].PromptTokens != 150 || logs[0].CompletionTokens != 10 {
		t.Fatalf("receipt missing: %+v / %v", logs, err)
	}
	updated, err := dbGetKeyByID(key.ID)
	if err != nil || math.Abs(updated.UsedUSD-logs[0].CostUSD) > 1e-12 || updated.SharedUsedUSD != 0 || math.Abs(updated.ModelQuotas["alias"].UsedUSD-logs[0].CostUSD) > 1e-12 {
		t.Fatal("quota and receipt disagree")
	}
	settingsMu.Lock()
	settingsCache.Pricing = map[string]Price{"alias": {In: 99, Out: 99}}
	settingsMu.Unlock()
	logs, _ = dbListLogs(1, 0)
	if logs[0].Billing.Price.In != 2 || logs[0].Billing.Price.CacheWrite1h == nil || *logs[0].Billing.Price.CacheWrite1h != 4 {
		t.Fatal("historical rate snapshot changed")
	}
}

func TestBillingAuditLogFailureRollsBackAllQuotaPools(t *testing.T) {
	for _, independent := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "independent"}[independent], func(t *testing.T) {
			publicTestDB(t)
			key := publicTestKey(t, "rollback", 1, 0, false)
			if _, err := db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON call_logs BEGIN SELECT RAISE(ABORT,'receipt rejected'); END`); err != nil {
				t.Fatal(err)
			}
			if err := dbRecordBilledCall(&CallLog{KeyID: key.ID, Model: "gpt-alpha", CostUSD: .25}, independent); err == nil {
				t.Fatal("failed receipt committed")
			}
			updated, err := dbGetKeyByID(key.ID)
			if err != nil || updated.UsedUSD != 0 || updated.SharedUsedUSD != 0 || updated.ModelQuotas["gpt-alpha"].UsedUSD != 0 {
				t.Fatal("failed receipt left a debit")
			}
		})
	}
}

func TestBillingAuditLegacyLogMigrationIsIdempotent(t *testing.T) {
	publicTestDB(t)
	if _, err := db.Exec(`ALTER TABLE call_logs DROP COLUMN billing_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO call_logs(model,prompt_tokens,completion_tokens,cost_usd) VALUES('legacy',7,3,.12)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateBillingLogSchema(); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 || logs[0].Billing != nil || logs[0].PromptTokens != 7 || logs[0].CostUSD != .12 {
		t.Fatal("legacy logs were rewritten")
	}
}

func TestBillingAuditAutomaticPricingIncludesCacheFields(t *testing.T) {
	publicTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"anthropic/model":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000008,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":0.0000025,"cache_creation_input_token_cost_above_1hr":0.000004,"output_cost_per_reasoning_token":0.000009},"invalid":{"input_cost_per_token":-1}}`))
	}))
	defer server.Close()
	previous := getSettings()
	settingsMu.Lock()
	settingsCache.AutoPricingURL = server.URL
	settingsMu.Unlock()
	t.Cleanup(func() { settingsMu.Lock(); settingsCache = previous; settingsMu.Unlock() })
	autoPricing.Lock()
	oldData, oldAt := autoPricing.data, autoPricing.fetchedAt
	autoPricing.Unlock()
	t.Cleanup(func() {
		autoPricing.Lock()
		autoPricing.data, autoPricing.fetchedAt = oldData, oldAt
		autoPricing.Unlock()
	})
	if _, err := refreshAutoPricing(); err != nil {
		t.Fatal(err)
	}
	autoPricing.RLock()
	price, ok := lookupAutoPrice(autoPricing.data, "anthropic/model")
	_, invalid := autoPricing.data["invalid"]
	autoPricing.RUnlock()
	if !ok || invalid || price.CacheRead == nil || *price.CacheRead != 0 || price.CacheWrite1h == nil || math.Abs(*price.CacheWrite1h-4) > 1e-12 || price.Reasoning == nil || math.Abs(*price.Reasoning-9) > 1e-12 {
		t.Fatalf("automatic detailed price lost: %+v", price)
	}
}
