package main

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	arenaRoutingMinSamples = 100
	arenaRoutingMaxAge     = 7 * 24 * time.Hour
)

type arenaRoutingEvidence struct {
	Status                      string   `json:"status"`
	Identity                    string   `json:"identity,omitempty"`
	Thinking                    string   `json:"effective_thinking,omitempty"`
	SourceModel                 string   `json:"source_model,omitempty"`
	Rank                        int      `json:"rank,omitempty"`
	Score                       *float64 `json:"score,omitempty"`
	Lower                       *float64 `json:"lower,omitempty"`
	Upper                       *float64 `json:"upper,omitempty"`
	Samples                     *int64   `json:"samples,omitempty"`
	QualityGroup                *int     `json:"quality_group,omitempty"`
	ChannelCalls                int64    `json:"channel_calls,omitempty"`
	ChannelCount                int      `json:"primary_channel_count,omitempty"`
	RecentCalls                 int64    `json:"recent_service_calls,omitempty"`
	RecentService               *float64 `json:"recent_service_success_ewma,omitempty"`
	Unreliable                  bool     `json:"channel_unreliable,omitempty"`
	ServiceSuccess              *float64 `json:"service_success_rate,omitempty"` // transport outcomes, NOT task accuracy
	ResponseMS                  *float64 `json:"channel_response_ms,omitempty"`
	ResponseKind                string   `json:"channel_response_kind,omitempty"`
	PriceKnown                  bool     `json:"price_known"`
	PriceSource                 string   `json:"price_source,omitempty"`
	ComparisonCost              *float64 `json:"comparison_cost_usd,omitempty"`
	CostOutputLimit             int      `json:"cost_reference_output_tokens,omitempty"`
	VersionReference            bool     `json:"version_reference,omitempty"`
	VersionReferenceNote        string   `json:"version_reference_note,omitempty"`
	ReferenceThinkingUnverified bool     `json:"reference_thinking_unverified,omitempty"`
}

type arenaRoutingDecision struct {
	Mode                 string   `json:"mode"`
	Board                string   `json:"board,omitempty"`
	Config               string   `json:"config,omitempty"`
	PublishedAt          string   `json:"published_at,omitempty"`
	FetchedAt            string   `json:"fetched_at,omitempty"`
	SourceURL            string   `json:"source_url,omitempty"`
	ReferenceOnly        bool     `json:"general_reference_only,omitempty"`
	FloorPolicy          string   `json:"floor_policy,omitempty"`
	QualityFloor         *float64 `json:"quality_floor,omitempty"`
	BaselineModel        string   `json:"baseline_model,omitempty"`
	BaselineSource       string   `json:"baseline_source,omitempty"`
	ProposedModel        string   `json:"proposed_model,omitempty"`
	ActualModel          string   `json:"actual_model,omitempty"`
	Different            bool     `json:"different"`
	Reason               string   `json:"reason,omitempty"`
	VersionReference     bool     `json:"version_reference,omitempty"`
	VersionReferenceNote string   `json:"version_reference_note,omitempty"`
}

type arenaRoutingOptions struct {
	Mode, Strategy, DefaultModel, PreviousModel, UserSub string
	Format                                               clientFormat
	NativeOnly, BanditEnabled                            bool
	TierThinking                                         map[string]string
	Targets                                              map[string]userRouteTarget
}

type arenaRoutePlan struct {
	model, tier, wire             string
	evidence                      *arenaRoutingEvidence
	trusted, unreliable, cacheHit bool
}

func normalizeArenaRoutingMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return "live", nil
	}
	switch mode {
	case "off", "shadow", "live":
		return mode, nil
	default:
		return "", errors.New("真实榜单路由模式仅支持 off、shadow 或 live")
	}
}

func arenaRequestBoard(demand TaskDemandProfile, cr *canonReq) (string, bool) {
	switch demand.PrimaryDomain {
	case DomainFrontendUI:
		return "webdev", false
	case DomainAgenticTools:
		return "agent", false // Agent IPS is an agent-task prior, never a BFCL accuracy score.
	case DomainLongContextMM:
		if cr != nil {
			for _, message := range cr.Messages {
				for _, part := range message.Parts {
					if part.Kind == "image" || part.Kind == "video" {
						return "vision", false
					}
				}
			}
		}
		// Document understanding is only a weak reference for arbitrary long-context requests.
		return "document", true
	case DomainCodingSWE, DomainReasoningMath:
		return "text", true // Do not manufacture SWE/AIME scores from an overall text rating.
	default:
		return "text", false
	}
}

