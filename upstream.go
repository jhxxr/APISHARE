package main

import (
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// ---------- 健康状态（内存） ----------

// 连续失败达到阈值后进入冷却期，冷却时间指数退避，上限 10 分钟。
// 冷却中的上游不参与新会话的调度，但已有粘性会话仍可继续使用
// （若粘性请求也失败，会走故障转移并重新绑定）。

var upHealth = struct {
	sync.Mutex
	fails map[int64]int
	until map[int64]time.Time
}{fails: map[int64]int{}, until: map[int64]time.Time{}}

func healthMarkFailure(id int64) {
	upHealth.Lock()
	defer upHealth.Unlock()
	upHealth.fails[id]++
	if n := upHealth.fails[id]; n >= 3 {
		backoff := 30 * time.Second << uint(n-3)
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		upHealth.until[id] = time.Now().Add(backoff)
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

// servesModelExplicit 返回 (是否服务该模型, 是否为显式白名单命中)。
// 白名单为空表示通配所有模型（非显式）。
func (u *Upstream) servesModelExplicit(m string) (bool, bool) {
	m = strings.TrimSpace(m)
	list := strings.TrimSpace(u.Models)
	if list == "" {
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
	if strings.TrimSpace(u.ModelMap) == "" {
		return nil
	}
	var m map[string]string
	if json.Unmarshal([]byte(u.ModelMap), &m) == nil {
		return m
	}
	return nil
}

func (u *Upstream) mapModel(clientModel string) string {
	if mm := u.modelMap(); mm != nil {
		if v, ok := mm[clientModel]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return clientModel
}

// pickUpstream 在健康且服务该模型的候选中按优先级分层后加权随机选择。
//
// 跨格式转换设计：候选不限于客户端原生类型的上游——只要某个上游（任意类型）
// 服务该模型就可以入选，按以下优先级取最优层：
//  1. 原生类型 + 显式白名单命中   （格式对、模型也对，最佳）
//  2. 其他类型 + 显式白名单命中   （模型对，需要跨格式转换）
//  3. 原生类型 + 通配             （格式对，模型是否真支持由上游裁决）
//  4. 其他类型 + 通配             （兜底）
//
// nativeOnly=true 时（如 embeddings）只考虑原生类型。
//
// exclude 里的上游（本轮已试过的）不重复选择。weight<=0 的上游不接新流量。
func pickUpstream(upType, model string, exclude map[int64]bool, nativeOnly bool) (*Upstream, error) {
	ups, err := dbListUpstreams()
	if err != nil {
		return nil, err
	}
	var best []Upstream
	bestTier := 99
	for _, u := range ups {
		if !u.Enabled || exclude[u.ID] || !healthAvailable(u.ID) || u.Weight <= 0 {
			continue
		}
		serves, explicit := u.servesModelExplicit(model)
		if !serves {
			continue
		}
		var tier int
		if nativeOnly {
			if u.Type != upType {
				continue
			}
			tier = 0
		} else {
			switch {
			case u.Type == upType && explicit:
				tier = 1
			case u.Type != upType && explicit:
				tier = 2
			case u.Type == upType:
				tier = 3
			default:
				tier = 4
			}
		}
		if tier < bestTier {
			bestTier = tier
			best = best[:0]
		}
		if tier == bestTier {
			best = append(best, u)
		}
	}
	if len(best) == 0 {
		return nil, errors.New("no available upstream for model " + model)
	}
	total := 0
	for _, u := range best {
		total += u.Weight
	}
	r := rand.Intn(total)
	for i := range best {
		r -= best[i].Weight
		if r < 0 {
			return &best[i], nil
		}
	}
	return &best[len(best)-1], nil
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
