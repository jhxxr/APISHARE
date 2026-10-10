package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

type headerRule struct {
	name    string
	pattern *regexp.Regexp
	value   string
	copy    bool
	literal bool
}

var headerVariable = regexp.MustCompile(`\{(api_key|codex_user_agent|claude_code_user_agent|client_header:([^{}]+))\}`)

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < 32 && value[i] != '\t') || value[i] == 127 {
			return false
		}
	}
	return true
}

func transportHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

func credentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-api-key", "x-goog-api-key", "cookie", "set-cookie":
		return true
	}
	return false
}

func parseHeaderOverrides(raw string) ([]headerRule, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("请求头配置不能超过 64 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, fmt.Errorf("请求头配置必须是 JSON 对象")
	}
	var rules []headerRule
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("请求头 JSON 格式不正确")
		}
		name, ok := token.(string)
		if !ok || seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("请求头名称不能重复（不区分大小写）")
		}
		seen[strings.ToLower(name)] = true
		rule := headerRule{name: name}
		if strings.HasPrefix(name, "re:") {
			if len(name) == 3 {
				return nil, fmt.Errorf("请求头正则表达式不能为空")
			}
			rule.pattern, err = regexp.Compile("(?i)" + name[3:])
			if err != nil {
				return nil, fmt.Errorf("请求头规则 %q 的正则表达式无效", name)
			}
		} else if name != "*" {
			if !validHeaderName(name) {
				return nil, fmt.Errorf("请求头名称 %q 无效", name)
			}
			if transportHeader(name) {
				return nil, fmt.Errorf("请求头 %q 由 HTTP 连接自动管理，不能覆盖", name)
			}
		}
		var value any
		if decoder.Decode(&value) != nil {
			return nil, fmt.Errorf("请求头 JSON 格式不正确")
		}
		switch value := value.(type) {
		case bool:
			rule.copy = value
		case string:
			if name == "*" || rule.pattern != nil {
				return nil, fmt.Errorf("通配符和正则规则的值必须是 true 或 false")
			}
			if !validHeaderValue(value) {
				return nil, fmt.Errorf("请求头 %q 的值不能包含换行或控制字符", name)
			}
			for _, match := range headerVariable.FindAllStringSubmatch(value, -1) {
				if match[2] != "" && !validHeaderName(match[2]) {
					return nil, fmt.Errorf("请求头 %q 引用的客户端请求头名称无效", name)
				}
			}
			rule.literal, rule.value = true, value
		default:
			return nil, fmt.Errorf("请求头 %q 的值必须是字符串、true 或 false", name)
		}
		rules = append(rules, rule)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("请求头 JSON 格式不正确")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("请求头 JSON 格式不正确")
	}
	// Apply broad rules first, exclusions after copies, then exact overrides.
	sort.Slice(rules, func(i, j int) bool {
		priority := func(r headerRule) int {
			if r.name == "*" {
				return 0
			}
			if r.pattern != nil {
				if r.copy {
					return 1
				}
				return 2
			}
			return 3
		}
		if a, b := priority(rules[i]), priority(rules[j]); a != b {
			return a < b
		}
		return rules[i].name < rules[j].name
	})
	return rules, nil
}

func applyHeaderOverrides(req *http.Request, raw, apiKey string, incoming http.Header) error {
	rules, err := parseHeaderOverrides(raw)
	if err != nil {
		return err
	}
	connectionHeaders := map[string]bool{}
	for _, value := range incoming.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			connectionHeaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	copyHeader := func(name string, values []string) error {
		if transportHeader(name) || connectionHeaders[strings.ToLower(name)] {
			return nil
		}
		if !validHeaderName(name) {
			return fmt.Errorf("客户端请求头名称无效")
		}
		for _, value := range values {
			if !validHeaderValue(value) {
				return fmt.Errorf("客户端请求头 %q 含非法字符", name)
			}
		}
		if len(values) > 0 {
			req.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
		return nil
	}
	for _, rule := range rules {
		if rule.name == "*" || rule.pattern != nil {
			// Include generated headers so false rules can remove defaults too.
			names := incoming.Clone()
			if names == nil {
				names = make(http.Header)
			}
			for name := range req.Header {
				if _, exists := names[name]; !exists {
					names[name] = nil
				}
			}
			for name := range names {
				if transportHeader(name) || credentialHeader(name) || connectionHeaders[strings.ToLower(name)] || (rule.pattern != nil && !rule.pattern.MatchString(name)) {
					continue
				}
				if rule.copy {
					if err := copyHeader(name, incoming.Values(name)); err != nil {
						return err
					}
				} else {
					req.Header.Del(name)
				}
			}
			continue
		}
		if rule.literal {
			missing := false
			var variableErr error
			value := headerVariable.ReplaceAllStringFunc(rule.value, func(variable string) string {
				if variable == "{api_key}" {
					return apiKey
				}
				if variable == "{codex_user_agent}" || variable == "{claude_code_user_agent}" {
					id := strings.TrimSuffix(strings.TrimPrefix(variable, "{"), "_user_agent}")
					value, err := clientVersions.userAgent(id)
					if err != nil {
						variableErr = err
					}
					return value
				}
				name := strings.TrimSuffix(strings.TrimPrefix(variable, "{client_header:"), "}")
				value := incoming.Get(name)
				if value == "" {
					missing = true
				}
				return value
			})
			// Probes have no client headers; never substitute administrator credentials.
			if missing {
				continue
			}
			if variableErr != nil {
				return variableErr
			}
			if !validHeaderValue(value) {
				return fmt.Errorf("请求头 %q 替换后的值含非法字符", rule.name)
			}
			req.Header.Set(rule.name, value)
		} else if rule.copy {
			if err := copyHeader(rule.name, incoming.Values(rule.name)); err != nil {
				return err
			}
		} else {
			req.Header.Del(rule.name)
		}
	}
	return nil
}
