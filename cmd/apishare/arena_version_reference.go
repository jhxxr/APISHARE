package main

import (
	"strings"
	"time"
)

const (
	deepSeekFlash1017Identity = "deepseek-v-4-1-flash-20261017"
	deepSeekFlash0910Identity = "deepseek-v-4-1-flash-20260910"
)

// Explicit, verified date spellings only. This does not erase thinking modes,
// equate arbitrary generations, or resolve rolling third-party aliases.
func arenaVersionedIdentity(model string) string {
	key := arenaModelIdentity(model)
	for _, pair := range [][2]string{
		{"deepseek-v-4-1-flash-1017", deepSeekFlash1017Identity},
		{"deepseek-v-4-1-flash-0910", deepSeekFlash0910Identity},
	} {
		if key == pair[0] || strings.HasPrefix(key, pair[0]+"-") {
			return pair[1] + strings.TrimPrefix(key, pair[0])
		}
	}
	return key
}

// An explicitly requested policy prior, not a measurement of the new model.
// Prefer the same thinking configuration; if absent, use the LOWEST supported
// old-version score and disclose the configuration mismatch. A real result for
// the new release (in any configuration) supersedes this provisional reference.
func arenaOlderVersionReference(identity string, rows []arenaModel, now time.Time) (*arenaModel, bool) {
	if now.Before(time.Date(2026, 10, 17, 0, 0, 0, 0, beijingLocation)) {
		return nil, false
	}
	switch strings.TrimPrefix(identity, deepSeekFlash1017Identity) {
	case "", "-none", "-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-adaptive":
		if !strings.HasPrefix(identity, deepSeekFlash1017Identity) {
			return nil, false
		}
	default:
		return nil, false
	}
	for _, row := range rows {
		if arenaPublicModelIdentity(row.Model) == deepSeekFlash1017Identity {
			return nil, false
		}
	}
	oldIdentity := deepSeekFlash0910Identity + strings.TrimPrefix(identity, deepSeekFlash1017Identity)
	counts := map[string]int{}
	for _, row := range rows {
		counts[arenaVersionedIdentity(row.Model)]++
	}
	var same, conservative *arenaModel
	for i := range rows {
		row := &rows[i]
		key := arenaVersionedIdentity(row.Model)
		if counts[key] > 1 || row.Votes == nil || *row.Votes < arenaRoutingMinSamples {
			continue
		}
		if key == oldIdentity {
			same = row
		}
		if arenaPublicModelIdentity(row.Model) == deepSeekFlash0910Identity &&
			(conservative == nil || row.Score < conservative.Score ||
				(row.Score == conservative.Score && row.Model < conservative.Model)) {
			conservative = row
		}
	}
	if same != nil {
		return same, false
	}
	return conservative, conservative != nil
}

func arenaVersionReferenceNote(row arenaModel, differentThinking bool) string {
	note := "旧版成绩参考／新版推断：采用 " + row.Model + " 的原始分数作为 1017 版的质量先验；分数、区间、样本均来自旧版。仅同分时略优先新版，不是新版实测成绩。"
	if differentThinking {
		note += " 未找到相同思考配置，使用旧版已收录配置中的最低参考分，思考配置差异未经验证。"
	}
	return note
}
