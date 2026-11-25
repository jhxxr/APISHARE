package requestredact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ModeOff        = "off"
	ModeObserve    = "observe"
	ModeEnforce    = "enforce"
	CoverageText   = "text"
	CoverageStrict = "strict"
	MaxBodyBytes   = 32 << 20
	MaxNodes       = 100000
	MaxDepth       = 64
	MaxMatches     = 4096
)

var categories = map[string]bool{
	"api_key": true, "email": true, "phone": true, "id_card": true,
	"bank_card": true, "ipv4": true,
}

var builtinPatterns = map[string]*regexp.Regexp{
	"api_key":   regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|gh[opsur]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|xox[abposr]-[A-Za-z0-9-]{10,}|[Bb]earer[ \t]+[A-Za-z0-9._~+/-]{20,}=*)`),
	"email":     regexp.MustCompile(`\b[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]{1,63}(?:\.[A-Za-z0-9-]{1,63}){0,8}\.[A-Za-z]{2,24}\b`),
	"id_card":   regexp.MustCompile(`\b[1-8]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`),
	"bank_card": regexp.MustCompile(`\b[2-6]\d{12,18}\b|\b[2-6]\d{3}(?:[ -]\d{4}){3}(?:[ -]\d{1,3})?\b`),
	"phone":     regexp.MustCompile(`\+[1-9]\d{7,14}\b|\b1[3-9]\d(?:[ -]?\d{4}){2}\b`),
	"ipv4":      regexp.MustCompile(`(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?:\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}`),
}

var categoryOrder = []string{"api_key", "email", "id_card", "bank_card", "phone", "ipv4"}
var mediaPlaceholder = regexp.MustCompile(`^\[media:\d+\]$`)

type Rule struct {
	ID          string `json:"id"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

type Config struct {
	Mode          string            `json:"mode"`
	Coverage      string            `json:"coverage"`
	Categories    []string          `json:"categories"`
	Replacements  map[string]string `json:"replacements"`
	Rules         []Rule            `json:"rules"`
	ToolArguments string            `json:"tool_arguments"`
}

type Finding struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type Result struct {
	Body      []byte    `json:"body,omitempty"`
	Findings  []Finding `json:"findings,omitempty"`
	Unchecked []string  `json:"unchecked,omitempty"`
	Changed   bool      `json:"changed"`
	Matched   int       `json:"matched"`
}

type Error struct {
	Kind     string
	Category string
}

func (e *Error) Error() string {
	switch e.Kind {
	case "unsupported":
		return "request contains content that cannot be checked by the selected coverage"
	case "tool_argument":
		return "request contains sensitive text in tool arguments"
	case "limit":
		return "request exceeds the redaction processing limit"
	default:
		return "request redaction failed"
	}
}

type compiledRule struct {
	id          string
	category    string
	pattern     *regexp.Regexp
	replacement string
	builtin     bool
}

type textMatch struct {
	start       int
	end         int
	category    string
	replacement string
	order       int
}

type Compiled struct {
	config Config
	rules  []compiledRule
}

func Categories() []string { return append([]string(nil), categoryOrder...) }

func PersonalConfig() Config {
	return Config{
		Mode: ModeEnforce, Coverage: CoverageText,
		Categories: []string{"api_key", "email", "phone", "id_card", "bank_card"},
		Replacements: map[string]string{
			"api_key": "[API_KEY]", "email": "[EMAIL]", "phone": "[PHONE]",
			"id_card": "[ID_CARD]", "bank_card": "[BANK_CARD]", "ipv4": "[IPV4]",
		},
		ToolArguments: "reject_on_match",
	}
}

func DefaultAdminConfig() Config {
	cfg := PersonalConfig()
	cfg.Mode = ModeOff
	cfg.Categories = []string{"api_key", "email", "phone", "id_card", "bank_card"}
	return cfg
}

