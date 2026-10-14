package main

import (
	"fmt"
	"strings"
)

func parseProtocolThinking(cr *canonReq, format clientFormat, root map[string]any) error {
	var config map[string]any
	if format == FormatAnthropic {
		config = object(root["thinking"])
		if root["thinking"] != nil && config == nil {
			return fmt.Errorf("thinking must be an object")
		}
		if config == nil {
			return nil
		}
		for key := range config {
			if key != "type" && key != "budget_tokens" {
				return unsupported("thinking." + key)
			}
		}
		cr.ThinkingMode = str(config["type"])
		if cr.ThinkingMode == "" {
			return fmt.Errorf("thinking requires a type")
		}
		if raw := config["budget_tokens"]; raw != nil {
			value, ok := protocolFloat(raw)
			if !ok || value < 1024 || value > float64(maxProtocolThinkingBudget) || value != float64(int(value)) {
				return fmt.Errorf("invalid thinking budget_tokens")
			}
			budget := int(value)
			cr.ThinkingBudget = &budget
		}
	} else if format == FormatGemini {
		config = object(object(root["generationConfig"])["thinkingConfig"])
		if config == nil {
			if object(root["generationConfig"])["thinkingConfig"] != nil {
				return fmt.Errorf("thinkingConfig must be an object")
			}
			return nil
		}
		for key := range config {
			if key != "thinkingBudget" && key != "thinkingLevel" {
				return unsupported("thinkingConfig." + key)
			}
		}
		if level := str(config["thinkingLevel"]); level != "" {
			if config["thinkingBudget"] != nil {
				return fmt.Errorf("thinkingLevel and thinkingBudget cannot both be set")
			}
			cr.ThinkingMode, cr.ReasoningEffort = "adaptive", strings.ToLower(level)
			switch cr.ReasoningEffort {
			case "minimal", "low", "medium", "high":
			default:
				return unsupported("Gemini thinkingLevel " + level)
			}
		} else if raw := config["thinkingBudget"]; raw != nil {
			value, ok := protocolFloat(raw)
			if !ok || value < -1 || value > float64(maxProtocolThinkingBudget) || value != float64(int(value)) {
				return fmt.Errorf("invalid thinkingBudget")
			}
			budget := int(value)
			switch budget {
			case -1:
				cr.ThinkingMode = "adaptive"
			case 0:
				cr.ThinkingMode = "disabled"
			default:
				cr.ThinkingMode, cr.ThinkingBudget = "enabled", &budget
			}
		} else {
			return fmt.Errorf("thinkingConfig requires thinkingBudget or thinkingLevel")
		}
	}
	if cr.ThinkingMode == "" {
		return nil
	}
	switch cr.ThinkingMode {
	case "disabled":
		if cr.ThinkingBudget != nil {
			return fmt.Errorf("disabled thinking cannot have a token budget")
		}
		// Follow sub2api: an explicit disable overrides a separate effort.
		cr.ReasoningEffort = "none"
	case "adaptive":
		if cr.ThinkingBudget != nil {
			return fmt.Errorf("adaptive thinking cannot have a fixed token budget")
		}
	case "enabled":
		if cr.ThinkingBudget == nil {
			return fmt.Errorf("enabled thinking requires a token budget")
		}
		if cr.MaxTokens > 0 && *cr.ThinkingBudget >= cr.MaxTokens {
			return fmt.Errorf("thinking budget must be less than the output token limit")
		}
	default:
		return unsupported("thinking mode " + cr.ThinkingMode)
	}
	return nil
}

const maxProtocolThinkingBudget = 1 << 24

func protocolModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	return model
}

