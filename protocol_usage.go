package main

// Responses and billing share the same disjoint token categories. Raw provider
// counters remain available, but totals must follow each provider's semantics.
func renderProtocolUsage(target clientFormat, usage upstreamUsage) map[string]any {
	counts := normalizeBillingUsage(usage)
	input, output := counts.inputTotal(), counts.outputTotal()
	switch target {
	case FormatAnthropic:
		uncached := counts.Input + counts.ToolPrompt
		out := map[string]any{"input_tokens": uncached, "output_tokens": output}
		if usage.CacheReadKnown {
			out["cache_read_input_tokens"] = counts.CacheRead
		}
		if usage.CacheWriteKnown || usage.CacheWrite5mKnown || usage.CacheWrite1hKnown {
			out["cache_creation_input_tokens"] = counts.CacheWrite
		}
		if usage.CacheWrite5mKnown || usage.CacheWrite1hKnown {
			out["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": counts.CacheWrite5m, "ephemeral_1h_input_tokens": counts.CacheWrite1h}
		}
		return out
	case FormatGemini:
		prompt := input
		if usage.Source == FormatGemini {
			prompt -= counts.ToolPrompt
		}
		candidates := counts.Output
		out := map[string]any{"promptTokenCount": prompt, "candidatesTokenCount": candidates, "totalTokenCount": input + output}
		if usage.ToolPromptKnown {
			out["toolUsePromptTokenCount"] = counts.ToolPrompt
		}
		if usage.CacheReadKnown {
			out["cachedContentTokenCount"] = counts.CacheRead
		}
		if usage.ReasoningKnown {
			out["thoughtsTokenCount"] = counts.Reasoning
		}
		return out
	default:
		in, completion, total := "prompt_tokens", "completion_tokens", "total_tokens"
		inputDetail, outputDetail := "prompt_tokens_details", "completion_tokens_details"
		if target == FormatOpenAIResponse {
			in, completion = "input_tokens", "output_tokens"
			inputDetail, outputDetail = "input_tokens_details", "output_tokens_details"
		}
		out := map[string]any{in: input, completion: output, total: input + output}
		detail := map[string]any{}
		if usage.CacheReadKnown {
			detail["cached_tokens"] = counts.CacheRead
		}
		if usage.CacheWriteKnown || usage.CacheWrite5mKnown || usage.CacheWrite1hKnown {
			// Compatible gateways use this extension; it is not an official
			// OpenAI cache-write billing category.
			detail["cache_creation_tokens"] = counts.CacheWrite
		}
		if len(detail) > 0 {
			out[inputDetail] = detail
		}
		if usage.ReasoningKnown {
			out[outputDetail] = map[string]any{"reasoning_tokens": counts.Reasoning}
		}
		return out
	}
}
