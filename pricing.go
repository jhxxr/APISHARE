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

// 内置兜底价格表（USD/1M tokens）：仅在官方价格库不可用且无手动覆盖时使用。
var builtInPricing = map[string]Price{
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
		InputCostPerToken  *float64 `json:"input_cost_per_token"`
		OutputCostPerToken *float64 `json:"output_cost_per_token"`
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

	log.Printf("[pricing] official prices synced: %d models from %s", len(byFull), url)
	return len(byFull), nil
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
	s := getSettings()
	if p, ok := s.Pricing[model]; ok {
		return p, "override"
	}
	if s.AutoPricingEnabled {
		autoPricing.RLock()
		data, fetchedAt := autoPricing.data, autoPricing.fetchedAt
		autoPricing.RUnlock()
		if p, ok := lookupAutoPrice(data, model); ok {
			_ = fetchedAt
			return p, "official"
		}
	}
	if p, ok := builtInPricing[model]; ok {
		return p, "builtin"
	}
	if p, ok := s.Pricing["default"]; ok {
		return p, "default"
	}
	return Price{}, "none"
}

func priceFor(model string, prompt, completion int) float64 {
	p, _ := lookupPrice(model)
	cost := (float64(prompt)*p.In + float64(completion)*p.Out) / 1e6
	if cost != cost || cost > 1e12 { // NaN / Inf 防御
		return 0
	}
	return cost
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
