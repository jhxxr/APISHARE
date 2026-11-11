package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StrategyBalanced     = "balanced"
	StrategyQualityFirst = "quality_first"
	StrategyCostFirst    = "cost_first"
	StrategyLatencyFirst = "latency_first"

	banditSettingsKey     = "auto_router_bandit_json"
	banditDecayFactor     = 0.992 // Sliding-window exponential decay for non-stationary upstream quality
	banditTransportWindow = 10 * time.Minute
)

// banditArmStats tracks online Thompson Sampling / Sliding-Window UCB statistics
// for a (UpstreamID, BaseModel, Domain) arm.
type banditArmStats struct {
	Key                    string  `json:"key"`
	UpstreamID             int64   `json:"upstream_id"`
	Model                  string  `json:"model"`
	Domain                 string  `json:"domain"`
	Alpha                  float64 `json:"alpha"`
	Beta                   float64 `json:"beta"`
	Calls                  int64   `json:"calls"`
	Successes              int64   `json:"successes"`
	Failures               int64   `json:"failures"`
	ImplicitPositive       int64   `json:"implicit_positive"`
	ImplicitNegative       int64   `json:"implicit_negative"`
	SpeculativeEscalations int64   `json:"speculative_escalations"`
	EWMATTFTMS             float64 `json:"ewma_ttft_ms"`
	EWMATPS                float64 `json:"ewma_tps"`
	UpdatedAt              string  `json:"updated_at"`
	TransportSamples       int64   `json:"transport_samples,omitempty"`
	TransportSuccesses     int64   `json:"transport_successes,omitempty"`
	TransportEWMA          float64 `json:"transport_success_ewma,omitempty"`
	TransportTTFTMS        float64 `json:"transport_ttft_ms,omitempty"`
	TransportResponseMS    float64 `json:"transport_response_ms,omitempty"`
	LastStreamAt           string  `json:"last_stream_at,omitempty"`
	LastResponseAt         string  `json:"last_response_at,omitempty"`
	LastTransportAt        string  `json:"last_transport_at,omitempty"`
}

func (s banditArmStats) PosteriorMean() float64 {
	a := math.Max(s.Alpha, 0.5)
	b := math.Max(s.Beta, 0.5)
	return a / (a + b)
}

func (s banditArmStats) UCBScore(totalDomainCalls int64) float64 {
	mean := s.PosteriorMean()
	effectiveN := s.Alpha + s.Beta
	if effectiveN < 1.0 {
		effectiveN = 1.0
	}
	total := float64(totalDomainCalls) + 4.0
	bonus := 0.14 * math.Sqrt(math.Log(total)/effectiveN)
	if bonus > 0.22 {
		bonus = 0.22
	}
	return clampRange(mean+bonus, 0.05, 1.0)
}

// SampleThompson draws a random sample from Beta(alpha, beta) using Gamma variates,
// blended with the posterior mean for stability when sample size is small.
func (s banditArmStats) SampleThompson() float64 {
	a := math.Max(s.Alpha, 0.8)
	b := math.Max(s.Beta, 0.8)
	x := sampleGamma(a)
	y := sampleGamma(b)
	if x+y <= 0 {
		return s.PosteriorMean()
	}
	sample := x / (x + y)
	// Blend 65% posterior mean + 35% Thompson sample to prevent wild cold-start jitter
	return clampRange(0.65*s.PosteriorMean()+0.35*sample, 0.05, 1.0)
}

func sampleGamma(alpha float64) float64 {
	if alpha < 1.0 {
		u := rand.Float64()
		if u <= 1e-9 {
			u = 1e-9
		}
		return sampleGamma(1.0+alpha) * math.Pow(u, 1.0/alpha)
	}
	d := alpha - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)
	for i := 0; i < 32; i++ {
		x := rand.NormFloat64()
		v := 1.0 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rand.Float64()
		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v
		}
		if u > 0 && math.Log(u) < 0.5*x*x+d*(1.0-v+math.Log(v)) {
			return d * v
		}
	}
	return alpha
}

