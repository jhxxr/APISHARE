package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/url"

	"strings"
	"sync"

	"apishare/internal/opencodecompat"
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

type openCodeToolCompatibility struct {
	Renames *opencodecompat.ToolRenames
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
	return u.isOpenCode() && u.openCodeTier() == "zen" && strings.TrimSpace(u.openCodeAPIKey()) == "public"
}

func (u *Upstream) openCodeFreeModel(model string) bool {
	if !u.isOpenCode() {
		return false
	}
	if !u.openCodeFreeOnly() && strings.Contains(strings.ToLower(model), "free") {
		return true
	}
	openCodeCatalogs.Lock()
	defer openCodeCatalogs.Unlock()
	if u.openCodeFreeOnly() {
		return openCodeCatalogFreeModel(latestOpenCodeCatalogLocked(), "zen", model)
	}
	return latestOpenCodeCatalogLocked().FreeModels[u.openCodeTier()][model]
}

// OpenCode 2.1 groups inference model routes by provider family, as the
// catalog adapter does. Keep custom bases and paid legacy Zen/Go paths intact.
func (u *Upstream) openCodeFreeEndpointURL(format clientFormat, endpoint string) string {
	base := u.openCodeEffectiveBaseURL()
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "opencode.ai") {
		return upstreamURL(base, endpoint)
	}
	target := u.endpointURL(format, endpoint)
	if format == FormatAnthropic && endpoint == "/v1/messages" && !u.openCodeLegacy() {
		if parsed, err := url.Parse(target); err == nil {
			query := parsed.Query()
			query.Set("beta", "true")
			parsed.RawQuery = query.Encode()
			return parsed.String()
		}
	}
	return target
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

func (u *Upstream) shapeOpenCodeFreeRequest(body []byte, format clientFormat, model string, compatibility ...*openCodeToolCompatibility) ([]byte, bool, error) {
	if !u.isOpenCode() || (!u.openCodePublic() && !u.openCodeFreeModel(model)) {
		return body, false, nil
	}
	if format != FormatOpenAIChat && format != FormatOpenAIResponse && format != FormatAnthropic {
		return body, false, nil
	}
	legacy := u.openCodeLegacy()
	payload, renames, err := opencodecompat.Prepare(body, opencodecompat.Protocol(format), legacy)
	if err != nil {
		return nil, false, err
	}
	if len(compatibility) > 0 && compatibility[0] != nil {
		*compatibility[0] = openCodeToolCompatibility{Renames: renames}
	}
	encoded, err := encodeOpenCodeFreeValues(payload, format, legacy)
	return encoded, true, err
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