func Compile(config Config) (*Compiled, error) {
	cfg := cloneConfig(config)
	switch cfg.Mode {
	case ModeOff, ModeObserve, ModeEnforce:
	default:
		return nil, errors.New("invalid redaction mode")
	}
	if cfg.Coverage == "" {
		cfg.Coverage = CoverageText
	}
	if cfg.Coverage != CoverageText && cfg.Coverage != CoverageStrict {
		return nil, errors.New("invalid redaction coverage")
	}
	if cfg.ToolArguments == "" {
		cfg.ToolArguments = "reject_on_match"
	}
	if cfg.ToolArguments != "reject_on_match" {
		return nil, errors.New("invalid tool argument policy")
	}
	if len(cfg.Categories) > len(categories) {
		return nil, errors.New("too many redaction categories")
	}
	selected := make(map[string]bool, len(cfg.Categories))
	for _, category := range cfg.Categories {
		if !categories[category] || selected[category] {
			return nil, fmt.Errorf("invalid or duplicate redaction category %q", category)
		}
		selected[category] = true
	}
	if len(cfg.Rules) > 50 {
		return nil, errors.New("too many custom redaction rules")
	}
	compiled := &Compiled{config: cfg}
	for _, category := range categoryOrder {
		if !selected[category] {
			continue
		}
		replacement := cfg.Replacements[category]
		if replacement == "" {
			replacement = "[" + strings.ToUpper(category) + "]"
		}
		if len(replacement) > 64 {
			return nil, fmt.Errorf("replacement for %s is too long", category)
		}
		compiled.rules = append(compiled.rules, compiledRule{
			id: category, category: category, pattern: builtinPatterns[category], replacement: replacement, builtin: true,
		})
	}
	seen := map[string]bool{}
	for index, rule := range cfg.Rules {
		id := strings.TrimSpace(rule.ID)
		if id == "" {
			id = fmt.Sprintf("rule-%d", index+1)
		}
		if len(id) > 64 || strings.ContainsAny(id, "\r\n\t\x00") || seen[id] {
			return nil, errors.New("custom rule IDs must be unique and at most 64 characters")
		}
		seen[id] = true
		if len(rule.Pattern) == 0 || len(rule.Pattern) > 1024 || len(rule.Replacement) > 64 {
			return nil, fmt.Errorf("custom rule %s has an invalid length", id)
		}
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("custom rule %s has an invalid regular expression", id)
		}
		if pattern.MatchString("") {
			return nil, fmt.Errorf("custom rule %s can match an empty string", id)
		}
		replacement := rule.Replacement
		if replacement == "" {
			replacement = "[REDACTED]"
		}
		compiled.rules = append(compiled.rules, compiledRule{id: id, category: "custom", pattern: pattern, replacement: replacement})
	}
	for _, replacement := range compiled.rules {
		for _, detector := range compiled.rules {
			if detector.pattern.MatchString(replacement.replacement) {
				return nil, errors.New("redaction replacement conflicts with a detection rule")
			}
		}
	}
	return compiled, nil
}

func (c *Compiled) Mode() string {
	if c == nil {
		return ModeOff
	}
	return c.config.Mode
}

func Process(format string, body []byte, config Config) (Result, error) {
	compiled, err := Compile(config)
	if err != nil {
		return Result{}, err
	}
	return compiled.Process(format, body)
}

func (c *Compiled) Process(format string, body []byte) (Result, error) {
	result := Result{Body: body}
	if c == nil || c.config.Mode == ModeOff {
		return result, nil
	}
	if len(body) > MaxBodyBytes {
		if c.config.Mode == ModeObserve {
			result.Unchecked = []string{"inspection_incomplete"}
			return result, nil
		}
		return result, &Error{Kind: "limit"}
	}
	root, err := decodeUniqueObject(body)
	if err != nil {
		if c.config.Mode == ModeObserve {
			result.Unchecked = []string{"inspection_incomplete"}
			return result, nil
		}
		return result, err
	}
	w := walker{rules: c.rules, enforce: c.config.Mode == ModeEnforce, findings: map[string]int{}, unchecked: map[string]bool{}}
	if err := w.request(format, root); err != nil {
		if c.config.Mode == ModeObserve {
			result.Unchecked = []string{"inspection_incomplete"}
			return result, nil
		}
		return result, err
	}
	if c.config.Mode == ModeEnforce && c.config.Coverage == CoverageStrict && len(w.unchecked) > 0 {
		return result, &Error{Kind: "unsupported"}
	}
	result.Unchecked = sortedKeys(w.unchecked)
	result.Findings = sortedFindings(w.findings)
	for _, finding := range result.Findings {
		result.Matched += finding.Count
	}
	if c.config.Mode != ModeEnforce || !w.changed {
		return result, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return Result{}, errors.New("could not encode the processed request")
	}
	final := walker{rules: c.rules, findings: map[string]int{}, unchecked: map[string]bool{}}
	if err := final.request(format, root); err != nil || len(final.findings) > 0 {
		return Result{}, &Error{Kind: "final_match"}
	}
	result.Body, result.Changed = encoded, true
	return result, nil
}

