package main

import (
	"net/http"
	"strings"
)

// An exit-level abuse rejection cannot be fixed by changing the client shape,
// lane or credential. Keep the original response for relay error handling.
func openCodeFreeIPBlocked(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "abusive traffic") ||
		strings.Contains(text, "abuse traffic") ||
		strings.Contains(text, "abuse of traffic") ||
		(strings.Contains(text, "free models aren't available") && strings.Contains(text, "from your current network"))
}
