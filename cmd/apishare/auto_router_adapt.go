package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var errAutoRouterModelIncompatible = errors.New("auto-router candidate model incompatible with request")

// routerModelSpec represents a parsed candidate model entry in Auto-Router tiers,
// supporting inline thinking effort/budget and context window overrides such as
// "gpt-5.2(high)", "claude-sonnet-4-5(4096)", "deepseek-chat(none,64k)", etc.
type routerModelSpec struct {
	Raw          string
	BaseModel    string
	Thinking     string // "", "auto", "keep", "none", "minimal", "low", "medium", "high", "xhigh", "max", "adaptive", "enabled"
	BudgetTokens int    // >0 when explicit token budget is specified
	ContextLimit int    // >0 when explicit context window limit is specified
}

func parseRouterModelSpec(raw string) routerModelSpec {
	trimmed := strings.TrimSpace(raw)
	spec := routerModelSpec{
		Raw:       trimmed,
		BaseModel: trimmed,
	}
	if trimmed == "" {
		return spec
	}
	openIdx := strings.LastIndex(trimmed, "(")
	if openIdx <= 0 || !strings.HasSuffix(trimmed, ")") {
		return spec
	}
	base := strings.TrimSpace(trimmed[:openIdx])
	inner := strings.TrimSpace(trimmed[openIdx+1 : len(trimmed)-1])
	if base == "" || inner == "" {
		return spec
	}

	var recognized bool
	var thinking string
	var budget int
	var ctxLimit int

	tokens := strings.FieldsFunc(inner, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' '
	})
	for _, tok := range tokens {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "ctx=") || strings.HasPrefix(t, "ctx:") || strings.HasPrefix(t, "context=") || strings.HasPrefix(t, "context:") {
			valStr := t[strings.IndexAny(t, "=:"):][1:]
			if n := parseTokenCountSpec(valStr); n > 0 {
				ctxLimit = n
				recognized = true
				continue
			}
		}
		if strings.HasPrefix(t, "budget=") || strings.HasPrefix(t, "budget:") {
			valStr := t[strings.IndexAny(t, "=:"):][1:]
			if n := parseTokenCountSpec(valStr); n > 0 {
				budget = n
				if thinking == "" {
					thinking = "enabled"
				}
				recognized = true
				continue
			}
		}
		if strings.HasPrefix(t, "think=") || strings.HasPrefix(t, "think:") || strings.HasPrefix(t, "thinking=") || strings.HasPrefix(t, "thinking:") || strings.HasPrefix(t, "effort=") || strings.HasPrefix(t, "effort:") {
			valStr := t[strings.IndexAny(t, "=:"):][1:]
			if norm := normalizeCandidateThinkingToken(valStr); norm != "" {
				thinking = norm
				recognized = true
				continue
			}
			if n := parseTokenCountSpec(valStr); n > 0 {
				budget = n
				thinking = "enabled"
				recognized = true
				continue
			}
		}
		if norm := normalizeCandidateThinkingToken(t); norm != "" {
			thinking = norm
			recognized = true
			continue
		}
		if n := parseTokenCountSpec(t); n > 0 {
			// Distinguish between a thinking token budget (e.g. 1024..32768) and a
			// context window override (e.g. 64k, 128k, 200k, 1m).
			if strings.HasSuffix(t, "m") || n >= 64000 || (thinking != "" && thinking != "enabled") {
				ctxLimit = n
			} else {
				budget = n
				if thinking == "" {
					thinking = "enabled"
				}
			}
			recognized = true
		}
	}

	if !recognized {
		return spec
	}
	spec.BaseModel = base
	spec.Thinking = thinking
	spec.BudgetTokens = budget
	spec.ContextLimit = ctxLimit
	return spec
}

func normalizeCandidateThinkingToken(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "default":
		return "auto"
	case "keep", "passthrough", "preserve":
		return "keep"
	case "none", "off", "disable", "disabled", "no", "false", "0":
		return "none"
	case "minimal", "min":
		return "minimal"
	case "low":
		return "low"
	case "medium", "med", "mid":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "very_high", "very-high":
		return "xhigh"
	case "max", "maximum":
		return "max"
	case "adaptive", "dynamic":
		return "adaptive"
	case "enabled", "on", "true":
		return "enabled"
	default:
		return ""
	}
}

func normalizeTierThinkingPolicy(raw string) string {
	norm := normalizeCandidateThinkingToken(raw)
	if norm == "" {
		return "auto"
	}
	return norm
}

func defaultTierThinkingMap() map[string]string {
	return map[string]string{
		TierSimple:    "auto",
		TierMedium:    "auto",
		TierComplex:   "auto",
		TierReasoning: "auto",
	}
}

func normalizeTierThinkingMap(in map[string]string) map[string]string {
	out := defaultTierThinkingMap()
	for _, tier := range allRouterTiers {
		if v, ok := in[tier]; ok {
			out[tier] = normalizeTierThinkingPolicy(v)
		} else if v, ok := in[strings.ToLower(tier)]; ok {
			out[tier] = normalizeTierThinkingPolicy(v)
		}
	}
	return out
}

func parseTokenCountSpec(raw string) int {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return 0
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "m"):
		mult = 1048576
		s = strings.TrimSpace(strings.TrimSuffix(s, "m"))
	case strings.HasSuffix(s, "k"):
		mult = 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "k"))
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		val := int(f * float64(mult))
		if mult == 1024 {
			switch int(f) {
			case 100, 200, 400, 500, 1000:
				val = int(f) * 1000
			}
		}
		return val
	}
	return 0
}

func canonicalModelIdent(model string) string {
	s := protocolModelName(model)
	return strings.ReplaceAll(s, ".", "-")
}

// Upstream & ApiKey helpers that transparently match both the raw candidate
// string and its stripped BaseModel when an inline spec like "gpt-5.2(high)" is used.

func servesRouterCandidate(u *Upstream, raw string) bool {
	if u == nil {
		return false
	}
	if u.servesModel(raw) {
		return true
	}
	spec := parseRouterModelSpec(raw)
	return spec.BaseModel != "" && spec.BaseModel != raw && u.servesModel(spec.BaseModel)
}

func resolveUpstreamCandidateModel(u *Upstream, raw string) string {
	spec := parseRouterModelSpec(raw)
	if u != nil && u.servesModel(raw) {
		return raw
	}
	if spec.BaseModel != "" {
		return spec.BaseModel
	}
	return raw
}

func pickUpstreamForRouterCandidate(format clientFormat, rawCandidate string, tried map[int64]bool, nativeOnly bool, cr *canonReq) (*Upstream, string, error) {
	up, err := pickUpstream(format, rawCandidate, tried, nativeOnly, cr)
	if err == nil {
		return up, rawCandidate, nil
	}
	spec := parseRouterModelSpec(rawCandidate)
	if spec.BaseModel != "" && spec.BaseModel != rawCandidate {
		up2, err2 := pickUpstream(format, spec.BaseModel, tried, nativeOnly, cr)
		if err2 == nil {
			return up2, spec.BaseModel, nil
		}
		return nil, spec.BaseModel, err2
	}
	return nil, rawCandidate, err
}

