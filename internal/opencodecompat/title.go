package opencodecompat

import "strings"

const titlePromptV1 = "generate a title for this conversation"

var titlePromptsV2 = []string{
	"you name conversations so the user can find them later in a list of past sessions",
	"you generate a descriptive title for this conversation",
	"you decide whether the session title still matches the conversation",
	"reply with a title and nothing else",
	"reply with a title for it and nothing else",
}

// isTitleGenerationRequest recognizes OpenCode's short, tool-free title calls
// before the official prompt or tool set changes their classification.
func isTitleGenerationRequest(payload map[string]any, protocol Protocol) bool {
	if len(SliceAt(payload, "tools")) > 0 || len(SliceAt(payload, "functions")) > 0 {
		return false
	}
	messages := SliceAt(payload, "messages")
	matched := false
	switch protocol {
	case Chat:
	case Anthropic:
		matched = matchesTitlePrompt(payload["system"])
	case Responses:
		matched = matchesTitlePrompt(payload["instructions"])
		messages = SliceAt(payload, "input")
		if text, ok := payload["input"].(string); ok && text != "" {
			messages = []any{map[string]any{"role": "user", "content": text}}
		}
	default:
		return false
	}
	if len(messages) == 0 || len(messages) > 3 {
		return false
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		role := strings.ToLower(strings.TrimSpace(StringAt(message, "role")))
		if role != "system" && role != "developer" && role != "user" {
			return false
		}
		if len(SliceAt(message, "tool_calls")) > 0 || message["function_call"] != nil {
			return false
		}
		for _, raw := range SliceAt(message, "content") {
			block, _ := raw.(map[string]any)
			switch StringAt(block, "type") {
			case "tool_use", "server_tool_use", "tool_result":
				return false
			}
		}
		text := strings.ToLower(strings.TrimSpace(strings.Join(textFragments(message["content"]), " ")))
		if role == "user" {
			matched = matched || strings.HasPrefix(text, titlePromptV1)
			continue
		}
		matched = matched || matchesTitlePrompt(text)
	}
	return matched
}

func matchesTitlePrompt(value any) bool {
	text := strings.ToLower(strings.Join(textFragments(value), " "))
	for _, prompt := range titlePromptsV2 {
		if strings.Contains(text, prompt) {
			return true
		}
	}
	return false
}