func arenaIdentityForChannel(up *Upstream, model, tier, cause, policy string, cr *canonReq) (identity, wire, status string) {
	wire = up.mapModel(resolveUpstreamCandidateModel(up, model))
	plan := resolveAutoRouterThinking(up, wire, tier, model, policy, cause, cr)
	base := parseRouterModelSpec(wire).BaseModel
	if base == "" {
		base = wire
	}
	if version, official := arenaOfficialDeepSeekVersion(up, base); official {
		base = version
	}
	switch {
	case plan.StripAll:
		if spec := parseRouterModelSpec(model); spec.Thinking != "" && spec.Thinking != "none" && spec.Thinking != "auto" {
			return "", wire, "unsupported_thinking_variant"
		}
		identity = base
	case plan.ThinkingBudget > 0:
		// A token budget is not an experimentally established equivalent of "(High)".
		return "", wire, "fixed_budget_variant_unknown"
	case plan.DisableExplicit || plan.Effort == "none" || plan.ThinkingMode == "disabled":
		identity = base + "(none)"
	case plan.Effort != "":
		suffix := "(" + plan.Effort + ")"
		identity = base
		if !strings.HasSuffix(arenaModelIdentity(base), "-"+arenaModelIdentity(suffix)) {
			identity += suffix
		}
	case plan.ThinkingMode != "":
		return "", wire, "thinking_variant_unknown"
	default:
		identity = base
	}
	return arenaVersionedIdentity(identity), wire, ""
}

func arenaCandidateChannels(model string, opts arenaRoutingOptions) []*Upstream {
	target, hasTarget := opts.Targets[model]
	if !hasTarget {
		target.AllowSite = true
	}
	var channels []*Upstream
	add := func(up Upstream) {
		if !up.Enabled || up.Weight <= 0 || !servesRouterCandidate(&up, model) ||
			(opts.NativeOnly && !supportsNativeEndpoint(&up, opts.Format)) {
			return
		}
		wire := up.mapModel(resolveUpstreamCandidateModel(&up, model))
		if up.availableForRelay(wire) {
			copy := up
			channels = append(channels, &copy)
		}
	}
	if target.AllowBYOK && opts.UserSub != "" {
		if personal, err := getUserUpstreams(opts.UserSub); err == nil {
			for _, up := range personal {
				add(up)
			}
		}
	}
	if target.AllowSite {
		if snapshot, err := getRoutingSnapshot(); err == nil {
			for _, up := range snapshot.upstreams {
				add(up)
			}
		}
	}
	// Prefer measurements from the primary channel type; fallback identities are
	// still checked below, so a mixed-model alias cannot borrow the best rating.
	sort.SliceStable(channels, func(i, j int) bool {
		if (channels[i].ID < 0) != (channels[j].ID < 0) {
			return (channels[i].ID < 0) == target.PreferBYOK
		}
		if channels[i].Priority != channels[j].Priority {
			if channels[i].ID < 0 {
				return channels[i].Priority > channels[j].Priority
			}
			return channels[i].Priority < channels[j].Priority
		}
		return channels[i].ID < channels[j].ID
	})
	return channels
}

