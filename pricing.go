package main

// 官方定价同步：各模型厂商没有定价 API，业界通用做法（LiteLLM、sub2api 一类网关）
// 是同步 LiteLLM 维护的官方牌价镜像库 model_prices_and_context_window.json，
// 它持续收录 OpenAI / Anthropic / Gemini / xAI Grok / DeepSeek 等的官方单价。
//
// 计费优先级：手动覆盖（后台价格表中显式配置的模型）
//

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const defaultAutoPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

const pricingRefreshInterval = 24 * time.Hour
const pricingCacheSchemaVersion = "2"

// 内置兜底价格表（USD/1M tokens）：仅在官方价格库不可用且无手动覆盖时使用。
var builtInPricing = map[string]Price{
	"deepseek-flash":            deepseekFlashFallbackPrice(),
	"gpt-4o":                    {In: 2.5, Out: 10},
	"gpt-4o-mini":               {In: 0.15, Out: 0.6},
	"gpt-4.1":                   {In: 2, Out: 8},
	"gpt-4.1-mini":              {In: 0.4, Out: 1.6},
	"deepseek-chat":             {In: 0.27, Out: 1.1},
	"deepseek-reasoner":         {In: 0.55, Out: 2.19},
	"claude-sonnet-4-20250514":  {In: 3, Out: 15},
	"claude-opus-4-20250514":    {In: 15, Out: 75},
	"claude-3-5-haiku-20241022": {In: 0.8, Out: 4},
	"glm-4.6":                   {In: 0.6, Out: 2.2},
}

// Snapshot of DeepSeek's peak rates, USD/1M tokens, checked 2026-10-14:
// https://api-docs.deepseek.com/quick_start/pricing/
// Configured and synchronized prices take precedence; off-peak scheduling is
// not inferred by the gateway's fixed-rate billing policy.
func deepseekFlashFallbackPrice() Price {
	cacheRead := 0.006
	return Price{In: 0.3, Out: 1.2, CacheRead: &cacheRead}
}

// All DeepSeek Flash names share one billing rate by administrator policy.
// This changes price selection only, never routing or quota-pool identity.
func canonicalPricingModel(model string) string {
	name := strings.ToLower(model)
	if strings.Contains(name, "deepseek") && strings.Contains(name, "flash") {
		return "deepseek-flash"
	}
	return model
}

// 官方价格缓存（内存），并持久化到 settings 表（重启离线也可用）。
var autoPricing = struct {
	sync.RWMutex
	data      map[string]Price
	fetchedAt string
}{data: map[string]Price{}}

var pricingHTTPClient = &http.Client{Timeout: 30 * time.Second}

// loadAutoPricingCache 启动时从 DB 恢复上次同步的价格。
func loadAutoPricingCache() {
	if blob, err := settingsGet("auto_pricing_json"); err == nil && blob != "" {
		var data map[string]Price
		if json.Unmarshal([]byte(blob), &data) == nil && len(data) > 0 {
			at, _ := settingsGet("auto_pricing_fetched_at")
			if version, _ := settingsGet("auto_pricing_schema_version"); version != pricingCacheSchemaVersion {
				// Pre-upgrade snapshots discarded cache prices. Keep base prices
				// available offline, but schedule a refresh immediately on startup.
				at = ""
			}
			autoPricing.Lock()
			autoPricing.data = data
			autoPricing.fetchedAt = at
			autoPricing.Unlock()
		}
	}
}