func splitRouterModelList(raw string) []string {
	var out []string
	var buf strings.Builder
	depth := 0
	for _, r := range raw {
		switch r {
		case '(':
			depth++
			buf.WriteRune(r)
		case ')':
			if depth > 0 {
				depth--
			}
			buf.WriteRune(r)
		case ',', '，', '\n', '\r':
			if depth == 0 {
				if s := strings.TrimSpace(buf.String()); s != "" {
					out = append(out, s)
				}
				buf.Reset()
				continue
			}
			buf.WriteRune(r)
		default:
			buf.WriteRune(r)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func allowsRouterCandidate(ak ApiKey, raw string) bool {
	if ak.ID == 0 {
		return true
	}
	if ak.allowsModel(raw) {
		return true
	}
	spec := parseRouterModelSpec(raw)
	return spec.BaseModel != "" && spec.BaseModel != raw && ak.allowsModel(spec.BaseModel)
}

func routerCandidateQuotaExhausted(ak ApiKey, virtualModel, raw string) bool {
	if ak.allowsModel(raw) {
		return autoRouterCandidateQuotaExhausted(ak, virtualModel, raw)
	}
	spec := parseRouterModelSpec(raw)
	if spec.BaseModel != "" && spec.BaseModel != raw {
		return autoRouterCandidateQuotaExhausted(ak, virtualModel, spec.BaseModel)
	}
	return autoRouterCandidateQuotaExhausted(ak, virtualModel, raw)
}

// estimateRequestContextTokens estimates the total input token footprint of a
// canonical request across system prompt, conversation history, tool calls/results,
// reasoning history, tool definitions, and media blocks.
func estimateRequestContextTokens(cr *canonReq) int {
	if cr == nil {
		return 0
	}
	total := estimateTokens(cr.System)
	for _, msg := range cr.Messages {
		total += 4 // per-message framing overhead
		total += estimateTokens(msg.Text)
		total += estimateTokens(msg.Reasoning)
		for _, ri := range msg.ReasoningItems {
			total += estimateTokens(ri.Text)
		}
		for _, part := range msg.Parts {
			if part.Kind == "image" {
				total += 768
			} else if part.Text != "" && msg.Text == "" {
				total += estimateTokens(part.Text)
			}
		}
		for _, tc := range msg.Calls {
			total += 8 + estimateTokens(tc.Name) + estimateTokens(tc.Arguments)
		}
	}
	for _, tool := range cr.Tools {
		total += 12 + estimateTokens(tool.Name) + estimateTokens(tool.Description)
		if tool.Schema != nil {
			total += estimateTokens(jsonText(tool.Schema))
		}
	}
	return total
}

var probedModelLimits = struct {
	sync.RWMutex
	data map[string]ModelTokenLimits
}{data: map[string]ModelTokenLimits{}}

var learnedModelLimits = struct {
	sync.RWMutex
	data map[string]ModelTokenLimits
}{data: map[string]ModelTokenLimits{}}

func firstPositiveInt(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func recordProbedModelLimits(model string, ctxLimit, maxOutput int) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || (ctxLimit <= 0 && maxOutput <= 0) {
		return
	}
	probedModelLimits.Lock()
	defer probedModelLimits.Unlock()
	merge := func(key string) {
		if key == "" {
			return
		}
		cur := probedModelLimits.data[key]
		if ctxLimit > 0 {
			cur.ContextLimit = ctxLimit
		}
		if maxOutput > 0 {
			cur.MaxOutput = maxOutput
		}
		probedModelLimits.data[key] = cur
	}
	merge(model)
	if bare := protocolModelName(model); bare != "" && bare != model {
		merge(bare)
	}
}

func lookupProbedModelLimits(model string) (ModelTokenLimits, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelTokenLimits{}, false
	}
	probedModelLimits.RLock()
	defer probedModelLimits.RUnlock()
	if lim, ok := probedModelLimits.data[model]; ok && (lim.ContextLimit > 0 || lim.MaxOutput > 0) {
		return lim, true
	}
	if bare := protocolModelName(model); bare != "" && bare != model {
		if lim, ok := probedModelLimits.data[bare]; ok && (lim.ContextLimit > 0 || lim.MaxOutput > 0) {
			return lim, true
		}
	}
	return ModelTokenLimits{}, false
}

func recordLearnedModelLimits(model string, ctxLimit, maxOutput int) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || (ctxLimit <= 0 && maxOutput <= 0) {
		return
	}
	learnedModelLimits.Lock()
	defer learnedModelLimits.Unlock()
	merge := func(key string) {
		if key == "" {
			return
		}
		cur := learnedModelLimits.data[key]
		if ctxLimit > 0 {
			cur.ContextLimit = ctxLimit
		}
		if maxOutput > 0 {
			cur.MaxOutput = maxOutput
		}
		learnedModelLimits.data[key] = cur
	}
	merge(model)
	if bare := protocolModelName(model); bare != "" && bare != model {
		merge(bare)
	}
}

func lookupLearnedModelLimits(model string) (ModelTokenLimits, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelTokenLimits{}, false
	}
	learnedModelLimits.RLock()
	defer learnedModelLimits.RUnlock()
	if lim, ok := learnedModelLimits.data[model]; ok && (lim.ContextLimit > 0 || lim.MaxOutput > 0) {
		return lim, true
	}
	if bare := protocolModelName(model); bare != "" && bare != model {
		if lim, ok := learnedModelLimits.data[bare]; ok && (lim.ContextLimit > 0 || lim.MaxOutput > 0) {
			return lim, true
		}
	}
	return ModelTokenLimits{}, false
}

