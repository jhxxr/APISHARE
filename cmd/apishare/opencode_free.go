package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Model-facing OpenCode CLI definitions, with upstream MIT attribution in
// third_party/opencode/. These declarations never execute tools in the gateway.
//
//go:embed opencode_tools.json third_party/opencode/LICENSE third_party/opencode/NOTICE
var openCodeToolAssets embed.FS

type openCodeToolDefinition struct {
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

var openCodeToolDefinitions = sync.OnceValues(func() (map[string]openCodeToolDefinition, error) {
	data, err := openCodeToolAssets.ReadFile("opencode_tools.json")
	if err != nil {
		return nil, fmt.Errorf("无法读取 OpenCode 工具定义")
	}
	var definitions map[string]openCodeToolDefinition
	if err := json.Unmarshal(data, &definitions); err != nil {
		return nil, fmt.Errorf("OpenCode 工具定义损坏")
	}
	for _, name := range []string{"shell", "edit", "glob", "grep", "read"} {
		definition := definitions[name]
		if definition.Description == "" || len(definition.Parameters) == 0 {
			return nil, fmt.Errorf("OpenCode 缺少工具定义 %s", name)
		}
	}
	return definitions, nil
})

func (u *Upstream) openCodePublic() bool {
	return u.Type == "opencode" && u.openCodeTier() == "zen" && strings.TrimSpace(u.APIKey) == "public"
}

func (u *Upstream) openCodeFreeModel(model string) bool {
	if u.Type != "opencode" {
		return false
	}
	if strings.Contains(strings.ToLower(model), "free") {
		return true
	}
	openCodeCatalogs.Lock()
	defer openCodeCatalogs.Unlock()
	return latestOpenCodeCatalogLocked().FreeModels[u.openCodeTier()][model]
}

func (u *Upstream) shapeOpenCodeFreeRequest(body []byte, format clientFormat, model string, toolAlias ...*string) ([]byte, bool, error) {
	if u.Type != "opencode" || (!u.openCodePublic() && !u.openCodeFreeModel(model)) {
		return body, false, nil
	}
	if format != FormatOpenAIChat && format != FormatOpenAIResponse && format != FormatAnthropic {
		return body, false, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, false, fmt.Errorf("OpenCode 免费模型请求必须是 JSON 对象")
	}
	// Retain raw JSON for every unrelated field, including large numbers and
	// opaque native-protocol options. The original relay task remains unchanged.
	payload["stream"] = json.RawMessage("true")
	if format == FormatOpenAIChat {
		options := make(map[string]json.RawMessage)
		if raw := payload["stream_options"]; len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if json.Unmarshal(raw, &options) != nil || options == nil {
				return nil, false, fmt.Errorf("OpenCode stream_options 必须是 JSON 对象")
			}
		}
		options["include_usage"] = json.RawMessage("true")
		payload["stream_options"], _ = json.Marshal(options)
	}
	var tools []json.RawMessage
	if raw := payload["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil, false, fmt.Errorf("OpenCode tools 必须是数组")
	}
	if u.openCodeLegacy() {
		alias, err := shapeLegacyOpenCodeTools(payload, tools, format)
		if err != nil {
			return nil, false, err
		}
		if len(toolAlias) > 0 && toolAlias[0] != nil {
			*toolAlias[0] = alias
		}
		encoded, err := json.Marshal(payload)
		return encoded, true, err
	}
	definitions, err := openCodeToolDefinitions()
	if err != nil {
		return nil, false, err
	}
	present := make(map[string]bool, len(tools))
	for index, tool := range tools {
		var entry struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(tool, &entry) != nil {
			continue
		}
		name := entry.Name
		if format == FormatOpenAIChat {
			name = entry.Function.Name
		}
		if name == "" {
			continue
		}
		present[name] = true
		if definition, known := definitions[name]; known {
			tools[index] = renderOpenCodeTool(name, definition, format)
		}
	}
	for _, name := range []string{"shell", "edit", "glob", "grep", "read"} {
		if !present[name] {
			tools = append(tools, renderOpenCodeTool(name, definitions[name], format))
		}
	}
	payload["tools"], _ = json.Marshal(tools)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("无法构造 OpenCode 免费模型请求")
	}
	return encoded, true, nil
}

func renderOpenCodeTool(name string, definition openCodeToolDefinition, format clientFormat) json.RawMessage {
	tool := map[string]any{"name": name, "description": definition.Description}
	if format == FormatAnthropic {
		tool["input_schema"] = definition.Parameters
	} else {
		tool["parameters"] = definition.Parameters
		if format == FormatOpenAIChat {
			tool = map[string]any{"type": "function", "function": tool}
		} else {
			tool["type"] = "function"
		}
	}
	encoded, _ := json.Marshal(tool)
	return encoded
}
