package opencodecompat

// officialToolCandidates lists known equivalents used by Claude Code and other
// agent clients. Only tools actually declared by this request may be selected.
func officialToolCandidates(name string) []string {
	switch name {
	case "bash", "shell":
		return []string{"Bash"}
	case "edit":
		return []string{"Edit"}
	case "glob":
		return []string{"Glob"}
	case "grep":
		return []string{"Grep"}
	case "question":
		return []string{"AskUserQuestion"}
	case "read":
		return []string{"Read"}
	case "skill":
		return []string{"Skill"}
	case "subagent", "task":
		return []string{"Agent", "Task", "Subagent"}
	case "todowrite":
		return []string{"TodoWrite"}
	case "webfetch":
		return []string{"WebFetch"}
	case "websearch":
		return []string{"WebSearch"}
	case "write":
		return []string{"Write"}
	case "patch":
		return []string{"Patch"}
	default:
		return nil
	}
}

func bindOfficialToolAliases(renames *ToolRenames, v1 bool) {
	for _, official := range officialToolNames(v1) {
		// A native tool declared under this exact name takes precedence.
		if _, ok := renames.Wire(official); ok {
			renames.BindOfficial(official, official)
			continue
		}
		for _, candidate := range append([]string{official}, officialToolCandidates(official)...) {
			client, _ := renames.Resolve(candidate)
			if _, ok := renames.Wire(client); ok {
				renames.BindOfficial(official, client)
				break
			}
		}
	}
}

func historyToolWireName(name string, declared map[string]string, renames *ToolRenames) (string, bool) {
	if wire, ok := declared[name]; ok {
		return wire, true
	}
	if _, ok := renames.Client(name); ok {
		return name, true
	}
	client, _ := renames.Resolve(name)
	wire, ok := declared[client]
	return wire, ok
}

// sanitizeToolHistory removes undeclared historical calls and their paired
// results before rewriting references. Tool inputs and ordinary text are opaque.
func sanitizeToolHistory(payload map[string]any, protocol Protocol, declared map[string]string, renames *ToolRenames) {
	resolve := func(container map[string]any) bool {
		wire, ok := historyToolWireName(StringAt(container, "name"), declared, renames)
		if ok {
			container["name"] = wire
		}
		return ok
	}
	switch protocol {
	case Anthropic:
		sanitizeAnthropicToolHistory(payload, resolve)
	case Chat:
		sanitizeChatToolHistory(payload, resolve)
	case Responses:
		sanitizeResponsesToolHistory(payload, resolve)
	}
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		switch StringAt(choice, "type") {
		case "tool", "function", "custom":
			target := choice
			if function, ok := choice["function"].(map[string]any); ok && protocol == Chat {
				target = function
			}
			if StringAt(target, "name") != "" && !resolve(target) {
				delete(payload, "tool_choice")
			}
		case "allowed_tools":
			for _, key := range []string{"tools", "allowed_tools"} {
				items, ok := choice[key].([]any)
				if !ok {
					continue
				}
				kept := make([]any, 0, len(items))
				for _, raw := range items {
					entry, _ := raw.(map[string]any)
					target := entry
					if function, ok := entry["function"].(map[string]any); ok {
						target = function
					}
					if StringAt(target, "name") == "" || resolve(target) {
						kept = append(kept, raw)
					}
				}
				if len(kept) == 0 {
					delete(payload, "tool_choice")
				} else {
					choice[key] = kept
				}
			}
		}
	}
	if call, ok := payload["function_call"].(map[string]any); ok && protocol == Chat && !resolve(call) {
		delete(payload, "function_call")
	}
}

func sanitizeAnthropicToolHistory(payload map[string]any, resolve func(map[string]any) bool) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	dropped := map[string]bool{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		for _, raw := range SliceAt(message, "content") {
			block, _ := raw.(map[string]any)
			if StringAt(block, "type") == "tool_use" && !resolve(block) {
				if id := StringAt(block, "id"); id != "" {
					dropped[id] = true
				}
			}
		}
	}
	keptMessages := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, ok := message["content"].([]any)
		if !ok {
			keptMessages = append(keptMessages, raw)
			continue
		}
		kept := make([]any, 0, len(content))
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			kind := StringAt(block, "type")
			if kind == "tool_use" && !resolve(block) || kind == "tool_result" && dropped[StringAt(block, "tool_use_id")] {
				continue
			}
			kept = append(kept, raw)
		}
		if len(kept) == 0 && len(content) > 0 {
			continue
		}
		message["content"] = kept
		keptMessages = append(keptMessages, raw)
	}
	payload["messages"] = keptMessages
}

func chatMessageHasContent(message map[string]any) bool {
	for _, key := range []string{"content", "reasoning_content", "reasoning", "function_call"} {
		switch value := message[key].(type) {
		case string:
			if value != "" {
				return true
			}
		case []any:
			if len(value) > 0 {
				return true
			}
		case map[string]any:
			if len(value) > 0 {
				return true
			}
		case nil:
		default:
			return true
		}
	}
	return false
}

func sanitizeChatToolHistory(payload map[string]any, resolve func(map[string]any) bool) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	dropped := map[string]bool{}
	dropEmpty := map[int]bool{}
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		if call, ok := message["function_call"].(map[string]any); ok && !resolve(call) {
			delete(message, "function_call")
			dropEmpty[index] = true
		}
		calls, ok := message["tool_calls"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(calls))
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			if resolve(function) {
				kept = append(kept, raw)
			} else if id := StringAt(call, "id"); id != "" {
				dropped[id] = true
			}
		}
		if len(kept) == 0 {
			delete(message, "tool_calls")
			dropEmpty[index] = dropEmpty[index] || len(calls) > 0
		} else {
			message["tool_calls"] = kept
		}
	}
	kept := make([]any, 0, len(messages))
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		switch StringAt(message, "role") {
		case "tool":
			if dropped[StringAt(message, "tool_call_id")] {
				continue
			}
			if StringAt(message, "name") != "" {
				resolve(message)
			}
		case "function":
			if !resolve(message) {
				continue
			}
		}
		if dropEmpty[index] && !chatMessageHasContent(message) && len(SliceAt(message, "tool_calls")) == 0 {
			continue
		}
		kept = append(kept, raw)
	}
	payload["messages"] = kept
}

func sanitizeResponsesToolHistory(payload map[string]any, resolve func(map[string]any) bool) {
	items, ok := payload["input"].([]any)
	if !ok { // Responses also accepts a plain input string.
		return
	}
	dropped := map[string]bool{}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		kind := StringAt(item, "type")
		if (kind == "function_call" || kind == "custom_tool_call") && !resolve(item) {
			id := StringAt(item, "call_id")
			if id == "" {
				id = StringAt(item, "id")
			}
			if id != "" {
				dropped[id] = true
			}
		}
	}
	kept := make([]any, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch StringAt(item, "type") {
		case "function_call", "custom_tool_call":
			if !resolve(item) {
				continue
			}
		case "function_call_output", "custom_tool_call_output":
			if dropped[StringAt(item, "call_id")] {
				continue
			}
		}
		kept = append(kept, raw)
	}
	payload["input"] = kept
}
