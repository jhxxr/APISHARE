package main

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	DomainCodingSWE        = "coding_swe"
	DomainFrontendUI       = "frontend_ui"
	DomainReasoningMath    = "reasoning_math"
	DomainAgenticTools     = "agentic_tools"
	DomainWritingKnowledge = "writing_knowledge"
	DomainLongContextMM    = "long_context_mm"
)

var allCapabilityDomains = []string{
	DomainCodingSWE,
	DomainFrontendUI,
	DomainReasoningMath,
	DomainAgenticTools,
	DomainWritingKnowledge,
	DomainLongContextMM,
}

// ModelCapabilityVector is the LEGACY heuristic/administrator profile used by
// the off/shadow baseline. Builtin "calibrated" numbers are not measured public
// benchmark results. Live Arena routing uses raw arenaRoutingEvidence instead;
// it never adds these model-name priors or thinking bonuses to an Arena score.
type ModelCapabilityVector struct {
	CodingSWE        float64 `json:"coding_swe"`
	FrontendUI       float64 `json:"frontend_ui"`
	ReasoningMath    float64 `json:"reasoning_math"`
	AgenticTools     float64 `json:"agentic_tools"`
	WritingKnowledge float64 `json:"writing_knowledge"`
	LongContextMM    float64 `json:"long_context_mm"`
	OverallScore     float64 `json:"overall_score"`
	SpeedPrior       float64 `json:"speed_prior"`
	BenchmarkSource  string  `json:"benchmark_source,omitempty"`
}

func (v ModelCapabilityVector) ScoreForDomain(domain string) float64 {
	switch domain {
	case DomainCodingSWE:
		return v.CodingSWE
	case DomainFrontendUI:
		return v.FrontendUI
	case DomainReasoningMath:
		return v.ReasoningMath
	case DomainAgenticTools:
		return v.AgenticTools
	case DomainWritingKnowledge:
		return v.WritingKnowledge
	case DomainLongContextMM:
		return v.LongContextMM
	default:
		return v.OverallScore
	}
}

func (v ModelCapabilityVector) MatchScore(weights map[string]float64) float64 {
	if len(weights) == 0 {
		return v.OverallScore
	}
	var sum, wSum float64
	for _, d := range allCapabilityDomains {
		if w := weights[d]; w > 0 {
			sum += w * v.ScoreForDomain(d)
			wSum += w
		}
	}
	if wSum <= 0 {
		return v.OverallScore
	}
	return roundOneDecimal(sum / wSum)
}

func (v *ModelCapabilityVector) computeOverall() {
	v.OverallScore = roundOneDecimal(
		0.24*v.CodingSWE +
			0.12*v.FrontendUI +
			0.22*v.ReasoningMath +
			0.18*v.AgenticTools +
			0.14*v.WritingKnowledge +
			0.10*v.LongContextMM,
	)
}

// TaskDemandProfile describes the 6-dimensional capability demand of an incoming request,
// its minimum required capability floor D(x) in [15, 95], and desired thinking intensity T(x) in [0, 1].
type TaskDemandProfile struct {
	Weights            map[string]float64 `json:"weights"`
	PrimaryDomain      string             `json:"primary_domain"`
	RequiredDifficulty float64            `json:"required_difficulty"`
	ThinkingIntensity  float64            `json:"thinking_intensity"`
	EquivalentTier     string             `json:"equivalent_tier"`
}

var autoBenchmarkCatalog = struct {
	sync.RWMutex
	data map[string]ModelCapabilityVector
}{data: map[string]ModelCapabilityVector{}}

func loadAutoBenchmarkCatalog() {
	if blob, err := settingsGet("auto_router_benchmarks_json"); err == nil && strings.TrimSpace(blob) != "" {
		var parsed map[string]ModelCapabilityVector
		if json.Unmarshal([]byte(blob), &parsed) == nil && len(parsed) > 0 {
			autoBenchmarkCatalog.Lock()
			autoBenchmarkCatalog.data = parsed
			autoBenchmarkCatalog.Unlock()
		}
	}
}

