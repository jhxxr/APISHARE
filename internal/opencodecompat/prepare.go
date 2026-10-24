package opencodecompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type Protocol string

const (
	Chat      Protocol = "openai-chat"
	Responses Protocol = "openai-responses"
	Anthropic Protocol = "anthropic"
	SystemOne Protocol = "system-one"
)

func StringAt(value map[string]any, path ...string) string {
	var current any = value
	for _, key := range path {
		object, _ := current.(map[string]any)
		current = object[key]
	}
	text, _ := current.(string)
	return text
}

func SliceAt(value map[string]any, key string) []any {
	items, _ := value[key].([]any)
	return items
}

func Marshal(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func Decode(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, fmt.Errorf("OpenCode 免费模型请求必须是 JSON 对象")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("OpenCode 免费模型请求包含多余 JSON 数据")
	}
	return payload, nil
}

// Prepare keeps client schemas and instructions while adding the captured CLI
// prompt and the full tool set. It only edits this request's decoded copy.
func Prepare(body []byte, protocol Protocol, v1 bool) (map[string]any, *ToolRenames, error) {
	payload, err := Decode(body)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range []string{"tools", "functions"} {
		if value := payload[name]; value != nil {
			if _, ok := value.([]any); !ok {
				return nil, nil, fmt.Errorf("OpenCode %s 必须是数组", name)
			}
		}
	}
	payload["stream"] = true
	if protocol == Chat {
		options, _ := payload["stream_options"].(map[string]any)
		if options == nil {
			if payload["stream_options"] != nil {
				return nil, nil, fmt.Errorf("OpenCode stream_options 必须是 JSON 对象")
			}
			options = map[string]any{}
		}
		options["include_usage"] = true
		payload["stream_options"] = options
		if _, completion := payload["max_completion_tokens"]; !completion && !v1 {
			if _, tokens := payload["max_tokens"]; !tokens {
				payload["max_completion_tokens"] = 32000
			}
		}
	}
	injectOfficialPrompt(payload, protocol, v1)
	renames, _ := renameAndCompleteTools(payload, protocol, v1)
	return payload, renames, nil
}