func ProcessText(text string, config Config) (string, []Finding, error) {
	compiled, err := Compile(config)
	if err != nil {
		return text, nil, err
	}
	if config.Mode == ModeOff {
		return text, nil, nil
	}
	w := walker{rules: compiled.rules, enforce: compiled.config.Mode == ModeEnforce, findings: map[string]int{}, unchecked: map[string]bool{}}
	updated, changed, _, err := w.text(text)
	if err != nil {
		return text, nil, err
	}
	if !changed || compiled.config.Mode != ModeEnforce {
		updated = text
	}
	if changed && compiled.config.Mode == ModeEnforce {
		final := walker{rules: compiled.rules, findings: map[string]int{}}
		if _, _, hits, err := final.text(updated); err != nil || len(hits) > 0 {
			return text, nil, &Error{Kind: "final_match"}
		}
	}
	return updated, sortedFindings(w.findings), nil
}

type walker struct {
	rules         []compiledRule
	enforce       bool
	findings      map[string]int
	unchecked     map[string]bool
	inspectionErr error
	totalMatches  int
	nodes         int
	changed       bool
}

func (w *walker) request(format string, root map[string]any) error {
	for _, key := range []string{"previous_response_id", "conversation", "cachedContent", "cached_content", "prompt"} {
		if root[key] != nil && root[key] != "" {
			w.unchecked["remote_context"] = true
		}
	}
	for _, key := range []string{"user", "metadata"} {
		if value, changed, err := w.stringLeaves(root[key]); err != nil {
			return err
		} else if changed {
			root[key] = value
		}
	}
	switch format {
	case "openai-chat":
		if value, changed, err := w.textInput(root["input"]); err != nil {
			return err
		} else if changed {
			root["input"] = value
		}
		for _, item := range asArray(root["messages"]) {
			message := asMap(item)
			if message == nil {
				continue
			}
			if value, changed, err := w.content(message["content"]); err != nil {
				return err
			} else if changed {
				message["content"] = value
			}
			if err := w.inspectToolValue(asMap(message["function_call"])["arguments"]); err != nil {
				return err
			}
			if err := w.chatToolArguments(message["tool_calls"]); err != nil {
				return err
			}
		}
		if err := w.toolDescriptions(root["functions"]); err != nil {
			return err
		}
		if err := w.toolDescriptions(root["tools"]); err != nil {
			return err
		}
	case "openai-responses":
		if err := w.rootContent(root, "instructions"); err != nil {
			return err
		}
		if value, changed, err := w.responsesInput(root["input"]); err != nil {
			return err
		} else if changed {
			root["input"] = value
		}
		for _, item := range asArray(root["tools"]) {
			if err := w.toolDescriptions([]any{item}); err != nil {
				return err
			}
		}
	case "anthropic":
		if err := w.rootContent(root, "system"); err != nil {
			return err
		}
		for _, item := range asArray(root["messages"]) {
			message := asMap(item)
			if message == nil {
				continue
			}
			if value, changed, err := w.content(message["content"]); err != nil {
				return err
			} else if changed {
				message["content"] = value
			}
		}
		if err := w.toolDescriptions(root["tools"]); err != nil {
			return err
		}
	case "gemini":
		for _, item := range asArray(root["contents"]) {
			content := asMap(item)
			if content == nil {
				continue
			}
			if value, changed, err := w.content(content["parts"]); err != nil {
				return err
			} else if changed {
				content["parts"] = value
			}
		}
		for _, key := range []string{"systemInstruction", "system_instruction"} {
			if value, changed, err := w.content(root[key]); err != nil {
				return err
			} else if changed {
				root[key] = value
			}
		}
		if err := w.toolDescriptions(root["tools"]); err != nil {
			return err
		}
	default:
		w.unchecked["unsupported_protocol"] = true
	}
	known := map[string]bool{}
	for _, key := range strings.Fields("model stream stream_options messages input instructions system contents systemInstruction system_instruction tools functions tool_choice toolConfig tool_config parallel_tool_calls max_tokens max_completion_tokens max_output_tokens temperature top_p stop stop_sequences n seed presence_penalty frequency_penalty logit_bias logprobs top_logprobs response_format text reasoning thinking output_config service_tier store user metadata previous_response_id conversation cachedContent cached_content prompt truncation include background safety_settings safetySettings generationConfig generation_config type event_id stream_id generate response_id") {
		known[key] = true
	}
	for key := range root {
		if !known[key] {
			w.unchecked["unknown_field"] = true
		}
	}
	return nil
}