var (
	errContextLimitPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)maximum context length is\s*(\d{3,9})`),
		regexp.MustCompile(`(?i)max(?:imum)?[_\s-]*context[_\s-]*(?:length|window|tokens?)\s*(?:is|of|=|:|exceeded[^\d]*)\s*(\d{3,9})`),
		regexp.MustCompile(`(?i)input token count[^\d]*exceeds[^\d]*(?:maximum[^\d]*(?:allowed|of)|limit[^\d]*of)?[^\d]*\(?\s*(\d{3,9})\s*\)?`),
		regexp.MustCompile(`(?i)prompt is too long:\s*\d+\s*tokens?\s*>\s*(\d{3,9})\s*maximum`),
		regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded[^\d]*max[^\d]*(\d{3,9})`),
		regexp.MustCompile(`(?i)range of input length should be\s*\[\d+,\s*(\d{3,9})\]`),
		regexp.MustCompile(`(?i)exceeds the model(?:'s)? maximum context[^\d]*(\d{3,9})`),
	}
	errMaxOutputPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:max_tokens|max_completion_tokens|max_output_tokens)\s*(?:must be|should be|cannot exceed|is too large)[^\d]*(?:less than or equal to|at most|<=|maximum(?: of)?|is)\s*(\d{3,8})`),
		regexp.MustCompile(`(?i)maximum allowed number of output tokens(?:\s+for\s+\S+)?\s+is\s+(\d{3,8})`),
		regexp.MustCompile(`(?i)(?:max_tokens|max_completion_tokens|max_output_tokens)\s*[:=]?\s*\d{3,8}\s*>\s*(\d{3,8})`),
		regexp.MustCompile(`(?i)range of (?:max_tokens|max_completion_tokens|max_output_tokens) should be\s*\[\d+,\s*(\d{3,8})\]`),
	}
)

// learnModelLimitFromErrorBody inspects 400/413/422 error payloads from upstreams
// for explicit context length or max_tokens ceilings, caching any discovered limit
// in learnedModelLimits so subsequent routing and parameter clamping adapt automatically.
func learnModelLimitFromErrorBody(model string, status int, body []byte) (learnedCtx int, learnedMaxOut int) {
	if status != 400 && status != 413 && status != 422 {
		return 0, 0
	}
	spec := parseRouterModelSpec(model)
	base := strings.TrimSpace(spec.BaseModel)
	if base == "" || len(body) == 0 {
		return 0, 0
	}
	text := string(body)
	for _, re := range errContextLimitPatterns {
		if m := re.FindStringSubmatch(text); len(m) >= 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 512 && n <= 10_000_000 {
				learnedCtx = n
				break
			}
		}
	}
	for _, re := range errMaxOutputPatterns {
		if m := re.FindStringSubmatch(text); len(m) >= 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 256 && n <= 1_000_000 {
				learnedMaxOut = n
				break
			}
		}
	}
	if learnedCtx > 0 || learnedMaxOut > 0 {
		recordLearnedModelLimits(base, learnedCtx, learnedMaxOut)
		log.Printf("[auto-router] learned token limits for model=%s from HTTP %d error: context=%d max_output=%d",
			base, status, learnedCtx, learnedMaxOut)
	}
	return learnedCtx, learnedMaxOut
}

// effectiveModelContextLimit returns the maximum context window (in tokens) for
// a candidate model, checking inline spec overrides, admin config overrides,
// runtime error-learned limits, upstream /v1/models probed limits, LiteLLM catalog
// synced limits, model name suffixes, and built-in model family profiles.
func effectiveModelContextLimit(rawModel string, customLimits map[string]int) int {
	limit, _ := effectiveModelContextLimitWithSource(rawModel, customLimits)
	return limit
}

func effectiveModelContextLimitWithSource(rawModel string, customLimits map[string]int) (int, string) {
	spec := parseRouterModelSpec(rawModel)
	if spec.ContextLimit > 0 {
		return spec.ContextLimit, "spec"
	}
	base := strings.TrimSpace(spec.BaseModel)
	if base == "" {
		return 128000, "builtin"
	}
	lowerRaw := strings.ToLower(strings.TrimSpace(rawModel))
	lowerBase := strings.ToLower(base)
	if len(customLimits) > 0 {
		if v := customLimits[lowerRaw]; v > 0 {
			return v, "config"
		}
		if v := customLimits[lowerBase]; v > 0 {
			return v, "config"
		}
	}
	if lim, ok := lookupLearnedModelLimits(base); ok && lim.ContextLimit > 0 {
		return lim.ContextLimit, "learned"
	}
	if lim, ok := lookupProbedModelLimits(base); ok && lim.ContextLimit > 0 {
		return lim.ContextLimit, "probed"
	}
	if lim, ok := lookupAutoModelLimits(base); ok && lim.ContextLimit > 0 {
		return lim.ContextLimit, "catalog"
	}
	ctx, _ := builtinModelContextAndOutputLimits(base)
	return ctx, "builtin"
}

func modelMaxOutputLimit(rawModel string) int {
	limit, _ := modelMaxOutputLimitWithSource(rawModel)
	return limit
}

func modelMaxOutputLimitWithSource(rawModel string) (int, string) {
	spec := parseRouterModelSpec(rawModel)
	base := strings.TrimSpace(spec.BaseModel)
	if base == "" {
		return 16384, "builtin"
	}
	if lim, ok := lookupLearnedModelLimits(base); ok && lim.MaxOutput > 0 {
		return lim.MaxOutput, "learned"
	}
	if lim, ok := lookupProbedModelLimits(base); ok && lim.MaxOutput > 0 {
		return lim.MaxOutput, "probed"
	}
	if lim, ok := lookupAutoModelLimits(base); ok && lim.MaxOutput > 0 {
		return lim.MaxOutput, "catalog"
	}
	_, maxOut := builtinModelContextAndOutputLimits(base)
	return maxOut, "builtin"
}

func builtinModelContextAndOutputLimits(model string) (contextLimit int, maxOutput int) {
	lower := protocolModelName(model)

	// Check explicit context suffixes in the model name first (e.g. moonshot-v1-8k, qwen-plus-128k).
	suffixContext := 0
	for _, suf := range []struct {
		suffix string
		tokens int
	}{
		{"-2m", 2097152},
		{"-1m", 1048576},
		{"-1000k", 1000000},
		{"-512k", 524288},
		{"-400k", 400000},
		{"-256k", 262144},
		{"-200k", 200000},
		{"-128k", 131072},
		{"-100k", 100000},
		{"-64k", 65536},
		{"-32k", 32768},
		{"-16k", 16384},
		{"-8k", 8192},
		{"-4k", 4096},
	} {
		if strings.HasSuffix(lower, suf.suffix) || strings.Contains(lower, suf.suffix+"-") {
			suffixContext = suf.tokens
			break
		}
	}

	canon := canonicalModelIdent(lower)
	contextLimit = 128000
	maxOutput = 16384

	switch {
	case strings.HasPrefix(canon, "gemini-1-5-pro"):
		contextLimit, maxOutput = 2097152, 8192
	case strings.HasPrefix(canon, "gemini-1-5-flash"), strings.HasPrefix(canon, "gemini-2-0-flash"):
		contextLimit, maxOutput = 1048576, 8192
	case strings.HasPrefix(canon, "gemini-2-5-"), strings.HasPrefix(canon, "gemini-3"), strings.HasPrefix(canon, "gemini-4"):
		contextLimit, maxOutput = 1048576, 65536
	case strings.HasPrefix(canon, "gemini-"):
		contextLimit, maxOutput = 1048576, 32768

	case supportsAnthropicAdaptive(lower) || supportsAnthropicAdaptive(canon):
		contextLimit, maxOutput = 1000000, 64000
	case supportsAnthropicFixedBudget(lower) || supportsAnthropicFixedBudget(canon) ||
		strings.HasPrefix(canon, "claude-opus-4") ||
		strings.HasPrefix(canon, "claude-sonnet-4") ||
		strings.HasPrefix(canon, "claude-haiku-4") ||
		strings.HasPrefix(canon, "claude-3-7-sonnet"):
		contextLimit, maxOutput = 200000, 64000
	case strings.HasPrefix(canon, "claude-3-5-"):
		contextLimit, maxOutput = 200000, 8192
	case strings.HasPrefix(canon, "claude-3-"):
		contextLimit, maxOutput = 200000, 4096

	case strings.HasPrefix(lower, "gpt-5"), strings.HasPrefix(lower, "codex-mini"):
		contextLimit, maxOutput = 400000, 128000
	case strings.HasPrefix(lower, "gpt-4.1"):
		contextLimit, maxOutput = 1047576, 32768
	case strings.HasPrefix(lower, "o1-mini"):
		contextLimit, maxOutput = 128000, 65536
	case strings.HasPrefix(lower, "o1-preview"):
		contextLimit, maxOutput = 128000, 32768
	case strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"):
		contextLimit, maxOutput = 200000, 100000
	case strings.HasPrefix(lower, "gpt-4o"), strings.HasPrefix(lower, "chatgpt-4o"):
		contextLimit, maxOutput = 128000, 16384
	case strings.HasPrefix(lower, "gpt-4-turbo"):
		contextLimit, maxOutput = 128000, 4096
	case lower == "gpt-4" || strings.HasPrefix(lower, "gpt-4-0"):
		contextLimit, maxOutput = 8192, 4096
	case strings.HasPrefix(lower, "gpt-3.5-turbo"):
		contextLimit, maxOutput = 16385, 4096

	case strings.HasPrefix(lower, "deepseek-reasoner"), strings.HasPrefix(lower, "deepseek-r1"), strings.HasPrefix(lower, "deepseek-v4"):
		contextLimit, maxOutput = 128000, 32768
	case strings.HasPrefix(lower, "deepseek-"):
		contextLimit, maxOutput = 128000, 8192

	case strings.HasPrefix(lower, "glm-4-long"):
		contextLimit, maxOutput = 1000000, 4096
	case strings.HasPrefix(lower, "glm-5"), strings.HasPrefix(lower, "glm-4.7"), strings.HasPrefix(lower, "glm-4.6"), strings.HasPrefix(lower, "glm-4.5"), strings.HasPrefix(lower, "glm-z1"):
		contextLimit, maxOutput = 128000, 32768
	case strings.HasPrefix(lower, "glm-4"):
		contextLimit, maxOutput = 128000, 4096

	case strings.HasPrefix(lower, "qwen-long"), strings.Contains(lower, "1m"):
		contextLimit, maxOutput = 1000000, 8192
	case strings.HasPrefix(lower, "qwen3"), strings.HasPrefix(lower, "qwq"):
		contextLimit, maxOutput = 131072, 32768
	case strings.HasPrefix(lower, "qwen-turbo"):
		contextLimit, maxOutput = 131072, 8192
	case strings.HasPrefix(lower, "qwen"):
		contextLimit, maxOutput = 131072, 16384

	case strings.HasPrefix(lower, "kimi-k2"):
		contextLimit, maxOutput = 131072, 16384
	case strings.HasPrefix(lower, "moonshot-v1"):
		contextLimit, maxOutput = 131072, 8192
	case strings.HasPrefix(lower, "grok-4"):
		contextLimit, maxOutput = 256000, 65536
	case strings.HasPrefix(lower, "grok-"):
		contextLimit, maxOutput = 131072, 32768
	case strings.HasPrefix(lower, "minimax"):
		contextLimit, maxOutput = 200000, 32768
	case strings.HasPrefix(lower, "doubao"):
		contextLimit, maxOutput = 128000, 16384
	}

	if suffixContext > 0 {
		contextLimit = suffixContext
		if maxOutput > contextLimit/2 && contextLimit >= 4096 {
			maxOutput = contextLimit / 2
		}
	}
	return contextLimit, maxOutput
}

// modelThinkingProfile describes how a target model supports reasoning/thinking
// so Auto-Router can adapt or strip thinking parameters across heterogeneous models.
type modelThinkingProfile struct {
	SupportsThinking        bool
	Efforts                 []string
	AnthropicAdaptive       bool // Claude 4.6+: thinking={type:"adaptive"} + output_config.effort
	AnthropicFixedBudget    bool // Claude 3.7/4.0/4.1/4.5: thinking={type:"enabled", budget_tokens:...}
	AnthropicSupportsEffort bool // Claude 4.5+: supports output_config.effort
	GeminiUseThinkingLevel  bool // Gemini 3/4: generationConfig.thinkingConfig.thinkingLevel
	GeminiUseThinkingBudget bool // Gemini 2.5: generationConfig.thinkingConfig.thinkingBudget
	GeminiMinBudget         int
	GeminiMaxBudget         int
	GeminiCanDisable        bool
	GeminiMandatoryThinking bool // e.g. gemini-2.5-pro cannot set budget=0
}

func classifyModelThinkingCapability(up *Upstream, rawModel string) modelThinkingProfile {
	spec := parseRouterModelSpec(rawModel)
	base := strings.TrimSpace(spec.BaseModel)
	if base == "" {
		return modelThinkingProfile{}
	}
	lower := protocolModelName(base)
	canon := canonicalModelIdent(lower)

	// 1. Anthropic Claude family
	if strings.HasPrefix(canon, "claude-") {
		if strings.HasPrefix(canon, "claude-3-5-") ||
			strings.HasPrefix(canon, "claude-3-opus") ||
			strings.HasPrefix(canon, "claude-3-sonnet") ||
			strings.HasPrefix(canon, "claude-3-haiku") ||
			strings.HasPrefix(canon, "claude-2") ||
			strings.HasPrefix(canon, "claude-instant") {
			return modelThinkingProfile{SupportsThinking: false}
		}
		isAdaptive := supportsAnthropicAdaptive(lower) || supportsAnthropicAdaptive(canon)
		isFixed := supportsAnthropicFixedBudget(lower) || supportsAnthropicFixedBudget(canon)
		if !isAdaptive && !isFixed {
			return modelThinkingProfile{SupportsThinking: false}
		}
		var efforts []string
		if up != nil {
			efforts = upstreamReasoningEfforts(up, base)
		} else {
			efforts = defaultReasoningEfforts(base)
		}
		if len(efforts) == 0 {
			switch {
			case strings.HasPrefix(canon, "claude-opus-4-7"),
				strings.HasPrefix(canon, "claude-sonnet-4-7"),
				strings.HasPrefix(canon, "claude-opus-4-8"),
				strings.HasPrefix(canon, "claude-sonnet-4-8"),
				strings.HasPrefix(canon, "claude-opus-5"),
				strings.HasPrefix(canon, "claude-sonnet-5"):
				efforts = []string{"low", "medium", "high", "xhigh", "max"}
			case strings.HasPrefix(canon, "claude-opus-4-6"),
				strings.HasPrefix(canon, "claude-sonnet-4-6"):
				efforts = []string{"low", "medium", "high", "max"}
			default:
				efforts = []string{"low", "medium", "high"}
			}
		}
		supportsEffort := isAdaptive ||
			strings.HasPrefix(canon, "claude-opus-4-5") ||
			strings.HasPrefix(canon, "claude-sonnet-4-5") ||
			strings.HasPrefix(canon, "claude-haiku-4-5")
		return modelThinkingProfile{
			SupportsThinking:        true,
			Efforts:                 efforts,
			AnthropicAdaptive:       isAdaptive,
			AnthropicFixedBudget:    !isAdaptive && isFixed,
			AnthropicSupportsEffort: supportsEffort,
		}
	}

	// 2. Google Gemini family
	if strings.HasPrefix(lower, "gemini-") {
		if levels := geminiThinkingLevels(lower); len(levels) > 0 {
			return modelThinkingProfile{
				SupportsThinking:       true,
				Efforts:                levels,
				GeminiUseThinkingLevel: true,
				GeminiCanDisable:       false,
			}
		}
		if strings.HasPrefix(lower, "gemini-2.5-pro") || strings.HasPrefix(canon, "gemini-2-5-pro") {
			return modelThinkingProfile{
				SupportsThinking:        true,
				Efforts:                 []string{"low", "medium", "high"},
				GeminiUseThinkingBudget: true,
				GeminiMinBudget:         128,
				GeminiMaxBudget:         32768,
				GeminiCanDisable:        false,
				GeminiMandatoryThinking: true,
			}
		}
		if strings.HasPrefix(lower, "gemini-2.5-flash") || strings.HasPrefix(canon, "gemini-2-5-flash") {
			minB := 128
			if strings.Contains(lower, "lite") {
				minB = 512
			}
			return modelThinkingProfile{
				SupportsThinking:        true,
				Efforts:                 []string{"low", "medium", "high"},
				GeminiUseThinkingBudget: true,
				GeminiMinBudget:         minB,
				GeminiMaxBudget:         24576,
				GeminiCanDisable:        true,
				GeminiMandatoryThinking: false,
			}
		}
		return modelThinkingProfile{SupportsThinking: false}
	}

	// 3. Check catalog / exact upstream reasoning efforts
	var catalogEfforts []string
	if up != nil {
		catalogEfforts = upstreamReasoningEfforts(up, base)
	} else {
		catalogEfforts = defaultReasoningEfforts(base)
	}
	if len(catalogEfforts) > 0 {
		return modelThinkingProfile{
			SupportsThinking: true,
			Efforts:          catalogEfforts,
		}
	}

	// 4. Known non-thinking OpenAI / domestic / third-party models
	if strings.HasPrefix(lower, "gpt-4o") ||
		strings.HasPrefix(lower, "chatgpt-4o") ||
		strings.HasPrefix(lower, "gpt-4.1") ||
		strings.HasPrefix(lower, "gpt-4-turbo") ||
		lower == "gpt-4" ||
		strings.HasPrefix(lower, "gpt-3.5") ||
		strings.HasPrefix(lower, "o1-mini") ||
		strings.HasPrefix(lower, "o1-preview") ||
		strings.HasPrefix(lower, "deepseek-coder") ||
		(strings.HasPrefix(lower, "deepseek-chat") && !strings.Contains(lower, "thinking") && !strings.Contains(lower, "reason")) ||
		(strings.HasPrefix(lower, "deepseek-v3") && !strings.Contains(lower, "thinking") && !strings.Contains(lower, "reason")) ||
		strings.HasPrefix(lower, "glm-4-flash") ||
		strings.HasPrefix(lower, "glm-4-air") ||
		strings.HasPrefix(lower, "glm-4-plus") ||
		strings.HasPrefix(lower, "glm-4-long") ||
		strings.HasPrefix(lower, "qwen-turbo") ||
		strings.HasPrefix(lower, "qwen-plus") ||
		strings.HasPrefix(lower, "qwen-max") ||
		strings.HasPrefix(lower, "qwen-long") ||
		(strings.HasPrefix(lower, "qwen2.5") && !strings.Contains(lower, "thinking")) ||
		(strings.HasPrefix(lower, "moonshot-v1") && !strings.Contains(lower, "thinking")) ||
		strings.HasPrefix(lower, "grok-2") {
		return modelThinkingProfile{SupportsThinking: false}
	}

	// 5. Prefix/family fallback for reasoning-capable models
	switch {
	case strings.HasPrefix(lower, "gpt-5.1-codex-max"), strings.HasPrefix(lower, "gpt-5.2-codex"), strings.HasPrefix(lower, "gpt-5.3-codex"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"low", "medium", "high", "xhigh"}}
	case strings.HasPrefix(lower, "gpt-5.2"), strings.HasPrefix(lower, "gpt-5.3"), strings.HasPrefix(lower, "gpt-5.4"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"none", "low", "medium", "high", "xhigh"}}
	case strings.HasPrefix(lower, "gpt-5.1"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"none", "low", "medium", "high"}}
	case strings.HasPrefix(lower, "gpt-5"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"minimal", "low", "medium", "high"}}
	case strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"), strings.HasPrefix(lower, "codex-mini"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"low", "medium", "high"}}
	case strings.HasPrefix(lower, "deepseek-reasoner"),
		strings.HasPrefix(lower, "deepseek-r1"),
		strings.HasPrefix(lower, "deepseek-v4"),
		strings.HasPrefix(lower, "deepseek-flash"),
		strings.Contains(lower, "reasoner"),
		strings.Contains(lower, "thinking"),
		strings.HasPrefix(lower, "qwq"),
		strings.HasPrefix(lower, "qwen3"),
		strings.HasPrefix(lower, "glm-5"),
		strings.HasPrefix(lower, "glm-4.7"),
		strings.HasPrefix(lower, "glm-4.6"),
		strings.HasPrefix(lower, "glm-4.5"),
		strings.HasPrefix(lower, "glm-z1"),
		strings.HasPrefix(lower, "grok-3-mini"),
		strings.HasPrefix(lower, "grok-4"),
		strings.HasPrefix(lower, "minimax-m1"),
		strings.HasPrefix(lower, "minimax-m2"):
		return modelThinkingProfile{SupportsThinking: true, Efforts: []string{"low", "medium", "high"}}
	default:
		return modelThinkingProfile{SupportsThinking: false}
	}
}

// autoRouterThinkingPlan describes the resolved thinking configuration for a
// specific candidate model and routing tier.
type autoRouterThinkingPlan struct {
	StripAll        bool   // Remove all thinking/reasoning parameters
	DisableExplicit bool   // Explicitly send disabled thinking config if needed
	Effort          string // Resolved effort level ("none", "minimal", "low", "medium", "high", "xhigh", "max")
	ThinkingMode    string // "", "adaptive", "enabled", "disabled"
	ThinkingBudget  int    // Resolved token budget (>0 when fixed budget is used)
	EffectiveLabel  string // Human-readable summary for dry-run / headers / logs
}

func resolveAutoRouterThinking(
	up *Upstream,
	targetModel string,
	sourceTier string,
	rawCandidate string,
	tierPolicy string,
	cause string,
	cr *canonReq,
) autoRouterThinkingPlan {
	if strings.TrimSpace(targetModel) == "" {
		targetModel = rawCandidate
	}
	profile := classifyModelThinkingCapability(up, targetModel)
	if !profile.SupportsThinking {
		return autoRouterThinkingPlan{
			StripAll:       true,
			EffectiveLabel: "none",
		}
	}

	spec := parseRouterModelSpec(rawCandidate)
	policy := normalizeTierThinkingPolicy(tierPolicy)

	var clientEffort string
	var clientMode string
	var clientBudget int
	if cr != nil {
		clientEffort = strings.ToLower(strings.TrimSpace(cr.ReasoningEffort))
		clientMode = strings.ToLower(strings.TrimSpace(cr.ThinkingMode))
		if cr.ThinkingBudget != nil {
			clientBudget = *cr.ThinkingBudget
		}
	}
	clientSentThinking := clientEffort != "" || clientMode != "" || clientBudget > 0

	// Determine desired directive and desired budget
	directive := ""
	desiredBudget := 0
	explicitConfig := false

	switch {
	case spec.Thinking != "" && spec.Thinking != "auto":
		directive = spec.Thinking
		desiredBudget = spec.BudgetTokens
		explicitConfig = true
	case spec.BudgetTokens > 0:
		directive = "enabled"
		desiredBudget = spec.BudgetTokens
		explicitConfig = true
	case policy != "" && policy != "auto":
		directive = policy
		explicitConfig = true
	default:
		// "auto" policy
		if strings.HasPrefix(cause, "housekeeping") {
			directive = "none"
		} else if clientMode == "disabled" || clientEffort == "none" {
			directive = "none"
		} else if clientSentThinking {
			directive, desiredBudget = adaptClientThinkingForTier(sourceTier, clientEffort, clientMode, clientBudget)
		} else {
			return autoRouterThinkingPlan{
				EffectiveLabel: "default",
			}
		}
	}

	if directive == "keep" {
		if !clientSentThinking {
			return autoRouterThinkingPlan{EffectiveLabel: "default"}
		}
		if clientMode == "disabled" || clientEffort == "none" {
			directive = "none"
		} else {
			directive = clientEffort
			if directive == "" {
				if clientBudget > 0 {
					directive = budgetToEffort(clientBudget)
				} else {
					directive = "high"
				}
			}
			desiredBudget = clientBudget
		}
	}

	// Handle "none" (disable thinking)
	if directive == "none" {
		if profile.GeminiMandatoryThinking {
			minB := profile.GeminiMinBudget
			if minB <= 0 {
				minB = 128
			}
			return autoRouterThinkingPlan{
				ThinkingMode:   "enabled",
				ThinkingBudget: minB,
				EffectiveLabel: fmt.Sprintf("budget:%d", minB),
			}
		}
		if explicitConfig {
			if containsEffort(profile.Efforts, "none") {
				return autoRouterThinkingPlan{
					DisableExplicit: true,
					Effort:          "none",
					EffectiveLabel:  "none",
				}
			}
			if profile.GeminiUseThinkingBudget && profile.GeminiCanDisable {
				return autoRouterThinkingPlan{
					DisableExplicit: true,
					Effort:          "none",
					ThinkingMode:    "disabled",
					ThinkingBudget:  0,
					EffectiveLabel:  "none",
				}
			}
		}
		return autoRouterThinkingPlan{
			StripAll:       true,
			EffectiveLabel: "none",
		}
	}

	// Map directive to effort & budget
	desiredEffort := directive
	if desiredEffort == "adaptive" || desiredEffort == "enabled" {
		if clientEffort != "" && clientEffort != "none" {
			desiredEffort = clientEffort
		} else if desiredBudget > 0 {
			desiredEffort = budgetToEffort(desiredBudget)
		} else {
			desiredEffort = defaultEffortForTier(sourceTier)
		}
	}
	if desiredBudget <= 0 {
		if clientBudget > 0 && !explicitConfig {
			desiredBudget = clientBudget
		} else {
			desiredBudget = effortToBudgetTokens(desiredEffort, sourceTier)
		}
	}

	matchedEffort := matchSupportedEffort(desiredEffort, profile.Efforts)

	// 1. Anthropic Adaptive models (Claude 4.6+)
	if profile.AnthropicAdaptive {
		if matchedEffort == "" {
			matchedEffort = "high"
		}
		return autoRouterThinkingPlan{
			Effort:         matchedEffort,
			ThinkingMode:   "adaptive",
			ThinkingBudget: 0,
			EffectiveLabel: "adaptive:" + matchedEffort,
		}
	}

	// 2. Anthropic Fixed-Budget models (Claude 3.7 / 4.0 / 4.1 / 4.5)
	if profile.AnthropicFixedBudget {
		budget := desiredBudget
		if budget < 1024 {
			budget = 1024
		}
		eff := ""
		if profile.AnthropicSupportsEffort && matchedEffort != "" {
			eff = matchedEffort
		}
		return autoRouterThinkingPlan{
			Effort:         eff,
			ThinkingMode:   "enabled",
			ThinkingBudget: budget,
			EffectiveLabel: fmt.Sprintf("budget:%d", budget),
		}
	}

	// 3. Gemini 2.5 ThinkingBudget models
	if profile.GeminiUseThinkingBudget {
		budget := desiredBudget
		if profile.GeminiMinBudget > 0 && budget < profile.GeminiMinBudget {
			budget = profile.GeminiMinBudget
		}
		if profile.GeminiMaxBudget > 0 && budget > profile.GeminiMaxBudget {
			budget = profile.GeminiMaxBudget
		}
		return autoRouterThinkingPlan{
			Effort:         "",
			ThinkingMode:   "enabled",
			ThinkingBudget: budget,
			EffectiveLabel: fmt.Sprintf("budget:%d", budget),
		}
	}

	// 4. Gemini 3/4 ThinkingLevel models
	if profile.GeminiUseThinkingLevel {
		if matchedEffort == "" {
			matchedEffort = "medium"
		}
		return autoRouterThinkingPlan{
			Effort:         matchedEffort,
			ThinkingMode:   "adaptive",
			ThinkingBudget: 0,
			EffectiveLabel: matchedEffort,
		}
	}

	// 5. OpenAI / DeepSeek / Qwen / GLM / Grok reasoning models
	if matchedEffort == "" {
		matchedEffort = "medium"
	}
	return autoRouterThinkingPlan{
		Effort:         matchedEffort,
		ThinkingMode:   "",
		ThinkingBudget: 0,
		EffectiveLabel: matchedEffort,
	}
}

func adaptClientThinkingForTier(tier, clientEffort, clientMode string, clientBudget int) (string, int) {
	eff := clientEffort
	if eff == "" {
		if clientBudget > 0 {
			eff = budgetToEffort(clientBudget)
		} else {
			eff = defaultEffortForTier(tier)
		}
	}
	budget := clientBudget

	switch strings.ToUpper(strings.TrimSpace(tier)) {
	case TierSimple:
		if effortRank(eff) > effortRank("low") {
			eff = "low"
		}
		if budget > 1024 {
			budget = 1024
		} else if budget <= 0 {
			budget = 1024
		}
	case TierMedium:
		if effortRank(eff) > effortRank("medium") {
			eff = "medium"
		}
		if budget > 4096 {
			budget = 4096
		} else if budget <= 0 {
			budget = 4096
		}
	case TierReasoning:
		if effortRank(eff) < effortRank("high") {
			eff = "high"
		}
		if budget > 0 && budget < 8192 {
			budget = 8192
		}
	default: // COMPLEX or DEFAULT
		if budget <= 0 {
			budget = effortToBudgetTokens(eff, tier)
		}
	}
	return eff, budget
}

func defaultEffortForTier(tier string) string {
	switch strings.ToUpper(strings.TrimSpace(tier)) {
	case TierSimple:
		return "low"
	case TierMedium:
		return "medium"
	case TierReasoning:
		return "high"
	default:
		return "high"
	}
}

func effortRank(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none":
		return 0
	case "minimal":
		return 1
	case "low":
		return 2
	case "medium":
		return 3
	case "high":
		return 4
	case "xhigh":
		return 5
	case "max":
		return 6
	default:
		return 3
	}
}

func effortToBudgetTokens(effort, tier string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return 1024
	case "low":
		if strings.EqualFold(tier, TierSimple) {
			return 1024
		}
		return 2048
	case "medium":
		return 4096
	case "high":
		if strings.EqualFold(tier, TierReasoning) {
			return 16384
		}
		return 8192
	case "xhigh":
		return 24576
	case "max":
		return 31999
	default:
		return 4096
	}
}

func budgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 1024:
		return "low"
	case budget <= 4096:
		return "medium"
	case budget <= 16384:
		return "high"
	case budget <= 24576:
		return "xhigh"
	default:
		return "max"
	}
}

func containsEffort(list []string, target string) bool {
	for _, item := range list {
		if strings.EqualFold(item, target) {
			return true
		}
	}
	return false
}

func matchSupportedEffort(desired string, supported []string) string {
	desired = strings.ToLower(strings.TrimSpace(desired))
	if desired == "" {
		return ""
	}
	if len(supported) == 0 {
		return desired
	}
	if containsEffort(supported, desired) {
		return desired
	}
	var fallbacks []string
	switch desired {
	case "max":
		fallbacks = []string{"xhigh", "high", "medium", "low"}
	case "xhigh":
		fallbacks = []string{"max", "high", "medium", "low"}
	case "high":
		fallbacks = []string{"medium", "xhigh", "max", "low"}
	case "medium":
		fallbacks = []string{"high", "low", "minimal"}
	case "low":
		fallbacks = []string{"minimal", "medium", "none", "high"}
	case "minimal":
		fallbacks = []string{"low", "none", "medium"}
	case "none":
		fallbacks = []string{"minimal", "low"}
	}
	for _, fb := range fallbacks {
		if containsEffort(supported, fb) {
			return fb
		}
	}
	return supported[len(supported)-1]
}

// computeClampedMaxTokens returns the safe max_tokens value for targetModel
// given the client's requestedMaxTokens and estimatedInputTokens.
func computeClampedMaxTokens(targetModel string, requestedMaxTokens, estimatedInputTokens int, customLimits map[string]int) int {
	if requestedMaxTokens <= 0 {
		return 0
	}
	maxOut := modelMaxOutputLimit(targetModel)
	ctxLimit := effectiveModelContextLimit(targetModel, customLimits)

	limit := maxOut
	if ctxLimit > 0 && estimatedInputTokens > 0 {
		rem := ctxLimit - estimatedInputTokens - 256
		if rem >= 256 && (limit <= 0 || rem < limit) {
			limit = rem
		}
	}
	if limit > 0 && requestedMaxTokens > limit {
		return limit
	}
	return requestedMaxTokens
}

func isRecoverableAutoRouterConversionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "thinking") ||
		strings.Contains(msg, "reasoning") ||
		strings.Contains(msg, "budget") ||
		strings.Contains(msg, "context_management") ||
		strings.Contains(msg, "includethoughts") ||
		strings.Contains(msg, "output_config")
}

// adaptAutoRouterCanonReq adapts a canonical request (used when converting between
// wire formats) for the selected candidate model's context window, multi-turn
// history compatibility, and thinking capability.
func adaptAutoRouterCanonReq(cr *canonReq, wireFormat clientFormat, up *Upstream, upModel string, task *relayTask, effortLevels []string) {
	if cr == nil || task == nil {
		return
	}

	plan := resolveAutoRouterThinking(
		up,
		upModel,
		task.sourceTier,
		task.candidateRawModel,
		task.tierThinkingPolicy,
		task.routerCause(),
		cr,
	)

	if isRecoverableAutoRouterConversionError(cr.ConversionError) {
		cr.ConversionError = nil
	}

	// 1. Multi-turn cross-model history sanitization
	sameModelAndUpstream := task.prevUpstreamID != 0 &&
		task.prevUpstreamID == up.ID &&
		strings.EqualFold(task.prevUpstreamModel, upModel)
	thinkingActive := !plan.StripAll && plan.Effort != "none" && plan.ThinkingMode != "disabled"

	if !sameModelAndUpstream || !thinkingActive {
		sanitized := make([]canonMsg, len(cr.Messages))
		copy(sanitized, cr.Messages)
		for i := range sanitized {
			msg := &sanitized[i]
			msg.ReasoningIDs = nil
			msg.ReasoningItems = nil
			if msg.Role == "assistant" && msg.Reasoning != "" {
				if strings.TrimSpace(msg.Text) == "" && len(msg.Parts) == 0 && len(msg.Calls) == 0 {
					msg.Text = msg.Reasoning
					msg.Parts = []canonPart{{Kind: "text", Text: msg.Reasoning}}
				}
				msg.Reasoning = ""
			}
		}
		cr.Messages = sanitized
	}

	// 2. Apply thinking plan to canonical request
	switch {
	case plan.StripAll:
		cr.ReasoningEffort = ""
		cr.ThinkingMode = ""
		cr.ThinkingBudget = nil
	case plan.DisableExplicit:
		cr.ReasoningEffort = plan.Effort
		cr.ThinkingMode = plan.ThinkingMode
		cr.ThinkingBudget = nil
	case plan.EffectiveLabel != "" && plan.EffectiveLabel != "default":
		cr.ReasoningEffort = plan.Effort
		cr.ThinkingMode = plan.ThinkingMode
		if plan.ThinkingBudget > 0 {
			b := plan.ThinkingBudget
			cr.ThinkingBudget = &b
		} else {
			cr.ThinkingBudget = nil
		}
	default:
		if len(effortLevels) > 0 {
			cr.ReasoningEffort = mapReasoningEffort(protocolReasoningEffort(wireFormat, cr), effortLevels)
		}
	}

	// Ensure applyProtocolThinking compatibility for target wireFormat
	switch wireFormat {
	case FormatGemini:
		if cr.ThinkingBudget != nil {
			cr.ReasoningEffort = ""
		} else if cr.ReasoningEffort != "" && cr.ReasoningEffort != "none" {
			levels := geminiThinkingLevels(protocolModelName(upModel))
			if len(levels) == 0 {
				cr.ReasoningEffort = ""
			} else {
				cr.ReasoningEffort = matchSupportedEffort(cr.ReasoningEffort, levels)
			}
		}
	case FormatAnthropic:
		if !supportsAnthropicFixedBudget(protocolModelName(upModel)) {
			cr.ThinkingBudget = nil
			if cr.ThinkingMode == "enabled" {
				if supportsAnthropicAdaptive(protocolModelName(upModel)) {
					cr.ThinkingMode = "adaptive"
				} else {
					cr.ThinkingMode = ""
				}
			}
		}
		if !supportsAnthropicAdaptive(protocolModelName(upModel)) && cr.ThinkingMode == "adaptive" {
			if supportsAnthropicFixedBudget(protocolModelName(upModel)) {
				b := 4096
				if plan.ThinkingBudget >= 1024 {
					b = plan.ThinkingBudget
				}
				cr.ThinkingMode = "enabled"
				cr.ThinkingBudget = &b
			} else {
				cr.ThinkingMode = ""
			}
		}
	case FormatOpenAIChat, FormatOpenAIResponse:
		if cr.ThinkingMode == "adaptive" && cr.ReasoningEffort == "" {
			cr.ThinkingMode = ""
		}
	}

	// 3. Clamp max_tokens to target model's output & context limits
	if cr.MaxTokens > 0 {
		cr.MaxTokens = computeClampedMaxTokens(upModel, cr.MaxTokens, task.estimatedInputTokens, task.modelContextLimits)
	}

	// 4. Enforce Anthropic budget_tokens < max_tokens invariant
	if wireFormat == FormatAnthropic && cr.ThinkingBudget != nil && *cr.ThinkingBudget > 0 {
		budget := *cr.ThinkingBudget
		maxOut := modelMaxOutputLimit(upModel)
		if maxOut <= 0 {
			maxOut = 64000
		}
		if cr.MaxTokens <= budget {
			desiredMax := budget + 4096
			if desiredMax > maxOut {
				desiredMax = maxOut
			}
			if desiredMax > budget {
				cr.MaxTokens = desiredMax
			} else if cr.MaxTokens >= 2048 {
				b := cr.MaxTokens - 1024
				cr.ThinkingBudget = &b
			} else {
				cr.ThinkingMode = ""
				cr.ThinkingBudget = nil
			}
		}
	}
}

// adaptAutoRouterNativeBody adapts a native-format JSON request body without
// mutating task.body or rounding 64-bit integer IDs.
func adaptAutoRouterNativeBody(body []byte, wireFormat clientFormat, up *Upstream, upModel string, task *relayTask, effortLevels []string) []byte {
	if len(body) == 0 || task == nil {
		return body
	}
	raw, err := decodeProtocolObject(body)
	if err != nil {
		return body
	}

	plan := resolveAutoRouterThinking(
		up,
		upModel,
		task.sourceTier,
		task.candidateRawModel,
		task.tierThinkingPolicy,
		task.routerCause(),
		task.canon,
	)

	sameModelAndUpstream := task.prevUpstreamID != 0 &&
		task.prevUpstreamID == up.ID &&
		strings.EqualFold(task.prevUpstreamModel, upModel)
	profile := classifyModelThinkingCapability(up, upModel)
	thinkingActive := profile.SupportsThinking && !plan.StripAll && plan.Effort != "none" && plan.ThinkingMode != "disabled"

	changed := false

	// 1. Multi-turn history sanitization in native body
	switch wireFormat {
	case FormatAnthropic:
		if sanitizeAnthropicNativeHistory(raw, sameModelAndUpstream, thinkingActive) {
			changed = true
		}
	case FormatOpenAIResponse:
		if sanitizeOpenAIResponsesNativeHistory(raw, sameModelAndUpstream, thinkingActive) {
			changed = true
		}
	case FormatGemini:
		if sanitizeGeminiNativeHistory(raw, sameModelAndUpstream, thinkingActive) {
			changed = true
		}
	case FormatOpenAIChat:
		if !thinkingActive && sanitizeOpenAIChatNativeHistory(raw) {
			changed = true
		}
	}

	// 2. Thinking parameter adaptation in native body
	if adaptNativeThinkingFields(raw, wireFormat, plan, effortLevels) {
		changed = true
	}

	// 3. Max tokens clamping & Anthropic budget_tokens < max_tokens enforcement
	if clampNativeMaxTokensFields(raw, wireFormat, upModel, task.estimatedInputTokens, task.modelContextLimits) {
		changed = true
	}

	if !changed {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(raw); err != nil {
		return body
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func sanitizeAnthropicNativeHistory(raw map[string]any, sameModelAndUpstream, thinkingActive bool) bool {
	msgs, ok := raw["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	for i, item := range msgs {
		msgMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		if role != "assistant" {
			continue
		}
		blocks, ok := msgMap["content"].([]any)
		if !ok || len(blocks) == 0 {
			continue
		}
		var filtered []any
		var fallbackThinkingTexts []string
		blocksModified := false
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				filtered = append(filtered, b)
				continue
			}
			bType, _ := bm["type"].(string)
			switch bType {
			case "thinking":
				sig, _ := bm["signature"].(string)
				if !thinkingActive || !sameModelAndUpstream || strings.TrimSpace(sig) == "" {
					if txt, _ := bm["thinking"].(string); strings.TrimSpace(txt) != "" {
						fallbackThinkingTexts = append(fallbackThinkingTexts, txt)
					}
					blocksModified = true
					continue
				}
			case "redacted_thinking":
				if !thinkingActive || !sameModelAndUpstream {
					blocksModified = true
					continue
				}
			}
			filtered = append(filtered, b)
		}
		if blocksModified {
			if len(filtered) == 0 {
				text := strings.Join(fallbackThinkingTexts, "\n\n")
				if strings.TrimSpace(text) == "" {
					text = "..."
				}
				filtered = []any{map[string]any{
					"type": "text",
					"text": text,
				}}
			}
			clonedMsg := cloneMapShallow(msgMap)
			clonedMsg["content"] = filtered
			msgs[i] = clonedMsg
			changed = true
		}
	}
	if changed {
		raw["messages"] = msgs
	}
	return changed
}

func sanitizeOpenAIResponsesNativeHistory(raw map[string]any, sameModelAndUpstream, thinkingActive bool) bool {
	if sameModelAndUpstream && thinkingActive {
		return false
	}
	changed := false
	if items, ok := raw["input"].([]any); ok && len(items) > 0 {
		if _, hasPrev := raw["previous_response_id"]; hasPrev && !sameModelAndUpstream {
			delete(raw, "previous_response_id")
			changed = true
		}
		var filtered []any
		for _, item := range items {
			im, ok := item.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			itemType, _ := im["type"].(string)
			if itemType == "reasoning" {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if changed && len(filtered) > 0 {
			raw["input"] = filtered
		}
	}
	return changed
}

func sanitizeGeminiNativeHistory(raw map[string]any, sameModelAndUpstream, thinkingActive bool) bool {
	if sameModelAndUpstream && thinkingActive {
		return false
	}
	contents, ok := raw["contents"].([]any)
	if !ok || len(contents) == 0 {
		return false
	}
	changed := false
	for i, item := range contents {
		cm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := cm["parts"].([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		var newParts []any
		var fallbackParts []any
		partsModified := false
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				newParts = append(newParts, p)
				continue
			}
			clonedPart := cloneMapShallow(pm)
			if _, hasSig := clonedPart["thoughtSignature"]; hasSig {
				delete(clonedPart, "thoughtSignature")
				partsModified = true
			}
			if _, hasSig := clonedPart["thought_signature"]; hasSig {
				delete(clonedPart, "thought_signature")
				partsModified = true
			}
			if thought, _ := clonedPart["thought"].(bool); thought {
				delete(clonedPart, "thought")
				fallbackParts = append(fallbackParts, clonedPart)
				partsModified = true
				continue
			}
			newParts = append(newParts, clonedPart)
		}
		if partsModified {
			if len(newParts) == 0 && len(fallbackParts) > 0 {
				newParts = fallbackParts
			}
			clonedContent := cloneMapShallow(cm)
			clonedContent["parts"] = newParts
			contents[i] = clonedContent
			changed = true
		}
	}
	if changed {
		raw["contents"] = contents
	}
	return changed
}

func sanitizeOpenAIChatNativeHistory(raw map[string]any) bool {
	msgs, ok := raw["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	changed := false
	for i, item := range msgs {
		mm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		_, hasRC := mm["reasoning_content"]
		_, hasR := mm["reasoning"]
		if hasRC || hasR {
			cloned := cloneMapShallow(mm)
			if str, _ := cloned["content"].(string); strings.TrimSpace(str) == "" {
				if rc, _ := cloned["reasoning_content"].(string); strings.TrimSpace(rc) != "" {
					if _, hasTC := cloned["tool_calls"]; !hasTC {
						cloned["content"] = rc
					}
				}
			}
			delete(cloned, "reasoning_content")
			delete(cloned, "reasoning")
			msgs[i] = cloned
			changed = true
		}
	}
	if changed {
		raw["messages"] = msgs
	}
	return changed
}

func adaptNativeThinkingFields(raw map[string]any, wireFormat clientFormat, plan autoRouterThinkingPlan, effortLevels []string) bool {
	changed := false

	// Default passthrough case: only map existing effort if effortLevels is non-empty
	if !plan.StripAll && !plan.DisableExplicit && (plan.EffectiveLabel == "" || plan.EffectiveLabel == "default") {
		if len(effortLevels) == 0 {
			return false
		}
		switch wireFormat {
		case FormatOpenAIChat:
			if v, ok := raw["reasoning_effort"].(string); ok && v != "" {
				if mapped := mapReasoningEffort(v, effortLevels); mapped != "" && mapped != v {
					raw["reasoning_effort"] = mapped
					changed = true
				}
			}
		case FormatOpenAIResponse:
			if rm, ok := raw["reasoning"].(map[string]any); ok {
				if v, ok := rm["effort"].(string); ok && v != "" {
					if mapped := mapReasoningEffort(v, effortLevels); mapped != "" && mapped != v {
						cloned := cloneMapShallow(rm)
						cloned["effort"] = mapped
						raw["reasoning"] = cloned
						changed = true
					}
				}
			}
		case FormatAnthropic:
			if oc, ok := raw["output_config"].(map[string]any); ok {
				if v, ok := oc["effort"].(string); ok && v != "" {
					if mapped := mapReasoningEffort(v, effortLevels); mapped != "" && mapped != v {
						cloned := cloneMapShallow(oc)
						cloned["effort"] = mapped
						raw["output_config"] = cloned
						changed = true
					}
				}
			}
		case FormatGemini:
			if gc, ok := raw["generationConfig"].(map[string]any); ok {
				if tc, ok := gc["thinkingConfig"].(map[string]any); ok {
					if v, ok := tc["thinkingLevel"].(string); ok && v != "" {
						if mapped := mapReasoningEffort(v, effortLevels); mapped != "" && mapped != v {
							clonedGC := cloneMapShallow(gc)
							clonedTC := cloneMapShallow(tc)
							clonedTC["thinkingLevel"] = mapped
							clonedGC["thinkingConfig"] = clonedTC
							raw["generationConfig"] = clonedGC
							changed = true
						}
					}
				}
			}
		}
		return changed
	}

	// StripAll: remove all thinking/reasoning fields from native body
	if plan.StripAll {
		switch wireFormat {
		case FormatOpenAIChat:
			for _, k := range []string{"reasoning_effort", "reasoning", "thinking", "enable_thinking", "thinking_budget"} {
				if _, ok := raw[k]; ok {
					delete(raw, k)
					changed = true
				}
			}
		case FormatOpenAIResponse:
			if _, ok := raw["reasoning"]; ok {
				delete(raw, "reasoning")
				changed = true
			}
		case FormatAnthropic:
			if _, ok := raw["thinking"]; ok {
				delete(raw, "thinking")
				changed = true
			}
			if oc, ok := raw["output_config"].(map[string]any); ok {
				if _, hasEffort := oc["effort"]; hasEffort {
					cloned := cloneMapShallow(oc)
					delete(cloned, "effort")
					if len(cloned) == 0 {
						delete(raw, "output_config")
					} else {
						raw["output_config"] = cloned
					}
					changed = true
				}
			}
		case FormatGemini:
			if gc, ok := raw["generationConfig"].(map[string]any); ok {
				if _, hasTC := gc["thinkingConfig"]; hasTC {
					clonedGC := cloneMapShallow(gc)
					delete(clonedGC, "thinkingConfig")
					raw["generationConfig"] = clonedGC
					changed = true
				}
			}
		}
		return changed
	}

	// Apply resolved thinking plan to native body
	switch wireFormat {
	case FormatOpenAIChat:
		if plan.Effort != "" {
			if cur, _ := raw["reasoning_effort"].(string); cur != plan.Effort {
				raw["reasoning_effort"] = plan.Effort
				changed = true
			}
		} else if _, ok := raw["reasoning_effort"]; ok {
			delete(raw, "reasoning_effort")
			changed = true
		}
	case FormatOpenAIResponse:
		if plan.Effort != "" {
			rm, _ := raw["reasoning"].(map[string]any)
			cloned := cloneMapShallow(rm)
			if cur, _ := cloned["effort"].(string); cur != plan.Effort {
				cloned["effort"] = plan.Effort
				raw["reasoning"] = cloned
				changed = true
			}
		} else if _, ok := raw["reasoning"]; ok {
			delete(raw, "reasoning")
			changed = true
		}
	case FormatAnthropic:
		switch plan.ThinkingMode {
		case "adaptive":
			tm, _ := raw["thinking"].(map[string]any)
			clonedTM := cloneMapShallow(tm)
			clonedTM["type"] = "adaptive"
			delete(clonedTM, "budget_tokens")
			raw["thinking"] = clonedTM
			if plan.Effort != "" {
				oc, _ := raw["output_config"].(map[string]any)
				clonedOC := cloneMapShallow(oc)
				clonedOC["effort"] = plan.Effort
				raw["output_config"] = clonedOC
			}
			changed = true
		case "enabled":
			budget := plan.ThinkingBudget
			if budget < 1024 {
				budget = 1024
			}
			tm, _ := raw["thinking"].(map[string]any)
			clonedTM := cloneMapShallow(tm)
			clonedTM["type"] = "enabled"
			clonedTM["budget_tokens"] = budget
			raw["thinking"] = clonedTM
			if plan.Effort != "" {
				oc, _ := raw["output_config"].(map[string]any)
				clonedOC := cloneMapShallow(oc)
				clonedOC["effort"] = plan.Effort
				raw["output_config"] = clonedOC
			} else if oc, ok := raw["output_config"].(map[string]any); ok {
				if _, hasEffort := oc["effort"]; hasEffort {
					clonedOC := cloneMapShallow(oc)
					delete(clonedOC, "effort")
					if len(clonedOC) == 0 {
						delete(raw, "output_config")
					} else {
						raw["output_config"] = clonedOC
					}
				}
			}
			changed = true
		case "disabled":
			delete(raw, "thinking")
			if oc, ok := raw["output_config"].(map[string]any); ok {
				clonedOC := cloneMapShallow(oc)
				delete(clonedOC, "effort")
				if len(clonedOC) == 0 {
					delete(raw, "output_config")
				} else {
					raw["output_config"] = clonedOC
				}
			}
			changed = true
		}
	case FormatGemini:
		gc, _ := raw["generationConfig"].(map[string]any)
		clonedGC := cloneMapShallow(gc)
		tc, _ := clonedGC["thinkingConfig"].(map[string]any)
		clonedTC := cloneMapShallow(tc)
		if plan.ThinkingMode == "disabled" {
			delete(clonedTC, "thinkingLevel")
			clonedTC["thinkingBudget"] = 0
			clonedGC["thinkingConfig"] = clonedTC
			raw["generationConfig"] = clonedGC
			changed = true
		} else if plan.ThinkingBudget > 0 {
			delete(clonedTC, "thinkingLevel")
			clonedTC["thinkingBudget"] = plan.ThinkingBudget
			clonedGC["thinkingConfig"] = clonedTC
			raw["generationConfig"] = clonedGC
			changed = true
		} else if plan.Effort != "" {
			delete(clonedTC, "thinkingBudget")
			clonedTC["thinkingLevel"] = plan.Effort
			clonedGC["thinkingConfig"] = clonedTC
			raw["generationConfig"] = clonedGC
			changed = true
		}
	}
	return changed
}

func clampNativeMaxTokensFields(raw map[string]any, wireFormat clientFormat, upModel string, estimatedInputTokens int, customLimits map[string]int) bool {
	changed := false

	switch wireFormat {
	case FormatOpenAIChat:
		for _, field := range []string{"max_tokens", "max_completion_tokens"} {
			if cur := readJSONInt(raw[field]); cur > 0 {
				clamped := computeClampedMaxTokens(upModel, cur, estimatedInputTokens, customLimits)
				if clamped > 0 && clamped != cur {
					raw[field] = clamped
					changed = true
				}
			}
		}
	case FormatOpenAIResponse:
		if cur := readJSONInt(raw["max_output_tokens"]); cur > 0 {
			clamped := computeClampedMaxTokens(upModel, cur, estimatedInputTokens, customLimits)
			if clamped > 0 && clamped != cur {
				raw["max_output_tokens"] = clamped
				changed = true
			}
		}
	case FormatAnthropic:
		curMax := readJSONInt(raw["max_tokens"])
		if curMax > 0 {
			clamped := computeClampedMaxTokens(upModel, curMax, estimatedInputTokens, customLimits)
			if clamped > 0 && clamped != curMax {
				raw["max_tokens"] = clamped
				curMax = clamped
				changed = true
			}
		}
		// Ensure thinking.budget_tokens < max_tokens
		if tm, ok := raw["thinking"].(map[string]any); ok {
			tType, _ := tm["type"].(string)
			budget := readJSONInt(tm["budget_tokens"])
			if tType == "enabled" && budget > 0 {
				maxOut := modelMaxOutputLimit(upModel)
				if maxOut <= 0 {
					maxOut = 64000
				}
				if curMax <= 0 {
					desired := budget + 4096
					if desired > maxOut {
						desired = maxOut
					}
					raw["max_tokens"] = desired
					changed = true
				} else if budget >= curMax {
					desired := budget + 4096
					if desired > maxOut {
						desired = maxOut
					}
					if desired > budget {
						raw["max_tokens"] = desired
						changed = true
					} else if curMax >= 2048 {
						clonedTM := cloneMapShallow(tm)
						clonedTM["budget_tokens"] = curMax - 1024
						raw["thinking"] = clonedTM
						changed = true
					} else {
						delete(raw, "thinking")
						changed = true
					}
				}
			}
		}
	case FormatGemini:
		if gc, ok := raw["generationConfig"].(map[string]any); ok {
			if cur := readJSONInt(gc["maxOutputTokens"]); cur > 0 {
				clamped := computeClampedMaxTokens(upModel, cur, estimatedInputTokens, customLimits)
				if clamped > 0 && clamped != cur {
					clonedGC := cloneMapShallow(gc)
					clonedGC["maxOutputTokens"] = clamped
					raw["generationConfig"] = clonedGC
					changed = true
				}
			}
		}
	}
	return changed
}

func readJSONInt(v any) int {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func cloneMapShallow(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// isAutoRouterFailoverableError inspects an upstream HTTP 400/404/413/422 response
// body when routing via Auto-Router and returns true if the error is caused by
// model-specific context overflow, unsupported thinking/reasoning parameters,
// foreign signature rejection, or model unavailability—allowing relayHandler to
// seamlessly try the next candidate model in the tier.
func isAutoRouterFailoverableError(status int, body []byte) bool {
	switch status {
	case 404, 413, 422:
		return true
	case 400:
		lower := strings.ToLower(string(body))
		for _, kw := range []string{
			"context_length_exceeded",
			"context length",
			"maximum context length",
			"too many tokens",
			"prompt is too long",
			"input is too long",
			"request too large",
			"max_tokens",
			"max_completion_tokens",
			"max_output_tokens",
			"maxoutputtokens",
			"budget_tokens",
			"thinking",
			"reasoning_effort",
			"reasoning",
			"output_config",
			"signature",
			"thought_signature",
			"thoughtsignature",
			"encrypted_content",
			"previous_response_id",
			"model_not_found",
			"model not found",
			"does not exist",
			"unsupported model",
			"unknown model",
		} {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}
