package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func openCodeRetryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		return time.Time{}
	}
	if until, err := http.ParseTime(value); err == nil && until.After(now) {
		return until
	}
	return time.Time{}
}

func healthMarkRetryAfter(id int64, until time.Time) {
	if until.IsZero() {
		return
	}
	upHealth.Lock()
	defer upHealth.Unlock()
	if until.After(upHealth.until[id]) {
		upHealth.until[id] = until
	}
}

// Only the explicit expired-reference error qualifies. Ordinary validation
// failures, reasoning settings, messages and function calls are kept intact.
func repairOpenCodeReasoning(body, errorBody []byte) ([]byte, bool) {
	message := strings.ToLower(string(errorBody))
	if !strings.Contains(message, "reasoning item") && !strings.Contains(message, "reasoning reference") {
		return body, false
	}
	expired := false
	for _, marker := range []string{"not found", "expired", "does not exist", "no longer"} {
		expired = expired || strings.Contains(message, marker)
	}
	if !expired {
		return body, false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	changed := false
	if previous, ok := payload["previous_response_id"]; ok && string(previous) != "null" && string(previous) != `""` {
		delete(payload, "previous_response_id")
		changed = true
	}
	var input []json.RawMessage
	if json.Unmarshal(payload["input"], &input) == nil && input != nil {
		kept := make([]json.RawMessage, 0, len(input))
		removed := false
		for _, item := range input {
			var kind struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(item, &kind) == nil && kind.Type == "reasoning" {
				removed = true
				continue
			}
			kept = append(kept, item)
		}
		if removed {
			payload["input"], _ = json.Marshal(kept)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(payload)
	return encoded, err == nil
}

func retryOpenCodeReasoning(client *http.Client, req *http.Request, resp *http.Response, body []byte, state *messagesKeepalive, cancel context.CancelFunc) (*http.Response, error) {
	// Bound the inspection, restoring the exact original error if it is not
	// eligible. This runs before any upstream response is delivered downstream.
	errorBody, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil {
		return resp, err
	}
	resp.Body = &openCodeRestoredBody{Reader: io.MultiReader(bytes.NewReader(errorBody), resp.Body), closer: resp.Body}
	if len(errorBody) > 16<<10 {
		return resp, nil
	}
	repaired, changed := repairOpenCodeReasoning(body, errorBody)
	if !changed {
		return resp, nil
	}
	resp.Body.Close()
	retry := req.Clone(req.Context())
	retry.Body = io.NopCloser(bytes.NewReader(repaired))
	retry.ContentLength = int64(len(repaired))
	retry.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(repaired)), nil }
	return messagesHTTPDo(client, retry, state, cancel)
}

type openCodeRestoredBody struct {
	io.Reader
	closer io.Closer
}

func (b *openCodeRestoredBody) Close() error { return b.closer.Close() }
