package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

const anthropicThinkingDisplayUpdatesBeta = "thinking-display-updates-2026-08-18"

// Merge generated and client beta flags, including repeated header lines.
func mergeAnthropicBetaHeaders(headers http.Header, values ...string) {
	var betas []string
	seen := make(map[string]bool)
	add := func(value string) {
		for _, beta := range strings.Split(value, ",") {
			beta = strings.TrimSpace(beta)
			if beta != "" && !seen[beta] {
				seen[beta] = true
				betas = append(betas, beta)
			}
		}
	}
	for _, value := range headers.Values("anthropic-beta") {
		add(value)
	}
	for _, value := range values {
		add(value)
	}
	if len(betas) > 0 {
		headers.Set("anthropic-beta", strings.Join(betas, ","))
	} else {
		headers.Del("anthropic-beta")
	}
}

func ensureAnthropicThinkingDisplayBeta(headers http.Header, body []byte) {
	var request struct {
		Thinking struct {
			Display string `json:"display"`
		} `json:"thinking"`
	}
	if json.Unmarshal(body, &request) == nil && request.Thinking.Display == "updates" {
		mergeAnthropicBetaHeaders(headers, anthropicThinkingDisplayUpdatesBeta)
	}
}