func recordSyncedModelBenchmark(model string, vec ModelCapabilityVector) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return
	}
	vec.computeOverall()
	if vec.BenchmarkSource == "" {
		vec.BenchmarkSource = "synced"
	}
	autoBenchmarkCatalog.Lock()
	autoBenchmarkCatalog.data[model] = vec
	if bare := protocolModelName(model); bare != "" && bare != model {
		autoBenchmarkCatalog.data[bare] = vec
	}
	autoBenchmarkCatalog.Unlock()
}

// resolveModelCapabilityVector returns the 6D benchmark-aligned capability profile for rawModel,
// applying inline thinking effort/budget Test-Time Compute adjustments.
func resolveModelCapabilityVector(rawModel string, custom map[string]ModelCapabilityVector) ModelCapabilityVector {
	spec := parseRouterModelSpec(rawModel)
	base := strings.TrimSpace(spec.BaseModel)
	if base == "" {
		base = rawModel
	}
	lowerRaw := strings.ToLower(strings.TrimSpace(rawModel))
	lowerBase := strings.ToLower(base)

	var vec ModelCapabilityVector
	found := false

	if len(custom) > 0 {
		if v, ok := custom[lowerRaw]; ok && v.OverallScore > 0 {
			vec = v
			vec.BenchmarkSource = "override"
			found = true
		} else if v, ok := custom[lowerBase]; ok && v.OverallScore > 0 {
			vec = v
			vec.BenchmarkSource = "override"
			found = true
		}
	}

	if !found {
		autoBenchmarkCatalog.RLock()
		if v, ok := autoBenchmarkCatalog.data[lowerBase]; ok && v.OverallScore > 0 {
			vec = v
			found = true
		} else if bare := protocolModelName(lowerBase); bare != "" {
			if v2, ok2 := autoBenchmarkCatalog.data[bare]; ok2 && v2.OverallScore > 0 {
				vec = v2
				found = true
			}
		}
		autoBenchmarkCatalog.RUnlock()
	}

	if !found {
		vec = builtinCalibratedCapabilityVector(base)
	}

	// Apply Test-Time Compute (Thinking Spec) capability scaling:
	// Explicitly boosting thinking effort increases ReasoningMath & CodingSWE ceiling
	// while slightly lowering SpeedPrior; disabling thinking does the reverse.
	switch spec.Thinking {
	case "max", "xhigh":
		vec.ReasoningMath = clamp100(vec.ReasoningMath + 6.5)
		vec.CodingSWE = clamp100(vec.CodingSWE + 4.0)
		vec.SpeedPrior = clamp100(vec.SpeedPrior - 14.0)
	case "high":
		vec.ReasoningMath = clamp100(vec.ReasoningMath + 4.0)
		vec.CodingSWE = clamp100(vec.CodingSWE + 2.5)
		vec.SpeedPrior = clamp100(vec.SpeedPrior - 8.0)
	case "low", "minimal":
		vec.ReasoningMath = clamp100(vec.ReasoningMath - 2.5)
		vec.SpeedPrior = clamp100(vec.SpeedPrior + 8.0)
	case "none":
		vec.ReasoningMath = clamp100(vec.ReasoningMath - 6.0)
		vec.CodingSWE = clamp100(vec.CodingSWE - 2.5)
		vec.SpeedPrior = clamp100(vec.SpeedPrior + 14.0)
	}
	if spec.BudgetTokens >= 16384 {
		vec.ReasoningMath = clamp100(vec.ReasoningMath + 5.0)
		vec.CodingSWE = clamp100(vec.CodingSWE + 3.0)
	}
	vec.computeOverall()
	return vec
}

