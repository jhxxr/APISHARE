package opencodecompat

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// EnvironmentSource is the original request, before protocol conversion. It is
// decoded afresh for each shaping attempt and never retained across requests.
type EnvironmentSource struct {
	Body     []byte
	Protocol Protocol
}

type clientEnvironment struct {
	WorkingDirectory string
	WorkspaceRoot    string
	Platform         string
	IsGitRepo        *bool
}

func resolveClientEnvironment(payload map[string]any, protocol Protocol) (clientEnvironment, error) {
	if raw, exists := payload["client_environment"]; exists {
		delete(payload, "client_environment")
		fields, ok := raw.(map[string]any)
		if !ok {
			return clientEnvironment{}, fmt.Errorf("client_environment must be an object")
		}
		var env clientEnvironment
		for name, value := range fields {
			if name == "is_git_repo" {
				flag, ok := value.(bool)
				if !ok {
					return env, fmt.Errorf("client_environment.is_git_repo must be a boolean")
				}
				env.IsGitRepo = &flag
				continue
			}
			text, ok := value.(string)
			if !ok {
				return env, fmt.Errorf("client_environment.%s must be a string", name)
			}
			if !setEnvironmentValue(&env, name, text) {
				return env, fmt.Errorf("invalid client_environment.%s", name)
			}
		}
		return env, nil
	}
	return nativeClientEnvironment(payload, protocol), nil
}

// Values are text only: never stat, clean or resolve paths on the gateway.
func setEnvironmentValue(env *clientEnvironment, name, value string) bool {
	if len(value) > 4096 {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) || ch == '\u2028' || ch == '\u2029' || ch == '<' || ch == '>' {
			return false
		}
	}
	value = strings.TrimSpace(value)
	switch name {
	case "working_directory":
		return mergeEnvironmentText(&env.WorkingDirectory, value)
	case "workspace_root":
		return mergeEnvironmentText(&env.WorkspaceRoot, value)
	case "platform":
		value = strings.ToLower(value)
		if value == "windows" {
			value = "win32"
		}
		switch value {
		case "", "unknown", "linux", "darwin", "win32":
		default:
			return false
		}
		return mergeEnvironmentText(&env.Platform, value)
	case "is_git_repo":
		if value == "" || strings.EqualFold(value, "unknown") {
			return true
		}
		var flag bool
		switch strings.ToLower(value) {
		case "yes", "true":
			flag = true
		case "no", "false":
		default:
			return false
		}
		if env.IsGitRepo != nil && *env.IsGitRepo != flag {
			return false
		}
		env.IsGitRepo = &flag
		return true
	default:
		return false
	}
}

func mergeEnvironmentText(target *string, value string) bool {
	if value == "" || strings.EqualFold(value, "unknown") {
		return true
	}
	if *target != "" && *target != value {
		return false
	}
	*target = value
	return true
}

// Only instruction text and standalone user updates are declarations; assistant
// text, quoted Markdown examples, images and nested tool results are not.
func environmentTexts(content any) []string {
	if text, ok := content.(string); ok {
		return []string{text}
	}
	blocks, _ := content.([]any)
	var texts []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" && block["type"] != "input_text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func nativeClientEnvironment(payload map[string]any, protocol Protocol) clientEnvironment {
	var env, latest clientEnvironment
	valid, updated := true, false
	instructions := func(content any) {
		for _, text := range environmentTexts(content) {
			if !parseEnvironmentBlocks(text, &env) {
				valid = false
			}
		}
	}
	user := func(content any) {
		texts := environmentTexts(content)
		if blocks, ok := content.([]any); ok && len(blocks) != len(texts) {
			return
		}
		text := strings.TrimSpace(strings.Join(texts, "\n"))
		if !strings.HasPrefix(text, "<environment_context>") {
			return
		}
		latest, updated = clientEnvironment{}, true
		if !parseEnvironmentContext(text, &latest) {
			latest = clientEnvironment{}
		}
	}
	field := "messages"
	switch protocol {
	case Chat:
	case Anthropic:
		instructions(payload["system"])
	case Responses:
		instructions(payload["instructions"])
		field = "input"
		if text, ok := payload[field].(string); ok {
			user(text)
		}
	default:
		return env
	}
	for _, raw := range SliceAt(payload, field) {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind := StringAt(message, "type"); kind != "" && kind != "message" {
			continue
		}
		switch message["role"] {
		case "system", "developer":
			instructions(message["content"])
		case "user":
			user(message["content"])
		}
	}
	if updated {
		return latest
	}
	if !valid {
		return clientEnvironment{}
	}
	return env
}

func parseEnvironmentBlocks(text string, env *clientEnvironment) bool {
	inside, valid := false, true
	var fence byte
	fenceLength := 0
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if !inside && len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
			n := 1
			for n < len(line) && line[n] == line[0] {
				n++
			}
			if fence == 0 && n >= 3 {
				fence, fenceLength = line[0], n
				continue
			}
			if line[0] == fence && n >= fenceLength && strings.TrimSpace(line[n:]) == "" {
				fence = 0
				continue
			}
		}
		if fence != 0 {
			continue
		}
		switch line {
		case "<env>":
			if inside {
				valid = false
			}
			inside = true
		case "</env>":
			if !inside {
				valid = false
			}
			inside = false
		default:
			if !inside {
				continue
			}
			label, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			field := map[string]string{"Working directory": "working_directory", "Workspace root folder": "workspace_root", "Platform": "platform", "Is directory a git repo": "is_git_repo"}[label]
			if field != "" && !setEnvironmentValue(env, field, strings.TrimSpace(value)) {
				valid = false
			}
		}
	}
	return valid && !inside
}

func parseEnvironmentContext(text string, env *clientEnvironment) bool {
	// Decode into flat elements: nested markup in recognized values is rejected.
	var block struct {
		XMLName xml.Name `xml:"environment_context"`
		Text    string   `xml:",chardata"`
		Fields  []struct {
			XMLName xml.Name
			Attrs   []xml.Attr `xml:",any,attr"`
			Text    string     `xml:",chardata"`
			Inner   string     `xml:",innerxml"`
		} `xml:",any"`
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	if !strings.HasSuffix(text, "</environment_context>") || decoder.Decode(&block) != nil || strings.TrimSpace(block.Text) != "" {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	for _, field := range block.Fields {
		name := field.XMLName.Local
		if name == "cwd" {
			name = "working_directory"
		}
		switch name {
		case "working_directory", "workspace_root", "platform", "is_git_repo":
		default:
			continue
		}
		if field.XMLName.Space != "" {
			continue
		}
		if len(field.Attrs) != 0 || strings.Contains(field.Inner, "<") || !setEnvironmentValue(env, name, field.Text) {
			return false
		}
	}
	return true
}
