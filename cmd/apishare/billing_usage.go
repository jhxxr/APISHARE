package main

import (
	"math"
)

type billingUsage struct {
	Input                 int  `json:"input"`
	Output                int  `json:"output"`
	CacheRead             int  `json:"cache_read"`
	CacheWrite            int  `json:"cache_write"`
	CacheWrite5m          int  `json:"cache_write_5m"`
	CacheWrite1h          int  `json:"cache_write_1h"`
	CacheWriteUnspecified int  `json:"cache_write_unspecified"`
	Reasoning             int  `json:"reasoning"`
	ToolPrompt            int  `json:"tool_prompt"`
	EstimatedInput        bool `json:"estimated_input"`
	EstimatedOutput       bool `json:"estimated_output"`
	InputAudio            int  `json:"input_audio,omitempty"`
	OutputAudio           int  `json:"output_audio,omitempty"`
	CacheReadAudio        int  `json:"cache_read_audio,omitempty"`
}

type billingCost struct {
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cache_read"`
	CacheWrite     float64 `json:"cache_write"`
	Reasoning      float64 `json:"reasoning"`
	ToolPrompt     float64 `json:"tool_prompt"`
	Total          float64 `json:"total"`
	InputAudio     float64 `json:"input_audio,omitempty"`
	OutputAudio    float64 `json:"output_audio,omitempty"`
	CacheReadAudio float64 `json:"cache_read_audio,omitempty"`
}

type billingDetails struct {
	Source        clientFormat  `json:"source"`
	Reported      upstreamUsage `json:"reported"`
	Usage         billingUsage  `json:"usage"`
	Cost          billingCost   `json:"cost"`
	Price         Price         `json:"price"`
	PriceSource   string        `json:"price_source"`
	PricingModel  string        `json:"pricing_model,omitempty"`
	UpstreamModel string        `json:"upstream_model,omitempty"`
	QuotaPool     string        `json:"quota_pool,omitempty"`
}

func nonnegativeTokens(value int) int {
	if value < 0 {
		return 0
	}
	// Bound malformed counters before integer addition and public SQL sums.
	if value > maxUsageTokens {
		return 0
	}
	return value
}

func cacheWriteBreakdown(usage upstreamUsage) (total, fiveMinutes, oneHour int) {
	fiveMinutes, oneHour = nonnegativeTokens(usage.CacheWrite5m), nonnegativeTokens(usage.CacheWrite1h)
	total = nonnegativeTokens(usage.CacheWrite)
	if !usage.CacheWriteKnown {
		total = fiveMinutes + oneHour
	}
	if fiveMinutes+oneHour > total {
		// Keep the reported ratio while respecting even an explicit zero.
		detailTotal := float64(fiveMinutes) + float64(oneHour)
		fiveMinutes = int(math.Round(float64(total) * float64(fiveMinutes) / detailTotal))
		oneHour = total - fiveMinutes
	}
	// Missing TTL detail uses the ordinary cache-write rate, not a guessed
	// one-hour premium. The aggregate always owns the total quantity.
	return total, fiveMinutes, oneHour
}

