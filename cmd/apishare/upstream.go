package main

import (
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ---------- 健康状态（内存） ----------

// 连续失败达到阈值后进入冷却期，冷却时间指数退避，上限 10 分钟。
// 冷却中的上游不参与调度，粘性会话也会转移并重新绑定。

const (
	defaultUpstreamCooldownThreshold = 3
	defaultKeyCooldownThreshold      = 1
	defaultCooldownSeconds           = 30
	defaultCooldownMaxSeconds        = 600
	maxConfigurableCooldownThreshold = 100
	maxConfigurableCooldownSeconds   = 86400
)

func (u *Upstream) validateCooldownConfig() error {
	if u.CooldownThreshold < 0 || u.CooldownThreshold > maxConfigurableCooldownThreshold {
		return errors.New("连续失败阈值需在 0 到 100 之间")
	}
	if u.CooldownSeconds < 0 || u.CooldownSeconds > maxConfigurableCooldownSeconds {
		return errors.New("初始冷却时间需在 0 到 86400 秒之间")
	}
	if u.CooldownMaxSeconds < 0 || u.CooldownMaxSeconds > maxConfigurableCooldownSeconds {
		return errors.New("最大冷却时间需在 0 到 86400 秒之间")
	}
	if u.CooldownSeconds > 0 && u.CooldownMaxSeconds > 0 && u.CooldownMaxSeconds < u.CooldownSeconds {
		return errors.New("最大冷却时间不能小于初始冷却时间")
	}
	return nil
}

func (u *Upstream) effectiveCooldownThreshold(forKey bool) int {
	if u != nil && u.CooldownThreshold > 0 {
		return u.CooldownThreshold
	}
	if forKey {
		return defaultKeyCooldownThreshold
	}
	return defaultUpstreamCooldownThreshold
}

func (u *Upstream) effectiveCooldownBase() time.Duration {
	if u != nil && u.CooldownSeconds > 0 {
		return time.Duration(u.CooldownSeconds) * time.Second
	}
	return defaultCooldownSeconds * time.Second
}

func (u *Upstream) effectiveCooldownMax() time.Duration {
	base := u.effectiveCooldownBase()
	if u != nil && u.CooldownMaxSeconds > 0 {
		maxDur := time.Duration(u.CooldownMaxSeconds) * time.Second
		if maxDur < base {
			return base
		}
		return maxDur
	}
	if base > defaultCooldownMaxSeconds*time.Second {
		return base
	}
	return defaultCooldownMaxSeconds * time.Second
}

func (u *Upstream) cooldownBackoff(failures int, forKey bool) (time.Duration, bool) {
	if u != nil && u.CooldownDisabled {
		return 0, false
	}
	threshold := u.effectiveCooldownThreshold(forKey)
	if failures < threshold {
		return 0, false
	}
	base := u.effectiveCooldownBase()
	maxDur := u.effectiveCooldownMax()
	shift := failures - threshold
	if shift > 10 {
		shift = 10
	}
	backoff := base << uint(shift)
	if backoff <= 0 || backoff > maxDur {
		backoff = maxDur
	}
	return backoff, true
}

var upHealth = struct {
	sync.Mutex
	fails map[int64]int
	until map[int64]time.Time
}{fails: map[int64]int{}, until: map[int64]time.Time{}}

func healthMarkFailure(id int64) {
	var up *Upstream
	if db != nil {
		if candidate, err := cachedRoutingUpstream(id); err == nil {
			up = candidate
		}
	}
	healthMarkFailureForUpstream(up, id, time.Time{})
}

func healthMarkFailureForUpstream(up *Upstream, id int64, retryAfter time.Time) {
	if up != nil && up.CooldownDisabled {
		return
	}
	upHealth.Lock()
	defer upHealth.Unlock()
	upHealth.fails[id]++
	n := upHealth.fails[id]
	if backoff, cool := up.cooldownBackoff(n, false); cool {
		until := time.Now().Add(backoff)
		if retryAfter.After(until) {
			until = retryAfter
		}
		if until.After(upHealth.until[id]) {
			upHealth.until[id] = until
		}
	} else if !retryAfter.IsZero() && retryAfter.After(upHealth.until[id]) {
		upHealth.until[id] = retryAfter
	}
}

func healthMarkSuccess(id int64) {
	upHealth.Lock()
	defer upHealth.Unlock()
	delete(upHealth.fails, id)
	delete(upHealth.until, id)
}

func healthAvailable(id int64) bool {
	upHealth.Lock()
	defer upHealth.Unlock()
	t, ok := upHealth.until[id]
	return !ok || time.Now().After(t)
}

func healthFails(id int64) int {
	upHealth.Lock()
	defer upHealth.Unlock()
	return upHealth.fails[id]
}

// ---------- 上游选择 ----------

func (u *Upstream) servesModel(m string) bool {
	serves, _ := u.servesModelExplicit(m)
	return serves
}

// Explicit mapping sources are public models, even if the allowlist only
// contains upstream identifiers. An empty allowlist remains a wildcard.
func (u *Upstream) servesModelExplicit(m string) (bool, bool) {
	m = strings.TrimSpace(m)
	if target, ok := u.modelMap()[m]; ok && strings.TrimSpace(target) != "" {
		return true, true
	}
	if config := u.config; config != nil && config.modelsRaw == u.Models {
		if config.models == nil {
			return true, false
		}
		return config.models[m], config.models[m]
	}
	list := strings.TrimSpace(u.Models)
	if list == "" {
		if defaults := defaultDecisionModelsForType(u.Type); len(defaults) > 0 {
			for _, dm := range defaults {
				if dm == m {
					return true, true
				}
			}
			return false, false
		}
		return true, false
	}
	for _, s := range strings.Split(list, ",") {
		if strings.TrimSpace(s) == m {
			return true, true
		}
	}
	return false, false
}

func (u *Upstream) modelMap() map[string]string {
	if config := u.config; config != nil && config.mapRaw == u.ModelMap {
		return config.modelMap
	}
	if strings.TrimSpace(u.ModelMap) == "" {
		return nil
	}
	m, _ := parseModelMap(u.ModelMap)
	return m
}

func (u *Upstream) mapModel(clientModel string) string {
	if mm := u.modelMap(); mm != nil {
		if v, ok := mm[clientModel]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return clientModel
}

// pickUpstream 在健康且服务该模型的候选中选择上游：
//
//  1. 用户优先级：只从当前可用候选中 priority 最小的一层选择（数值越小越优先，
//     0 最高）。低优先级渠道天然作为备用，仅在更高层不可用时接入流量。
//  2. 格式与白名单层级：同一优先级内按以下顺序取最优层（跨格式转换设计：
//     候选不限于客户端原生格式的上游——只要某个上游（任意格式）服务该模型
//     就可以入选）：
//  1. 原生格式 + 显式白名单命中   （格式对、模型也对，最佳）
//  2. 其他格式 + 显式白名单命中   （模型对，需要跨格式转换）
//  3. 原生格式 + 通配             （格式对，模型是否真支持由上游裁决）
//  4. 其他格式 + 通配             （兜底）
//  3. 权重：同一优先级、同一格式层内按 weight 加权随机。
//
// nativeOnly=true 时（如 embeddings）只考虑原生类型。
//
// exclude 里的上游（本轮已试过的）不重复选择。weight<=0 的上游不接新流量。
func supportsNativeEndpoint(up *Upstream, format clientFormat) bool {
	if up.Type == "cliproxy" {
		// Compact uses Responses. Embeddings/audio must continue to use a
		// dedicated upstream instead of an OAuth pool without those endpoints.
		return format == FormatOpenAIResponse
	}
	if up.Type != format.nativeUpstreamType() {
		return false
	}
	// Responses-only operations such as compact cannot use a Chat-only channel.
	return format != FormatOpenAIResponse || up.wireFormat(format) == FormatOpenAIResponse
}

func pickUpstream(format clientFormat, model string, exclude map[int64]bool, nativeOnly bool, requests ...*canonReq) (*Upstream, error) {
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return nil, err
	}
	// Keep candidate indexes on the stack for typical pools. Only the selected
	// configuration is copied, so callers cannot mutate the shared snapshot.
	var candidates [32]int
	best := candidates[:0]
	total := 0
	var limited *openCodeRateLimitError
	bestPriority := int(^uint(0) >> 1)
	bestTier := 99
	for index := range snapshot.upstreams {
		u := &snapshot.upstreams[index]
		if !u.Enabled || exclude[u.ID] || u.Weight <= 0 {
			continue
		}
		if u.Priority > bestPriority {
			continue
		}
		serves, explicit := u.servesModelExplicit(model)
		if !serves {
			continue
		}
		if nativeOnly && !supportsNativeEndpoint(u, format) {
			continue
		}
		if !u.CooldownDisabled {
			if limit := u.openCodeRateLimit(u.mapModel(model)); limit != nil {
				if limited == nil || limit.until.Before(limited.until) {
					limited = limit
				}
				continue
			}
		}
		if !u.availableForRelay(model) {
			continue
		}
		var tier int
		if nativeOnly {
			tier = 0
		} else {
			native := u.routingWireFormat(format, model) == format
			switch {
			case native && explicit:
				tier = 1
			case !native && explicit:
				tier = 2
			case native:
				tier = 3
			default:
				tier = 4
			}
			if len(requests) > 0 && requests[0] != nil && requests[0].OutputFormat != nil {
				// Preserve explicit model routing tiers. Within a tier prefer
				// schema-capable conversion over opt-in JSON compatibility.
				tier *= 3
				if !native {
					switch u.StructuredOutput {
					case "json_object":
						tier++
					case "disabled":
						tier += 2
					}
				}
			}
		}
		if u.Priority < bestPriority {
			bestPriority = u.Priority
			bestTier = tier
			best = best[:0]
			total = 0
		} else if tier < bestTier {
			bestTier = tier
			best = best[:0]
			total = 0
		} else if tier > bestTier {
			continue
		}
		best = append(best, index)
		total += u.Weight
	}
	if len(best) == 0 {
		if limited != nil {
			return nil, limited
		}
		return nil, errors.New("no available upstream for model " + model)
	}
	r := rand.Intn(total)
	for _, index := range best {
		r -= snapshot.upstreams[index].Weight
		if r < 0 {
			selected := snapshot.upstreams[index]
			return &selected, nil
		}
	}
	selected := snapshot.upstreams[best[len(best)-1]]
	return &selected, nil
}

// upstreamURL 拼接上游地址：base_url 以 /v1 或 /v1beta 结尾时自动去重。
func upstreamURL(base, endpoint string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + strings.TrimPrefix(endpoint, "/v1")
	}
	if strings.HasSuffix(base, "/v1beta") {
		return base + strings.TrimPrefix(endpoint, "/v1beta")
	}
	return base + endpoint
}