func fillArenaChannelEvidence(plan *arenaRoutePlan, channels []*Upstream, opts arenaRoutingOptions, cr *canonReq, demand TaskDemandProfile) {
	if len(channels) == 0 {
		return
	}
	up := channels[0]
	stream := cr != nil && cr.Stream
	plan.evidence.ResponseKind = "nonstream_total"
	if stream {
		plan.evidence.ResponseKind = "stream_first_output"
	}
	if opts.BanditEnabled {
		// Never reuse a global/other owner's arm as evidence for this channel.
		var successes, calls int64
		var weightedRate, weightedTTFT, totalWeight float64
		allObserved, allRecent, allLatency := true, true, true
		autoRouterBandit.RLock()
		for _, channel := range channels {
			if (channel.ID < 0) != (up.ID < 0) || channel.Priority != up.Priority {
				continue
			}
			plan.evidence.ChannelCount++
			var stats banditArmStats
			if arm := autoRouterBandit.arms[banditArmKey(channel.ID, plan.model, demand.PrimaryDomain)]; arm != nil {
				stats = *arm
			}
			weight := float64(channel.Weight)
			totalWeight += weight
			last, parseErr := time.Parse(time.RFC3339, stats.LastTransportAt)
			recent := parseErr == nil && stats.TransportSamples > 0 && time.Since(last) <= banditTransportWindow && !last.After(time.Now().Add(time.Minute))
			if !recent {
				allObserved, allRecent, allLatency = false, false, false
				continue
			}
			successes, calls = successes+stats.TransportSuccesses, calls+stats.TransportSamples
			plan.evidence.RecentCalls += stats.TransportSamples
			weightedRate += weight * clampRange(stats.TransportEWMA, 0, 1)
			allObserved = allObserved && stats.TransportSamples >= 5
			responseMS, responseAt := stats.TransportResponseMS, stats.LastResponseAt
			if stream {
				responseMS, responseAt = stats.TransportTTFTMS, stats.LastStreamAt
			}
			observedAt, responseErr := time.Parse(time.RFC3339, responseAt)
			if responseMS > 0 && responseErr == nil && time.Since(observedAt) <= banditTransportWindow && !observedAt.After(time.Now().Add(time.Minute)) {
				weightedTTFT += weight * responseMS
			} else {
				allLatency = false
			}
		}
		autoRouterBandit.RUnlock()
		plan.evidence.ChannelCalls = calls
		if calls > 0 {
			rate := float64(successes) / float64(calls)
			plan.evidence.ServiceSuccess = &rate
		}
		plan.unreliable = allObserved && allRecent && totalWeight > 0 && weightedRate/totalWeight < 0.5
		plan.evidence.Unreliable = plan.unreliable
		if allRecent && totalWeight > 0 {
			rate := weightedRate / totalWeight
			plan.evidence.RecentService = &rate
		}
		if allLatency && allRecent && totalWeight > 0 && !math.IsInf(weightedTTFT, 0) && !math.IsNaN(weightedTTFT) {
			ttft := weightedTTFT / totalWeight
			plan.evidence.ResponseMS = &ttft
		}
	}
	if up.ID < 0 {
		plan.evidence.PriceSource = "byok_cost_unknown"
		return // Gateway billing is not the user's BYOK provider bill.
	}
	price, source, _ := lookupRelayPrice(parseRouterModelSpec(plan.model).BaseModel, plan.wire)
	plan.evidence.PriceSource = source
	if source == "none" || price.In < 0 || price.Out < 0 || math.IsNaN(price.In+price.Out) || math.IsInf(price.In+price.Out, 0) {
		return
	}
	output := 1024
	if cr != nil && cr.MaxTokens > 0 {
		output = cr.MaxTokens
	}
	input := 0
	if cr != nil {
		input = estimateRequestContextTokens(cr)
	}
	cost := (float64(max(input, 0))*price.In + float64(output)*price.Out) / 1e6
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return
	}
	plan.evidence.PriceKnown, plan.evidence.ComparisonCost = true, &cost
	plan.evidence.CostOutputLimit = output
	// No speculative prompt-cache discount or guessed model speed is applied.
}

