package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A mapping is one hop: the key is the public model, the value is the exact
// upstream identifier. Values are never recursively interpreted as aliases.
func parseModelMap(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("模型映射必须是 JSON 对象，例如 {\"gpt-4o\":\"deepseek-chat\"}")
	}
	mapping := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("模型映射 JSON 格式不正确")
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("请求模型名称必须是字符串")
		}
		name = strings.TrimSpace(name)
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("模型映射 JSON 格式不正确")
		}
		target, ok := value.(string)
		if !ok || name == "" || strings.TrimSpace(target) == "" {
			return nil, fmt.Errorf("请求模型名称和上游模型名称必须是非空字符串")
		}
		if _, duplicate := mapping[name]; duplicate {
			return nil, fmt.Errorf("请求模型 %q 存在重复映射", name)
		}
		mapping[name] = strings.TrimSpace(target)
	}
	if _, err := decoder.Token(); err != nil || !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("模型映射 JSON 格式不正确")
	}
	return mapping, nil
}

func normalizeUpstreamModelMap(up *Upstream) error {
	mapping, err := parseModelMap(up.ModelMap)
	if err != nil {
		return err
	}
	up.ModelMap = ""
	if len(mapping) > 0 {
		encoded, err := json.Marshal(mapping)
		if err != nil {
			return err
		}
		up.ModelMap = string(encoded)
	}
	return nil
}
