package main

import (
	"encoding/json"
	"strings"
	"time"
)

// Use declarations already discovered by the model catalog. Relaying a request
// must not depend on another network request to the models endpoint.
func upstreamReasoningEfforts(up *Upstream, model string) []string {
	key := upstreamModelCacheKey(up)
	modelsCache.Lock()
	cached, ok := modelsCache.m[key]
	levels := cached.efforts[model]
	modelsCache.Unlock()
	if ok && time.Since(cached.at) < modelsCacheTTL && len(levels) > 0 {
		return levels
	}
	return defaultReasoningEfforts(model)
}

// Keep this fallback narrow: unknown and future models retain their input.
// Provider-qualified IDs use the same rules as their unqualified model ID.
func defaultReasoningEfforts(model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if strings.HasPrefix(model, "deepseek-") {
		return []string{"low", "high", "max"}
	}
	for _, profile := range []struct {
		model  string
		levels []string
	}{
		{"gpt-5.1", []string{"low", "medium", "high"}},
		{"gpt-5.2", []string{"low", "medium", "high", "xhigh"}},
		{"claude-opus-4-5", []string{"low", "medium", "high"}},
		{"claude-opus-4-6", []string{"low", "medium", "high", "max"}},
		{"claude-sonnet-4-6", []string{"low", "medium", "high", "max"}},
		{"claude-opus-4-7", []string{"low", "medium", "high", "xhigh", "max"}},
		{"claude-opus-4-8", []string{"low", "medium", "high", "xhigh", "max"}},
	} {
		if model == profile.model || strings.HasPrefix(model, profile.model+"-20") {
			return profile.levels
		}
	}
	return nil
}

// Pick the next available strength, capping at the model's highest level.
// In particular, xhigh becomes max when a model declares high/max. none and
// vendor-specific values are left alone; disabling thinking is not a strength.
func mapReasoningEffort(effort string, levels []string) string {
	ranks := map[string]int{"minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6, "ultra": 7}
	normalized := strings.ToLower(strings.TrimSpace(effort))
	want, known := ranks[normalized]
	if !known || len(levels) == 0 {
		return effort
	}
	next, highest := "", ""
	nextRank, highestRank := 0, 0
	for _, level := range levels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level == normalized {
			return level
		}
		rank, known := ranks[level]
		if !known {
			continue
		}
		if rank > highestRank {
			highest, highestRank = level, rank
		}
		if rank >= want && (next == "" || rank < nextRank) {
			next, nextRank = level, rank
		}
	}
	if next != "" {
		return next
	}
	if highest != "" {
		return highest
	}
	return effort
}

// Patch only the effort field on native paths, retaining opaque protocol fields
// and JSON number precision. Requests that need no mapping stay byte-for-byte.
func mapReasoningEffortBody(body []byte, format clientFormat, levels []string) []byte {
	if len(levels) == 0 {
		return body
	}
	container, field := "", "effort"
	switch format {
	case FormatOpenAIChat:
		field = "reasoning_effort"
	case FormatOpenAIResponse:
		container = "reasoning"
	case FormatAnthropic:
		container = "output_config"
	default:
		return body
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return body
	}
	target := root
	if container != "" {
		target = nil
		if json.Unmarshal(root[container], &target) != nil || target == nil {
			return body
		}
	}
	var effort string
	if json.Unmarshal(target[field], &effort) != nil {
		return body
	}
	mapped := mapReasoningEffort(effort, levels)
	if mapped == effort {
		return body
	}
	target[field], _ = json.Marshal(mapped)
	if container != "" {
		root[container], _ = json.Marshal(target)
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
