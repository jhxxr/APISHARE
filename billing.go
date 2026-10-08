package main

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// 计费优先级：
//  1. 上游报告的真实 usage（非流式响应、Anthropic 流式事件、OpenAI include_usage 尾块）
//  2. tiktoken 本地分词估算（OpenAI 系模型精确，其他模型近似）
//  3. 字符启发式估算（tiktoken 不可用/离线时的兜底）

var encCache sync.Map // model -> *tiktoken.Encoding

func countTokens(model, text string) int {
	if text == "" {
		return 0
	}
	if enc, ok := encCache.Load(model); ok {
		return len(enc.(*tiktoken.Tiktoken).Encode(text, nil, nil))
	}
	enc, err := tiktoken.EncodingForModel(model)
	if err != nil {
		return estimateTokens(text)
	}
	encCache.Store(model, enc)
	return len(enc.Encode(text, nil, nil))
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
