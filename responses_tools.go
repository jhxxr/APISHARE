package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// responseTool remembers the client identity while the upstream sees an ordinary
// function. It belongs to one request, including tools discovered in its history.
type responseTool struct {
	Type, Name, Namespace string
}

func responsesFunctionName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	full := namespace + "__" + name
	var safe strings.Builder
	for _, r := range full {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			safe.WriteRune(r)
		} else {
			safe.WriteByte('_')
		}
	}
	flat := safe.String()
	if len(flat) > 64 || flat != full {
		sum := sha256.Sum256([]byte(namespace + "\x00" + name))
		if len(flat) > 47 {
			flat = flat[:47]
		}
		flat += "_" + hex.EncodeToString(sum[:8])
	}
	return flat
}

func responseInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "Complete raw text for the tool, preserved verbatim."}}, "required": []string{"input"}, "additionalProperties": false}
}

func copyProtocolObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// lowerResponsesTools works on decoded JSON only. The original request bytes
// remain available for a native Responses upstream, even on conversion failure.
func lowerResponsesTools(cr *canonReq, m map[string]any) error {
	cr.ResponseTools = make(map[string]responseTool)
	definitions := make(map[string]string)
	tools := []any{}
	var add func(any, string, string, int) error
	add = func(raw any, namespace, description string, depth int) error {
		if depth > 8 {
			return fmt.Errorf("Responses tool namespaces are nested too deeply")
		}
		j := object(raw)
		if j == nil {
			return fmt.Errorf("Responses tools must be objects")
		}
		typ, name := str(j["type"]), str(j["name"])
		if typ == "namespace" {
			if name == "" || namespace != "" {
				return fmt.Errorf("Responses namespace must have a name and cannot be nested")
			}
			children, ok := j["tools"].([]any)
			if !ok {
				return fmt.Errorf("Responses namespace %q is missing tools", name)
			}
			for _, child := range children {
				if err := add(child, name, str(j["description"]), depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if typ == "tool_search" {
			// Hosted discovery cannot be emulated by handing it to the client.
			if str(j["execution"]) != "client" || namespace != "" {
				return unsupported("server-executed Responses tool_search")
			}
			name = "tool_search"
		}
		if typ != "function" && typ != "custom" && typ != "tool_search" {
			return unsupported("Responses tool type " + typ)
		}
		if name == "" {
			return fmt.Errorf("Responses tool name is required")
		}
		flat := responsesFunctionName(namespace, name)
		meta := responseTool{Type: typ, Name: name, Namespace: namespace}
		f := map[string]any{"type": "function", "name": flat}
		desc := joinSystem(description, str(j["description"]))
		if typ == "custom" {
			desc = joinSystem(`Gateway compatibility: this is a callable function wrapper for a client-executed freeform tool. Call it with {"input":"the complete raw tool text"}. Instructions below about not using JSON apply to the text inside input, not to this function's argument envelope. Do not invoke a shell command of the same name instead of this tool.`, desc)
			if cfg := object(j["format"]); cfg != nil {
				switch str(cfg["type"]) {
				case "", "text":
				case "grammar":
					desc = joinSystem(desc, "The raw input must match this "+str(cfg["syntax"])+" grammar:\n"+str(cfg["definition"]))
				default:
					return unsupported("Responses custom tool input format")
				}
			}
			f["parameters"] = responseInputSchema()
		} else {
			f["parameters"] = j["parameters"]
			if typ == "tool_search" && f["parameters"] == nil {
				f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer"}}, "required": []string{"query"}}
			}
			if v, ok := j["strict"].(bool); ok {
				f["strict"] = v
			}
		}
		if desc != "" {
			f["description"] = desc
		}
		encoded := jsonText(f)
		if prior, ok := cr.ResponseTools[flat]; ok {
			if prior != meta || definitions[flat] != encoded {
				return fmt.Errorf("Responses tool %q conflicts with another tool after conversion", flat)
			}
			return nil
		}
		cr.ResponseTools[flat], definitions[flat] = meta, encoded
		tools = append(tools, f)
		return nil
	}
	addList := func(raw any) error {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("Responses tools must be an array")
		}
		for _, v := range list {
			if err := add(v, "", "", 0); err != nil {
				return err
			}
		}
		return nil
	}
	if m["tools"] != nil {
		if err := addList(m["tools"]); err != nil {
			return err
		}
	}
	items, inputIsArray := m["input"].([]any)
	// Tools loaded by client-side search become executable on the next turn.
	for _, v := range items {
		j := object(v)
		if str(j["type"]) == "additional_tools" || str(j["type"]) == "tool_search_output" {
			if str(j["type"]) == "tool_search_output" && str(j["execution"]) != "client" {
				return unsupported("server-executed Responses tool search history")
			}
			if j["tools"] != nil || str(j["type"]) == "additional_tools" {
				if err := addList(j["tools"]); err != nil {
					return err
				}
			}
		}
	}
	registerHistory := func(typ, namespace, name string) (string, error) {
		flat := responsesFunctionName(namespace, name)
		meta := responseTool{Type: typ, Name: name, Namespace: namespace}
		if namespace == "" {
			if resolved, original, ok := resolveResponseTool(flat, cr.ResponseTools); ok && original.Type == typ {
				return resolved, nil
			}
		}
		if prior, ok := cr.ResponseTools[flat]; ok && prior != meta {
			return "", fmt.Errorf("Responses history tool %q conflicts with its declaration", flat)
		}
		cr.ResponseTools[flat] = meta
		return flat, nil
	}
	input := []any{}
	invalidCalls := make(map[string]bool)
	for _, v := range items {
		j := copyProtocolObject(object(v))
		typ := str(j["type"])
		switch typ {
		case "additional_tools":
			continue
		case "function_call", "custom_tool_call", "tool_search_call":
			kind, name := "function", str(j["name"])
			if typ == "function_call" {
				id := str(j["call_id"])
				if id == "" || name == "" {
					return fmt.Errorf("function_call history is missing its ID or name")
				}
				arguments := str(j["arguments"])
				if arguments == "" {
					if j["arguments"] != nil && j["arguments"] != "" {
						return fmt.Errorf("function_call arguments must be a JSON string")
					}
					arguments = "{}"
				}
				var args map[string]any
				if json.Unmarshal([]byte(arguments), &args) != nil || args == nil {
					// Interrupted streams can leave a partial historical call. Do not
					// replay it or its result into a Chat provider's strict validator.
					invalidCalls[id] = true
					continue
				}
				delete(invalidCalls, id)
				j["arguments"] = arguments
			}
			if typ == "custom_tool_call" {
				kind = "custom"
				raw, ok := j["input"].(string)
				if !ok {
					return fmt.Errorf("custom_tool_call input must be a string")
				}
				j["arguments"] = jsonText(map[string]any{"input": raw})
			} else if typ == "tool_search_call" {
				if str(j["execution"]) != "client" {
					return unsupported("server-executed Responses tool search history")
				}
				kind, name = "tool_search", "tool_search"
				if _, ok := j["arguments"].(string); !ok {
					j["arguments"] = jsonText(j["arguments"])
				}
			}
			flat, err := registerHistory(kind, str(j["namespace"]), name)
			if err != nil {
				return err
			}
			j["type"], j["name"] = "function_call", flat
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			if invalidCalls[str(j["call_id"])] {
				continue
			}
			if typ == "tool_search_output" && j["output"] == nil {
				j["output"] = jsonText(j["tools"])
			}
			j["type"] = "function_call_output"
		}
		input = append(input, j)
	}
	if inputIsArray {
		m["input"] = input
	}
	m["tools"] = tools
	if choice := object(m["tool_choice"]); choice != nil {
		resolve := func(j map[string]any) (string, error) {
			typ, name := str(j["type"]), str(j["name"])
			if typ == "tool_search" {
				name = "tool_search"
			}
			flat := responsesFunctionName(str(j["namespace"]), name)
			meta, ok := cr.ResponseTools[flat]
			if !ok || meta.Type != typ || definitions[flat] == "" {
				return "", fmt.Errorf("Responses tool_choice refers to undeclared tool %q", flat)
			}
			return flat, nil
		}
		if str(choice["type"]) == "allowed_tools" {
			allowed := map[string]bool{}
			for _, raw := range array(choice["tools"]) {
				name, err := resolve(object(raw))
				if err != nil {
					return err
				}
				allowed[name] = true
			}
			if len(allowed) == 0 {
				return fmt.Errorf("Responses allowed_tools must not be empty")
			}
			filtered := []any{}
			for _, raw := range tools {
				if allowed[str(object(raw)["name"])] {
					filtered = append(filtered, raw)
				}
			}
			m["tools"] = filtered
			mode := str(choice["mode"])
			if mode == "" {
				mode = "auto"
			}
			if mode != "auto" && mode != "required" {
				return unsupported("Responses allowed_tools mode " + mode)
			}
			m["tool_choice"] = mode
		} else {
			name, err := resolve(choice)
			if err != nil {
				return err
			}
			m["tool_choice"] = map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

// Exact declarations own their names. Only unambiguous aliases of a custom
// tool may be recovered; never steal the name of a declared ordinary function.
func resolveResponseTool(name string, tools map[string]responseTool) (string, responseTool, bool) {
	if meta, ok := tools[name]; ok {
		return name, meta, true
	}
	namespaces := make(map[string]bool)
	for _, meta := range tools {
		if meta.Namespace != "" {
			namespaces[meta.Namespace] = true
		}
	}
	match := ""
	var matched responseTool
	for flat, meta := range tools {
		if meta.Type != "custom" {
			continue
		}
		alias := meta.Namespace != "" && name == meta.Name
		if meta.Namespace == "" {
			for namespace := range namespaces {
				if responsesFunctionName(namespace, meta.Name) == name {
					alias = true
					break
				}
			}
		}
		if alias {
			if match != "" && match != flat {
				return "", responseTool{}, false
			}
			match, matched = flat, meta
		}
	}
	return match, matched, match != ""
}

func restoreResponseTool(b responseBlock, tools map[string]responseTool) responseBlock {
	if b.Kind == "tool" && b.ToolType == "" {
		if _, meta, ok := resolveResponseTool(b.Call.Name, tools); ok {
			b.ToolType, b.Namespace, b.Call.Name = meta.Type, meta.Namespace, meta.Name
		}
	}
	return b
}

func responsesToolItem(b responseBlock, id string) map[string]any {
	j := map[string]any{"id": id, "type": "function_call", "status": "completed", "call_id": b.Call.ID, "name": b.Call.Name, "arguments": b.Call.Arguments}
	if b.Namespace != "" {
		j["namespace"] = b.Namespace
	}
	switch b.ToolType {
	case "custom":
		var args map[string]any
		_ = json.Unmarshal([]byte(b.Call.Arguments), &args)
		j["type"], j["input"] = "custom_tool_call", str(args["input"])
		delete(j, "arguments")
	case "tool_search":
		args := map[string]any{}
		_ = json.Unmarshal([]byte(b.Call.Arguments), &args)
		j["type"], j["execution"], j["arguments"] = "tool_search_call", "client", args
		delete(j, "name")
	}
	return j
}

func validateResponseToolArguments(b responseBlock) error {
	if b.ToolType != "custom" {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(b.Call.Arguments), &args) != nil {
		return fmt.Errorf("upstream custom tool arguments are not valid JSON")
	}
	if _, ok := args["input"].(string); !ok {
		return fmt.Errorf("upstream custom tool arguments are missing the raw input string")
	}
	return nil
}