var autoRouterBandit = struct {
	sync.RWMutex
	arms        map[string]*banditArmStats
	domainCalls map[string]int64
	dirtyWrites int
}{
	arms:        make(map[string]*banditArmStats),
	domainCalls: make(map[string]int64),
}

func resetAutoRouterBandit() {
	autoRouterBandit.Lock()
	autoRouterBandit.arms = make(map[string]*banditArmStats)
	autoRouterBandit.domainCalls = make(map[string]int64)
	autoRouterBandit.dirtyWrites = 0
	autoRouterBandit.Unlock()
}

func loadAutoRouterBanditCache() {
	resetAutoRouterBandit()
	if db == nil {
		return
	}
	raw, err := settingsGet(banditSettingsKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}
	var list []banditArmStats
	if json.Unmarshal([]byte(raw), &list) != nil {
		return
	}
	autoRouterBandit.Lock()
	defer autoRouterBandit.Unlock()
	autoRouterBandit.arms = make(map[string]*banditArmStats, len(list))
	autoRouterBandit.domainCalls = make(map[string]int64)
	for _, item := range list {
		if item.Key == "" {
			continue
		}
		cp := item
		autoRouterBandit.arms[item.Key] = &cp
		if item.UpstreamID == 0 {
			autoRouterBandit.domainCalls[item.Domain] += item.Calls
		} else if item.UpstreamID < 0 {
			autoRouterBandit.domainCalls[banditDomainKey(item.UpstreamID, item.Domain)] += item.Calls
		}
	}
}

func maybePersistBanditAsync() {
	autoRouterBandit.Lock()
	autoRouterBandit.dirtyWrites++
	if autoRouterBandit.dirtyWrites < 8 || db == nil {
		autoRouterBandit.Unlock()
		return
	}
	autoRouterBandit.dirtyWrites = 0
	list := make([]banditArmStats, 0, len(autoRouterBandit.arms))
	for _, arm := range autoRouterBandit.arms {
		list = append(list, *arm)
	}
	autoRouterBandit.Unlock()

	if blob, err := json.Marshal(list); err == nil {
		_ = settingsSet(banditSettingsKey, string(blob))
	}
}

func banditArmKey(upstreamID int64, model, domain string) string {
	base := protocolModelName(parseRouterModelSpec(model).BaseModel)
	if base == "" {
		base = strings.ToLower(strings.TrimSpace(model))
	}
	if domain == "" {
		domain = "general"
	}
	return fmt.Sprintf("%d:%s:%s", upstreamID, base, domain)
}

// Personal upstream IDs are globally unique negative IDs. Their observations
// must never feed the site's shared model priors or other personal channels.
func banditDomainKey(upstreamID int64, domain string) string {
	if upstreamID < 0 {
		return fmt.Sprintf("%d:%s", upstreamID, domain)
	}
	return domain
}

func getOrInitBanditArmLocked(upstreamID int64, model, domain string) *banditArmStats {
	key := banditArmKey(upstreamID, model, domain)
	if arm, ok := autoRouterBandit.arms[key]; ok {
		return arm
	}
	base := protocolModelName(parseRouterModelSpec(model).BaseModel)
	capVec := resolveModelCapabilityVector(base, nil)
	domainCap := capVec.ScoreForDomain(domain)
	// Warm-start Beta prior from benchmark alignment: higher-capability models start with
	// slightly stronger prior alpha so cold-start routing respects benchmark reality.
	priorAlpha := 2.0 + (domainCap / 50.0) // [2.0, 4.0]
	priorBeta := 1.0
	arm := &banditArmStats{
		Key:        key,
		UpstreamID: upstreamID,
		Model:      base,
		Domain:     domain,
		Alpha:      priorAlpha,
		Beta:       priorBeta,
		UpdatedAt:  nowStr(),
	}
	autoRouterBandit.arms[key] = arm
	return arm
}

