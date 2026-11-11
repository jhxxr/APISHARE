package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// A tool-count rejection is an endpoint capability mismatch, not evidence of a
// broken channel or weak model. Never truncate client tools to hide the limit.
type upstreamToolLimitError struct {
	Limit, ClientCount, WireCount int
}

func (e *upstreamToolLimitError) Error() string {
	return fmt.Sprintf("upstream tools limit is %d (client tool entries: %d, final upstream tool entries: %d); all tools were preserved; reduce enabled tools/MCP servers or use an upstream supporting this tool count",
		e.Limit, e.ClientCount, e.WireCount)
}

var toolsArrayLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bmaximum number of items is\s+([0-9]+)\b`),
	regexp.MustCompile(`(?i)\bmaximum (?:array )?length(?: of)?\s+([0-9]+)\b`),
	regexp.MustCompile(`(?i)\bat most\s+([0-9]+)\s+(?:tools|items|elements)\b`),
}

func requestToolEntries(body []byte) int {
	var request struct {
		Tools     []json.RawMessage `json:"tools"`
		Functions []json.RawMessage `json:"functions"`
	}
	if json.Unmarshal(body, &request) != nil {
		return 0
	}
	return len(request.Tools) + len(request.Functions)
}

// Require an explicitly identified top-level tools array and a measured
// over-limit final request. A schema's maxItems, missing tool name or invalid
// arguments must not become a retryable tools-count error.
func upstreamToolsLimit(status int, body []byte, wireCount int) int {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return 0
	}
	var envelope struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = string(body)
	}
	lower := strings.ToLower(message)
	if envelope.Error.Param != "" && envelope.Error.Param != "tools" {
		return 0
	}
	if envelope.Error.Param != "tools" &&
		!strings.Contains(lower, "'tools'") && !strings.Contains(lower, `"tools"`) {
		return 0
	}
	for _, pattern := range toolsArrayLimitPatterns {
		match := pattern.FindStringSubmatch(message)
		if len(match) != 2 {
			continue
		}
		limit, err := strconv.Atoi(match[1])
		if err == nil && limit > 0 && wireCount > limit {
			return limit
		}
	}
	return 0
}
