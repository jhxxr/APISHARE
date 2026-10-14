package main

// Keep provider counters for the existing billing path. Normalize totals only
// when representing usage in another protocol; cache prices require a separate
// pricing policy rather than charging all normalized input at the full rate.
func renderProtocolUsage(target clientFormat, usage upstreamUsage) map[string]any {
	input, output := usage.Prompt, usage.Completion
	if usage.Source == FormatAnthropic {
		input += usage.CacheRead + usage.CacheWrite
	}
	if usage.Source == FormatGemini {
		input += usage.ToolPrompt
		output += usage.Reasoning
	}
	switch target {
	case FormatAnthropic:
		uncached := input - usage.CacheRead - usage.CacheWrite
		if uncached < 0 {
			uncached = 0
		}
		out := map[string]any{"input_tokens": uncached, "output_tokens": output}
		if usage.CacheReadKnown {
			out["cache_read_input_tokens"] = usage.CacheRead
		}
		if usage.CacheWriteKnown {
			out["cache_creation_input_tokens"] = usage.CacheWrite
		}
		return out
	case FormatGemini:
		prompt := input
		if usage.Source == FormatGemini {
			prompt -= usage.ToolPrompt
		}
		candidates := output - usage.Reasoning
		if candidates < 0 {
			candidates = 0
		}
		out := map[string]any{"promptTokenCount": prompt, "candidatesTokenCount": candidates, "totalTokenCount": input + output}
		if usage.ToolPromptKnown {
			out["toolUsePromptTokenCount"] = usage.ToolPrompt
		}
		if usage.CacheReadKnown {
			out["cachedContentTokenCount"] = usage.CacheRead
		}
		if usage.ReasoningKnown {
			out["thoughtsTokenCount"] = usage.Reasoning
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
			detail["cached_tokens"] = usage.CacheRead
		}
		if usage.CacheWriteKnown {
			// Compatible gateways use this extension; it is not an official
			// OpenAI cache-write billing category.
			detail["cache_creation_tokens"] = usage.CacheWrite
		}
		if len(detail) > 0 {
			out[inputDetail] = detail
		}
		if usage.ReasoningKnown {
			out[outputDetail] = map[string]any{"reasoning_tokens": usage.Reasoning}
		}
		return out
	}
}