func prepareArenaPlans(d *routerDecisionDetails, models []string, opts arenaRoutingOptions, cr *canonReq, demand TaskDemandProfile) []*arenaRoutePlan {
	steps := make(map[string]*routerCandidateStep)
	for i := range d.Steps {
		steps[d.Steps[i].Model] = &d.Steps[i]
	}
	plans := make([]*arenaRoutePlan, 0, len(models))
	for _, model := range models {
		tier := d.Tier
		if step := steps[model]; step != nil && step.SourceTier != "" {
			tier = step.SourceTier
		}
		plan := &arenaRoutePlan{model: model, tier: tier, evidence: &arenaRoutingEvidence{Status: "model_unmatched"},
			cacheHit: d.EstimatedInputTokens >= 3000 && opts.PreviousModel != "" && model == opts.PreviousModel}
		if step := steps[model]; step != nil && (step.Status == "skipped" || step.Status == "failed") {
			// Existing last-resort compression/availability fallbacks remain
			// intact, but a failed precheck must not acquire a benchmark endorsement.
			plan.evidence.Status = "precheck_fallback_no_benchmark"
			step.Arena = plan.evidence
			plans = append(plans, plan)
			continue
		}
		channels := arenaCandidateChannels(model, opts)
		if len(channels) == 0 {
			plan.evidence.Status = "no_eligible_channel"
		} else {
			wire := channels[0].mapModel(resolveUpstreamCandidateModel(channels[0], model))
			thinking := resolveAutoRouterThinking(channels[0], wire, tier, model, opts.TierThinking[tier], d.Cause, cr)
			plan.evidence.Thinking = thinking.EffectiveLabel
			if opts.Mode == "live" {
				if step := steps[model]; step != nil {
					step.Thinking = thinking.EffectiveLabel
				}
			}
		}
		for _, up := range channels {
			id, wire, status := arenaIdentityForChannel(up, model, tier, d.Cause, opts.TierThinking[tier], cr)
			if status != "" {
				plan.evidence.Status, plan.evidence.Identity = status, ""
				break
			}
			if plan.evidence.Identity != "" && plan.evidence.Identity != id {
				plan.evidence.Status, plan.evidence.Identity = "ambiguous_upstream_mapping", ""
				break
			}
			plan.evidence.Identity, plan.wire = id, wire
		}
		fillArenaChannelEvidence(plan, channels, opts, cr, demand)
		if step := steps[model]; step != nil {
			step.Arena = plan.evidence
		}
		plans = append(plans, plan)
	}
	return plans
}

func arenaUsableSnapshot(snapshot *arenaSnapshot, now time.Time) bool {
	if snapshot == nil {
		return false
	}
	published, err := time.ParseInLocation("2006-01-02", snapshot.PublishedAt, beijingLocation)
	if err != nil || published.After(now) || now.Sub(published) > arenaRoutingMaxAge {
		return false
	}
	fetched, err := time.Parse(time.RFC3339, snapshot.FetchedAt)
	return err == nil && !fetched.After(now.Add(5*time.Minute)) && now.Sub(fetched) <= arenaRoutingMaxAge
}

func assignArenaEvidence(plans []*arenaRoutePlan, snapshot *arenaSnapshot, now time.Time) int {
	usable := arenaUsableSnapshot(snapshot, now)
	index := make(map[string][]arenaModel)
	if usable {
		for _, model := range snapshot.Models {
			key := arenaVersionedIdentity(model.Model)
			index[key] = append(index[key], model)
		}
	}
	matched := 0
	for _, plan := range plans {
		plan.trusted = false
		e := plan.evidence
		e.SourceModel, e.Rank, e.Score, e.Lower, e.Upper, e.Samples = "", 0, nil, nil, nil, nil
		e.VersionReference, e.ReferenceThinkingUnverified, e.VersionReferenceNote = false, false, ""
		if e.Identity == "" {
			continue
		}
		e.Status = "model_unmatched"
		if !usable {
			e.Status = "benchmark_unavailable_or_outdated"
			continue
		}
		entries := index[e.Identity]
		if len(entries) > 1 {
			e.Status = "ambiguous_source_model"
			continue
		}
		var row arenaModel
		if len(entries) == 0 {
			reference, differentThinking := arenaOlderVersionReference(e.Identity, snapshot.Models, now)
			if reference == nil {
				continue
			}
			row = *reference
			e.VersionReference, e.ReferenceThinkingUnverified = true, differentThinking
			e.VersionReferenceNote = arenaVersionReferenceNote(row, differentThinking)
		} else {
			row = entries[0]
		}
		value := row.Score
		e.SourceModel, e.Rank, e.Score, e.Lower, e.Upper, e.Samples = row.Model, row.Rank, &value, row.RatingLower, row.RatingUpper, row.Votes
		e.Status = "insufficient_samples"
		if row.Votes != nil && *row.Votes >= arenaRoutingMinSamples {
			e.Status, plan.trusted = "matched", true
			if e.VersionReference {
				e.Status = "version_reference"
			}
			matched++
		}
	}
	return matched
}