// stringLeaves is used only for protocol-defined free-form text containers.
func (w *walker) stringLeaves(value any) (any, bool, error) {
	switch item := value.(type) {
	case string:
		// Tool results may be encoded JSON; decode before inspecting leaves.
		if strings.HasPrefix(strings.TrimSpace(item), "{") || strings.HasPrefix(strings.TrimSpace(item), "[") {
			var decoded any
			decoder := json.NewDecoder(strings.NewReader(item))
			decoder.UseNumber()
			if decoder.Decode(&decoded) == nil {
				updated, changed, err := w.stringLeaves(decoded)
				if err != nil {
					return value, false, err
				}
				if changed {
					encoded, err := json.Marshal(updated)
					return string(encoded), true, err
				}
			}
		}
		updated, changed, _, err := w.text(item)
		return updated, changed, err
	case []any:
		changed := false
		for i, child := range item {
			updated, didChange, err := w.stringLeaves(child)
			if err != nil {
				return value, false, err
			}
			if didChange {
				item[i], changed = updated, true
			}
		}
		return item, changed, nil
	case map[string]any:
		changed := false
		for key, child := range item {
			updated, didChange, err := w.stringLeaves(child)
			if err != nil {
				return value, false, err
			}
			if didChange {
				item[key], changed = updated, true
			}
		}
		return item, changed, nil
	}
	return value, false, nil
}

func (w *walker) rootContent(root map[string]any, key string) error {
	value, changed, err := w.content(root[key])
	if err == nil && changed {
		root[key] = value
	}
	return err
}

func (w *walker) textInput(value any) (any, bool, error) {
	switch input := value.(type) {
	case string:
		updated, changed, _, err := w.text(input)
		return updated, changed, err
	case []any:
		changed := false
		for i, item := range input {
			if text, ok := item.(string); ok {
				updated, didChange, _, err := w.text(text)
				if err != nil {
					return value, false, err
				}
				if didChange {
					input[i], changed = updated, true
				}
			} else if _, ok := item.(float64); !ok {
				if _, ok := item.(json.Number); !ok {
					w.unchecked["embedding_input"] = true
				}
			}
		}
		return input, changed, nil
	default:
		return value, false, nil
	}
}

func (w *walker) content(value any) (any, bool, error) {
	w.nodes++
	if w.nodes > MaxNodes {
		return value, false, &Error{Kind: "limit"}
	}
	switch item := value.(type) {
	case string:
		if mediaPlaceholder.MatchString(item) {
			return item, false, nil
		}
		updated, changed, _, err := w.text(item)
		return updated, changed, err
	case []any:
		changed := false
		for i := range item {
			updated, didChange, err := w.content(item[i])
			if err != nil {
				return value, false, err
			}
			if didChange {
				item[i], changed = updated, true
			}
		}
		return item, changed, nil
	case map[string]any:
		if item["thought"] == true || hasSignature(item) {
			w.unchecked["signature"] = true
			return item, false, nil
		}
		typ := stringValue(item["type"])
		if typ == "document" {
			w.unchecked["file"] = true
			return item, false, nil
		}
		if isOpaqueContentType(typ) {
			w.unchecked[contentKind(typ)] = true
			return item, false, nil
		}
		if typ == "tool_result" {
			updated, changed, err := w.stringLeaves(item["content"])
			if changed {
				item["content"] = updated
			}
			return item, changed, err
		}
		if typ == "tool_use" {
			return item, false, w.inspectToolValue(item["input"])
		}
		if fn := asMap(item["functionCall"]); fn != nil {
			return item, false, w.inspectToolValue(fn["args"])
		}
		if fn := asMap(item["functionResponse"]); fn != nil {
			updated, changed, err := w.stringLeaves(fn["response"])
			if changed {
				fn["response"] = updated
			}
			return item, changed, err
		}
		for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
			if item[key] != nil {
				w.unchecked["file"] = true
			}
		}
		if typ != "" && !isTextContentType(typ) && typ != "tool_result" {
			w.unchecked["unknown_content"] = true
		}
		changed := false
		if text, ok := item["text"].(string); ok && (typ == "" || isTextContentType(typ)) {
			updated, didChange, _, err := w.text(text)
			if err != nil {
				return value, false, err
			}
			if didChange {
				item["text"], changed = updated, true
			}
		}
		for _, key := range []string{"content", "parts", "output", "source", "data"} {
			nested := item[key]
			if key == "data" && stringValue(item["type"]) != "document" {
				continue
			}
			if _, ok := nested.(string); !ok && nested == nil {
				continue
			}
			updated, didChange, err := w.content(nested)
			if err != nil {
				return value, false, err
			}
			if didChange {
				item[key], changed = updated, true
			}
		}
		for key := range item {
			switch key {
			case "type", "text", "content", "parts", "output", "source", "data", "cache_control", "role", "tool_use_id", "is_error", "annotations", "index":
			default:
				w.unchecked["unknown_content_field"] = true
			}
		}
		return item, changed, nil
	}
	return value, false, nil
}