func applyProtocolThinking(target clientFormat, cr *canonReq, model string, body, geminiConfig map[string]any) error {
	name := protocolModelName(model)
	if cr.ThinkingBudget != nil {
		budget := *cr.ThinkingBudget
		switch target {
		case FormatGemini:
			if cr.ReasoningEffort != "" {
				return unsupported("simultaneous fixed budget and reasoning effort on Gemini")
			}
			max, min := 24576, 0
			if strings.HasPrefix(name, "gemini-2.5-pro") {
				max, min = 32768, 128
			} else if !strings.HasPrefix(name, "gemini-2.5-flash") {
				return unsupported("fixed thinking budget on this Gemini model")
			} else if strings.Contains(name, "lite") {
				min = 512
			}
			if budget < min || budget > max {
				return fmt.Errorf("thinking budget is outside the target model range")
			}
			geminiConfig["thinkingConfig"] = map[string]any{"thinkingBudget": budget}
		case FormatAnthropic:
			if !supportsAnthropicFixedBudget(name) {
				return unsupported("fixed thinking budget on this Anthropic model")
			}
			if budget < 1024 || cr.MaxTokens <= budget {
				return fmt.Errorf("Anthropic thinking budget needs at least 1024 tokens and a larger explicit max_tokens")
			}
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		default:
			return unsupported("fixed thinking token budget on an effort-only protocol")
		}
		return nil
	}
	if target == FormatGemini {
		if cr.ReasoningEffort == "none" {
			if !strings.HasPrefix(name, "gemini-2.5-flash") {
				return unsupported("disabling thinking on this Gemini model")
			}
			geminiConfig["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
		} else if cr.ReasoningEffort != "" {
			allowed := false
			for _, level := range geminiThinkingLevels(name) {
				allowed = allowed || level == cr.ReasoningEffort
			}
			if !allowed {
				return unsupported("this reasoning effort on the Gemini model")
			}
			geminiConfig["thinkingConfig"] = map[string]any{"thinkingLevel": cr.ReasoningEffort}
		} else if cr.ThinkingMode == "adaptive" {
			if !strings.HasPrefix(name, "gemini-2.5-") {
				return unsupported("dynamic thinking budget on this Gemini model")
			}
			geminiConfig["thinkingConfig"] = map[string]any{"thinkingBudget": -1}
		}
	} else if target == FormatAnthropic {
		if cr.ReasoningEffort == "none" {
			if !supportsAnthropicFixedBudget(name) {
				return unsupported("disabling thinking on this Anthropic model")
			}
			delete(body, "output_config")
			body["thinking"] = map[string]any{"type": "disabled"}
		} else if cr.ThinkingMode == "adaptive" {
			if !supportsAnthropicAdaptive(name) {
				return unsupported("adaptive thinking on this Anthropic model")
			}
			body["thinking"] = map[string]any{"type": "adaptive"}
		}
	} else if cr.ThinkingMode == "adaptive" && cr.ReasoningEffort == "" {
		return unsupported("adaptive thinking without an explicit effort on this protocol")
	}
	return nil
}

func geminiThinkingLevels(model string) []string {
	// Match a documented family boundary, not every future gemini-3/4 model.
	for _, profile := range []struct {
		prefix string
		levels []string
	}{
		{"gemini-3.1-flash-lite-image", []string{"minimal", "high"}},
		{"gemini-3.8-flash", []string{"low", "medium", "high"}},
		{"gemini-4-argon", []string{"low", "medium", "high"}},
		{"gemini-3.6-flash", []string{"minimal", "low", "medium", "high"}},
		{"gemini-3.5-flash-lite", []string{"minimal", "low", "medium", "high"}},
		{"gemini-3.1-flash-lite", []string{"minimal", "low", "medium", "high"}},
		{"gemini-3-flash", []string{"minimal", "low", "medium", "high"}},
	} {
		if model == profile.prefix || strings.HasPrefix(model, profile.prefix+"-") {
			return profile.levels
		}
	}
	return nil
}

func supportsAnthropicFixedBudget(model string) bool {
	for _, prefix := range []string{"claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5", "claude-sonnet-4-20", "claude-opus-4-20", "claude-3-7-sonnet"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") || strings.HasPrefix(model, prefix) && strings.HasSuffix(prefix, "-20") {
			return true
		}
	}
	return false
}

func supportsAnthropicAdaptive(model string) bool {
	for _, prefix := range []string{"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6", "claude-sonnet-4-7", "claude-sonnet-4-8", "claude-opus-5", "claude-sonnet-5", "claude-haiku-5", "claude-fable-5", "claude-mythos-5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") || strings.HasPrefix(model, prefix+".") {
			return true
		}
	}
	return false
}