// refreshAutoPricing 拉取并解析官方价格库，成功后写内存 + DB。
func refreshAutoPricing() (int, error) {
	url := getSettings().AutoPricingURL
	if url == "" {
		url = defaultAutoPricingURL
	}
	resp, err := pricingHTTPClient.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("pricing source returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, err
	}

	// LiteLLM 原始结构：{ "模型名": { "input_cost_per_token": 2.5e-6, "output_cost_per_token": 1e-5, ... } }
	var src map[string]struct {
		InputCostPerToken        *float64 `json:"input_cost_per_token"`
		OutputCostPerToken       *float64 `json:"output_cost_per_token"`
		CacheReadCostPerToken    *float64 `json:"cache_read_input_token_cost"`
		CacheWriteCostPerToken   *float64 `json:"cache_creation_input_token_cost"`
		CacheWrite1hCostPerToken *float64 `json:"cache_creation_input_token_cost_above_1hr"`
		ReasoningCostPerToken    *float64 `json:"output_cost_per_reasoning_token"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return 0, err
	}

	// 归一化索引：全名（含 provider/ 前缀）与归一化裸名两套索引。
	byFull := map[string]Price{}
	type bareEntry struct {
		p    Price
		tier int
	}
	bare := map[string]bareEntry{}
	for key, e := range src {
		if e.InputCostPerToken == nil {
			continue // 无定价的条目（部分 audio/图像/实验条目）跳过
		}
		p := Price{In: *e.InputCostPerToken * 1e6}
		if e.OutputCostPerToken != nil {
			p.Out = *e.OutputCostPerToken * 1e6
		}
		p.CacheRead = ratePerMillion(e.CacheReadCostPerToken)
		p.CacheWrite = ratePerMillion(e.CacheWriteCostPerToken)
		p.CacheWrite1h = ratePerMillion(e.CacheWrite1hCostPerToken)
		p.Reasoning = ratePerMillion(e.ReasoningCostPerToken)
		if !validPrice(p) {
			continue
		}
		byFull[key] = p
		if name, tier := normalizePricingKey(key); name != "" {
			if old, exists := bare[name]; !exists || tier < old.tier {
				bare[name] = bareEntry{p: p, tier: tier}
			}
		}
	}
	if len(byFull) == 0 {
		return 0, fmt.Errorf("pricing source parsed empty")
	}

	fetchedAt := nowStr()
	autoPricing.Lock()
	autoPricing.data = map[string]Price{}
	for k, v := range byFull {
		autoPricing.data[k] = v
	}
	for k, v := range bare {
		autoPricing.data["bare:"+k] = v.p
	}
	autoPricing.fetchedAt = fetchedAt
	autoPricing.Unlock()

	blob, _ := json.Marshal(byFull)
	_ = settingsSet("auto_pricing_json", string(blob))
	_ = settingsSet("auto_pricing_fetched_at", fetchedAt)
	_ = settingsSet("auto_pricing_schema_version", pricingCacheSchemaVersion)

	log.Printf("[pricing] official prices synced: %d models from %s", len(byFull), url)
	return len(byFull), nil
}

func ratePerMillion(value *float64) *float64 {
	if value == nil {
		return nil
	}
	rate := *value * 1e6
	return &rate
}

// ---------- 键名归一化 ----------

// 第一方 provider/ 前缀（斜杠形态）：其后的裸名按官方价处理。
var slashProviders = map[string]bool{
	"openai": true, "anthropic": true, "gemini": true, "google": true,
	"xai": true, "deepseek": true, "mistral": true, "zhipuai": true,
	"moonshot": true, "dashscope": true, "minimax": true, "cohere": true, "meta": true,
}

// 厂商点前缀（Bedrock/Vertex 形态，如 anthropic.claude-...-v1:0）。
var dotProviders = map[string]bool{
	"anthropic": true, "google": true, "gemini": true, "meta": true, "mistral": true,
	"amazon": true, "cohere": true, "deepseek": true, "xai": true, "openai": true,
	"moonshot": true, "zhipuai": true, "qwen": true,
}

// 区域前缀（Bedrock 跨区推理等，价格略高于官方全球价，优先级最低）。
var regionPrefixes = []string{
	"us-gov-east-1.", "us-gov-west-1.", "us-gov.", "apac.", "eu-central-1.",
	"us-east-1.", "us-west-2.", "eu.", "us.", "au.", "jp.",
}

var versionSuffixRe = regexp.MustCompile(`(-v\d+(:\d+)?)$|(:\d+)$|(-latest)$`)

func stripVersionSuffix(s string) string {
	for i := 0; i < 3; i++ {
		n := versionSuffixRe.ReplaceAllString(s, "")
		if n == s {
			break
		}
		s = n
	}
	return s
}

// normalizePricingKey 把价格库的各种键形态归一化为裸模型名与优先级：
//
//	1: 纯裸名（claude-opus-4-5）
//	2: 第一方斜杠前缀（gemini/gemini-2.5-flash、xai/grok-4）
//	3: 点前缀/全球区（anthropic.claude-sonnet-4-20250514-v1:0、global.anthropic.x）
//	4: 区域变体（us.anthropic.x，价格通常略高）
//
// 第三方聚合商前缀（aihubmix/ 等）不归一化，避免非官方价污染。
func normalizePricingKey(key string) (string, int) {
	if !strings.Contains(key, "/") && !strings.Contains(key, ".") {
		return key, 1
	}
	if i := strings.Index(key, "/"); i > 0 {
		provider := key[:i]
		if !slashProviders[provider] {
			return "", 99
		}
		rest := key[i+1:]
		if j := strings.LastIndex(rest, "/"); j >= 0 {
			rest = rest[j+1:] // bedrock/<region>/<model> 取末段
		}
		return stripVersionSuffix(rest), 2
	}
	name := key
	tier := 3
	for _, r := range regionPrefixes {
		if strings.HasPrefix(name, r) {
			name = name[len(r):]
			tier = 4
			break
		}
	}
	if strings.HasPrefix(name, "global.") {
		name = name[len("global."):]
	}
	dot := strings.Index(name, ".")
	if dot <= 0 {
		return "", 99
	}
	if !dotProviders[name[:dot]] {
		return "", 99
	}
	return stripVersionSuffix(name[dot+1:]), tier
}

// lookupAutoPrice 在官方价格缓存中查找：归一化裸名 → 全名 → 常见前缀全名。
func lookupAutoPrice(data map[string]Price, model string) (Price, bool) {
	if p, ok := data["bare:"+model]; ok {
		return p, true
	}
	// 请求模型自带版本后缀时（如 xxx-v1:0），归一化后再查裸名索引
	if name, _ := normalizePricingKey(model); name != "" && name != model {
		if p, ok := data["bare:"+name]; ok {
			return p, true
		}
	}
	if p, ok := data[model]; ok {
		return p, true
	}
	for _, prefix := range []string{"openai/", "anthropic/", "gemini/", "google/", "xai/", "deepseek/", "mistral/"} {
		if p, ok := data[prefix+model]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// lookupPrice 返回某模型的有效单价与来源。
func lookupPrice(model string) (Price, string) {
	p, source, _ := lookupRelayPrice(model, "")
	return p, source
}

// Prefer a matched client-model price, then the actual upstream model, before
// applying the global default. Explicit free model rates remain authoritative.
func lookupRelayPrice(model, upstreamModel string) (Price, string, string) {
	s := getSettings()
	autoPricing.RLock()
	defer autoPricing.RUnlock()
	model = canonicalPricingModel(model)
	upstreamModel = canonicalPricingModel(upstreamModel)
	for i, candidate := range []string{model, upstreamModel} {
		if candidate == "" || (i == 1 && candidate == model) {
			continue
		}
		if p, source, ok := lookupModelPrice(s, autoPricing.data, candidate); ok {
			return p, source, candidate
		}
	}
	if p, ok := s.Pricing["default"]; ok {
		return p, "default", "default"
	}
	return Price{}, "none", ""
}

func lookupModelPrice(s AppSettings, data map[string]Price, model string) (Price, string, bool) {
	if p, ok := s.Pricing[model]; ok {
		return p, "override", true
	}
	if s.AutoPricingEnabled {
		if p, ok := lookupAutoPrice(data, model); ok {
			return p, "official", true
		}
	}
	if p, ok := builtInPricing[model]; ok {
		return p, "builtin", true
	}
	return Price{}, "", false
}

func priceFor(model string, prompt, completion int) float64 {
	p, _ := lookupPrice(model)
	return costForUsage(billingUsage{Input: nonnegativeTokens(prompt), Output: nonnegativeTokens(completion)}, p).Total
}

// pricingRefresherLoop 启动时按需刷新（超过 24h 或为空），此后每小时检查一次。
func pricingRefresherLoop() {
	for {
		s := getSettings()
		if s.AutoPricingEnabled {
			autoPricing.RLock()
			fetchedAt, n := autoPricing.fetchedAt, len(autoPricing.data)
			autoPricing.RUnlock()
			stale := n == 0
			if !stale {
				if t, err := time.Parse(time.RFC3339, fetchedAt); err == nil {
					stale = time.Since(t) > pricingRefreshInterval
				} else {
					stale = true
				}
			}
			if stale {
				if _, err := refreshAutoPricing(); err != nil {
					log.Printf("[pricing] sync failed (will retry): %v", err)
				}
			}
		}
		time.Sleep(time.Hour)
	}
}
