package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type modelRequestSpec struct {
	Raw          string
	BaseModel    string
	Thinking     string // "", "auto", "keep", "none", "minimal", "low", "medium", "high", "xhigh", "max", "adaptive", "enabled"
	BudgetTokens int    // >0 when explicit token budget is specified
	ContextLimit int    // >0 when explicit context window limit is specified
}

func parseModelRequestSpec(raw string) modelRequestSpec {
	trimmed := strings.TrimSpace(raw)
	spec := modelRequestSpec{
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
			if norm := normalizeModelThinkingToken(valStr); norm != "" {
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
		if norm := normalizeModelThinkingToken(t); norm != "" {
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

func normalizeModelThinkingToken(raw string) string {
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

func servesModelRequest(u *Upstream, raw string) bool {
	if u == nil {
		return false
	}
	if u.servesModel(raw) {
		return true
	}
	spec := parseModelRequestSpec(raw)
	return spec.BaseModel != "" && spec.BaseModel != raw && u.servesModel(spec.BaseModel)
}

func resolveModelRequest(u *Upstream, raw string) string {
	spec := parseModelRequestSpec(raw)
	if u != nil && u.servesModel(raw) {
		return raw
	}
	if spec.BaseModel != "" {
		return spec.BaseModel
	}
	return raw
}

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
	spec := parseModelRequestSpec(model)
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
		log.Printf("[model-limits] learned token limits for model=%s from HTTP %d error: context=%d max_output=%d",
			base, status, learnedCtx, learnedMaxOut)
	}
	return learnedCtx, learnedMaxOut
}

// effectiveModelContextLimit returns the maximum context window (in tokens) for
// a model, checking explicit model options and supplied context limits,
// runtime error-learned limits, upstream /v1/models probed limits, LiteLLM catalog
// synced limits, model name suffixes, and built-in model family profiles.
func effectiveModelContextLimit(rawModel string, customLimits map[string]int) int {
	limit, _ := effectiveModelContextLimitWithSource(rawModel, customLimits)
	return limit
}

func effectiveModelContextLimitWithSource(rawModel string, customLimits map[string]int) (int, string) {
	spec := parseModelRequestSpec(rawModel)
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
	spec := parseModelRequestSpec(rawModel)
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
// for explicit model options across supported provider formats.

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
	spec := parseModelRequestSpec(rawModel)
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

// modelThinkingPlan describes explicit options supplied in the model name.
type modelThinkingPlan struct {
	StripAll        bool   // Remove all thinking/reasoning parameters
	DisableExplicit bool   // Explicitly send disabled thinking config if needed
	Effort          string // Resolved effort level ("none", "minimal", "low", "medium", "high", "xhigh", "max")
	ThinkingMode    string // "", "adaptive", "enabled", "disabled"
	ThinkingBudget  int    // Resolved token budget (>0 when fixed budget is used)
	EffectiveLabel  string // Explicit directive; empty/default preserves client parameters
}

func resolveModelThinking(
	up *Upstream,
	targetModel string,
	rawModel string,
	cr *canonReq,
) modelThinkingPlan {
	spec := parseModelRequestSpec(rawModel)
	if spec.BudgetTokens == 0 && (spec.Thinking == "" || spec.Thinking == "auto" || spec.Thinking == "keep") {
		return modelThinkingPlan{EffectiveLabel: "default"}
	}
	if strings.TrimSpace(targetModel) == "" {
		targetModel = rawModel
	}
	profile := classifyModelThinkingCapability(up, targetModel)
	if !profile.SupportsThinking {
		return modelThinkingPlan{
			StripAll:       true,
			EffectiveLabel: "none",
		}
	}

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
	default:
		return modelThinkingPlan{EffectiveLabel: "default"}
	}

	if directive == "keep" {
		if !clientSentThinking {
			return modelThinkingPlan{EffectiveLabel: "default"}
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
			return modelThinkingPlan{
				ThinkingMode:   "enabled",
				ThinkingBudget: minB,
				EffectiveLabel: fmt.Sprintf("budget:%d", minB),
			}
		}
		if explicitConfig {
			if containsEffort(profile.Efforts, "none") {
				return modelThinkingPlan{
					DisableExplicit: true,
					Effort:          "none",
					EffectiveLabel:  "none",
				}
			}
			if profile.GeminiUseThinkingBudget && profile.GeminiCanDisable {
				return modelThinkingPlan{
					DisableExplicit: true,
					Effort:          "none",
					ThinkingMode:    "disabled",
					ThinkingBudget:  0,
					EffectiveLabel:  "none",
				}
			}
		}
		return modelThinkingPlan{
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
			desiredEffort = "high"
		}
	}
	if desiredBudget <= 0 {
		if clientBudget > 0 && !explicitConfig {
			desiredBudget = clientBudget
		} else {
			desiredBudget = explicitThinkingBudget(desiredEffort)
		}
	}

	matchedEffort := matchSupportedEffort(desiredEffort, profile.Efforts)

	// 1. Anthropic Adaptive models (Claude 4.6+)
	if profile.AnthropicAdaptive {
		if matchedEffort == "" {
			matchedEffort = "high"
		}
		return modelThinkingPlan{
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
		return modelThinkingPlan{
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
		return modelThinkingPlan{
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
		return modelThinkingPlan{
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
	return modelThinkingPlan{
		Effort:         matchedEffort,
		ThinkingMode:   "",
		ThinkingBudget: 0,
		EffectiveLabel: matchedEffort,
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

func adaptNativeThinkingFields(raw map[string]any, wireFormat clientFormat, plan modelThinkingPlan, effortLevels []string) bool {
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

func explicitThinkingBudget(effort string) int {
	switch effort {
	case "minimal":
		return 1024
	case "low":
		return 2048
	case "medium":
		return 4096
	case "high":
		return 8192
	case "xhigh":
		return 24576
	case "max":
		return 31999
	default:
		return 4096
	}
}

// applyModelThinking applies only options supplied in the model name.
func applyModelThinking(cr *canonReq, plan modelThinkingPlan) {
	switch {
	case plan.StripAll:
		cr.ReasoningEffort, cr.ThinkingMode, cr.ThinkingBudget = "", "", nil
	case plan.DisableExplicit || (plan.EffectiveLabel != "" && plan.EffectiveLabel != "default"):
		cr.ReasoningEffort, cr.ThinkingMode, cr.ThinkingBudget = plan.Effort, plan.ThinkingMode, nil
		if plan.ThinkingBudget > 0 {
			budget := plan.ThinkingBudget
			cr.ThinkingBudget = &budget
		}
	}
}

func adaptModelRequestBody(body []byte, format clientFormat, up *Upstream, model string, task *relayTask, efforts []string) []byte {
	raw, err := decodeProtocolObject(body)
	if err != nil {
		return body
	}
	plan := resolveModelThinking(up, model, task.modelRequest(), task.canon)
	changed := adaptNativeThinkingFields(raw, format, plan, efforts)
	changed = clampNativeMaxTokensFields(raw, format, model, estimateRequestContextTokens(task.canon), task.contextLimits(model)) || changed
	if !changed {
		return body
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return encoded
}

func (t *relayTask) contextLimits(model string) map[string]int {
	if t != nil {
		if limit := parseModelRequestSpec(t.modelRequest()).ContextLimit; limit > 0 {
			return map[string]int{strings.ToLower(model): limit}
		}
	}
	return nil
}

func adaptModelRequestCanon(cr *canonReq, format clientFormat, up *Upstream, model string, task *relayTask, efforts []string) {
	applyModelThinking(cr, resolveModelThinking(up, model, task.modelRequest(), cr))
	cr.ReasoningEffort = mapReasoningEffort(protocolReasoningEffort(format, cr), efforts)
	input := estimateRequestContextTokens(cr)
	limits := task.contextLimits(model)
	if format == FormatAnthropic && cr.ThinkingBudget != nil && cr.MaxTokens <= *cr.ThinkingBudget {
		cr.MaxTokens = *cr.ThinkingBudget + 4096
	}
	cr.MaxTokens = computeClampedMaxTokens(model, cr.MaxTokens, input, limits)
	if format == FormatAnthropic && cr.ThinkingBudget != nil && *cr.ThinkingBudget >= cr.MaxTokens {
		if cr.MaxTokens >= 2048 {
			budget := cr.MaxTokens - 1024
			cr.ThinkingBudget = &budget
		} else {
			cr.ThinkingBudget, cr.ThinkingMode, cr.ReasoningEffort = nil, "", ""
		}
	}
}

func canonicalModelIdent(model string) string {
	return strings.ReplaceAll(protocolModelName(model), ".", "-")
}