type banditTransportObservation struct {
	Valid  bool
	Stream bool
}

func updateTransportArmLocked(arm *banditArmStats, success bool, responseMS int64, observation banditTransportObservation) {
	if observation.Valid {
		now := time.Now()
		last, parseErr := time.Parse(time.RFC3339, arm.LastTransportAt)
		if parseErr != nil || now.Sub(last) > banditTransportWindow || last.After(now.Add(time.Minute)) {
			arm.TransportSamples, arm.TransportEWMA, arm.TransportTTFTMS = 0, 0, 0
			arm.TransportSuccesses, arm.TransportResponseMS, arm.LastStreamAt, arm.LastResponseAt = 0, 0, "", ""
		}
		outcome := 0.0
		if success {
			outcome = 1
			arm.TransportSuccesses++
		}
		if arm.TransportSamples == 0 {
			arm.TransportEWMA = outcome
		} else {
			arm.TransportEWMA = 0.8*arm.TransportEWMA + 0.2*outcome
		}
		arm.TransportSamples++
		if success && responseMS > 0 {
			value, at := &arm.TransportResponseMS, &arm.LastResponseAt
			if observation.Stream {
				value, at = &arm.TransportTTFTMS, &arm.LastStreamAt
			}
			previous, err := time.Parse(time.RFC3339, *at)
			if err != nil || now.Sub(previous) > banditTransportWindow || *value <= 0 {
				*value = float64(responseMS)
			} else {
				*value = 0.75*(*value) + 0.25*float64(responseMS)
			}
			*at = now.UTC().Format(time.RFC3339)
		}
		arm.LastTransportAt = now.UTC().Format(time.RFC3339)
	}
}

func updateSingleArmLocked(arm *banditArmStats, deltaAlpha, deltaBeta float64, ttftMS int64, tps float64, isCall, isSuccess, isFail, isImplicitPos, isImplicitNeg, isSpecEscalate bool) {
	// Apply sliding-window exponential decay to keep α + β responsive to live changes
	arm.Alpha = math.Max(0.8, arm.Alpha*banditDecayFactor) + deltaAlpha
	arm.Beta = math.Max(0.5, arm.Beta*banditDecayFactor) + deltaBeta
	if isCall {
		arm.Calls++
	}
	if isSuccess {
		arm.Successes++
	}
	if isFail {
		arm.Failures++
	}
	if isImplicitPos {
		arm.ImplicitPositive++
	}
	if isImplicitNeg {
		arm.ImplicitNegative++
	}
	if isSpecEscalate {
		arm.SpeculativeEscalations++
	}
	if ttftMS > 0 {
		if arm.EWMATTFTMS <= 0 {
			arm.EWMATTFTMS = float64(ttftMS)
		} else {
			arm.EWMATTFTMS = 0.75*arm.EWMATTFTMS + 0.25*float64(ttftMS)
		}
	}
	if tps > 0 {
		if arm.EWMATPS <= 0 {
			arm.EWMATPS = tps
		} else {
			arm.EWMATPS = 0.75*arm.EWMATPS + 0.25*tps
		}
	}
	arm.UpdatedAt = nowStr()
}