func arenaTaskQualityFloor(snapshot *arenaSnapshot, demand TaskDemandProfile, strategy string) (*float64, string) {
	if snapshot == nil || normalizeStrategyProfile(strategy) == StrategyQualityFirst || demand.RequiredDifficulty > 68 {
		return nil, "leader_confidence_band"
	}
	quantile, policy := 0.75, "source_upper_quartile_for_medium"
	if demand.RequiredDifficulty <= 38 {
		quantile, policy = 0.5, "source_median_for_simple"
	}
	var scores []float64
	for _, model := range snapshot.Models {
		if model.Votes != nil && *model.Votes >= arenaRoutingMinSamples {
			scores = append(scores, model.Score)
		}
	}
	if len(scores) == 0 {
		return nil, "leader_confidence_band"
	}
	sort.Float64s(scores)
	floor := scores[int(math.Ceil(quantile*float64(len(scores))))-1]
	return &floor, policy
}

func arenaSameQualityBand(anchor, candidate *arenaRoutePlan) bool {
	a, b := anchor.evidence, candidate.evidence
	if a.Rank == b.Rank || *a.Score == *b.Score {
		return true
	}
	// Anchor bands, rather than pairwise sorting by CI overlap, avoid a
	// non-transitive comparator and prevent a wide low-scoring CI bridging bands.
	return a.Lower != nil && a.Upper != nil && b.Lower != nil && b.Upper != nil &&
		*b.Score >= *a.Lower && *b.Upper >= *a.Lower && *b.Lower <= *a.Upper
}

func arenaScoreLess(a, b *arenaRoutePlan) bool {
	if *a.evidence.Score != *b.evidence.Score {
		return *a.evidence.Score > *b.evidence.Score
	}
	if a.evidence.Rank != b.evidence.Rank {
		return a.evidence.Rank < b.evidence.Rank
	}
	// Group an identical source deterministically before its version tie-break,
	// keeping the comparator transitive when unrelated rows share score/rank.
	if a.evidence.SourceModel != b.evidence.SourceModel {
		return a.evidence.SourceModel < b.evidence.SourceModel
	}
	// A deliberately small preference: no made-up Elo/IPS increment and no
	// leap over any measured higher score. It only breaks a shared-source tie.
	if a.evidence.VersionReference != b.evidence.VersionReference {
		return a.evidence.VersionReference
	}
	return a.model < b.model
}

func arenaMetricLess(a, b *float64) (less, different bool) {
	if (a == nil) != (b == nil) {
		return a != nil, true
	}
	if a != nil && *a != *b {
		return *a < *b, true
	}
	return false, false
}

func arenaPeerLess(a, b *arenaRoutePlan, strategy string) bool {
	switch normalizeStrategyProfile(strategy) {
	case StrategyCostFirst:
		if less, different := arenaMetricLess(a.evidence.ComparisonCost, b.evidence.ComparisonCost); different {
			return less
		}
	case StrategyLatencyFirst:
		if less, different := arenaMetricLess(a.evidence.ResponseMS, b.evidence.ResponseMS); different {
			return less
		}
	default:
		if normalizeStrategyProfile(strategy) == StrategyBalanced && a.cacheHit != b.cacheHit {
			return a.cacheHit
		}
	}
	return arenaScoreLess(a, b)
}

func arenaDominates(a, b *arenaRoutePlan) bool {
	better, comparable := false, false
	for _, pair := range [][2]*float64{
		{a.evidence.ComparisonCost, b.evidence.ComparisonCost},
		{a.evidence.ResponseMS, b.evidence.ResponseMS},
	} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return false // Missing costs/latencies are unknown, not zero/free/fast.
		}
		if pair[0] != nil {
			comparable = true
			if *pair[0] > *pair[1] {
				return false
			}
			better = better || *pair[0] < *pair[1]
		}
	}
	return comparable && better
}

