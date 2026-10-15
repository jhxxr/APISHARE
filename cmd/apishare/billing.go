package main

import (
	"strings"
	"sync"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// 计费优先级：
//  1. 上游报告的真实 usage（非流式响应、Anthropic 流式事件、OpenAI include_usage 尾块）
//  2. tiktoken 本地分词估算（OpenAI 系模型精确，其他模型近似）
//  3. 字符启发式估算（tiktoken 不可用/离线时的兜底）

type tokenEncodingEntry struct {
	encoding *tiktoken.Tiktoken
	ready    chan struct{}
	retryAt  time.Time
}

type tokenEncodingCache struct {
	sync.RWMutex
	entries map[string]*tokenEncodingEntry
	load    func(string) (*tiktoken.Tiktoken, error)
}

var tokenEncodings = tokenEncodingCache{load: tiktoken.GetEncoding}

// Cold loads run in the background. A slow or unavailable vocabulary server
// must never park inference or billing; requests use the existing heuristic.
func (cache *tokenEncodingCache) get(name string) *tiktoken.Tiktoken {
	cache.RLock()
	entry := cache.entries[name]
	if entry != nil && (entry.encoding != nil || entry.retryAt.IsZero() || time.Now().Before(entry.retryAt)) {
		encoding := entry.encoding
		cache.RUnlock()
		return encoding
	}
	cache.RUnlock()
	cache.Lock()
	defer cache.Unlock()
	entry = cache.entries[name]
	if entry != nil && (entry.encoding != nil || entry.retryAt.IsZero() || time.Now().Before(entry.retryAt)) {
		return entry.encoding
	}
	if cache.entries == nil {
		cache.entries = make(map[string]*tokenEncodingEntry)
	}
	entry = &tokenEncodingEntry{ready: make(chan struct{})}
	cache.entries[name] = entry
	go func() {
		encoding, err := cache.load(name)
		cache.Lock()
		defer cache.Unlock()
		if err == nil {
			entry.encoding = encoding
		} else {
			entry.retryAt = time.Now().Add(time.Minute)
		}
		close(entry.ready)
	}()
	return nil
}

func tokenEncodingName(model string) string {
	if name := tiktoken.MODEL_TO_ENCODING[model]; name != "" {
		return name
	}
	for prefix, name := range tiktoken.MODEL_PREFIX_TO_ENCODING {
		if strings.HasPrefix(model, prefix) {
			return name
		}
	}
	return ""
}

func warmTokenEncodings() {
	for _, name := range []string{tiktoken.MODEL_CL100K_BASE, tiktoken.MODEL_O200K_BASE} {
		tokenEncodings.get(name)
	}
}

func countTokens(model, text string) int {
	if text == "" {
		return 0
	}
	if name := tokenEncodingName(model); name != "" {
		if encoding := tokenEncodings.get(name); encoding != nil {
			return len(encoding.Encode(text, nil, nil))
		}
	}
	return estimateTokens(text)
}

func estimatePromptTokens(task *relayTask) int {
	cr := task.canon
	if cr == nil {
		return 0
	}
	// Keep the stable session prefix independent from accounting. Ordinary
	// conversations include every textual message and tool definition/result.
	if cr.ConversionError != nil || (cr.System == "" && len(cr.Messages) == 0) {
		return estimateTokensModel(task.model, cr.InputText)
	}
	var text strings.Builder
	appendText := func(value string) {
		if value != "" {
			text.WriteString(value)
			text.WriteByte('\n')
		}
	}
	appendText(cr.System)
	for _, msg := range cr.Messages {
		appendText(msg.Text)
		for _, call := range msg.Calls {
			appendText(call.Name)
			appendText(call.Arguments)
		}
	}
	for _, tool := range cr.Tools {
		appendText(tool.Name)
		appendText(tool.Description)
		appendText(jsonText(tool.Schema))
	}
	return estimateTokensModel(task.model, text.String())
}

// estimateTokens 混合启发式：ASCII 约 4 字符/token，CJK 等宽字符约 1 字符/token。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range s {
		if r > 0x2E7F { // CJK 统一表意文字、假名、谚文、Emoji 等
			wide++
		} else {
			ascii++
		}
	}
	n := ascii/4 + wide
	if n < 1 {
		n = 1
	}
	return n
}

func estimateTokensModel(model, text string) int {
	// 非 OpenAI 模型 tiktoken 不认识，直接走启发式，避免每次都查失败
	if !strings.HasPrefix(model, "gpt") && !strings.HasPrefix(model, "o1") &&
		!strings.HasPrefix(model, "o3") && !strings.HasPrefix(model, "o4") &&
		!strings.HasPrefix(model, "text-embedding") {
		return estimateTokens(text)
	}
	return countTokens(model, text)
}