// recordBanditExplicitOutcome updates the Bandit posterior when a relay attempt completes.
func recordBanditExplicitOutcome(upstreamID int64, model, domain string, success bool, firstTokenMS, latencyMS, durationMS int64, completionTokens int, observations ...banditTransportObservation) {
	if strings.TrimSpace(model) == "" {
		return
	}
	if domain == "" {
		domain = DomainWritingKnowledge
	}
	observation := banditTransportObservation{Valid: true, Stream: true}
	if len(observations) > 0 {
		observation = observations[0]
	}
	responseMS := firstTokenMS
	if !observation.Stream {
		responseMS = durationMS
	}
	ttft := firstTokenMS
	if ttft <= 0 && latencyMS > 0 {
		ttft = latencyMS
	}
	var tps float64
	if success && completionTokens > 0 && durationMS > 50 {
		tps = float64(completionTokens) / (float64(durationMS) / 1000.0)
	}
	var dAlpha, dBeta float64
	if success {
		dAlpha = 0.75
		// Speed bonus for fast TTFT (<1.2s)
		if ttft > 0 && ttft < 1200 {
			dAlpha += 0.20
		}
	} else {
		dBeta = 2.0
	}

	autoRouterBandit.Lock()
	autoRouterBandit.domainCalls[banditDomainKey(upstreamID, domain)]++
	if upstreamID != 0 {
		armUp := getOrInitBanditArmLocked(upstreamID, model, domain)
		updateSingleArmLocked(armUp, dAlpha, dBeta, ttft, tps, true, success, !success, false, false, false)
		updateTransportArmLocked(armUp, success, responseMS, observation)
	}
	if upstreamID >= 0 {
		armGlobal := getOrInitBanditArmLocked(0, model, domain)
		updateSingleArmLocked(armGlobal, dAlpha, dBeta, ttft, tps, true, success, !success, false, false, false)
		updateTransportArmLocked(armGlobal, success, responseMS, observation)
	}
	autoRouterBandit.Unlock()

	maybePersistBanditAsync()
}

// recordBanditImplicitFeedback retroactively rewards or penalizes the previous turn's
// (UpstreamID, Model, Domain) arm based on the user's next-turn behavior:
//   - Frustration / Retry ("不对", "还是报错", "wrong", "still failing") -> strong negative penalty
//   - Smooth continuation -> positive reward
func recordBanditImplicitFeedback(upstreamID int64, model, domain string, frustrated bool) {
	if strings.TrimSpace(model) == "" {
		return
	}
	if domain == "" {
		domain = DomainCodingSWE
	}
	var dAlpha, dBeta float64
	if frustrated {
		dBeta = 1.8
	} else {
		dAlpha = 0.65
	}
	autoRouterBandit.Lock()
	if upstreamID != 0 {
		armUp := getOrInitBanditArmLocked(upstreamID, model, domain)
		updateSingleArmLocked(armUp, dAlpha, dBeta, 0, 0, false, false, false, !frustrated, frustrated, false)
	}
	if upstreamID >= 0 {
		armGlobal := getOrInitBanditArmLocked(0, model, domain)
		updateSingleArmLocked(armGlobal, dAlpha, dBeta, 0, 0, false, false, false, !frustrated, frustrated, false)
	}
	autoRouterBandit.Unlock()

	maybePersistBanditAsync()
}

// recordBanditSpeculativeEscalation records that a fast probe model triggered a
// Speculative Cascade escalation (e.g. refusal, TTFT timeout, or malformed tool output).
func recordBanditSpeculativeEscalation(upstreamID int64, model, domain string) {
	if strings.TrimSpace(model) == "" {
		return
	}
	if domain == "" {
		domain = DomainCodingSWE
	}
	autoRouterBandit.Lock()
	autoRouterBandit.domainCalls[banditDomainKey(upstreamID, domain)]++
	if upstreamID != 0 {
		armUp := getOrInitBanditArmLocked(upstreamID, model, domain)
		updateSingleArmLocked(armUp, 0, 1.5, 0, 0, true, false, true, false, false, true)
	}
	if upstreamID >= 0 {
		armGlobal := getOrInitBanditArmLocked(0, model, domain)
		updateSingleArmLocked(armGlobal, 0, 1.5, 0, 0, true, false, true, false, false, true)
	}
	autoRouterBandit.Unlock()
	maybePersistBanditAsync()
}