func orderArenaPeers(peers []*arenaRoutePlan, strategy string) []*arenaRoutePlan {
	if normalizeStrategyProfile(strategy) != StrategyBalanced {
		sort.SliceStable(peers, func(i, j int) bool { return arenaPeerLess(peers[i], peers[j], strategy) })
		return peers
	}
	// Pareto fronts, not a made-up weighted sum of Elo, IPS, dollars and HTTP 200s.
	var ordered []*arenaRoutePlan
	for len(peers) > 0 {
		var front, rest []*arenaRoutePlan
		for _, candidate := range peers {
			dominated := false
			for _, other := range peers {
				if other != candidate && arenaDominates(other, candidate) {
					dominated = true
					break
				}
			}
			if dominated {
				rest = append(rest, candidate)
			} else {
				front = append(front, candidate)
			}
		}
		sort.SliceStable(front, func(i, j int) bool { return arenaPeerLess(front[i], front[j], strategy) })
		ordered, peers = append(ordered, front...), rest
	}
	return ordered
}

func orderArenaSegment(plans []*arenaRoutePlan, opts arenaRoutingOptions, floor *float64) []*arenaRoutePlan {
	var ordered []*arenaRoutePlan
	group := 0
	for _, unhealthy := range []bool{false, true} {
		var ranked, unknown []*arenaRoutePlan
		for _, plan := range plans {
			if plan.unreliable != unhealthy {
				continue
			}
			if plan.trusted {
				ranked = append(ranked, plan)
			} else {
				unknown = append(unknown, plan)
			}
		}
		sort.SliceStable(ranked, func(i, j int) bool { return arenaScoreLess(ranked[i], ranked[j]) })
		first := true
		for len(ranked) > 0 {
			anchor := ranked[0]
			var peers, rest []*arenaRoutePlan
			for _, plan := range ranked {
				accepted := arenaSameQualityBand(anchor, plan)
				if first && floor != nil && *anchor.evidence.Score >= *floor {
					accepted = *plan.evidence.Score >= *floor
				}
				if accepted {
					peers = append(peers, plan)
				} else {
					rest = append(rest, plan)
				}
			}
			for _, plan := range peers {
				g := group
				plan.evidence.QualityGroup = &g
			}
			ordered = append(ordered, orderArenaPeers(peers, opts.Strategy)...)
			ranked, group, first = rest, group+1, false
		}
		sort.SliceStable(unknown, func(i, j int) bool {
			a, b := unknown[i].model, unknown[j].model
			if (a == opts.DefaultModel) != (b == opts.DefaultModel) {
				return a == opts.DefaultModel
			}
			return a < b
		})
		ordered = append(ordered, unknown...)
	}
	return ordered
}

