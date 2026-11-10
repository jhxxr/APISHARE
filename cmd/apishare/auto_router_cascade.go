package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const defaultSpeculativeProbeRunes = 96

var (
	speculativeSelfEscalateRe = regexp.MustCompile(`(?i)(\[ESCALATE\]|<escalate>|\[NEED_STRONGER_MODEL\]|\[UNCERTAIN\])`)

	speculativeRefusalPrefixRe = regexp.MustCompile(`(?i)^\s*(i'm sorry,?\s*i (cannot|can't)|i am sorry,?\s*i (cannot|can't)|sorry,?\s*([^.!\n]{0,40})?\bi (cannot|can't) (help|assist|solve|answer|complete|fulfill)|i cannot (fulfill|complete|solve) this|this (task|request|problem) is beyond my capabilities|as an? [a-z0-9 -]{1,30} model,?\s*i (cannot|can't)|抱歉[，,]?\s*([^。！!\n]{0,25})?我(无法|不能)|对不起[，,]?\s*([^。！!\n]{0,25})?我(无法|不能)|这超出了我的能力|我无法(完成|解决|回答|处理)(该|这个|此)|作为一个[^。！!\n]{0,20}模型[，,]?\s*我(无法|不能))`)
)

// shouldEnableSpeculativeProbe determines whether Candidate #1 (fast/cost-effective)
// should run with a Speculative Cascade pre-commit inspection buffer so that if it
// refuses, stalls, or emits an uncertainty marker, the gateway can transparently
// escalate to Candidate #2 (stronger flagship model) before flushing any bytes to the client.
func shouldEnableSpeculativeProbe(
	enabled bool,
	candidates []string,
	demand TaskDemandProfile,
	customCaps map[string]ModelCapabilityVector,
) bool {
	if !enabled || len(candidates) < 2 {
		return false
	}
	fast := candidates[0]
	strong := candidates[1]
	if strings.EqualFold(parseRouterModelSpec(fast).BaseModel, parseRouterModelSpec(strong).BaseModel) {
		return false
	}
	return true
}

// shouldEscalateSpeculativeOutput inspects the initial buffered text of a speculative
// probe candidate before committing the HTTP response to the downstream client.
func shouldEscalateSpeculativeOutput(text string, hasToolCall bool, streamComplete bool) (bool, string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		if streamComplete && !hasToolCall {
			return true, "empty_response"
		}
		return false, ""
	}
	head := trimmed
	if utf8.RuneCountInString(head) > 200 {
		r := []rune(head)
		head = string(r[:200])
	}
	if speculativeSelfEscalateRe.MatchString(head) {
		return true, "self_escalate_marker"
	}
	if speculativeRefusalPrefixRe.MatchString(head) {
		return true, "refusal_or_punt"
	}
	return false, ""
}