func lookupBanditArmSummary(upstreamID int64, model, domain string) (banditArmStats, bool) {
	autoRouterBandit.RLock()
	defer autoRouterBandit.RUnlock()
	if upstreamID != 0 {
		if arm, ok := autoRouterBandit.arms[banditArmKey(upstreamID, model, domain)]; ok {
			return *arm, true
		}
	}
	if upstreamID >= 0 {
		if arm, ok := autoRouterBandit.arms[banditArmKey(0, model, domain)]; ok {
			return *arm, true
		}
	}
	return banditArmStats{}, false
}

func snapshotBanditLeaderboard() []banditArmStats {
	autoRouterBandit.RLock()
	list := make([]banditArmStats, 0, len(autoRouterBandit.arms))
	for _, arm := range autoRouterBandit.arms {
		if arm.UpstreamID == 0 && (arm.Calls > 0 || arm.ImplicitNegative > 0 || arm.ImplicitPositive > 0) {
			list = append(list, *arm)
		}
	}
	autoRouterBandit.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Calls != list[j].Calls {
			return list[i].Calls > list[j].Calls
		}
		return list[i].PosteriorMean() > list[j].PosteriorMean()
	})
	if len(list) > 50 {
		list = list[:50]
	}
	return list
}

func normalizeStrategyProfile(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case StrategyQualityFirst, "quality", "best":
		return StrategyQualityFirst
	case StrategyCostFirst, "cost", "cheap", "economy":
		return StrategyCostFirst
	case StrategyLatencyFirst, "latency", "fast", "speed":
		return StrategyLatencyFirst
	default:
		return StrategyBalanced
	}
}

type candidateUtilityBreakdown struct {
	RawModel        string  `json:"raw_model"`
	CapabilityScore float64 `json:"capability_score"`
	BanditWinRate   float64 `json:"bandit_win_rate"`
	SpeedScore      float64 `json:"speed_score"`
	CostScore       float64 `json:"cost_score"`
	UtilityScore    float64 `json:"utility_score"`
}