func (w *walker) responsesInput(value any) (any, bool, error) {
	switch input := value.(type) {
	case string:
		updated, changed, _, err := w.text(input)
		return updated, changed, err
	case []any:
		changed := false
		for i, raw := range input {
			item := asMap(raw)
			if item == nil {
				updated, didChange, err := w.content(raw)
				if err != nil {
					return value, false, err
				}
				if didChange {
					input[i], changed = updated, true
				}
				continue
			}
			typ := stringValue(item["type"])
			if typ == "function_call" {
				if err := w.inspectToolValue(item["arguments"]); err != nil {
					return value, false, err
				}
				continue
			}
			if hasSignature(item) || typ == "reasoning" || typ == "item_reference" {
				w.unchecked["opaque_history"] = true
				continue
			}
			if typ == "function_call_output" {
				if updated, didChange, err := w.stringLeaves(item["output"]); err != nil {
					return value, false, err
				} else if didChange {
					item["output"], changed = updated, true
				}
				continue
			}
			if typ != "" && typ != "message" {
				w.unchecked["unknown_input_item"] = true
			}
			if updated, didChange, err := w.content(item["content"]); err != nil {
				return value, false, err
			} else if didChange {
				item["content"], changed = updated, true
			}
		}
		return input, changed, nil
	case map[string]any:
		if prompt, ok := input["prompt"].(string); ok {
			updated, changed, _, err := w.text(prompt)
			if err != nil {
				return value, false, err
			}
			if changed {
				input["prompt"] = updated
			}
			return input, changed, nil
		}
	}
	return value, false, nil
}

func (w *walker) chatToolArguments(value any) error {
	for _, raw := range asArray(value) {
		toolCall := asMap(raw)
		fn := asMap(toolCall["function"])
		if fn == nil {
			continue
		}
		if err := w.inspectToolValue(fn["arguments"]); err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) inspectToolValue(value any) error {
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) != nil {
			if w.enforce {
				return &Error{Kind: "tool_argument"}
			}
			decoded = text
		}
		if w.inspectStrings(decoded, 0) {
			if w.enforce {
				return &Error{Kind: "tool_argument"}
			}
		}
		return w.inspectionErr
	}
	if w.inspectStrings(value, 0) {
		if w.enforce {
			return &Error{Kind: "tool_argument"}
		}
	}
	return w.inspectionErr
}

func (w *walker) inspectStringsValue(value any) error {
	if w.inspectStrings(value, 0) && w.enforce {
		return &Error{Kind: "tool_argument"}
	}
	return w.inspectionErr
}

func (w *walker) inspectStrings(value any, depth int) bool {
	if depth > MaxDepth {
		w.inspectionErr = &Error{Kind: "limit"}
		w.unchecked["depth_limit"] = true
		return false
	}
	w.nodes++
	if w.nodes > MaxNodes {
		w.inspectionErr = &Error{Kind: "limit"}
		w.unchecked["node_limit"] = true
		return false
	}
	switch item := value.(type) {
	case string:
		_, _, findings, err := w.text(item)
		if err != nil {
			w.inspectionErr = err
		}
		return len(findings) > 0
	case []any:
		matched := false
		for _, child := range item {
			matched = w.inspectStrings(child, depth+1) || matched
		}
		return matched
	case map[string]any:
		matched := false
		for _, child := range item {
			matched = w.inspectStrings(child, depth+1) || matched
		}
		return matched
	}
	return false
}

