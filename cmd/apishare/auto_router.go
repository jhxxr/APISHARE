package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	autoRouterSettingsKey = "auto_router"

	TierSimple    = "SIMPLE"
	TierMedium    = "MEDIUM"
	TierComplex   = "COMPLEX"
	TierReasoning = "REASONING"
	TierDefault   = "DEFAULT"
)

var allRouterTiers = []string{TierSimple, TierMedium, TierComplex, TierReasoning}

func normalizeTierName(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case TierSimple:
		return TierSimple
	case TierMedium:
		return TierMedium
	case TierComplex:
		return TierComplex
	case TierReasoning:
		return TierReasoning
	case TierDefault:
		return TierDefault
	default:
		return ""
	}
}

type AutoRouterConfig struct {
	userExternal                 bool                             // User-owned classifiers must not inherit site credentials or network access.
	Enabled                      bool                             `json:"enabled"`
	Mode                         string                           `json:"mode"` // "off" | "shadow" | "live"
	VirtualModel                 string                           `json:"virtual_model"`
	StrategyProfile              string                           `json:"strategy_profile"` // "balanced" | "quality_first" | "cost_first" | "latency_first"
	ModelPool                    []string                         `json:"model_pool,omitempty"`
	ExcludedModels               []string                         `json:"excluded_models,omitempty"`
	BanditEnabled                bool                             `json:"bandit_enabled"`
	SpeculativeCascade           bool                             `json:"speculative_cascade"`
	SpeculativeTTFTMS            int                              `json:"speculative_ttft_ms,omitempty"`
	ModelCapabilities            map[string]ModelCapabilityVector `json:"model_capabilities,omitempty"`
	Classifier                   string                           `json:"classifier"` // "cloudflare" | "jev" | "systemone" | "openai_chat" | "gateway_upstream" | "heuristic"
	ClassifierModel              string                           `json:"classifier_model"`
	CloudflareAccountID          string                           `json:"cloudflare_account_id"`
	EndpointURL                  string                           `json:"endpoint_url"`
	APIKey                       string                           `json:"api_key,omitempty"`
	ProxyID                      int64                            `json:"proxy_id"`
	ClassifierTimeoutMS          int                              `json:"classifier_timeout_ms"`
	ClassifierFallback           string                           `json:"classifier_fallback"` // "heuristic" | "default_model"
	ClassifierInstructions       string                           `json:"classifier_instructions"`
	MinConfidence                float64                          `json:"min_confidence"`
	ContextUserTurns             int                              `json:"context_user_turns"`
	SkipShortChars               int                              `json:"skip_short_chars"`
	LongContextThreshold         int                              `json:"long_context_threshold"`
	RouteHousekeeping            bool                             `json:"route_housekeeping"`
	SessionMode                  string                           `json:"session_mode"` // "user_turn" | "session_lock" | "every_turn"
	CascadeTiers                 bool                             `json:"cascade_tiers"`
	AllowUnlistedTargetModels    bool                             `json:"allow_unlisted_target_models"`
	DefaultModel                 string                           `json:"default_model"`
	Tiers                        map[string][]string              `json:"tiers"`
	TierThinking                 map[string]string                `json:"tier_thinking"`
	TierCriteria                 map[string]string                `json:"tier_criteria"`
	ModelContextLimits           map[string]int                   `json:"model_context_limits,omitempty"`
	ResponseHeaders              bool                             `json:"response_headers"`
	EscalationKeyword            string                           `json:"escalation_keyword"`
	AllowUserAutoRouter          bool                             `json:"allow_user_auto_router"`
	ShareSiteClassifierWithUsers bool                             `json:"share_site_classifier_with_users"`
	SharedClassifierModels       []string                         `json:"shared_classifier_models"`
}

type routerDecisionDetails struct {
	RequestedModel        string                `json:"requested_model,omitempty"`
	SelectedModel         string                `json:"selected_model,omitempty"`
	SelectedSourceTier    string                `json:"selected_source_tier,omitempty"`
	SelectedThinking      string                `json:"selected_thinking,omitempty"`
	StrategyProfile       string                `json:"strategy_profile,omitempty"`
	PrimaryDomain         string                `json:"primary_domain,omitempty"`
	RequiredDifficulty    float64               `json:"required_difficulty,omitempty"`
	DomainWeights         map[string]float64    `json:"domain_weights,omitempty"`
	AutonomousPool        bool                  `json:"autonomous_pool,omitempty"`
	SpeculativeEscalated  bool                  `json:"speculative_escalated,omitempty"`
	SpeculativeReason     string                `json:"speculative_reason,omitempty"`
	EstimatedInputTokens  int                   `json:"estimated_input_tokens,omitempty"`
	ContextUpgraded       bool                  `json:"context_upgraded,omitempty"`
	DegradedToDefault     bool                  `json:"degraded_to_default,omitempty"`
	FailedModels          []string              `json:"failed_models,omitempty"`
	Tier                  string                `json:"tier,omitempty"`
	Cause                 string                `json:"cause,omitempty"`
	ClassifierProvider    string                `json:"classifier_provider,omitempty"`
	ClassifierModel       string                `json:"classifier_model,omitempty"`
	ClassifierMS          int64                 `json:"classifier_ms,omitempty"`
	Confidence            float64               `json:"confidence,omitempty"`
	HeuristicScore        *float64              `json:"heuristic_score,omitempty"`
	Probabilities         map[string]float64    `json:"probabilities,omitempty"`
	UpgradedLowConfidence bool                  `json:"upgraded_low_confidence,omitempty"`
	FallbackReason        string                `json:"fallback_reason,omitempty"`
	Shadow                bool                  `json:"shadow,omitempty"`
	ShadowModel           string                `json:"shadow_model,omitempty"`
	SkippedCandidates     int                   `json:"skipped_candidates,omitempty"`
	FallbacksTried        []string              `json:"fallbacks_tried,omitempty"`
	Steps                 []routerCandidateStep `json:"steps,omitempty"`
}

func (d *routerDecisionDetails) syncOutcome(defaultModel string) {
	if d == nil {
		return
	}
	defaultModel = strings.TrimSpace(defaultModel)
	seenFailed := make(map[string]bool)
	failed := make([]string, 0)
	addFailed := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seenFailed[m] {
			seenFailed[m] = true
			failed = append(failed, m)
		}
	}

	var matchedStepTier string
	for _, s := range d.Steps {
		if d.SelectedModel != "" && s.Model == d.SelectedModel {
			matchedStepTier = s.SourceTier
			break
		}
		if s.Status == "skipped" || s.Status == "failed" {
			addFailed(s.Model)
		}
	}
	for _, m := range d.FallbacksTried {
		if m != d.SelectedModel {
			addFailed(m)
		}
	}
	d.FailedModels = failed

	if matchedStepTier != "" {
		d.SelectedSourceTier = matchedStepTier
	}
	if d.SelectedModel != "" && defaultModel != "" && strings.EqualFold(d.SelectedModel, defaultModel) {
		if d.SelectedSourceTier == "" || d.SelectedSourceTier == TierDefault || (d.Tier != "" && d.SelectedSourceTier != d.Tier && len(failed) > 0) {
			d.SelectedSourceTier = TierDefault
			d.DegradedToDefault = true
		}
	}
	if d.SelectedSourceTier == TierDefault {
		d.DegradedToDefault = true
	}
}

func (d *routerDecisionDetails) markRuntimeFailover(failedModel string, failErr error, nextModel string, defaultModel string) {
	if d == nil {
		return
	}
	alreadyTried := false
	for _, m := range d.FallbacksTried {
		if m == failedModel {
			alreadyTried = true
			break
		}
	}
	if !alreadyTried && failedModel != "" {
		d.FallbacksTried = append(d.FallbacksTried, failedModel)
	}
	errReason := "upstream_failed"
	if failErr != nil {
		errReason = failErr.Error()
	}
	if strings.Contains(errReason, "speculative_probe:") {
		d.SpeculativeEscalated = true
		if idx := strings.Index(errReason, "speculative_probe:"); idx >= 0 {
			d.SpeculativeReason = strings.TrimSpace(errReason[idx+len("speculative_probe:"):])
		}
		if !strings.Contains(d.Cause, "+speculative_escalated") {
			d.Cause += "+speculative_escalated"
		}
	}
	for i := range d.Steps {
		if d.Steps[i].Model == failedModel {
			d.Steps[i].Status = "failed"
			d.Steps[i].Reason = errReason
		} else if d.Steps[i].Model == nextModel {
			d.Steps[i].Status = "selected"
		}
	}
	if !d.Shadow && nextModel != "" {
		d.SelectedModel = nextModel
	}
	d.syncOutcome(defaultModel)
}

type routerCandidateStep struct {
	Priority        int     `json:"priority"`
	Model           string  `json:"model"`
	SourceTier      string  `json:"source_tier"`
	Status          string  `json:"status"` // "selected" | "standby" | "skipped" | "failed"
	Reason          string  `json:"reason,omitempty"`
	Thinking        string  `json:"thinking,omitempty"`
	ContextLimit    int     `json:"context_limit,omitempty"`
	CapabilityScore float64 `json:"capability_score,omitempty"`
	BanditWinRate   float64 `json:"bandit_win_rate,omitempty"`
	UtilityScore    float64 `json:"utility_score,omitempty"`
	UpstreamID      int64   `json:"upstream_id,omitempty"`
	UpstreamName    string  `json:"upstream_name,omitempty"`
}

func defaultAutoRouterTierCriteria() map[string]string {
	return map[string]string{
		TierSimple:    "闲聊、翻译、改写、简单事实问答，快模型一次能答好",
		TierMedium:    "一般写作、总结、短代码片段，快模型基本能答好",
		TierComplex:   "多文件代码、调试、架构设计、长文档分析，需要强模型",
		TierReasoning: "数学证明、多步推理、复杂规划，需要最强推理模型",
	}
}

func defaultAutoRouterTiers() map[string][]string {
	return map[string][]string{
		TierSimple:    {},
		TierMedium:    {},
		TierComplex:   {},
		TierReasoning: {},
	}
}

var legacyPlaceholderRouterModels = map[string]bool{
	"deepseek-v4.1-flash":  true,
	"glm-5.3-flash":        true,
	"mimo-v2.6-flash":      true,
	"glm-5.3":              true,
	"deepseek-v4-pro-0813": true,
	"grok-4.7":             true,
}

func isModelExplicitlyBoundByAnyUpstream(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return false
	}
	for i := range snapshot.upstreams {
		u := &snapshot.upstreams[i]
		if serves, explicit := u.servesModelExplicit(model); serves && explicit {
			return true
		}
	}
	return false
}