// scoreCandidateUtility computes the unified multi-objective utility score for a candidate
// model given the request's 6D TaskDemandProfile, strategy profile, Bandit posterior,
// latency EWMA, price, and prompt-cache stickiness.
func scoreCandidateUtility(
	upstreamID int64,
	rawModel string,
	demand TaskDemandProfile,
	strategy string,
	isSessionCacheHit bool,
	estimatedTokens int,
	ctxLimit int,
	customCaps map[string]ModelCapabilityVector,
	banditEnabled bool,
	stochastic bool,
) candidateUtilityBreakdown {
	spec := parseRouterModelSpec(rawModel)
	base := spec.BaseModel
	if base == "" {
		base = rawModel
	}
	capVec := resolveModelCapabilityVector(rawModel, customCaps)
	capMatch := capVec.MatchScore(demand.Weights)

	// 1. Bandit Quality Score [0, 100]
	autoRouterBandit.RLock()
	totalDomain := autoRouterBandit.domainCalls[banditDomainKey(upstreamID, demand.PrimaryDomain)]
	var arm *banditArmStats
	if banditEnabled && upstreamID != 0 {
		arm = autoRouterBandit.arms[banditArmKey(upstreamID, base, demand.PrimaryDomain)]
	}
	if banditEnabled && arm == nil && upstreamID >= 0 {
		arm = autoRouterBandit.arms[banditArmKey(0, base, demand.PrimaryDomain)]
	}
	var banditProb float64
	var ewmaTTFT, ewmaTPS float64
	hasBanditHistory := false
	if arm != nil && (arm.Calls > 0 || arm.ImplicitNegative > 0 || arm.ImplicitPositive > 0) {
		hasBanditHistory = true
		if stochastic {
			banditProb = 0.5*arm.SampleThompson() + 0.5*arm.UCBScore(totalDomain)
		} else {
			banditProb = arm.UCBScore(totalDomain)
		}
		ewmaTTFT = arm.EWMATTFTMS
		ewmaTPS = arm.EWMATPS
	} else {
		// Cold start: neutral-optimistic Bandit win-rate
		banditProb = 0.72 + (capMatch / 500.0)
	}
	autoRouterBandit.RUnlock()

	// 2. Effective Capability Satisfaction vs RequiredDifficulty D(x)
	//    - If CapMatch < D(x): steep deficit penalty so weak models are avoided on hard tasks.
	//    - If CapMatch > D(x) + 12 on simple/medium tasks: diminishing marginal return so
	//      expensive slow flagship models don't unnecessarily hog simple greetings!
	reqDiff := demand.RequiredDifficulty
	var effectiveQual float64
	if capMatch < reqDiff {
		deficit := reqDiff - capMatch
		effectiveQual = capMatch - deficit*1.85
	} else {
		surplus := capMatch - reqDiff
		if reqDiff <= 45.0 && strategy != StrategyQualityFirst {
			// For simple tasks, capability above D(x)+10 has 0.15x marginal weight
			if surplus > 10.0 {
				effectiveQual = reqDiff + 10.0 + (surplus-10.0)*0.15
			} else {
				effectiveQual = capMatch
			}
		} else if reqDiff <= 68.0 && strategy == StrategyCostFirst {
			if surplus > 12.0 {
				effectiveQual = reqDiff + 12.0 + (surplus-12.0)*0.25
			} else {
				effectiveQual = capMatch
			}
		} else {
			effectiveQual = capMatch
		}
	}

	// Blend Benchmark Capability with Bandit Empirical Win-Rate
	// When Bandit has observed live calls/feedback, its weight increases up to 45%!
	banditWeight := 0.22
	if hasBanditHistory {
		banditWeight = 0.42
	}
	combinedQuality := (1.0-banditWeight)*effectiveQual + banditWeight*(banditProb*100.0)

	// 3. Speed Score [0, 100] (combines SpeedPrior with live EWMA TTFT & TPS)
	speedScore := capVec.SpeedPrior
	if ewmaTTFT > 0 {
		// 300ms -> 98, 1000ms -> 82, 3000ms -> 55, 8000ms -> 20
		ttftScore := clamp100(105.0 - 22.0*math.Log10(math.Max(ewmaTTFT, 100.0)/100.0)*2.2)
		speedScore = 0.45*speedScore + 0.55*ttftScore
	}
	if ewmaTPS > 0 {
		tpsScore := clamp100(ewmaTPS * 1.1)
		speedScore = 0.70*speedScore + 0.30*tpsScore
	}

	// 4. Cost Efficiency Score [0, 100] (100 = free/ultra-cheap, 15 = $15+/1M Opus)
	price, _, _ := lookupRelayPrice(base, "")
	inRate := price.In
	if isSessionCacheHit && estimatedTokens >= 2000 {
		// Prompt Cache hit typically costs 10% of base input rate!
		if price.CacheRead != nil && *price.CacheRead > 0 {
			inRate = *price.CacheRead
		} else {
			inRate = price.In * 0.15
		}
	}
	blendedPer1M := 0.70*inRate + 0.30*price.Out
	costScore := 80.0
	if blendedPer1M <= 0.01 {
		costScore = 98.0
	} else {
		// Log-scale cost score: $0.2 -> 92, $1.0 -> 75, $5.0 -> 50, $25.0 -> 22
		costScore = clamp100(75.0 - 34.0*math.Log10(blendedPer1M))
	}

	// 5. Strategy Profile Weights
	var wQual, wCost, wLat float64
	switch normalizeStrategyProfile(strategy) {
	case StrategyQualityFirst:
		wQual, wCost, wLat = 0.78, 0.06, 0.16
	case StrategyCostFirst:
		wQual, wCost, wLat = 0.40, 0.44, 0.16
	case StrategyLatencyFirst:
		wQual, wCost, wLat = 0.42, 0.10, 0.48
	default: // StrategyBalanced
		if reqDiff >= 72.0 {
			// Hard engineering/reasoning tasks automatically prioritize quality over pennies
			wQual, wCost, wLat = 0.68, 0.16, 0.16
		} else if reqDiff <= 38.0 {
			// Simple tasks prioritize speed and low cost
			wQual, wCost, wLat = 0.38, 0.32, 0.30
		} else {
			wQual, wCost, wLat = 0.56, 0.24, 0.20
		}
	}

	utility := wQual*combinedQuality + wCost*costScore + wLat*speedScore

	// 6. Context Sweet-Spot & Session Cache Stickiness Bonuses
	if isSessionCacheHit && estimatedTokens >= 3000 && capMatch >= reqDiff-3.0 {
		utility += 6.5 // Strong incentive to preserve warm KV cache when model is still capable
	}
	if ctxLimit > 0 && estimatedTokens > 4000 {
		utilRatio := float64(estimatedTokens) / float64(ctxLimit)
		if utilRatio > 0.82 {
			// Penalize models entering the >82% Lost-in-the-Middle context degradation zone
			utility -= (utilRatio - 0.82) * 55.0
		}
	}

	return candidateUtilityBreakdown{
		RawModel:        rawModel,
		CapabilityScore: roundOneDecimal(capMatch),
		BanditWinRate:   roundTwoDecimals(banditProb),
		SpeedScore:      roundOneDecimal(speedScore),
		CostScore:       roundOneDecimal(costScore),
		UtilityScore:    roundOneDecimal(utility),
	}
}

