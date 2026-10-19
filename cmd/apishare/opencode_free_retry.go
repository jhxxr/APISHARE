package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type openCodeFreeAttempt struct {
	request      *http.Request
	response     *http.Response
	body         []byte
	tools        openCodeToolCompatibility
	previousPath string
}

// Try the other official Zen entry once, only after a structured FreeTierError
// and before forwarding model output. Rebuild from the original client body:
// a legacy read stub/alias must never become an inference CLI declaration.
// Keep the same credential, proxy, session and context on both attempts.
func retryOpenCodeFreeTier(up *Upstream, req *http.Request, resp *http.Response, originalBody []byte, format clientFormat, model string, incoming http.Header, do func(*http.Request) (*http.Response, error)) (*openCodeFreeAttempt, error) {
	if resp.StatusCode != http.StatusForbidden || !up.isOpenCode() || up.openCodeTier() != "zen" || (!up.openCodePublic() && !up.openCodeFreeModel(model)) {
		return nil, nil
	}
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, "opencode.ai") || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" {
		return nil, nil
	}
	endpoint := nativeEndpoint(format, &canonReq{Model: model, Stream: true})
	var alternativeBase string
	switch req.URL.Path {
	case "/zen" + endpoint:
		// Foreign tool schemas cannot safely be replaced by CLI schemas. Keep
		// their legacy alias path until an actual argument bridge is available.
		var payload struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if json.Unmarshal(originalBody, &payload) != nil {
			return nil, nil
		}
		if len(payload.Tools) > 0 {
			compatible, err := openCodeNativeToolContracts(payload.Tools, format)
			if err != nil || !compatible {
				return nil, nil
			}
		}
		alternativeBase = "https://opencode.ai/inference"
	case "/inference" + endpoint, "/inference/openai" + endpoint, "/inference/anthropic" + endpoint:
		alternativeBase = "https://opencode.ai/zen"
	default:
		return nil, nil
	}

	route := *up
	route.BaseURL = alternativeBase
	attempt := &openCodeFreeAttempt{previousPath: req.URL.EscapedPath()}
	var shaped bool
	var err error
	attempt.body, shaped, err = route.shapeOpenCodeFreeRequest(originalBody, format, model, &attempt.tools)
	if err != nil || !shaped {
		return nil, nil
	}
	attempt.request, err = http.NewRequestWithContext(req.Context(), req.Method, route.openCodeFreeEndpointURL(format, endpoint), bytes.NewReader(attempt.body))
	if err != nil {
		return nil, nil
	}
	attempt.request.Header.Set("Content-Type", "application/json")
	if !openCodeFreeFingerprintRequest(up, attempt.request, model) {
		attempt.request.Header.Set("Accept-Encoding", "identity")
	}
	setOpenCodeHeaders(attempt.request, up.openCodeAPIKey(), format, req.Header.Get("x-opencode-session"), incoming, nil, attempt.body)
	if err := up.applyHeaders(attempt.request, incoming); err != nil {
		return nil, nil
	}

	// Bound error inspection and restore it byte-for-byte when it is not the
	// eligibility error. Never retry an ordinary 403 or a large/invalid body.
	if relayNetwork.BodyIdleTimeout > 0 {
		resp.Body = newActivityDeadlineBody(resp.Body, relayNetwork.BodyIdleTimeout)
	}
	errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	resp.Body = &openCodeRestoredBody{Reader: io.MultiReader(bytes.NewReader(errorBody), resp.Body), closer: resp.Body}
	if readErr != nil || len(errorBody) > 16<<10 {
		return nil, nil
	}
	var failure struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(errorBody, &failure) != nil || failure.Error.Type != "FreeTierError" {
		return nil, nil
	}
	resp.Body.Close()
	attempt.response, err = do(attempt.request)
	return attempt, err
}