func stripUnboundLegacyPlaceholderModels(cfg *AutoRouterConfig) bool {
	if cfg == nil {
		return false
	}
	changed := false
	if legacyPlaceholderRouterModels[cfg.DefaultModel] && !isModelExplicitlyBoundByAnyUpstream(cfg.DefaultModel) {
		cfg.DefaultModel = ""
		changed = true
	}
	for _, tier := range allRouterTiers {
		list := cfg.Tiers[tier]
		if len(list) == 0 {
			continue
		}
		filtered := make([]string, 0, len(list))
		for _, m := range list {
			if legacyPlaceholderRouterModels[m] && !isModelExplicitlyBoundByAnyUpstream(m) {
				changed = true
				continue
			}
			filtered = append(filtered, m)
		}
		cfg.Tiers[tier] = filtered
	}
	return changed
}

func defaultAutoRouterConfig() AutoRouterConfig {
	return AutoRouterConfig{
		Enabled:                      false,
		Mode:                         "off",
		VirtualModel:                 "auto",
		StrategyProfile:              StrategyBalanced,
		ModelPool:                    []string{},
		ExcludedModels:               []string{},
		BanditEnabled:                true,
		SpeculativeCascade:           true,
		SpeculativeTTFTMS:            2500,
		ModelCapabilities:            map[string]ModelCapabilityVector{},
		Classifier:                   "cloudflare",
		ClassifierModel:              defaultClefFlashModel,
		ClassifierTimeoutMS:          1500,
		ClassifierFallback:           "heuristic",
		ClassifierInstructions:       defaultClassifierInstructions,
		MinConfidence:                0.55,
		ContextUserTurns:             2,
		SkipShortChars:               60,
		LongContextThreshold:         32000,
		RouteHousekeeping:            true,
		SessionMode:                  "user_turn",
		CascadeTiers:                 true,
		AllowUnlistedTargetModels:    true,
		DefaultModel:                 "",
		Tiers:                        defaultAutoRouterTiers(),
		TierThinking:                 defaultTierThinkingMap(),
		TierCriteria:                 defaultAutoRouterTierCriteria(),
		ModelContextLimits:           map[string]int{},
		ResponseHeaders:              true,
		EscalationKeyword:            "",
		AllowUserAutoRouter:          true,
		ShareSiteClassifierWithUsers: false,
		SharedClassifierModels:       []string{},
	}
}

func (cfg AutoRouterConfig) effectiveTierCriteria() map[string]string {
	out := defaultAutoRouterTierCriteria()
	for k, v := range cfg.TierCriteria {
		if tk := normalizeTierName(k); tk != "" && strings.TrimSpace(v) != "" {
			out[tk] = strings.TrimSpace(v)
		}
	}
	return out
}