// Reorders only already-admitted candidates. No HTTP, no extra model calls, and
// no bypass of the existing permission/quota/protocol/modality/context checks.
func applyArenaRoutePolicy(d *routerDecisionDetails, models []string, opts arenaRoutingOptions, cr *canonReq, demand TaskDemandProfile) []string {
	mode, err := normalizeArenaRoutingMode(opts.Mode)
	if err != nil || mode == "off" || d == nil || len(models) == 0 {
		return models
	}
	opts.Mode = mode
	plans := prepareArenaPlans(d, models, opts, cr, demand)
	boardID, weak := arenaRequestBoard(demand, cr)
	now := time.Now()
	source, _, _ := getArenaSnapshot(db, boardID)
	matched := assignArenaEvidence(plans, source, now)
	if matched == 0 && boardID != "text" {
		boardID, weak = "text", true
		source, _, _ = getArenaSnapshot(db, boardID)
		matched = assignArenaEvidence(plans, source, now)
	}
	floor, floorPolicy := arenaTaskQualityFloor(source, demand, opts.Strategy)
	if !arenaUsableSnapshot(source, now) {
		floor = nil
	}
	board, _ := findArenaBoard(boardID)
	evaluation := &arenaRoutingDecision{Mode: mode, Board: boardID, Config: board.Config, SourceURL: arenaDatasetURL,
		ReferenceOnly: weak, FloorPolicy: floorPolicy, QualityFloor: floor, BaselineModel: models[0], ActualModel: models[0], BaselineSource: "legacy_router"}
	if mode == "live" {
		evaluation.BaselineSource = "configured_candidates"
	}
	if source != nil {
		evaluation.PublishedAt = source.PublishedAt
		evaluation.FetchedAt = source.FetchedAt
	}
	ordered := orderArenaSegment(plans, opts, floor)
	// The first model follows the requested cost/speed strategy. If it fails,
	// prefer the strongest remaining member of that accepted quality band.
	if len(ordered) > 1 && ordered[0].trusted && ordered[0].evidence.QualityGroup != nil {
		end := 1
		for end < len(ordered) && ordered[end].trusted && ordered[end].evidence.QualityGroup != nil &&
			*ordered[end].evidence.QualityGroup == *ordered[0].evidence.QualityGroup {
			end++
		}
		sort.SliceStable(ordered[1:end], func(i, j int) bool { return arenaScoreLess(ordered[i+1], ordered[j+1]) })
	}
	evaluation.Reason = "source_quality_then_strategy"
	if matched == 0 {
		evaluation.Reason = "no_trusted_benchmark_configured_fallback"
		if len(plans) == 1 && plans[0].evidence.Status == "precheck_fallback_no_benchmark" {
			evaluation.Reason = "existing_precheck_fallback_not_reranked"
		}
	}
	if strings.HasPrefix(d.Cause, "session_affinity") {
		for i, plan := range ordered {
			if plan.model == d.SelectedModel {
				copy(ordered[1:i+1], ordered[:i])
				ordered[0] = plan
				evaluation.Reason = "session_affinity_preserved"
				break
			}
		}
	}
	proposed := make([]string, 0, len(ordered))
	for _, plan := range ordered {
		proposed = append(proposed, plan.model)
	}
	evaluation.ProposedModel, evaluation.Different = proposed[0], proposed[0] != evaluation.BaselineModel
	if ordered[0].evidence.VersionReference {
		evaluation.VersionReference = true
		evaluation.VersionReferenceNote = ordered[0].evidence.VersionReferenceNote
		evaluation.Reason += "+version_reference_minor_preference"
	}
	d.Arena = evaluation
	if mode == "shadow" {
		return models
	}
	evaluation.ActualModel = proposed[0]
	priorities := make(map[string]int)
	for i, model := range proposed {
		priorities[model] = i + 1
	}
	for i := range d.Steps {
		if priority, ok := priorities[d.Steps[i].Model]; ok {
			d.Steps[i].Priority = priority
			d.Steps[i].Status = "standby"
			if priority == 1 {
				d.Steps[i].Status = "selected"
				d.SelectedThinking = d.Steps[i].Thinking
			}
		}
	}
	sort.SliceStable(d.Steps, func(i, j int) bool {
		a, aok := priorities[d.Steps[i].Model]
		b, bok := priorities[d.Steps[j].Model]
		if aok != bok {
			return aok
		}
		if aok {
			return a < b
		}
		return d.Steps[i].Priority < d.Steps[j].Priority
	})
	for i := range d.Steps {
		d.Steps[i].Priority = i + 1
	}
	d.Cause += "+arena_live"
	return proposed
}

func arenaRoutingHeaders(d *routerDecisionDetails) map[string]string {
	if d == nil || d.Arena == nil {
		return nil
	}
	headers := map[string]string{"X-APISHARE-Router-Arena-Mode": d.Arena.Mode, "X-APISHARE-Router-Arena-Board": d.Arena.Board}
	headers["X-APISHARE-Router-Arena-Different"] = strconv.FormatBool(d.Arena.Different)
	return headers
}

func arenaAllowsSpeculativeProbe(d *routerDecisionDetails, first, second string) bool {
	if d == nil || d.Arena == nil || d.Arena.Mode != "live" {
		return true
	}
	var a, b *arenaRoutingEvidence
	for _, step := range d.Steps {
		if step.Model == first {
			a = step.Arena
		}
		if step.Model == second {
			b = step.Arena
		}
	}
	return a != nil && b != nil && a.Status == "matched" && b.Status == "matched" &&
		!b.Unreliable && a.Score != nil && b.Score != nil && *b.Score > *a.Score
}
