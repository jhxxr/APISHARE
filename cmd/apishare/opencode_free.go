package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
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

// The inference gate requires CLI schemas, but the gateway cannot execute the
// tools it declares. Generic clients must use the legacy Zen gate, where their
// own schemas can be preserved. Only change a known official origin and path;
// custom gateways and Go credentials must never be redirected to another tier.
func (u *Upstream) openCodeFreeRoute(body []byte, format clientFormat, model string) (*Upstream, error) {
	if u.Type != "opencode" || (!u.openCodePublic() && !u.openCodeFreeModel(model)) || u.openCodeLegacy() {
		return u, nil
	}
	if format != FormatOpenAIChat && format != FormatOpenAIResponse && format != FormatAnthropic {
		return u, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("OpenCode 免费模型请求必须是 JSON 对象")
	}
	var tools []json.RawMessage
	if raw := payload["tools"]; len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil, fmt.Errorf("OpenCode tools 必须是数组")
	}
	compatible, err := openCodeNativeToolContracts(tools, format)
	if err != nil {
		return nil, err
	}
	if compatible {
		return u, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(u.BaseURL))
	if err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "opencode.ai") && u.openCodeTier() == "zen" {
		switch strings.TrimRight(parsed.Path, "/") {
		case "/inference", "/inference/v1", "/inference/openai", "/inference/openai/v1", "/inference/anthropic", "/inference/anthropic/v1":
			parsed.Path, parsed.RawPath = "/zen", ""
			route := *u
			route.BaseURL = parsed.String()
			return &route, nil
		}
	}
	return nil, fmt.Errorf("OpenCode inference 免费模型要求完整的原生工具及参数；当前客户端工具不兼容。Zen 渠道请将上游地址改为 https://opencode.ai/zen；自定义网关请配置对应的 /zen 入口")
}

func openCodeToolContract(raw json.RawMessage, format clientFormat) (string, json.RawMessage) {
	var tool map[string]json.RawMessage
	if json.Unmarshal(raw, &tool) != nil {
		return "", nil
	}
	if format == FormatOpenAIChat {
		var function map[string]json.RawMessage
		if json.Unmarshal(tool["function"], &function) != nil {
			return "", nil
		}
		tool = function
	} else if format == FormatOpenAIResponse {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind != "function" {
			return "", nil
		}
	} else if format == FormatAnthropic {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind != "" && kind != "custom" {
			return "", nil
		}
	}
	var name string
	_ = json.Unmarshal(tool["name"], &name)
	if format == FormatAnthropic {
		return name, tool["input_schema"]
	}
	return name, tool["parameters"]
}

// Names alone are insufficient: read.path and Read.file_path, for example,
// are different contracts. Never replace a caller's schema or add capabilities
// that its tool executor did not offer.
func openCodeNativeToolContracts(tools []json.RawMessage, format clientFormat) (bool, error) {
	definitions, err := openCodeToolDefinitions()
	if err != nil {
		return false, err
	}
	present := make(map[string]bool, len(tools))
	for _, raw := range tools {
		name, schema := openCodeToolContract(raw, format)
		if definition, known := definitions[name]; known {
			var actual, expected any
			if json.Unmarshal(schema, &actual) != nil || json.Unmarshal(definition.Parameters, &expected) != nil || !reflect.DeepEqual(actual, expected) {
				return false, nil
			}
			present[name] = true
		}
	}
	for _, name := range []string{"shell", "edit", "glob", "grep", "read"} {
		if !present[name] {
			return false, nil
		}
	}
	return true, nil
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
	compatible, err := openCodeNativeToolContracts(tools, format)
	if err != nil {
		return nil, false, err
	}
	if !compatible {
		return nil, false, fmt.Errorf("OpenCode inference 免费模型工具与客户端不兼容，请使用 Zen 兼容入口")
	}
	for index, tool := range tools {
		name, _ := openCodeToolContract(tool, format)
		if definition, known := definitions[name]; known {
			// Keep strict/cache flags and other client fields while normalizing
			// only the description required by the inference gate.
			var entry map[string]json.RawMessage
			_ = json.Unmarshal(tool, &entry)
			if format == FormatOpenAIChat {
				var function map[string]json.RawMessage
				_ = json.Unmarshal(entry["function"], &function)
				function["description"], _ = json.Marshal(definition.Description)
				entry["function"], _ = json.Marshal(function)
			} else {
				entry["description"], _ = json.Marshal(definition.Description)
			}
			tools[index], _ = json.Marshal(entry)
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