// Partition inclusive and exclusive provider counters into disjoint billing
// categories. Estimated counts cover the whole transcript, not just uncached
// input or visible text; retain known subcategories without adding them twice.
func normalizeBillingUsage(usage upstreamUsage) billingUsage {
	result := billingUsage{CacheRead: nonnegativeTokens(usage.CacheRead), Reasoning: nonnegativeTokens(usage.Reasoning), EstimatedInput: !usage.InKnown, EstimatedOutput: !usage.OutKnown}
	result.CacheWrite, result.CacheWrite5m, result.CacheWrite1h = cacheWriteBreakdown(usage)
	input, output := nonnegativeTokens(usage.Prompt), nonnegativeTokens(usage.Completion)
	if usage.Source == FormatAnthropic && usage.InKnown {
		if usage.PromptMissAliasKnown {
			input = nonnegativeTokens(usage.PromptMissAlias) + result.CacheRead + result.CacheWrite
		} else if usage.PromptTotalAliasKnown {
			input = nonnegativeTokens(usage.PromptTotalAlias)
		} else {
			input += result.CacheRead + result.CacheWrite
		}
	}
	if usage.Source == FormatGemini {
		result.ToolPrompt = nonnegativeTokens(usage.ToolPrompt)
		if usage.InKnown {
			input += result.ToolPrompt
		}
		if usage.OutKnown {
			output += result.Reasoning
		}
	} else if (usage.Source == FormatOpenAIChat || usage.Source == FormatOpenAIResponse) && usage.InKnown && usage.OutKnown && usage.TotalKnown && result.Reasoning > 0 {
		// xAI's exclusive reasoning counters are evidenced by total_tokens.
		// Do not add reasoning to OpenAI's already-inclusive completion count.
		if usage.Total == input+output+result.Reasoning {
			output += result.Reasoning
		}
	}
	if result.EstimatedInput {
		input = max(input, result.CacheRead+result.CacheWrite+result.ToolPrompt)
	}
	result.CacheRead = min(result.CacheRead, input)
	result.CacheWrite = min(result.CacheWrite, input-result.CacheRead)
	if result.CacheWrite5m+result.CacheWrite1h > result.CacheWrite {
		result.CacheWrite1h = min(result.CacheWrite1h, result.CacheWrite)
		result.CacheWrite5m = result.CacheWrite - result.CacheWrite1h
	}
	result.CacheWriteUnspecified = result.CacheWrite - result.CacheWrite5m - result.CacheWrite1h
	result.ToolPrompt = min(result.ToolPrompt, input-result.CacheRead-result.CacheWrite)
	result.Input = input - result.CacheRead - result.CacheWrite - result.ToolPrompt
	result.CacheReadAudio = min(nonnegativeTokens(usage.CacheReadAudio), result.CacheRead)
	result.InputAudio = min(max(nonnegativeTokens(usage.InputAudio)-result.CacheReadAudio, 0), result.Input)
	result.Input -= result.InputAudio
	result.CacheRead -= result.CacheReadAudio
	if result.EstimatedOutput {
		output = max(output, result.Reasoning)
	}
	result.Reasoning = min(result.Reasoning, output)
	result.Output = output - result.Reasoning
	result.OutputAudio = min(nonnegativeTokens(usage.OutputAudio), result.Output)
	result.Output -= result.OutputAudio
	return result
}

func (usage billingUsage) inputTotal() int {
	return usage.Input + usage.CacheRead + usage.CacheWrite + usage.ToolPrompt + usage.InputAudio + usage.CacheReadAudio
}

func (usage billingUsage) outputTotal() int {
	return usage.Output + usage.Reasoning + usage.OutputAudio
}

func validPrice(price Price) bool {
	rates := []float64{price.In, price.Out}
	for _, optional := range []*float64{price.CacheRead, price.CacheWrite, price.CacheWrite1h, price.Reasoning, price.InAudio, price.OutAudio, price.CacheReadAudio} {
		if optional != nil {
			rates = append(rates, *optional)
		}
	}
	for _, rate := range rates {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return false
		}
	}
	return true
}

func optionalRate(rate *float64, fallback float64) float64 {
	if rate != nil {
		return *rate
	}
	return fallback
}

func costForUsage(usage billingUsage, price Price) billingCost {
	if !validPrice(price) {
		return billingCost{}
	}
	writeRate := optionalRate(price.CacheWrite, price.In)
	cost := billingCost{
		Input:          float64(usage.Input) * price.In / 1e6,
		Output:         float64(usage.Output) * price.Out / 1e6,
		CacheRead:      float64(usage.CacheRead) * optionalRate(price.CacheRead, price.In) / 1e6,
		CacheWrite:     (float64(usage.CacheWrite5m+usage.CacheWriteUnspecified)*writeRate + float64(usage.CacheWrite1h)*optionalRate(price.CacheWrite1h, writeRate)) / 1e6,
		Reasoning:      float64(usage.Reasoning) * optionalRate(price.Reasoning, price.Out) / 1e6,
		ToolPrompt:     float64(usage.ToolPrompt) * price.In / 1e6,
		InputAudio:     float64(usage.InputAudio) * optionalRate(price.InAudio, price.In) / 1e6,
		OutputAudio:    float64(usage.OutputAudio) * optionalRate(price.OutAudio, price.Out) / 1e6,
		CacheReadAudio: float64(usage.CacheReadAudio) * optionalRate(price.CacheReadAudio, optionalRate(price.CacheRead, price.In)) / 1e6,
	}
	cost.Total = cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite + cost.Reasoning + cost.ToolPrompt + cost.InputAudio + cost.OutputAudio + cost.CacheReadAudio
	if math.IsNaN(cost.Total) || math.IsInf(cost.Total, 0) || cost.Total < 0 || cost.Total > 1e12 {
		return billingCost{}
	}
	return cost
}
