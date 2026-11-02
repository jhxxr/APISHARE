package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

const internAIDiscoveryHost = "discovery-api.intern-ai.org.cn"

func internAIAnthropicEndpoint(format clientFormat, target string) bool {
	if format != FormatAnthropic {
		return false
	}
	endpoint, err := url.Parse(target)
	return err == nil && strings.EqualFold(endpoint.Hostname(), internAIDiscoveryHost)
}

// InternAI's Messages adapter rejects thinking.display=updates even with the
// beta header, and rejects empty text blocks in otherwise valid tool history.
// Keep these repairs scoped to its endpoint rather than rewriting native
// requests to providers that support the original content.
func normalizeInternAIAnthropicRequest(body []byte, format clientFormat, target string) ([]byte, error) {
	if !internAIAnthropicEndpoint(format, target) {
		return body, nil
	}
	payload, err := decodeProtocolObject(body)
	if err != nil {
		return body, nil
	}
	changed := false
	if thinking := object(payload["thinking"]); thinking["display"] == "updates" {
		delete(thinking, "display")
		changed = true
	}
	if blocks, cleaned := cleanInternAITextBlocks(payload["system"]); cleaned {
		if len(blocks) == 0 {
			delete(payload, "system")
		} else {
			payload["system"] = blocks
		}
		changed = true
	}
	messages := array(payload["messages"])
	kept := make([]any, 0, len(messages))
	for index, raw := range messages {
		message := object(raw)
		if blocks, cleaned := cleanInternAITextBlocks(message["content"]); cleaned {
			role := str(message["role"])
			if len(blocks) == 0 && (role == "user" || role == "assistant") {
				// An empty final user turn has no recoverable prompt. Preserve
				// its validation error rather than replaying an earlier turn.
				if role == "user" && index == len(messages)-1 {
					kept = append(kept, raw)
					continue
				}
				changed = true
				continue
			}
			message["content"] = blocks
			changed = true
		}
		kept = append(kept, raw)
	}
	if !changed || (len(messages) > 0 && len(kept) == 0) {
		return body, nil
	}
	if len(messages) > 0 {
		payload["messages"] = kept
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), nil
}

func normalizeInternAIAnthropicHeaders(headers http.Header, format clientFormat, target string) {
	if !internAIAnthropicEndpoint(format, target) {
		return
	}
	var kept []string
	changed := false
	for _, value := range headers.Values("anthropic-beta") {
		for _, beta := range strings.Split(value, ",") {
			beta = strings.TrimSpace(beta)
			if beta == anthropicThinkingDisplayUpdatesBeta {
				changed = true
			} else if beta != "" {
				kept = append(kept, beta)
			}
		}
	}
	if changed {
		headers.Del("anthropic-beta")
		mergeAnthropicBetaHeaders(headers, kept...)
	}
}

// Only visit protocol text blocks and tool-result content. Tool inputs,
// thinking/signatures, images and other opaque blocks must remain intact.
func cleanInternAITextBlocks(content any) ([]any, bool) {
	blocks, ok := content.([]any)
	if !ok {
		return nil, false
	}
	kept := make([]any, 0, len(blocks))
	changed := false
	for _, raw := range blocks {
		block := object(raw)
		switch str(block["type"]) {
		case "text":
			text, isString := block["text"].(string)
			if block["text"] == nil || (isString && text == "") {
				changed = true
				continue
			}
		case "tool_result":
			if nested, cleaned := cleanInternAITextBlocks(block["content"]); cleaned {
				block["content"] = nested
				changed = true
			}
		}
		kept = append(kept, raw)
	}
	return kept, changed
}