func summarizeModelBanditStats(rawModel string) (winRate float64, calls int64, ttftMS float64, tps float64) {
	base := protocolModelName(parseRouterModelSpec(rawModel).BaseModel)
	if base == "" {
		base = strings.ToLower(strings.TrimSpace(rawModel))
	}
	autoRouterBandit.RLock()
	defer autoRouterBandit.RUnlock()

	var alphaDelta, betaDelta float64
	var ttftWeighted, tpsWeighted float64
	var ttftWeight, tpsWeight int64
	found := false
	for _, arm := range autoRouterBandit.arms {
		if arm.UpstreamID != 0 || arm.Model != base {
			continue
		}
		if arm.Calls == 0 && arm.ImplicitNegative == 0 && arm.ImplicitPositive == 0 {
			continue
		}
		found = true
		calls += arm.Calls
		alphaDelta += math.Max(0, arm.Alpha-2.2)
		betaDelta += math.Max(0, arm.Beta-1.0)
		w := arm.Calls
		if w <= 0 {
			w = 1
		}
		if arm.EWMATTFTMS > 0 {
			ttftWeighted += arm.EWMATTFTMS * float64(w)
			ttftWeight += w
		}
		if arm.EWMATPS > 0 {
			tpsWeighted += arm.EWMATPS * float64(w)
			tpsWeight += w
		}
	}
	if !found {
		return 0, 0, 0, 0
	}
	a := 2.2 + alphaDelta
	b := 1.0 + betaDelta
	winRate = roundTwoDecimals(a / (a + b))
	if ttftWeight > 0 {
		ttftMS = roundOneDecimal(ttftWeighted / float64(ttftWeight))
	}
	if tpsWeight > 0 {
		tps = roundOneDecimal(tpsWeighted / float64(tpsWeight))
	}
	return winRate, calls, ttftMS, tps
}

func hasAnyBanditHistoryForModels(models []string, domain string) bool {
	autoRouterBandit.RLock()
	defer autoRouterBandit.RUnlock()
	for _, m := range models {
		base := strings.ToLower(strings.TrimSpace(parseRouterModelSpec(m).BaseModel))
		if base == "" {
			base = strings.ToLower(strings.TrimSpace(m))
		}
		if arm, ok := autoRouterBandit.arms[banditArmKey(0, base, domain)]; ok {
			if arm.Calls >= 3 || arm.ImplicitNegative > 0 || arm.SpeculativeEscalations > 0 {
				return true
			}
		}
	}
	return false
}