func builtinCalibratedCapabilityVector(model string) ModelCapabilityVector {
	lower := protocolModelName(model)
	canon := canonicalModelIdent(lower)

	mk := func(swe, ui, mathScore, tools, writing, longMM, speed float64, src string) ModelCapabilityVector {
		v := ModelCapabilityVector{
			CodingSWE:        swe,
			FrontendUI:       ui,
			ReasoningMath:    mathScore,
			AgenticTools:     tools,
			WritingKnowledge: writing,
			LongContextMM:    longMM,
			SpeedPrior:       speed,
			BenchmarkSource:  src,
		}
		v.computeOverall()
		return v
	}

	switch {
	// --- Anthropic Claude Family (SWE-bench, WebDev Arena & BFCL Leaders) ---
	case strings.HasPrefix(canon, "claude-opus-4-6"), strings.HasPrefix(canon, "claude-opus-4-5"):
		return mk(96, 95, 93, 96, 95, 92, 48, "calibrated")
	case strings.HasPrefix(canon, "claude-sonnet-4-6"), strings.HasPrefix(canon, "claude-sonnet-4-5"):
		return mk(94, 96, 89, 95, 92, 90, 68, "calibrated")
	case strings.HasPrefix(canon, "claude-opus-4"):
		return mk(93, 92, 90, 93, 93, 88, 45, "calibrated")
	case strings.HasPrefix(canon, "claude-sonnet-4"), strings.HasPrefix(canon, "claude-3-7-sonnet"):
		return mk(90, 93, 86, 92, 90, 86, 70, "calibrated")
	case strings.HasPrefix(canon, "claude-3-5-sonnet"):
		return mk(87, 90, 80, 89, 88, 83, 74, "calibrated")
	case strings.HasPrefix(canon, "claude-haiku-4-5"):
		return mk(78, 82, 74, 82, 80, 78, 92, "calibrated")
	case strings.HasPrefix(canon, "claude-3-5-haiku"), strings.HasPrefix(canon, "claude-3-haiku"):
		return mk(70, 72, 65, 74, 74, 70, 94, "calibrated")

	// --- OpenAI GPT-5 / o-Series / GPT-4.1 Family ---
	case strings.HasPrefix(lower, "gpt-5.4"), strings.HasPrefix(lower, "gpt-5.3"), strings.HasPrefix(lower, "gpt-5.2-pro"):
		return mk(95, 93, 96, 95, 93, 91, 54, "calibrated")
	case strings.HasPrefix(lower, "gpt-5.2"), strings.HasPrefix(lower, "gpt-5.1"):
		return mk(93, 92, 94, 94, 91, 90, 64, "calibrated")
	case strings.HasPrefix(lower, "gpt-5-mini"), strings.HasPrefix(lower, "gpt-5-nano"), strings.HasPrefix(lower, "codex-mini"):
		return mk(82, 80, 83, 84, 78, 80, 88, "calibrated")
	case strings.HasPrefix(lower, "gpt-5"):
		return mk(91, 90, 92, 92, 90, 88, 66, "calibrated")
	case strings.HasPrefix(lower, "o3-pro"):
		return mk(92, 85, 97, 90, 86, 84, 38, "calibrated")
	case strings.HasPrefix(lower, "o3-mini"), strings.HasPrefix(lower, "o4-mini"):
		return mk(84, 78, 90, 83, 76, 78, 82, "calibrated")
	case strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o1"):
		return mk(89, 83, 95, 88, 84, 82, 48, "calibrated")
	case strings.HasPrefix(lower, "gpt-4.1-nano"):
		return mk(64, 65, 60, 68, 68, 75, 96, "calibrated")
	case strings.HasPrefix(lower, "gpt-4.1-mini"):
		return mk(78, 78, 74, 82, 79, 86, 90, "calibrated")
	case strings.HasPrefix(lower, "gpt-4.1"):
		return mk(86, 86, 82, 89, 86, 91, 76, "calibrated")
	case strings.HasPrefix(lower, "gpt-4o-mini"):
		return mk(68, 68, 64, 72, 72, 70, 92, "calibrated")
	case strings.HasPrefix(lower, "gpt-4o"), strings.HasPrefix(lower, "chatgpt-4o"):
		return mk(82, 83, 78, 85, 85, 82, 78, "calibrated")

	// --- Google Gemini Family (Long Context & Multimodal Leaders) ---
	case strings.HasPrefix(canon, "gemini-3-pro"), strings.HasPrefix(canon, "gemini-2-5-pro"):
		return mk(90, 91, 92, 89, 89, 97, 66, "calibrated")
	case strings.HasPrefix(canon, "gemini-3-flash"), strings.HasPrefix(canon, "gemini-2-5-flash"):
		if strings.Contains(canon, "lite") {
			return mk(68, 70, 66, 72, 71, 86, 96, "calibrated")
		}
		return mk(81, 82, 82, 83, 80, 93, 91, "calibrated")
	case strings.HasPrefix(canon, "gemini-2-0-flash"), strings.HasPrefix(canon, "gemini-1-5-flash"):
		return mk(72, 73, 70, 75, 74, 88, 93, "calibrated")
	case strings.HasPrefix(canon, "gemini-"):
		return mk(82, 82, 80, 82, 81, 92, 80, "calibrated")

	// --- DeepSeek Family ---
	case strings.Contains(lower, "deepseek") && strings.Contains(lower, "flash"):
		return mk(75, 72, 74, 76, 76, 72, 92, "calibrated")
	case strings.HasPrefix(lower, "deepseek-v4-pro"), strings.HasPrefix(lower, "deepseek-v4"):
		return mk(91, 86, 92, 89, 90, 84, 68, "calibrated")
	case strings.HasPrefix(lower, "deepseek-reasoner"), strings.HasPrefix(lower, "deepseek-r1"):
		return mk(89, 80, 95, 80, 88, 76, 46, "calibrated")
	case strings.HasPrefix(lower, "deepseek-chat"), strings.HasPrefix(lower, "deepseek-v3"):
		return mk(86, 83, 84, 85, 88, 78, 76, "calibrated")

	// --- Qwen / Alibaba Family ---
	case strings.HasPrefix(lower, "qwen3-max"), strings.HasPrefix(lower, "qwen-max"):
		return mk(87, 84, 86, 88, 89, 84, 72, "calibrated")
	case strings.HasPrefix(lower, "qwen3-coder"), strings.Contains(lower, "qwen-coder"), strings.Contains(lower, "qwen2.5-coder"):
		return mk(88, 84, 82, 85, 78, 80, 78, "calibrated")
	case strings.HasPrefix(lower, "qwq"):
		return mk(84, 76, 90, 78, 80, 75, 58, "calibrated")
	case strings.HasPrefix(lower, "qwen-plus"), strings.HasPrefix(lower, "qwen3"):
		return mk(80, 78, 80, 82, 83, 82, 84, "calibrated")
	case strings.HasPrefix(lower, "qwen-turbo"), strings.HasPrefix(lower, "qwen-flash"):
		return mk(68, 66, 66, 72, 74, 78, 94, "calibrated")

	// --- Zhipu GLM Family ---
	case strings.HasPrefix(lower, "glm-5"), strings.HasPrefix(lower, "glm-4.7"), strings.HasPrefix(lower, "glm-4.6"):
		if strings.Contains(lower, "flash") {
			return mk(74, 72, 72, 76, 76, 76, 93, "calibrated")
		}
		return mk(86, 84, 85, 87, 87, 82, 74, "calibrated")
	case strings.HasPrefix(lower, "glm-z1"):
		return mk(83, 76, 89, 78, 82, 76, 62, "calibrated")
	case strings.HasPrefix(lower, "glm-4"):
		if strings.Contains(lower, "flash") || strings.Contains(lower, "air") {
			return mk(68, 66, 65, 72, 73, 74, 92, "calibrated")
		}
		return mk(79, 77, 77, 81, 82, 78, 78, "calibrated")

	// --- xAI Grok / Kimi / MiniMax / MiMo ---
	case strings.HasPrefix(lower, "grok-4"):
		return mk(91, 88, 94, 90, 89, 88, 62, "calibrated")
	case strings.HasPrefix(lower, "grok-3-mini"):
		return mk(80, 76, 86, 80, 78, 78, 86, "calibrated")
	case strings.HasPrefix(lower, "grok-"):
		return mk(86, 84, 87, 85, 86, 82, 72, "calibrated")
	case strings.HasPrefix(lower, "kimi-k2"), strings.HasPrefix(lower, "moonshot"):
		return mk(86, 83, 84, 88, 91, 86, 74, "calibrated")
	case strings.HasPrefix(lower, "minimax"):
		return mk(83, 82, 82, 84, 87, 88, 76, "calibrated")
	case strings.HasPrefix(lower, "mimo") && strings.Contains(lower, "flash"):
		return mk(72, 70, 72, 74, 73, 74, 94, "calibrated")
	}

	// Zero-Config Family & Parameter Heuristic Inferencer for unknown/custom models
	if isFastLightweightModel(lower) {
		return mk(70, 68, 68, 72, 72, 74, 92, "inferred")
	}
	if strings.Contains(lower, "reason") || strings.Contains(lower, "-r1") || strings.Contains(lower, "think") {
		return mk(85, 76, 90, 78, 82, 76, 54, "inferred")
	}
	if strings.Contains(lower, "pro") || strings.Contains(lower, "max") || strings.Contains(lower, "opus") || strings.Contains(lower, "ultra") {
		return mk(88, 86, 88, 88, 88, 86, 62, "inferred")
	}
	return mk(78, 76, 76, 78, 78, 78, 78, "inferred")
}