func normalizeModelSlice(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		for _, part := range splitRouterModelList(item) {
			m := strings.TrimSpace(part)
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func routerModelExclusionFilter(models []string) func(string) bool {
	excluded := make(map[string]bool, len(models)*2)
	for _, model := range models {
		if raw := strings.ToLower(strings.TrimSpace(model)); raw != "" {
			excluded[raw] = true
			if base := strings.ToLower(strings.TrimSpace(parseRouterModelSpec(model).BaseModel)); base != "" {
				excluded[base] = true
			}
		}
	}
	return func(model string) bool {
		raw := strings.ToLower(strings.TrimSpace(model))
		base := strings.ToLower(strings.TrimSpace(parseRouterModelSpec(model).BaseModel))
		return excluded[raw] || (base != "" && excluded[base])
	}
}

func normalizeAutoRouterConfig(cfg *AutoRouterConfig) error {
	def := defaultAutoRouterConfig()
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch cfg.Mode {
	case "":
		if cfg.Enabled {
			cfg.Mode = "live"
		} else {
			cfg.Mode = "off"
		}
	case "off", "shadow", "live":
	default:
		return errors.New("运行模式仅支持 off、shadow 或 live")
	}
	cfg.Enabled = cfg.Mode != "off"

	cfg.VirtualModel = strings.TrimSpace(cfg.VirtualModel)
	if cfg.VirtualModel == "" {
		cfg.VirtualModel = def.VirtualModel
	}
	if strings.ContainsAny(cfg.VirtualModel, " ,，\t\r\n/") {
		return errors.New("虚拟模型名称不能包含空格、逗号或斜杠")
	}

	rawClassifier := strings.ToLower(strings.TrimSpace(cfg.Classifier))
	switch rawClassifier {
	case "", "clef_flash", "clef-flash":
		cfg.Classifier = "cloudflare"
		if strings.TrimSpace(cfg.ClassifierModel) == "" {
			cfg.ClassifierModel = defaultClefFlashModel
		}
	case "clef":
		cfg.Classifier = "cloudflare"
		if strings.TrimSpace(cfg.ClassifierModel) == "" || cfg.ClassifierModel == defaultClefFlashModel {
			cfg.ClassifierModel = defaultClefModel
		}
	case "openrouter", "openrouter_decisions", "openrouter-decisions":
		cfg.Classifier = "openrouter"
		if strings.TrimSpace(cfg.ClassifierModel) == "" || strings.HasPrefix(cfg.ClassifierModel, "@cf/") {
			cfg.ClassifierModel = defaultOpenRouterDecisionsModel
		}
	case "cloudflare", "jev", "systemone", "openai_chat", "gateway_upstream", "heuristic":
		cfg.Classifier = rawClassifier
	default:
		return fmt.Errorf("不支持的分类器类型 %q", cfg.Classifier)
	}

	cfg.ClassifierModel = strings.TrimSpace(cfg.ClassifierModel)
	if cfg.ClassifierModel == "" {
		switch cfg.Classifier {
		case "cloudflare":
			cfg.ClassifierModel = defaultClefFlashModel
		case "jev":
			cfg.ClassifierModel = "jev"
		case "systemone":
			cfg.ClassifierModel = "decision-model"
		}
	}
	cfg.CloudflareAccountID = strings.TrimSpace(cfg.CloudflareAccountID)
	cfg.EndpointURL = strings.TrimSpace(cfg.EndpointURL)
	if cfg.EndpointURL != "" {
		if _, err := url.ParseRequestURI(cfg.EndpointURL); err != nil {
			return errors.New("决策接口 URL 格式不正确")
		}
	}
	if cfg.ProxyID != 0 {
		if err := validateUpstreamProxy(cfg.ProxyID); err != nil {
			return err
		}
	}

	if cfg.ClassifierTimeoutMS <= 0 {
		cfg.ClassifierTimeoutMS = def.ClassifierTimeoutMS
	} else if cfg.ClassifierTimeoutMS < 100 || cfg.ClassifierTimeoutMS > 15000 {
		return errors.New("分类器超时时间需在 100 到 15000 毫秒之间")
	}

	cfg.ClassifierFallback = strings.ToLower(strings.TrimSpace(cfg.ClassifierFallback))
	if cfg.ClassifierFallback == "" {
		cfg.ClassifierFallback = def.ClassifierFallback
	}
	if cfg.ClassifierFallback != "heuristic" && cfg.ClassifierFallback != "default_model" {
		return errors.New("分类器失败回退策略仅支持 heuristic 或 default_model")
	}

	cfg.ClassifierInstructions = strings.TrimSpace(cfg.ClassifierInstructions)
	if cfg.ClassifierInstructions == "" {
		cfg.ClassifierInstructions = def.ClassifierInstructions
	}

	if cfg.MinConfidence < 0 || cfg.MinConfidence > 1 {
		return errors.New("最低置信度阈值需在 0 到 1 之间")
	}
	if cfg.ContextUserTurns < 0 || cfg.ContextUserTurns > 10 {
		return errors.New("携带用户上下文轮数需在 0 到 10 之间")
	}
	if cfg.SkipShortChars < 0 || cfg.SkipShortChars > 2000 {
		return errors.New("短句直通字符阈值需在 0 到 2000 之间")
	}

	if cfg.LongContextThreshold < 0 {
		cfg.LongContextThreshold = 0
	} else if cfg.LongContextThreshold > 2000000 {
		cfg.LongContextThreshold = 2000000
	}

	cfg.SessionMode = strings.ToLower(strings.TrimSpace(cfg.SessionMode))
	switch cfg.SessionMode {
	case "":
		cfg.SessionMode = def.SessionMode
	case "user_turn", "session_lock", "every_turn":
	default:
		return errors.New("会话分档模式仅支持 user_turn、session_lock 或 every_turn")
	}

	cfg.StrategyProfile = normalizeStrategyProfile(cfg.StrategyProfile)
	cfg.ModelPool = normalizeModelSlice(cfg.ModelPool)
	filteredPool := make([]string, 0, len(cfg.ModelPool))
	for _, m := range cfg.ModelPool {
		if m != cfg.VirtualModel && parseRouterModelSpec(m).BaseModel != cfg.VirtualModel {
			filteredPool = append(filteredPool, m)
		}
	}
	cfg.ModelPool = filteredPool
	cfg.ExcludedModels = normalizeModelSlice(cfg.ExcludedModels)
	if cfg.SpeculativeTTFTMS <= 0 {
		cfg.SpeculativeTTFTMS = 2500
	} else if cfg.SpeculativeTTFTMS < 300 {
		cfg.SpeculativeTTFTMS = 300
	} else if cfg.SpeculativeTTFTMS > 30000 {
		cfg.SpeculativeTTFTMS = 30000
	}
	normCaps := make(map[string]ModelCapabilityVector, len(cfg.ModelCapabilities))
	for k, v := range cfg.ModelCapabilities {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" {
			normCaps[k] = v
		}
	}
	cfg.ModelCapabilities = normCaps

	cfg.DefaultModel = strings.TrimSpace(cfg.DefaultModel)
	if cfg.DefaultModel == cfg.VirtualModel || parseRouterModelSpec(cfg.DefaultModel).BaseModel == cfg.VirtualModel {
		return errors.New("默认兜底模型不能与虚拟路由模型同名")
	}

	normalizedTiers := make(map[string][]string, len(allRouterTiers))
	for _, tier := range allRouterTiers {
		var list []string
		if cfg.Tiers != nil {
			list = normalizeModelSlice(cfg.Tiers[tier])
		}
		filtered := make([]string, 0, len(list))
		for _, m := range list {
			if m == cfg.VirtualModel || parseRouterModelSpec(m).BaseModel == cfg.VirtualModel {
				return fmt.Errorf("档位 %s 的候选模型不能包含虚拟模型自身 (%s)", tier, cfg.VirtualModel)
			}
			filtered = append(filtered, m)
		}
		normalizedTiers[tier] = filtered
	}
	cfg.Tiers = normalizedTiers
	cfg.TierThinking = normalizeTierThinkingMap(cfg.TierThinking)
	cfg.TierCriteria = cfg.effectiveTierCriteria()
	normCtxLimits := make(map[string]int, len(cfg.ModelContextLimits))
	for k, v := range cfg.ModelContextLimits {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && v > 0 {
			normCtxLimits[k] = v
		}
	}
	cfg.ModelContextLimits = normCtxLimits
	cfg.EscalationKeyword = strings.TrimSpace(cfg.EscalationKeyword)
	cfg.SharedClassifierModels = normalizeModelSlice(cfg.SharedClassifierModels)

	if cfg.Enabled {
		totalCandidates := len(cfg.ModelPool)
		for _, list := range cfg.Tiers {
			totalCandidates += len(list)
		}
		if cfg.DefaultModel == "" && totalCandidates == 0 {
			boundModels, _ := collectGatewayUpstreamBoundModels(context.Background(), cfg.VirtualModel)
			if len(boundModels) > 0 {
				cfg.DefaultModel = boundModels[0]
			} else {
				return errors.New("启用智能路由前，请至少绑定可用上游模型、配置模型池或默认兜底模型")
			}
		}
	}
	return nil
}

// ---------- In-memory config & session caches (bound to active db) ----------

var autoRouterState = struct {
	sync.RWMutex
	database *sql.DB
	loaded   bool
	cfg      AutoRouterConfig
}{cfg: defaultAutoRouterConfig()}

func loadAutoRouterCache() error {
	cfg := defaultAutoRouterConfig()
	if db != nil {
		if raw, err := settingsGet(autoRouterSettingsKey); err == nil && strings.TrimSpace(raw) != "" {
			parsed := defaultAutoRouterConfig()
			if json.Unmarshal([]byte(raw), &parsed) == nil {
				stripUnboundLegacyPlaceholderModels(&parsed)
				_ = normalizeAutoRouterConfig(&parsed)
				cfg = parsed
			}
		}
	}
	autoRouterState.Lock()
	autoRouterState.database = db
	autoRouterState.loaded = true
	autoRouterState.cfg = cfg
	autoRouterState.Unlock()
	loadAutoRouterBanditCache()
	return nil
}

func getAutoRouterConfig() AutoRouterConfig {
	autoRouterState.RLock()
	if autoRouterState.loaded && autoRouterState.database == db {
		cfg := cloneAutoRouterConfig(autoRouterState.cfg)
		autoRouterState.RUnlock()
		return cfg
	}
	autoRouterState.RUnlock()
	_ = loadAutoRouterCache()
	autoRouterState.RLock()
	defer autoRouterState.RUnlock()
	return cloneAutoRouterConfig(autoRouterState.cfg)
}

func saveAutoRouterConfig(cfg AutoRouterConfig) error {
	if err := normalizeAutoRouterConfig(&cfg); err != nil {
		return err
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := settingsSet(autoRouterSettingsKey, string(encoded)); err != nil {
		return err
	}
	autoRouterState.Lock()
	autoRouterState.database = db
	autoRouterState.loaded = true
	autoRouterState.cfg = cloneAutoRouterConfig(cfg)
	autoRouterState.Unlock()
	resetAutoRouterBreaker()
	invalidatePublicOverviews()
	return nil
}

func cloneAutoRouterConfig(src AutoRouterConfig) AutoRouterConfig {
	dst := src
	dst.ModelPool = append([]string(nil), src.ModelPool...)
	dst.ExcludedModels = append([]string(nil), src.ExcludedModels...)
	dst.ModelCapabilities = make(map[string]ModelCapabilityVector, len(src.ModelCapabilities))
	for k, v := range src.ModelCapabilities {
		dst.ModelCapabilities[k] = v
	}
	dst.Tiers = make(map[string][]string, len(src.Tiers))
	for k, v := range src.Tiers {
		dst.Tiers[k] = append([]string(nil), v...)
	}
	dst.TierThinking = make(map[string]string, len(src.TierThinking))
	for k, v := range src.TierThinking {
		dst.TierThinking[k] = v
	}
	dst.TierCriteria = make(map[string]string, len(src.TierCriteria))
	for k, v := range src.TierCriteria {
		dst.TierCriteria[k] = v
	}
	dst.ModelContextLimits = make(map[string]int, len(src.ModelContextLimits))
	for k, v := range src.ModelContextLimits {
		dst.ModelContextLimits[k] = v
	}
	dst.SharedClassifierModels = append([]string(nil), src.SharedClassifierModels...)
	return dst
}

func isAutoRouterModel(model string) bool {
	cfg := getAutoRouterConfig()
	return cfg.Enabled && cfg.Mode != "off" && strings.TrimSpace(model) == cfg.VirtualModel
}

// ---------- Session model affinity for coding agents ----------

type autoRouterSessionEntry struct {
	model         string
	tier          string
	domain        string
	upstreamID    int64
	upstreamModel string
	expiresAt     time.Time
}

var autoRouterSessions = struct {
	sync.Mutex
	database *sql.DB
	entries  map[string]autoRouterSessionEntry
}{entries: make(map[string]autoRouterSessionEntry)}

func lookupAutoRouterSession(sessionKey string) (model, tier string, ok bool) {
	model, tier, _, _, ok = lookupAutoRouterSessionFull(sessionKey)
	return model, tier, ok
}

func lookupAutoRouterSessionFull(sessionKey string) (model, tier string, upstreamID int64, upstreamModel string, ok bool) {
	model, tier, _, upstreamID, upstreamModel, ok = lookupAutoRouterSessionWithDomain(sessionKey)
	return model, tier, upstreamID, upstreamModel, ok
}

func lookupAutoRouterSessionWithDomain(sessionKey string) (model, tier, domain string, upstreamID int64, upstreamModel string, ok bool) {
	if sessionKey == "" {
		return "", "", "", 0, "", false
	}
	now := time.Now()
	autoRouterSessions.Lock()
	defer autoRouterSessions.Unlock()
	if autoRouterSessions.database != db {
		autoRouterSessions.database = db
		autoRouterSessions.entries = make(map[string]autoRouterSessionEntry)
		return "", "", "", 0, "", false
	}
	entry, exists := autoRouterSessions.entries[sessionKey]
	if !exists || now.After(entry.expiresAt) {
		delete(autoRouterSessions.entries, sessionKey)
		return "", "", "", 0, "", false
	}
	entry.expiresAt = now.Add(affinityTTL())
	autoRouterSessions.entries[sessionKey] = entry
	return entry.model, entry.tier, entry.domain, entry.upstreamID, entry.upstreamModel, true
}

func rememberAutoRouterSession(sessionKey, model, tier string) {
	rememberAutoRouterSessionFull(sessionKey, model, tier, 0, "")
}

func rememberAutoRouterSessionFull(sessionKey, model, tier string, upstreamID int64, upstreamModel string) {
	rememberAutoRouterSessionWithDomain(sessionKey, model, tier, "", upstreamID, upstreamModel)
}

func rememberAutoRouterSessionWithDomain(sessionKey, model, tier, domain string, upstreamID int64, upstreamModel string) {
	if sessionKey == "" || model == "" {
		return
	}
	now := time.Now()
	autoRouterSessions.Lock()
	defer autoRouterSessions.Unlock()
	if autoRouterSessions.database != db {
		autoRouterSessions.database = db
		autoRouterSessions.entries = make(map[string]autoRouterSessionEntry)
	}
	if len(autoRouterSessions.entries) > 4096 {
		for k, v := range autoRouterSessions.entries {
			if now.After(v.expiresAt) {
				delete(autoRouterSessions.entries, k)
			}
		}
	}
	if upstreamModel == "" {
		upstreamModel = parseRouterModelSpec(model).BaseModel
	}
	autoRouterSessions.entries[sessionKey] = autoRouterSessionEntry{
		model:         model,
		tier:          tier,
		domain:        domain,
		upstreamID:    upstreamID,
		upstreamModel: upstreamModel,
		expiresAt:     now.Add(affinityTTL()),
	}
}

// ---------- Candidate quota & resolution ----------

func autoRouterCandidateQuotaExhausted(ak ApiKey, virtualModel, candidate string) bool {
	if budget, independent := ak.ModelQuotas[candidate]; independent {
		return budget.QuotaUSD > 0 && budget.UsedUSD >= budget.QuotaUSD
	}
	if virtualModel != "" {
		if budget, independent := ak.ModelQuotas[virtualModel]; independent {
			return budget.QuotaUSD > 0 && budget.UsedUSD >= budget.QuotaUSD
		}
	}
	return ak.QuotaUSD > 0 && ak.SharedUsedUSD >= ak.QuotaUSD
}

func orderedCascadeTiers(startTier string, cascade bool) []string {
	idx := -1
	for i, t := range allRouterTiers {
		if t == startTier {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	if !cascade {
		return []string{startTier}
	}
	return allRouterTiers[idx:]
}

func escalateTierOneStep(tier string) string {
	switch tier {
	case TierSimple:
		return TierMedium
	case TierMedium:
		return TierComplex
	case TierComplex, TierReasoning:
		return TierReasoning
	default:
		return TierComplex
	}
}

// resolveAutoRoute classifies the request and builds the priority-ordered candidate
// list for relayHandler (or Dry-Run).
func resolveAutoRoute(
	ctx context.Context,
	ak ApiKey,
	format clientFormat,
	nativeOnly bool,
	headers http.Header,
	cr *canonReq,
) (decision *routerDecisionDetails, candidates []string, quotaErr error) {
	cfg := getAutoRouterConfig()
	return resolveAutoRouteWithConfig(ctx, cfg, ak, format, nativeOnly, headers, cr, false)
}

func resolveAutoRouteWithConfig(
	ctx context.Context,
	cfg AutoRouterConfig,
	ak ApiKey,
	format clientFormat,
	nativeOnly bool,
	headers http.Header,
	cr *canonReq,
	bypassBreaker bool,
) (decision *routerDecisionDetails, candidates []string, quotaErr error) {
	reqModel := cfg.VirtualModel
	if cr != nil && cr.Model != "" {
		reqModel = cr.Model
	}
	estimatedTokens := estimateRequestContextTokens(cr)
	strategyProfile := normalizeStrategyProfile(cfg.StrategyProfile)
	decision = &routerDecisionDetails{
		RequestedModel:       reqModel,
		StrategyProfile:      strategyProfile,
		EstimatedInputTokens: estimatedTokens,
		Shadow:               cfg.Mode == "shadow",
	}

	contextTurns := cfg.ContextUserTurns
	if contextTurns <= 0 {
		contextTurns = 1
	}
	lastUser, stateText := extractClassifierState(cr, contextTurns, 6000)

	// Compute session key for multi-turn agent consistency
	var rSessionKey string
	var prevSessionModel, prevSessionTier, prevSessionDomain string
	var prevUpstreamID int64
	var hasPrevSession bool
	if cr != nil && cfg.SessionMode != "every_turn" {
		endpoint := nativeEndpoint(format, cr)
		rSessionKey = scopedSessionKey(ak.ID, sessionKeyFromCanon(endpoint, headers, cr))
		if rSessionKey != "" {
			prevSessionModel, prevSessionTier, prevSessionDomain, prevUpstreamID, _, hasPrevSession = lookupAutoRouterSessionWithDomain(rSessionKey)
		}
	}

	// Extract full-conversation structural, modality, and retry/follow-up signals
	sig := analyzeRequestSignals(cr, lastUser, stateText, prevSessionTier)

	// Online Contextual Bandit: Implicit Next-Turn User Satisfaction Feedback
	// If the user expresses frustration/retry on the turn immediately following prevSessionModel,
	// retroactively penalize that arm; if it's a smooth continuation/tool-loop, reward it.
	if cfg.BanditEnabled && hasPrevSession && prevSessionModel != "" && !bypassBreaker {
		if sig.IsFrustrationOrRetry {
			recordBanditImplicitFeedback(prevUpstreamID, prevSessionModel, prevSessionDomain, true)
		} else if sig.IsToolLoop || sig.IsFollowUpDirective {
			recordBanditImplicitFeedback(prevUpstreamID, prevSessionModel, prevSessionDomain, false)
		}
	}

	// 1. Determine target tier (with Session Ratchet & Prompt-Cache Affinity)
	var chosenTier string
	var clfResult *classifierDecisionResult
	if hasPrevSession {
		wantSticky := cfg.SessionMode == "session_lock" ||
			(cfg.SessionMode == "user_turn" && (sig.IsToolLoop || sig.IsFollowUpDirective || (estimatedTokens >= 6000 && tierRank(prevSessionTier) >= tierRank(TierComplex) && !sig.IsFrustrationOrRetry)))
		// Session Ratchet: if the user expresses frustration/retry or the current turn's
		// structural floor demands a higher tier than prevSessionTier, unlock and ratchet UP!
		needsRatchetUp := sig.IsFrustrationOrRetry || (sig.StructuralFloor != "" && tierRank(sig.StructuralFloor) > tierRank(prevSessionTier))
		if cfg.SessionMode == "session_lock" && !needsRatchetUp {
			if hTier, _ := classifyHeuristic(lastUser, stateText, cr); tierRank(hTier) > tierRank(prevSessionTier) {
				needsRatchetUp = true
			}
		}
		if wantSticky && !needsRatchetUp {
			prevCtxLimit := effectiveModelContextLimit(prevSessionModel, cfg.ModelContextLimits)
			usableCap := effectiveContextCapacityForRequest(prevCtxLimit, estimatedTokens)
			fitsContext := usableCap <= 0 || estimatedTokens <= usableCap
			modOK, _ := modelSupportsRequestModalities(prevSessionModel, sig)
			if fitsContext && modOK &&
				!routerCandidateQuotaExhausted(ak, cfg.VirtualModel, prevSessionModel) &&
				(cfg.AllowUnlistedTargetModels || allowsRouterCandidate(ak, prevSessionModel)) {
				if _, _, err := pickUpstreamForRouterCandidate(format, prevSessionModel, nil, nativeOnly, cr); err == nil {
					chosenTier = prevSessionTier
					if chosenTier == "" {
						chosenTier = TierComplex
					}
					decision.Tier = chosenTier
					decision.Cause = "session_affinity"
					decision.SelectedModel = prevSessionModel
				}
			}
		}
	}

	// Do not short-circuit to SIMPLE if the multi-turn conversation context is already large,
	// or contains tool/code/error/retry signals.
	canShortCircuit := estimatedTokens <= 2000 &&
		(cr == nil || len(cr.Messages) <= 4) &&
		!sig.IsToolLoop &&
		!sig.HasCodeInHistory &&
		!sig.HasErrorOrStackTrace &&
		!sig.IsFrustrationOrRetry &&
		!sig.HasVision &&
		sig.StructuralFloor == ""

	if chosenTier == "" {
		if cfg.RouteHousekeeping && isHousekeepingRequest(cr, lastUser) {
			chosenTier = TierSimple
			decision.Tier = TierSimple
			decision.Cause = "housekeeping"
		} else if canShortCircuit && shouldShortCircuitSimple(lastUser, cfg.SkipShortChars) {
			chosenTier = TierSimple
			decision.Tier = TierSimple
			decision.Cause = "short_circuit"
		} else if normalizeClassifierProvider(cfg.Classifier) == "heuristic" {
			tier, score := classifyHeuristic(lastUser, stateText, cr)
			chosenTier = tier
			decision.Tier = tier
			decision.Cause = "heuristic"
			decision.HeuristicScore = &score
		} else {
			res, err := classifyWithDecisionModel(ctx, cfg, sig.EnrichedState, bypassBreaker)
			if err == nil && res != nil {
				clfResult = res
				chosenTier = res.Tier
				// Probability Margin Fusion: if classifier is nearly tied (<0.12 margin) with a higher
				// tier while technical/code/tool signals exist, prefer the safer higher tier.
				if (sig.HasCodeInHistory || sig.HasTools || sig.HasErrorOrStackTrace) && len(res.Probabilities) > 0 {
					for _, higher := range allRouterTiers {
						if tierRank(higher) > tierRank(chosenTier) {
							if p, ok := res.Probabilities[higher]; ok && p >= 0.25 && (res.Confidence-p) <= 0.12 {
								chosenTier = higher
								res.UpgradedLowConfidence = true
							}
						}
					}
				}
				decision.Tier = chosenTier
				decision.Cause = res.Provider
				if res.Provider == "cloudflare" {
					decision.Cause = "clef_flash"
					if strings.HasSuffix(res.Model, "/clef") {
						decision.Cause = "clef"
					}
				}
				decision.ClassifierProvider = res.Provider
				decision.ClassifierModel = res.Model
				decision.ClassifierMS = res.LatencyMS
				decision.Confidence = res.Confidence
				decision.Probabilities = res.Probabilities
				decision.UpgradedLowConfidence = res.UpgradedLowConfidence
			} else {
				if err != nil {
					decision.FallbackReason = err.Error()
				}
				if cfg.ClassifierFallback == "default_model" && cfg.DefaultModel != "" {
					chosenTier = TierDefault
					decision.Tier = TierDefault
					decision.Cause = "default_fallback"
				} else {
					tier, score := classifyHeuristic(lastUser, stateText, cr)
					chosenTier = tier
					decision.Tier = tier
					decision.Cause = "heuristic_fallback"
					decision.HeuristicScore = &score
				}
			}
		}
	}

	// Enforce Structural Floor & Retry/Frustration Auto-Escalation
	if decision.Cause != "housekeeping" && chosenTier != TierDefault && sig.StructuralFloor != "" {
		if tierRank(sig.StructuralFloor) > tierRank(chosenTier) {
			chosenTier = sig.StructuralFloor
			decision.Tier = chosenTier
			if sig.IsFrustrationOrRetry {
				decision.Cause += "+retry_escalated"
			} else {
				decision.Cause += "+structural_floor"
			}
		} else if sig.IsFrustrationOrRetry && !strings.Contains(decision.Cause, "+retry_escalated") {
			decision.Cause += "+retry_escalated"
		}
	}

	// Automatic long-context tier upgrade: when total conversation tokens exceed
	// LongContextThreshold, upgrade fast tiers (SIMPLE/MEDIUM) to COMPLEX.
	if cfg.LongContextThreshold > 0 && estimatedTokens >= cfg.LongContextThreshold &&
		(chosenTier == TierSimple || chosenTier == TierMedium) && decision.Cause != "housekeeping" {
		chosenTier = TierComplex
		decision.Tier = TierComplex
		decision.ContextUpgraded = true
		decision.Cause += "+long_context"
	}

	// Optional escalation keyword in user message
	if cfg.EscalationKeyword != "" && strings.Contains(lastUser, cfg.EscalationKeyword) {
		chosenTier = escalateTierOneStep(chosenTier)
		decision.Tier = chosenTier
		decision.Cause += "+escalated"
	}

	// 1b. Compute 6-Dimensional Benchmark Demand Profile & Required Difficulty Floor D(x)
	demand := profileRequestDemand(cr, lastUser, stateText, sig, estimatedTokens, clfResult)
	switch {
	case decision.Cause == "housekeeping" || decision.Cause == "short_circuit":
		if demand.RequiredDifficulty > 24.0 {
			demand.RequiredDifficulty = 24.0
		}
		demand.EquivalentTier = TierSimple
	case chosenTier == TierReasoning && demand.RequiredDifficulty < 76.0:
		demand.RequiredDifficulty = 78.0
		demand.EquivalentTier = TierReasoning
	case chosenTier == TierComplex && demand.RequiredDifficulty < 62.0:
		demand.RequiredDifficulty = 65.0
		demand.EquivalentTier = TierComplex
	}
	decision.PrimaryDomain = demand.PrimaryDomain
	decision.RequiredDifficulty = demand.RequiredDifficulty
	decision.DomainWeights = demand.Weights

	// 2. Build priority-ordered candidate list:
	//    - Autonomous Pool Mode (Smart Router 3.0 default when no manual Tiers are configured):
	//      Automatically gathers all models from cfg.ModelPool or bound upstreams (minus ExcludedModels)
	//      and ranks them via 6D Benchmark Capability Match + Online Contextual Bandit + Strategy Profile.
	//    - Legacy Manual Tiers Mode (when cfg.Tiers has configured models):
	//      Uses orderedCascadeTiers, enhanced with intra-tier Bandit/Domain utility ranking when active.
	type rawCandidate struct {
		model      string
		sourceTier string
		utility    candidateUtilityBreakdown
	}
	var ordered []rawCandidate
	seen := make(map[string]bool)
	isExcluded := routerModelExclusionFilter(cfg.ExcludedModels)

	evalUtility := func(m string, stochastic bool) candidateUtilityBreakdown {
		cLim := effectiveModelContextLimit(m, cfg.ModelContextLimits)
		isCacheHit := hasPrevSession && strings.EqualFold(parseRouterModelSpec(m).BaseModel, parseRouterModelSpec(prevSessionModel).BaseModel)
		var upID int64
		if up, _, err := pickUpstreamForRouterCandidate(format, m, nil, nativeOnly, cr); err == nil && up != nil {
			upID = up.ID
		}
		return scoreCandidateUtility(upID, m, demand, strategyProfile, isCacheHit, estimatedTokens, cLim, cfg.ModelCapabilities, cfg.BanditEnabled, stochastic)
	}

	addRaw := func(m, srcTier string, u candidateUtilityBreakdown) {
		m = strings.TrimSpace(m)
		if m == "" || m == cfg.VirtualModel || parseRouterModelSpec(m).BaseModel == cfg.VirtualModel || seen[m] || isExcluded(m) {
			return
		}
		seen[m] = true
		ordered = append(ordered, rawCandidate{model: m, sourceTier: srcTier, utility: u})
	}

	totalTierModels := 0
	for _, list := range cfg.Tiers {
		totalTierModels += len(list)
	}

	if totalTierModels == 0 {
		// Autonomous Pool Mode (no manual 4-tier classification needed)
		decision.AutonomousPool = true
		pool := append([]string(nil), cfg.ModelPool...)
		if len(pool) == 0 {
			boundModels, _ := collectGatewayUpstreamBoundModels(ctx, cfg.VirtualModel)
			pool = append(pool, boundModels...)
		}
		if cfg.DefaultModel != "" {
			pool = append(pool, cfg.DefaultModel)
		}
		pool = normalizeModelSlice(pool)

		scoredPool := make([]rawCandidate, 0, len(pool))
		for _, m := range pool {
			if m == "" || m == cfg.VirtualModel || parseRouterModelSpec(m).BaseModel == cfg.VirtualModel || isExcluded(m) {
				continue
			}
			u := evalUtility(m, !bypassBreaker && cfg.BanditEnabled)
			scoredPool = append(scoredPool, rawCandidate{
				model:      m,
				sourceTier: chosenTier,
				utility:    u,
			})
		}

		sort.SliceStable(scoredPool, func(i, j int) bool {
			// Preserve sticky session model at #1 if it is still healthy in Bandit
			if decision.Cause == "session_affinity" && decision.SelectedModel != "" {
				if scoredPool[i].model == decision.SelectedModel && scoredPool[i].utility.BanditWinRate >= 0.45 {
					return true
				}
				if scoredPool[j].model == decision.SelectedModel && scoredPool[j].utility.BanditWinRate >= 0.45 {
					return false
				}
			}
			if scoredPool[i].utility.UtilityScore != scoredPool[j].utility.UtilityScore {
				return scoredPool[i].utility.UtilityScore > scoredPool[j].utility.UtilityScore
			}
			return scoredPool[i].utility.CapabilityScore > scoredPool[j].utility.CapabilityScore
		})

		for _, item := range scoredPool {
			addRaw(item.model, item.sourceTier, item.utility)
		}
	} else {
		// Legacy / Hybrid Tier Mode
		if decision.Cause == "session_affinity" && decision.SelectedModel != "" {
			addRaw(decision.SelectedModel, chosenTier, evalUtility(decision.SelectedModel, false))
		}
		for _, t := range orderedCascadeTiers(chosenTier, cfg.CascadeTiers) {
			tierList := append([]string(nil), cfg.Tiers[t]...)
			if len(tierList) > 1 && cfg.BanditEnabled && cfg.Mode != "shadow" && hasAnyBanditHistoryForModels(tierList, demand.PrimaryDomain) {
				// Dynamic intra-tier Bandit & 6D capability re-ranking
				scored := make([]rawCandidate, 0, len(tierList))
				for _, m := range tierList {
					scored = append(scored, rawCandidate{
						model:      m,
						sourceTier: t,
						utility:    evalUtility(m, !bypassBreaker),
					})
				}
				sort.SliceStable(scored, func(i, j int) bool {
					return scored[i].utility.UtilityScore > scored[j].utility.UtilityScore
				})
				for _, sc := range scored {
					addRaw(sc.model, t, sc.utility)
				}
				continue
			}
			if len(tierList) > 1 && estimatedTokens >= 4000 {
				reordered := make([]string, 0, len(tierList))
				var dangerZone []string
				for _, m := range tierList {
					if hasPrevSession && strings.EqualFold(parseRouterModelSpec(m).BaseModel, parseRouterModelSpec(prevSessionModel).BaseModel) {
						reordered = append([]string{m}, reordered...)
						continue
					}
					cLim := effectiveModelContextLimit(m, cfg.ModelContextLimits)
					if cLim > 0 && estimatedTokens <= cLim && float64(estimatedTokens)/float64(cLim) > 0.82 {
						dangerZone = append(dangerZone, m)
					} else {
						reordered = append(reordered, m)
					}
				}
				tierList = append(reordered, dangerZone...)
			}
			for _, m := range tierList {
				addRaw(m, t, evalUtility(m, false))
			}
		}
		if cfg.DefaultModel != "" {
			addRaw(cfg.DefaultModel, TierDefault, evalUtility(cfg.DefaultModel, false))
		}
	}

	// 3. Filter candidates in priority order
	steps := make([]routerCandidateStep, 0, len(ordered))
	eligible := make([]string, 0, len(ordered))
	quotaSkipped := 0
	for idx, item := range ordered {
		ctxLimit := effectiveModelContextLimit(item.model, cfg.ModelContextLimits)
		usableCap := effectiveContextCapacityForRequest(ctxLimit, estimatedTokens)
		u := item.utility
		if u.RawModel == "" {
			u = evalUtility(item.model, false)
		}
		step := routerCandidateStep{
			Priority:        idx + 1,
			Model:           item.model,
			SourceTier:      item.sourceTier,
			ContextLimit:    ctxLimit,
			CapabilityScore: u.CapabilityScore,
			BanditWinRate:   u.BanditWinRate,
			UtilityScore:    u.UtilityScore,
		}
		if !cfg.AllowUnlistedTargetModels && ak.ID > 0 && !allowsRouterCandidate(ak, item.model) {
			step.Status = "skipped"
			step.Reason = "key_not_allowed"
			decision.SkippedCandidates++
			steps = append(steps, step)
			continue
		}
		if routerCandidateQuotaExhausted(ak, cfg.VirtualModel, item.model) {
			step.Status = "skipped"
			step.Reason = "quota_exhausted"
			quotaSkipped++
			decision.SkippedCandidates++
			steps = append(steps, step)
			continue
		}
		if modOK, modReason := modelSupportsRequestModalities(item.model, sig); !modOK {
			step.Status = "skipped"
			step.Reason = modReason
			decision.SkippedCandidates++
			steps = append(steps, step)
			continue
		}
		if usableCap > 0 && estimatedTokens > usableCap {
			step.Status = "skipped"
			step.Reason = "context_exceeded"
			decision.SkippedCandidates++
			steps = append(steps, step)
			continue
		}
		up, resolvedUpModel, err := pickUpstreamForRouterCandidate(format, item.model, nil, nativeOnly, cr)
		if err != nil {
			step.Status = "skipped"
			step.Reason = "no_healthy_upstream"
			decision.SkippedCandidates++
			steps = append(steps, step)
			continue
		}
		tierPolicy := cfg.TierThinking[item.sourceTier]
		tPlan := resolveAutoRouterThinking(up, resolvedUpModel, item.sourceTier, item.model, tierPolicy, decision.Cause, cr)
		step.Thinking = tPlan.EffectiveLabel
		step.UpstreamID = up.ID
		step.UpstreamName = up.Name
		if len(eligible) == 0 {
			step.Status = "selected"
			decision.SelectedThinking = tPlan.EffectiveLabel
		} else {
			step.Status = "standby"
		}
		eligible = append(eligible, item.model)
		steps = append(steps, step)
	}
	decision.Steps = steps

	// If all configured models were skipped solely due to quota exhaustion, report 402
	if len(eligible) == 0 && len(ordered) > 0 && quotaSkipped == len(ordered) {
		return decision, nil, fmt.Errorf("quota exhausted for model %s (all candidate models exhausted)", reqModel)
	}

	// If no candidate passed pre-check (e.g. all exceeded context or cooling),
	// fall back to the non-quota-exhausted candidate with the largest context window.
	if len(eligible) == 0 {
		bestModel := ""
		bestCtx := -1
		for _, item := range ordered {
			if !routerCandidateQuotaExhausted(ak, cfg.VirtualModel, item.model) &&
				(cfg.AllowUnlistedTargetModels || ak.ID == 0 || allowsRouterCandidate(ak, item.model)) {
				cLim := effectiveModelContextLimit(item.model, cfg.ModelContextLimits)
				if bestModel == "" || cLim > bestCtx {
					bestModel = item.model
					bestCtx = cLim
				}
			}
		}
		if bestModel != "" {
			eligible = append(eligible, bestModel)
		} else {
			if quotaSkipped > 0 {
				return decision, nil, fmt.Errorf("quota exhausted for model %s (no permitted candidate has remaining quota)", reqModel)
			}
			return decision, nil, errModelNotAllowed
		}
	}

	if len(eligible) > 0 {
		decision.SelectedModel = eligible[0]
	}
	decision.syncOutcome(cfg.DefaultModel)

	if cfg.Mode == "shadow" {
		decision.Shadow = true
		decision.ShadowModel = decision.SelectedModel
		actual := cfg.DefaultModel
		if actual == "" && len(eligible) > 0 {
			actual = eligible[0]
		}
		if !cfg.AllowUnlistedTargetModels && ak.ID > 0 && !allowsRouterCandidate(ak, actual) {
			return decision, nil, errModelNotAllowed
		}
		if routerCandidateQuotaExhausted(ak, cfg.VirtualModel, actual) {
			return decision, nil, fmt.Errorf("quota exhausted for model %s", actual)
		}
		decision.SelectedModel = actual
		if actual != "" {
			return decision, []string{actual}, nil
		}
	}
	return decision, eligible, nil
}

func applyAutoRouterHeaders(c *gin.Context, cfg AutoRouterConfig, decision *routerDecisionDetails) {
	if !cfg.ResponseHeaders || decision == nil || c == nil {
		return
	}
	h := c.Writer.Header()
	if decision.Tier != "" {
		h.Set("X-APISHARE-Router-Tier", decision.Tier)
	}
	if decision.PrimaryDomain != "" {
		h.Set("X-APISHARE-Router-Domain", decision.PrimaryDomain)
	}
	if decision.RequiredDifficulty > 0 {
		h.Set("X-APISHARE-Router-Difficulty", strconv.FormatFloat(decision.RequiredDifficulty, 'f', 1, 64))
	}
	if decision.StrategyProfile != "" {
		h.Set("X-APISHARE-Router-Strategy", decision.StrategyProfile)
	}
	if decision.SelectedModel != "" {
		h.Set("X-APISHARE-Router-Model", decision.SelectedModel)
	}
	if decision.SelectedThinking != "" && decision.SelectedThinking != "default" {
		h.Set("X-APISHARE-Router-Thinking", decision.SelectedThinking)
	}
	if decision.Cause != "" {
		h.Set("X-APISHARE-Router-Cause", decision.Cause)
	}
	if decision.SpeculativeEscalated {
		reason := decision.SpeculativeReason
		if reason == "" {
			reason = "escalated"
		}
		h.Set("X-APISHARE-Router-Speculative", reason)
	}
	if decision.ClassifierMS > 0 {
		h.Set("X-APISHARE-Router-Latency-MS", strconv.FormatInt(decision.ClassifierMS, 10))
	}
	if decision.DegradedToDefault {
		h.Set("X-APISHARE-Router-Degraded", "default")
	}
	if len(decision.FailedModels) > 0 {
		h.Set("X-APISHARE-Router-Failed-Models", strings.Join(decision.FailedModels, ","))
	}
	if decision.Shadow {
		h.Set("X-APISHARE-Router-Shadow", "1")
		if decision.ShadowModel != "" {
			h.Set("X-APISHARE-Router-Shadow-Model", decision.ShadowModel)
		}
	}
}

// ---------- Admin API Routes (#tab-router) ----------

type autoRouterModelHealth struct {
	Model            string                `json:"model"`
	BaseModel        string                `json:"base_model,omitempty"`
	Family           string                `json:"family"`
	HealthyCount     int                   `json:"healthy_count"`
	TotalCount       int                   `json:"total_count"`
	UpstreamNames    []string              `json:"upstream_names"`
	Status           string                `json:"status"` // "healthy" | "cooling" | "missing"
	PriceIn          float64               `json:"price_in"`
	PriceOut         float64               `json:"price_out"`
	ContextLimit     int                   `json:"context_limit"`
	ContextSource    string                `json:"context_source,omitempty"`
	MaxOutput        int                   `json:"max_output"`
	SupportsThinking bool                  `json:"supports_thinking"`
	Efforts          []string              `json:"efforts,omitempty"`
	Capabilities     ModelCapabilityVector `json:"capabilities"`
	BanditWinRate    float64               `json:"bandit_win_rate,omitempty"`
	BanditCalls      int64                 `json:"bandit_calls,omitempty"`
	EWMATTFTMS       float64               `json:"ewma_ttft_ms,omitempty"`
	EWMATPS          float64               `json:"ewma_tps,omitempty"`
}

type autoRouterUpstreamModelGroup struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Enabled bool     `json:"enabled"`
	Healthy bool     `json:"healthy"`
	Models  []string `json:"models"`
}

func collectGatewayUpstreamBoundModels(ctx context.Context, virtualModel string) ([]string, []autoRouterUpstreamModelGroup) {
	virtualModel = strings.TrimSpace(virtualModel)
	if virtualModel == "" {
		virtualModel = "auto"
	}
	enabledSet := make(map[string]bool)
	if mods, err := availableModels(ctx); err == nil {
		for _, m := range mods {
			id := strings.TrimSpace(m.ID)
			if id == "" || strings.EqualFold(id, virtualModel) || m.Owner == "apishare-router" {
				continue
			}
			enabledSet[id] = true
		}
	}

	snapshot, err := getRoutingSnapshot()
	if err != nil {
		models := make([]string, 0, len(enabledSet))
		for m := range enabledSet {
			models = append(models, m)
		}
		sort.Strings(models)
		return models, []autoRouterUpstreamModelGroup{}
	}
	groups := make([]autoRouterUpstreamModelGroup, 0, len(snapshot.upstreams))

	for i := range snapshot.upstreams {
		u := &snapshot.upstreams[i]
		isEnabled := u.Enabled && u.Weight > 0
		seenUp := make(map[string]bool)
		upModels := make([]string, 0)
		addModel := func(m string) {
			m = strings.TrimSpace(m)
			if m == "" || strings.EqualFold(m, virtualModel) || seenUp[m] {
				return
			}
			if (u.openCodePublic() || u.openCodeFreeOnly()) && !u.openCodeFreeModel(u.mapModel(m)) {
				return
			}
			seenUp[m] = true
			upModels = append(upModels, m)
			if isEnabled {
				enabledSet[m] = true
			}
		}
		if list := strings.TrimSpace(u.Models); list != "" {
			for _, part := range strings.Split(list, ",") {
				addModel(part)
			}
		} else if defaults := defaultDecisionModelsForType(u.Type); len(defaults) > 0 {
			for _, dm := range defaults {
				addModel(dm)
			}
		} else if isEnabled {
			ids, _ := publicWildcardModels(*u)
			for _, id := range ids {
				addModel(id)
			}
		}
		if mm := u.modelMap(); len(mm) > 0 {
			mapKeys := make([]string, 0, len(mm))
			for client := range mm {
				if u.servesModel(client) {
					mapKeys = append(mapKeys, client)
				}
			}
			sort.Strings(mapKeys)
			for _, client := range mapKeys {
				addModel(client)
			}
		}
		groups = append(groups, autoRouterUpstreamModelGroup{
			ID:      u.ID,
			Name:    u.Name,
			Type:    u.Type,
			Enabled: isEnabled,
			Healthy: isEnabled && u.availableForRelay(),
			Models:  upModels,
		})
	}

	models := make([]string, 0, len(enabledSet))
	for m := range enabledSet {
		models = append(models, m)
	}
	sort.Strings(models)
	return models, groups
}

func buildAutoRouterModelHealthMap(ctx context.Context, cfg AutoRouterConfig, availableModelsList []string) map[string]autoRouterModelHealth {
	out := make(map[string]autoRouterModelHealth)
	snapshot, err := getRoutingSnapshot()
	if err != nil {
		return out
	}
	candidates := make(map[string]bool)
	if len(availableModelsList) == 0 {
		availableModelsList, _ = collectGatewayUpstreamBoundModels(ctx, cfg.VirtualModel)
	}
	for _, m := range availableModelsList {
		if m != "" {
			candidates[m] = true
		}
	}
	if cfg.DefaultModel != "" {
		candidates[cfg.DefaultModel] = true
	}
	for _, m := range cfg.ModelPool {
		if m != "" {
			candidates[m] = true
		}
	}
	for _, list := range cfg.Tiers {
		for _, m := range list {
			if m != "" {
				candidates[m] = true
			}
		}
	}
	for _, m := range cfg.SharedClassifierModels {
		if m != "" {
			candidates[m] = true
		}
	}
	if cfg.Classifier == "gateway_upstream" && strings.TrimSpace(cfg.ClassifierModel) != "" {
		candidates[strings.TrimSpace(cfg.ClassifierModel)] = true
	}
	for model := range candidates {
		spec := parseRouterModelSpec(model)
		baseModel := spec.BaseModel
		if baseModel == "" {
			baseModel = model
		}
		tCap := classifyModelThinkingCapability(nil, baseModel)
		ctxLimit, ctxSource := effectiveModelContextLimitWithSource(model, cfg.ModelContextLimits)
		bWin, bCalls, bTTFT, bTPS := summarizeModelBanditStats(baseModel)
		info := autoRouterModelHealth{
			Model:            model,
			BaseModel:        baseModel,
			Family:           publicFamily(baseModel, ""),
			Status:           "missing",
			ContextLimit:     ctxLimit,
			ContextSource:    ctxSource,
			MaxOutput:        modelMaxOutputLimit(baseModel),
			SupportsThinking: tCap.SupportsThinking,
			Efforts:          tCap.Efforts,
			Capabilities:     resolveModelCapabilityVector(model, cfg.ModelCapabilities),
			BanditWinRate:    bWin,
			BanditCalls:      bCalls,
			EWMATTFTMS:       bTTFT,
			EWMATPS:          bTPS,
		}
		price, _, _ := lookupRelayPrice(baseModel, "")
		info.PriceIn = price.In
		info.PriceOut = price.Out
		for i := range snapshot.upstreams {
			u := &snapshot.upstreams[i]
			if !u.Enabled || u.Weight <= 0 || !servesRouterCandidate(u, model) {
				continue
			}
			info.TotalCount++
			info.UpstreamNames = append(info.UpstreamNames, u.Name)
			upTarget := resolveUpstreamCandidateModel(u, model)
			if u.availableForRelay(upTarget) {
				info.HealthyCount++
			}
			if info.ContextSource == "builtin" && upTarget != "" && upTarget != baseModel {
				if mappedLim, mappedSrc := effectiveModelContextLimitWithSource(upTarget, cfg.ModelContextLimits); mappedSrc != "builtin" {
					info.ContextLimit = mappedLim
					info.ContextSource = mappedSrc
				}
			}
		}
		switch {
		case info.HealthyCount > 0:
			info.Status = "healthy"
		case info.TotalCount > 0:
			info.Status = "cooling"
		default:
			info.Status = "missing"
		}
		out[model] = info
	}
	return out
}

func buildAutoRouterConfigView(ctx context.Context, cfg AutoRouterConfig) gin.H {
	stripUnboundLegacyPlaceholderModels(&cfg)
	availableModelsList, upstreamGroups := collectGatewayUpstreamBoundModels(ctx, cfg.VirtualModel)
	provider, endpoint, resolvedModel, apiKey, keySource, credErr := cfg.effectiveClassifierCredentials()
	breakerOpen, breakerUntil, breakerFails, breakerLast := autoRouterBreakerOpen()
	var breakerUntilStr string
	if !breakerUntil.IsZero() {
		breakerUntilStr = breakerUntil.UTC().Format(time.RFC3339)
	}
	envAccountID := strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	envToken := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN")) != "" || strings.TrimSpace(os.Getenv("CLOUDFLARE_API_KEY")) != ""
	envJevKey := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" || strings.TrimSpace(os.Getenv("JEV_API_KEY")) != ""
	envOpenRouterKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) != ""

	credReady := credErr == nil
	credMessage := "就绪"
	if credErr != nil {
		credMessage = credErr.Error()
	}

	return gin.H{
		"enabled":                          cfg.Enabled,
		"mode":                             cfg.Mode,
		"virtual_model":                    cfg.VirtualModel,
		"strategy_profile":                 normalizeStrategyProfile(cfg.StrategyProfile),
		"model_pool":                       cfg.ModelPool,
		"excluded_models":                  cfg.ExcludedModels,
		"bandit_enabled":                   cfg.BanditEnabled,
		"speculative_cascade":              cfg.SpeculativeCascade,
		"speculative_ttft_ms":              cfg.SpeculativeTTFTMS,
		"model_capabilities":               cfg.ModelCapabilities,
		"bandit_leaderboard":               snapshotBanditLeaderboard(),
		"classifier":                       cfg.Classifier,
		"classifier_provider":              provider,
		"classifier_model":                 cfg.ClassifierModel,
		"resolved_classifier_model":        resolvedModel,
		"resolved_endpoint":                endpoint,
		"cloudflare_account_id":            cfg.CloudflareAccountID,
		"env_cloudflare_account_id":        envAccountID,
		"endpoint_url":                     cfg.EndpointURL,
		"api_key_saved":                    strings.TrimSpace(cfg.APIKey) != "",
		"api_key_masked":                   maskKey(apiKey),
		"api_key_source":                   keySource,
		"env_cloudflare_token_present":     envToken,
		"env_jev_key_present":              envJevKey,
		"env_openrouter_key_present":       envOpenRouterKey,
		"credentials_ready":                credReady,
		"credentials_message":              credMessage,
		"proxy_id":                         cfg.ProxyID,
		"classifier_timeout_ms":            cfg.ClassifierTimeoutMS,
		"classifier_fallback":              cfg.ClassifierFallback,
		"classifier_instructions":          cfg.ClassifierInstructions,
		"min_confidence":                   cfg.MinConfidence,
		"context_user_turns":               cfg.ContextUserTurns,
		"skip_short_chars":                 cfg.SkipShortChars,
		"long_context_threshold":           cfg.LongContextThreshold,
		"route_housekeeping":               cfg.RouteHousekeeping,
		"session_mode":                     cfg.SessionMode,
		"cascade_tiers":                    cfg.CascadeTiers,
		"allow_unlisted_target_models":     cfg.AllowUnlistedTargetModels,
		"default_model":                    cfg.DefaultModel,
		"tiers":                            cfg.Tiers,
		"tier_thinking":                    normalizeTierThinkingMap(cfg.TierThinking),
		"tier_criteria":                    cfg.effectiveTierCriteria(),
		"default_tier_criteria":            defaultAutoRouterTierCriteria(),
		"model_context_limits":             cfg.ModelContextLimits,
		"response_headers":                 cfg.ResponseHeaders,
		"escalation_keyword":               cfg.EscalationKeyword,
		"allow_user_auto_router":           cfg.AllowUserAutoRouter,
		"share_site_classifier_with_users": cfg.ShareSiteClassifierWithUsers,
		"shared_classifier_models":         cfg.SharedClassifierModels,
		"available_models":                 availableModelsList,
		"upstream_models":                  upstreamGroups,
		"breaker": gin.H{
			"open":       breakerOpen,
			"until":      breakerUntilStr,
			"fails":      breakerFails,
			"last_error": breakerLast,
		},
		"model_health": buildAutoRouterModelHealthMap(ctx, cfg, availableModelsList),
	}
}

func registerAutoRouterAdminRoutes(auth *gin.RouterGroup) {
	auth.GET("/auto-router/config", func(c *gin.Context) {
		cfg := getAutoRouterConfig()
		c.JSON(http.StatusOK, buildAutoRouterConfigView(c.Request.Context(), cfg))
	})

	auth.POST("/auto-router/bandit/reset", func(c *gin.Context) {
		if db != nil {
			if err := settingsSet(banditSettingsKey, ""); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "清空 Bandit 记忆失败，请稍后重试"})
				return
			}
		}
		resetAutoRouterBandit()
		invalidatePublicOverviews()
		c.JSON(http.StatusOK, buildAutoRouterConfigView(c.Request.Context(), getAutoRouterConfig()))
	})

	auth.PUT("/auto-router/config", func(c *gin.Context) {
		var req struct {
			Mode                         *string                           `json:"mode"`
			Enabled                      *bool                             `json:"enabled"`
			VirtualModel                 *string                           `json:"virtual_model"`
			StrategyProfile              *string                           `json:"strategy_profile"`
			ModelPool                    *[]string                         `json:"model_pool"`
			ExcludedModels               *[]string                         `json:"excluded_models"`
			BanditEnabled                *bool                             `json:"bandit_enabled"`
			SpeculativeCascade           *bool                             `json:"speculative_cascade"`
			SpeculativeTTFTMS            *int                              `json:"speculative_ttft_ms"`
			ModelCapabilities            *map[string]ModelCapabilityVector `json:"model_capabilities"`
			Classifier                   *string                           `json:"classifier"`
			ClassifierModel              *string                           `json:"classifier_model"`
			CloudflareAccountID          *string                           `json:"cloudflare_account_id"`
			EndpointURL                  *string                           `json:"endpoint_url"`
			APIKey                       *string                           `json:"api_key"`
			ClearAPIKey                  *bool                             `json:"clear_api_key"`
			ProxyID                      *int64                            `json:"proxy_id"`
			ClassifierTimeoutMS          *int                              `json:"classifier_timeout_ms"`
			ClassifierFallback           *string                           `json:"classifier_fallback"`
			ClassifierInstructions       *string                           `json:"classifier_instructions"`
			MinConfidence                *float64                          `json:"min_confidence"`
			ContextUserTurns             *int                              `json:"context_user_turns"`
			SkipShortChars               *int                              `json:"skip_short_chars"`
			LongContextThreshold         *int                              `json:"long_context_threshold"`
			RouteHousekeeping            *bool                             `json:"route_housekeeping"`
			SessionMode                  *string                           `json:"session_mode"`
			CascadeTiers                 *bool                             `json:"cascade_tiers"`
			AllowUnlistedTargetModels    *bool                             `json:"allow_unlisted_target_models"`
			DefaultModel                 *string                           `json:"default_model"`
			Tiers                        *map[string][]string              `json:"tiers"`
			TierThinking                 *map[string]string                `json:"tier_thinking"`
			TierCriteria                 *map[string]string                `json:"tier_criteria"`
			ModelContextLimits           *map[string]int                   `json:"model_context_limits"`
			ResponseHeaders              *bool                             `json:"response_headers"`
			EscalationKeyword            *string                           `json:"escalation_keyword"`
			AllowUserAutoRouter          *bool                             `json:"allow_user_auto_router"`
			ShareSiteClassifierWithUsers *bool                             `json:"share_site_classifier_with_users"`
			SharedClassifierModels       *[]string                         `json:"shared_classifier_models"`
		}
		if err := c.BindJSON(&req); err != nil {
			return
		}
		cfg := getAutoRouterConfig()
		if req.Enabled != nil && req.Mode == nil {
			if *req.Enabled {
				if cfg.Mode == "off" || cfg.Mode == "" {
					cfg.Mode = "live"
				}
			} else {
				cfg.Mode = "off"
			}
		}
		if req.Mode != nil {
			cfg.Mode = *req.Mode
		}
		if req.VirtualModel != nil {
			cfg.VirtualModel = *req.VirtualModel
		}
		if req.StrategyProfile != nil {
			cfg.StrategyProfile = *req.StrategyProfile
		}
		if req.ModelPool != nil {
			cfg.ModelPool = *req.ModelPool
		}
		if req.ExcludedModels != nil {
			cfg.ExcludedModels = *req.ExcludedModels
		}
		if req.BanditEnabled != nil {
			cfg.BanditEnabled = *req.BanditEnabled
		}
		if req.SpeculativeCascade != nil {
			cfg.SpeculativeCascade = *req.SpeculativeCascade
		}
		if req.SpeculativeTTFTMS != nil {
			cfg.SpeculativeTTFTMS = *req.SpeculativeTTFTMS
		}
		if req.ModelCapabilities != nil {
			cfg.ModelCapabilities = *req.ModelCapabilities
		}
		if req.Classifier != nil {
			cfg.Classifier = *req.Classifier
		}
		if req.ClassifierModel != nil {
			cfg.ClassifierModel = *req.ClassifierModel
		}
		if req.CloudflareAccountID != nil {
			cfg.CloudflareAccountID = *req.CloudflareAccountID
		}
		if req.EndpointURL != nil {
			cfg.EndpointURL = *req.EndpointURL
		}
		if req.ClearAPIKey != nil && *req.ClearAPIKey {
			cfg.APIKey = ""
		} else if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			cfg.APIKey = strings.TrimSpace(*req.APIKey)
		}
		if req.ProxyID != nil {
			cfg.ProxyID = *req.ProxyID
		}
		if req.ClassifierTimeoutMS != nil {
			cfg.ClassifierTimeoutMS = *req.ClassifierTimeoutMS
		}
		if req.ClassifierFallback != nil {
			cfg.ClassifierFallback = *req.ClassifierFallback
		}
		if req.ClassifierInstructions != nil {
			cfg.ClassifierInstructions = *req.ClassifierInstructions
		}
		if req.MinConfidence != nil {
			cfg.MinConfidence = *req.MinConfidence
		}
		if req.ContextUserTurns != nil {
			cfg.ContextUserTurns = *req.ContextUserTurns
		}
		if req.SkipShortChars != nil {
			cfg.SkipShortChars = *req.SkipShortChars
		}
		if req.LongContextThreshold != nil {
			cfg.LongContextThreshold = *req.LongContextThreshold
		}
		if req.RouteHousekeeping != nil {
			cfg.RouteHousekeeping = *req.RouteHousekeeping
		}
		if req.SessionMode != nil {
			cfg.SessionMode = *req.SessionMode
		}
		if req.CascadeTiers != nil {
			cfg.CascadeTiers = *req.CascadeTiers
		}
		if req.AllowUnlistedTargetModels != nil {
			cfg.AllowUnlistedTargetModels = *req.AllowUnlistedTargetModels
		}
		if req.DefaultModel != nil {
			cfg.DefaultModel = *req.DefaultModel
		}
		if req.Tiers != nil {
			cfg.Tiers = *req.Tiers
		}
		if req.TierThinking != nil {
			cfg.TierThinking = *req.TierThinking
		}
		if req.TierCriteria != nil {
			cfg.TierCriteria = *req.TierCriteria
		}
		if req.ModelContextLimits != nil {
			cfg.ModelContextLimits = *req.ModelContextLimits
		}
		if req.ResponseHeaders != nil {
			cfg.ResponseHeaders = *req.ResponseHeaders
		}
		if req.EscalationKeyword != nil {
			cfg.EscalationKeyword = *req.EscalationKeyword
		}
		if req.AllowUserAutoRouter != nil {
			cfg.AllowUserAutoRouter = *req.AllowUserAutoRouter
		}
		if req.ShareSiteClassifierWithUsers != nil {
			cfg.ShareSiteClassifierWithUsers = *req.ShareSiteClassifierWithUsers
		}
		if req.SharedClassifierModels != nil {
			cfg.SharedClassifierModels = *req.SharedClassifierModels
		}
		if err := saveAutoRouterConfig(cfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, buildAutoRouterConfigView(c.Request.Context(), getAutoRouterConfig()))
	})

	// Test external classifier connectivity & latency
	auth.POST("/auto-router/test-classifier", func(c *gin.Context) {
		var req struct {
			Prompt              string            `json:"prompt"`
			Classifier          *string           `json:"classifier"`
			ClassifierModel     *string           `json:"classifier_model"`
			CloudflareAccountID *string           `json:"cloudflare_account_id"`
			EndpointURL         *string           `json:"endpoint_url"`
			APIKey              *string           `json:"api_key"`
			ProxyID             *int64            `json:"proxy_id"`
			ClassifierTimeoutMS *int              `json:"classifier_timeout_ms"`
			MinConfidence       *float64          `json:"min_confidence"`
			TierCriteria        map[string]string `json:"tier_criteria"`
		}
		_ = c.ShouldBindJSON(&req)
		cfg := getAutoRouterConfig()
		if req.Classifier != nil {
			cfg.Classifier = *req.Classifier
		}
		if req.ClassifierModel != nil {
			cfg.ClassifierModel = *req.ClassifierModel
		}
		if req.CloudflareAccountID != nil {
			cfg.CloudflareAccountID = *req.CloudflareAccountID
		}
		if req.EndpointURL != nil {
			cfg.EndpointURL = *req.EndpointURL
		}
		if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
			cfg.APIKey = strings.TrimSpace(*req.APIKey)
		}
		if req.ProxyID != nil {
			cfg.ProxyID = *req.ProxyID
		}
		if req.ClassifierTimeoutMS != nil && *req.ClassifierTimeoutMS > 0 {
			cfg.ClassifierTimeoutMS = *req.ClassifierTimeoutMS
		}
		if req.MinConfidence != nil {
			cfg.MinConfidence = *req.MinConfidence
		}
		if len(req.TierCriteria) > 0 {
			cfg.TierCriteria = req.TierCriteria
		}
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			prompt = "Refactor this async Go HTTP handler to use a worker pool, context cancellation, and exponential backoff retries."
		}
		if normalizeClassifierProvider(cfg.Classifier) == "heuristic" {
			start := time.Now()
			tier, score := classifyHeuristic(prompt, prompt, &canonReq{Messages: []canonMsg{{Role: "user", Text: prompt}}})
			c.JSON(http.StatusOK, gin.H{
				"ok":         true,
				"provider":   "heuristic",
				"model":      "heuristic",
				"tier":       tier,
				"score":      score,
				"latency_ms": time.Since(start).Milliseconds(),
			})
			return
		}
		res, err := classifyWithDecisionModel(c.Request.Context(), cfg, prompt, true)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
			return
		}
		resetAutoRouterBreaker()
		c.JSON(http.StatusOK, gin.H{
			"ok":                      true,
			"provider":                res.Provider,
			"model":                   res.Model,
			"tier":                    res.Tier,
			"raw_choice":              res.RawChoice,
			"confidence":              res.Confidence,
			"probabilities":           res.Probabilities,
			"upgraded_low_confidence": res.UpgradedLowConfidence,
			"latency_ms":              res.LatencyMS,
		})
	})

	// Interactive Dry-Run playground: runs full routing resolution without calling answer upstreams
	auth.POST("/auto-router/dry-run", func(c *gin.Context) {
		var req struct {
			Prompt          string            `json:"prompt"`
			KeyID           int64             `json:"key_id"`
			Format          string            `json:"format"`
			ReasoningEffort string            `json:"reasoning_effort"`
			Override        *AutoRouterConfig `json:"override"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数格式不正确"})
			return
		}
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入要试算的测试提问内容"})
			return
		}
		cfg := getAutoRouterConfig()
		if req.Override != nil {
			merged := *req.Override
			if strings.TrimSpace(merged.APIKey) == "" {
				merged.APIKey = cfg.APIKey
			}
			if err := normalizeAutoRouterConfig(&merged); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			cfg = merged
		}
		if cfg.Mode == "off" {
			cfg.Mode = "live"
			cfg.Enabled = true
		}

		var ak ApiKey
		if req.KeyID > 0 {
			loaded, err := dbGetKeyByID(req.KeyID)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "所选 API 密钥不存在"})
				return
			}
			ak = *loaded
		}
		format := FormatOpenAIChat
		switch strings.ToLower(strings.TrimSpace(req.Format)) {
		case "openai-responses", "responses":
			format = FormatOpenAIResponse
		case "anthropic", "messages":
			format = FormatAnthropic
		case "gemini":
			format = FormatGemini
		}
		cr := &canonReq{
			Model:           cfg.VirtualModel,
			Messages:        []canonMsg{{Role: "user", Text: prompt}},
			InputText:       prompt,
			ReasoningEffort: strings.TrimSpace(req.ReasoningEffort),
		}
		decision, candidates, quotaErr := resolveAutoRouteWithConfig(c.Request.Context(), cfg, ak, format, false, http.Header{}, cr, true)
		var quotaErrStr string
		if quotaErr != nil {
			quotaErrStr = quotaErr.Error()
		}
		c.JSON(http.StatusOK, gin.H{
			"decision":    decision,
			"candidates":  candidates,
			"quota_error": quotaErrStr,
		})
	})

	// Telemetry & tier distribution statistics for #tab-router
	auth.GET("/auto-router/stats", func(c *gin.Context) {
		period := c.DefaultQuery("period", "24h")
		stats, err := queryAutoRouterStats(c.Request.Context(), period)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, stats)
	})
}

type autoRouterTierStat struct {
	Tier    string  `json:"tier"`
	Calls   int64   `json:"calls"`
	CostUSD float64 `json:"cost_usd"`
}

type autoRouterModelStat struct {
	Model   string  `json:"model"`
	Tier    string  `json:"tier"`
	Calls   int64   `json:"calls"`
	CostUSD float64 `json:"cost_usd"`
}

func queryAutoRouterStats(ctx context.Context, period string) (gin.H, error) {
	now := beijingNow()
	var start time.Time
	switch period {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, beijingLocation)
	case "7d":
		start = now.Add(-7 * 24 * time.Hour)
	case "30d":
		start = now.Add(-30 * 24 * time.Hour)
	default:
		period = "24h"
		start = now.Add(-24 * time.Hour)
	}

	cfg := getAutoRouterConfig()
	defaultPrice, _, _ := lookupRelayPrice(cfg.DefaultModel, "")

	const routerFilter = `json_valid(billing_json) AND COALESCE(json_extract(billing_json,'$.router.requested_model'),'')<>''`

	rows, err := db.QueryContext(ctx, `SELECT
		model,
		status,
		prompt_tokens,
		completion_tokens,
		cost_usd,
		COALESCE(json_extract(billing_json,'$.router.tier'),'DEFAULT'),
		COALESCE(json_extract(billing_json,'$.router.cause'),'unknown'),
		COALESCE(CAST(json_extract(billing_json,'$.router.classifier_ms') AS INTEGER),0),
		COALESCE(CAST(json_extract(billing_json,'$.router.skipped_candidates') AS INTEGER),0),
		COALESCE(CAST(json_extract(billing_json,'$.router.shadow') AS INTEGER),0),
		COALESCE(CAST(json_extract(billing_json,'$.router.degraded_to_default') AS INTEGER),0)
		FROM call_logs
		WHERE unixepoch(created_at)>=? AND `+routerFilter, start.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		totalCalls      int64
		successCalls    int64
		fastCalls       int64
		shadowCalls     int64
		fallbackCalls   int64
		skippedModels   int64
		actualCostUSD   float64
		baselineCostUSD float64
		classifierMS    []int64
	)
	byTier := map[string]*autoRouterTierStat{
		TierSimple:    {Tier: TierSimple},
		TierMedium:    {Tier: TierMedium},
		TierComplex:   {Tier: TierComplex},
		TierReasoning: {Tier: TierReasoning},
		TierDefault:   {Tier: TierDefault},
	}
	byCause := map[string]int64{}
	byModel := map[string]*autoRouterModelStat{}

	for rows.Next() {
		var (
			model, tier, cause                            string
			status, promptTok, compTok                    int
			cost                                          float64
			clfMS, skippedCount, shadowFlag, degradedFlag int64
		)
		if err := rows.Scan(&model, &status, &promptTok, &compTok, &cost, &tier, &cause, &clfMS, &skippedCount, &shadowFlag, &degradedFlag); err != nil {
			return nil, err
		}
		normTier := normalizeTierName(tier)
		if normTier == "" {
			normTier = TierDefault
		}
		totalCalls++
		if shadowFlag != 0 {
			shadowCalls++
		}
		if skippedCount > 0 {
			skippedModels += skippedCount
		}
		if strings.Contains(cause, "fallback") || degradedFlag != 0 {
			fallbackCalls++
		}
		if clfMS > 0 {
			classifierMS = append(classifierMS, clfMS)
		}
		if normTier == TierSimple || normTier == TierMedium {
			fastCalls++
		}
		byCause[cause]++
		slot := byTier[normTier]
		slot.Calls++

		if status >= 200 && status < 300 {
			successCalls++
			actualCostUSD += cost
			slot.CostUSD += cost
			if defaultPrice.In > 0 || defaultPrice.Out > 0 {
				baselineCostUSD += (float64(promptTok)*defaultPrice.In + float64(compTok)*defaultPrice.Out) / 1e6
			} else {
				baselineCostUSD += cost
			}
		}

		mKey := model + "\x00" + normTier
		ms := byModel[mKey]
		if ms == nil {
			ms = &autoRouterModelStat{Model: model, Tier: normTier}
			byModel[mKey] = ms
		}
		ms.Calls++
		if status >= 200 && status < 300 {
			ms.CostUSD += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(classifierMS, func(i, j int) bool { return classifierMS[i] < classifierMS[j] })
	var p50MS, p95MS int64
	if n := len(classifierMS); n > 0 {
		p50MS = classifierMS[n/2]
		idx95 := int(float64(n)*0.95) - 1
		if idx95 < 0 {
			idx95 = 0
		}
		p95MS = classifierMS[idx95]
	}

	savedUSD := baselineCostUSD - actualCostUSD
	if savedUSD < 0 {
		savedUSD = 0
	}

	tiersOut := make([]autoRouterTierStat, 0, 5)
	for _, t := range []string{TierSimple, TierMedium, TierComplex, TierReasoning, TierDefault} {
		tiersOut = append(tiersOut, *byTier[t])
	}

	modelsOut := make([]autoRouterModelStat, 0, len(byModel))
	for _, m := range byModel {
		modelsOut = append(modelsOut, *m)
	}
	sort.Slice(modelsOut, func(i, j int) bool {
		if modelsOut[i].Calls != modelsOut[j].Calls {
			return modelsOut[i].Calls > modelsOut[j].Calls
		}
		return modelsOut[i].CostUSD > modelsOut[j].CostUSD
	})
	if len(modelsOut) > 12 {
		modelsOut = modelsOut[:12]
	}

	return gin.H{
		"period":            period,
		"total_calls":       totalCalls,
		"success_calls":     successCalls,
		"fast_calls":        fastCalls,
		"shadow_calls":      shadowCalls,
		"fallback_calls":    fallbackCalls,
		"skipped_models":    skippedModels,
		"actual_cost_usd":   actualCostUSD,
		"baseline_cost_usd": baselineCostUSD,
		"saved_usd":         savedUSD,
		"classifier_p50_ms": p50MS,
		"classifier_p95_ms": p95MS,
		"tiers":             tiersOut,
		"causes":            byCause,
		"models":            modelsOut,
	}, nil
}