func (w *walker) toolDescriptions(value any) error {
	return w.walkDescriptions(value, 0)
}

func (w *walker) walkDescriptions(value any, depth int) error {
	if depth > MaxDepth {
		return &Error{Kind: "limit"}
	}
	w.nodes++
	if w.nodes > MaxNodes {
		return &Error{Kind: "limit"}
	}
	switch item := value.(type) {
	case []any:
		for _, child := range item {
			if err := w.walkDescriptions(child, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, child := range item {
			if key == "enum" || key == "const" || key == "default" {
				if err := w.inspectStringsValue(child); err != nil {
					return err
				}
				continue
			}
			if strings.EqualFold(key, "description") || strings.EqualFold(key, "title") {
				if text, ok := child.(string); ok {
					updated, changed, _, err := w.text(text)
					if err != nil {
						return err
					}
					if changed {
						item[key] = updated
					}
					continue
				}
			}
			if err := w.walkDescriptions(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *walker) text(original string) (string, bool, map[string]int, error) {
	if len(original) > 1<<20 {
		return original, false, nil, &Error{Kind: "limit"}
	}
	counts := make(map[string]int)
	matches := make([]textMatch, 0)
	for order, rule := range w.rules {
		for _, bounds := range rule.pattern.FindAllStringIndex(original, MaxMatches+1) {
			start, end := bounds[0], bounds[1]
			value := original[start:end]
			if value == "" || rule.builtin && (!validCandidate(rule.category, value) || !validBoundary(rule.category, original, start, end)) {
				continue
			}
			if len(matches) >= MaxMatches {
				return original, false, nil, &Error{Kind: "limit"}
			}
			matches = append(matches, textMatch{start: start, end: end, category: rule.category, replacement: rule.replacement, order: order})
		}
	}
	if len(matches) == 0 {
		return original, false, counts, nil
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		if matches[i].end != matches[j].end {
			return matches[i].end > matches[j].end
		}
		return matches[i].order < matches[j].order
	})
	selected := matches[:0]
	cursor := 0
	for _, match := range matches {
		if match.start < cursor {
			continue
		}
		selected = append(selected, match)
		cursor = match.end
		counts[match.category]++
		w.findings[match.category]++
		w.totalMatches++
		if w.totalMatches > MaxMatches {
			return original, false, nil, &Error{Kind: "limit"}
		}
	}
	var output strings.Builder
	output.Grow(len(original))
	cursor = 0
	for _, match := range selected {
		output.WriteString(original[cursor:match.start])
		if w.enforce {
			output.WriteString(match.replacement)
		} else {
			output.WriteString(original[match.start:match.end])
		}
		cursor = match.end
	}
	output.WriteString(original[cursor:])
	text := output.String()
	changed := w.enforce && text != original
	if changed {
		w.changed = true
	}
	return text, changed, counts, nil
}

func validCandidate(category, value string) bool {
	switch category {
	case "api_key":
		if strings.HasPrefix(strings.ToLower(value), "bearer ") || strings.HasPrefix(strings.ToLower(value), "bearer\t") {
			_, token, _ := strings.Cut(value, " ")
			return hasLetterAndDigit(token)
		}
		return hasLetterAndDigit(value)
	case "email":
		return !strings.Contains(strings.ToLower(value), ".png") && !strings.Contains(strings.ToLower(value), ".jpg") && !strings.Contains(strings.ToLower(value), ".svg")
	case "id_card":
		return validIDCard(value)
	case "bank_card":
		return luhn(value)
	case "ipv4":
		ip := net.ParseIP(value)
		return ip != nil && ip.To4() != nil
	default:
		return true
	}
}

func validBoundary(category, text string, start, end int) bool {
	isWord := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
	}
	switch category {
	case "api_key":
		if end < len(text) && (isWord(text[end]) || text[end] == '-') {
			return false
		}
	case "phone":
		if start > 0 && (isWord(text[start-1]) || text[start-1] == '+' || text[start-1] == '.') {
			return false
		}
	case "ipv4":
		if start > 0 && (isWord(text[start-1]) || text[start-1] == '.') {
			return false
		}
		if end < len(text) && (isWord(text[end]) || text[end] == '.') {
			return false
		}
		prefix := strings.ToLower(strings.TrimSpace(text[:start]))
		wordStart := strings.LastIndexAny(prefix, " \t\r\n,;:()[]{}") + 1
		word := prefix[wordStart:]
		if word == "version" || word == "versions" || word == "ver" || word == "v" {
			return false
		}
	}
	return true
}

func hasLetterAndDigit(value string) bool {
	letter, digit := false, false
	for _, r := range value {
		letter = letter || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		digit = digit || r >= '0' && r <= '9'
	}
	return letter && digit
}

func validIDCard(value string) bool {
	if len(value) != 18 {
		return false
	}
	date, err := time.Parse("20060102", value[6:14])
	if err != nil || date.Format("20060102") != value[6:14] {
		return false
	}
	weights := [...]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checks := "10X98765432"
	sum := 0
	for i, weight := range weights {
		sum += int(value[i]-'0') * weight
	}
	return checks[sum%11] == byte(strings.ToUpper(value[17:])[0])
}

func luhn(value string) bool {
	var digits []byte
	for i := 0; i < len(value); i++ {
		if value[i] >= '0' && value[i] <= '9' {
			digits = append(digits, value[i]-'0')
		}
	}
	sum, double := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		digit := int(digits[i])
		if double {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		double = !double
	}
	return len(digits) >= 13 && sum%10 == 0
}

func isOpaqueContentType(typ string) bool {
	switch typ {
	case "image", "input_image", "output_image", "image_url", "input_audio", "output_audio", "audio", "input_file", "file", "file_search_call", "computer_call", "video":
		return true
	default:
		return false
	}
}

func contentKind(typ string) string {
	if strings.Contains(typ, "image") {
		return "image"
	}
	if strings.Contains(typ, "audio") {
		return "audio"
	}
	return "file"
}

func isTextContentType(typ string) bool {
	switch typ {
	case "text", "input_text", "output_text", "text_delta", "tool_result", "document":
		return true
	default:
		return false
	}
}

func hasSignature(item map[string]any) bool {
	for _, key := range []string{"signature", "thought_signature", "thoughtSignature", "redacted_thinking", "encrypted_content"} {
		if item[key] != nil && item[key] != "" {
			return true
		}
	}
	return stringValue(item["type"]) == "redacted_thinking"
}

func decodeUniqueObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	nodes := 0
	if err := consumeValue(decoder, 0, &nodes); err != nil {
		return nil, errors.New("invalid JSON request")
	}
	if _, err := decoder.Token(); errors.Is(err, io.EOF) {
		// The first token walk must consume exactly one JSON value.
	} else if err != nil {
		return nil, errors.New("invalid JSON request")
	} else {
		return nil, errors.New("invalid JSON request")
	}
	var root map[string]any
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return root, nil
}

func consumeValue(decoder *json.Decoder, depth int, nodes *int) error {
	*nodes++
	if depth > MaxDepth || *nodes > MaxNodes {
		return errors.New("depth limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON key")
			}
			seen[key] = true
			if err := consumeValue(decoder, depth+1, nodes); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeValue(decoder, depth+1, nodes); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func cloneConfig(config Config) Config {
	config.Categories = append([]string(nil), config.Categories...)
	config.Rules = append([]Rule(nil), config.Rules...)
	if config.Replacements != nil {
		copyMap := make(map[string]string, len(config.Replacements))
		for key, value := range config.Replacements {
			copyMap[key] = value
		}
		config.Replacements = copyMap
	}
	return config
}

func asArray(value any) []any        { items, _ := value.([]any); return items }
func asMap(value any) map[string]any { item, _ := value.(map[string]any); return item }
func stringValue(value any) string   { text, _ := value.(string); return text }

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedFindings(values map[string]int) []Finding {
	keys := make([]string, 0, len(values))
	for key, count := range values {
		if count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]Finding, 0, len(keys))
	for _, key := range keys {
		out = append(out, Finding{Category: key, Count: values[key]})
	}
	return out
}