var (
	domainFrontendUIRe = regexp.MustCompile(`(?i)` +
		`\b(react|vue|svelte|next\.?js|nuxt|tailwind|css|scss|flexbox|grid|animation|keyframes|svg|canvas|webgl|three\.?js|shadcn|radix|chakra|material-ui|responsive|landing page|dashboard ui|component|html5|dom|z-index|glassmorphism|dark mode)\b|` +
		`(前端|页面设计|组件|样式|响应式|布局|动效|动画|毛玻璃|深色模式|配色|界面|按钮|弹窗|表单|导航栏|卡片布局|视觉效果)`)

	domainAgenticToolsRe = regexp.MustCompile(`(?i)` +
		`\b(json_schema|structured output|tool_call|function_call|mcp|agent|workflow|automation|cron|webhook|pipeline)\b|` +
		`(工具调用|结构化输出|工作流|自动化|多步执行|编排)`)

	domainReasoningMathRe = regexp.MustCompile(`(?i)` +
		`\b(prove|proof|theorem|lemma|corollary|induction|contradiction|prime|mersenne|eigenvalue|eigenvector|integral|differential|stochastic|bayesian|markov|combinatorics|number theory|group theory|topology|manifold|riemann|fermat|euler|lagrange|game theory|np-hard|time complexity proof)\b|` +
		`(数学归纳|反证法|严格证明|证明[:：]|定理|引理|推论|素数|质数|梅森|微积分|偏微分|矩阵|特征值|概率分布|博弈论|流形|拓扑|群论|数论|同余|欧拉|黎曼|费马|拉格朗日|马尔可夫|贝叶斯|质因数)`)
)

// profileRequestDemand analyzes the full canonical request and extracts its
// 6-dimensional capability demand weights, primary domain, required capability floor D(x),
// and continuous thinking intensity T(x).
func profileRequestDemand(
	cr *canonReq,
	lastUser string,
	stateText string,
	sig routerRequestSignals,
	estimatedTokens int,
	clfRes *classifierDecisionResult,
) TaskDemandProfile {
	text := strings.TrimSpace(stateText)
	if text == "" {
		text = strings.TrimSpace(lastUser)
	}

	rawWeights := map[string]float64{
		DomainCodingSWE:        0.10,
		DomainFrontendUI:       0.05,
		DomainReasoningMath:    0.10,
		DomainAgenticTools:     0.10,
		DomainWritingKnowledge: 0.25,
		DomainLongContextMM:    0.05,
	}

	uiMatches := len(domainFrontendUIRe.FindAllStringIndex(text, 8))
	sweMatches := len(autoRouterCodeSignalRe.FindAllStringIndex(text, 10)) + len(autoRouterComplexRe.FindAllStringIndex(text, 8))
	mathMatches := len(autoRouterReasoningRe.FindAllStringIndex(text, 8)) + len(domainReasoningMathRe.FindAllStringIndex(text, 10))
	agentMatches := len(domainAgenticToolsRe.FindAllStringIndex(text, 6))
	writingMatches := len(autoRouterMediumRe.FindAllStringIndex(text, 6)) + len(autoRouterSimpleRe.FindAllStringIndex(lastUser, 4))

	if mathMatches >= 2 && !sig.HasCodeInHistory && sig.AssistantCodeBlocks == 0 && !strings.Contains(text, "```") {
		sweMatches = 0
	}

	if uiMatches > 0 {
		rawWeights[DomainFrontendUI] += float64(uiMatches) * 0.45
		rawWeights[DomainCodingSWE] += float64(uiMatches) * 0.20
	}
	if sweMatches > 0 {
		rawWeights[DomainCodingSWE] += float64(sweMatches) * 0.38
	}
	if sig.HasCodeInHistory || sig.AssistantCodeBlocks > 0 {
		rawWeights[DomainCodingSWE] += 0.45
	}
	if sig.HasErrorOrStackTrace {
		rawWeights[DomainCodingSWE] += 0.50
		rawWeights[DomainReasoningMath] += 0.25
	}
	if mathMatches > 0 {
		rawWeights[DomainReasoningMath] += float64(mathMatches) * 0.65
	}
	if sig.HasTools || sig.ToolCount > 0 || sig.ToolLoopDepth > 0 || sig.HasStructuredOutput || agentMatches > 0 {
		rawWeights[DomainAgenticTools] += 0.55 + float64(sig.ToolLoopDepth)*0.15 + float64(agentMatches)*0.20
		if sig.ToolCount >= 5 {
			rawWeights[DomainAgenticTools] += 0.30
		}
	}
	if sig.HasVision || estimatedTokens >= 16000 {
		rawWeights[DomainLongContextMM] += 0.80
		if estimatedTokens >= 48000 {
			rawWeights[DomainLongContextMM] += 0.60
		}
	}
	if writingMatches > 0 && sweMatches == 0 && mathMatches == 0 && uiMatches == 0 {
		rawWeights[DomainWritingKnowledge] += float64(writingMatches) * 0.35
	}

	// Normalize weights to sum to 1.0 and identify PrimaryDomain
	var totalW float64
	primaryDomain := DomainWritingKnowledge
	bestW := -1.0
	for _, d := range allCapabilityDomains {
		w := rawWeights[d]
		totalW += w
		// Prefer specialized technical domains over generic writing when tied
		if w > bestW || (w == bestW && d != DomainWritingKnowledge) {
			bestW = w
			primaryDomain = d
		}
	}
	normWeights := make(map[string]float64, len(allCapabilityDomains))
	for _, d := range allCapabilityDomains {
		normWeights[d] = roundTwoDecimals(rawWeights[d] / totalW)
	}

	// Compute RequiredDifficulty D(x) in [18, 92] and ThinkingIntensity T(x) in [0, 1]
	hTier, hScore := classifyHeuristic(lastUser, text, cr)
	var difficulty float64
	switch hTier {
	case TierSimple:
		difficulty = 28.0 + clampRange(hScore*15.0, -8.0, 12.0)
	case TierMedium:
		difficulty = 52.0 + clampRange(hScore*18.0, -6.0, 14.0)
	case TierComplex:
		difficulty = 74.0 + clampRange((hScore-0.35)*15.0, -4.0, 10.0)
	case TierReasoning:
		difficulty = 85.0 + clampRange((hScore-0.80)*10.0, -2.0, 7.0)
	default:
		difficulty = 60.0
	}

	// Fuse external classifier probability distribution if present
	if clfRes != nil {
		tierAnchors := map[string]float64{
			TierSimple:    26.0,
			TierMedium:    54.0,
			TierComplex:   76.0,
			TierReasoning: 88.0,
		}
		var clfDiff float64
		if len(clfRes.Probabilities) > 0 {
			var pSum float64
			for t, p := range clfRes.Probabilities {
				if anchor, ok := tierAnchors[strings.ToUpper(t)]; ok && p > 0 {
					clfDiff += p * anchor
					pSum += p
				}
			}
			if pSum > 0 {
				clfDiff /= pSum
			}
		}
		if clfDiff <= 0 {
			if anchor, ok := tierAnchors[clfRes.Tier]; ok {
				clfDiff = anchor
			}
		}
		if clfDiff > 0 {
			// Blend 65% external classifier + 35% structural/heuristic, while respecting StructuralFloor
			difficulty = 0.65*clfDiff + 0.35*difficulty
		}
	}

	// Structural Floor & Conversational Momentum Adjustments
	switch sig.StructuralFloor {
	case TierMedium:
		if difficulty < 50.0 {
			difficulty = 52.0
		}
	case TierComplex:
		if difficulty < 72.0 {
			difficulty = 74.0
		}
	case TierReasoning:
		if difficulty < 84.0 {
			difficulty = 86.0
		}
	}
	if sig.IsFrustrationOrRetry {
		difficulty = math.Max(difficulty+8.0, 78.0)
	}
	if estimatedTokens >= 32000 && difficulty < 70.0 {
		difficulty = 72.0
	}
	if utf8.RuneCountInString(strings.TrimSpace(lastUser)) <= 20 && !sig.HasCodeInHistory && !sig.HasErrorOrStackTrace && !sig.IsFrustrationOrRetry && !sig.IsToolLoop && sweMatches == 0 && mathMatches == 0 {
		if difficulty > 35.0 && clfRes == nil {
			difficulty = 28.0
		}
	}
	difficulty = roundOneDecimal(clampRange(difficulty, 18.0, 92.0))

	// Continuous Thinking Intensity T(x) in [0, 1]
	thinkingIntensity := clampRange((difficulty-35.0)/55.0, 0.0, 1.0)
	if primaryDomain == DomainReasoningMath {
		thinkingIntensity = clampRange(thinkingIntensity+0.25, 0.20, 1.0)
	}
	if sig.IsFrustrationOrRetry || sig.HasErrorOrStackTrace {
		thinkingIntensity = clampRange(thinkingIntensity+0.25, 0.50, 1.0)
	}
	if difficulty <= 34.0 && !sig.IsFrustrationOrRetry {
		thinkingIntensity = 0.0
	}

	// Derive human-readable EquivalentTier from D(x) (or clfRes if explicit)
	equivTier := difficultyToEquivalentTier(difficulty, primaryDomain)

	return TaskDemandProfile{
		Weights:            normWeights,
		PrimaryDomain:      primaryDomain,
		RequiredDifficulty: difficulty,
		ThinkingIntensity:  roundTwoDecimals(thinkingIntensity),
		EquivalentTier:     equivTier,
	}
}

func difficultyToEquivalentTier(difficulty float64, primaryDomain string) string {
	switch {
	case difficulty >= 82.0 || (primaryDomain == DomainReasoningMath && difficulty >= 72.0):
		return TierReasoning
	case difficulty >= 64.0:
		return TierComplex
	case difficulty >= 42.0:
		return TierMedium
	default:
		return TierSimple
	}
}

func clamp100(v float64) float64 {
	return clampRange(v, 0.0, 100.0)
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundOneDecimal(v float64) float64 {
	return math.Round(v*10) / 10
}

func roundTwoDecimals(v float64) float64 {
	return math.Round(v*100) / 100
}
