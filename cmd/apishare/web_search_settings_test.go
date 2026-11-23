package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWebSearchAdminSettings(t *testing.T) {
	router := testProxyAdminRouter(t)
	t.Setenv("RESPONSES_BRAVE_SEARCH_API_KEY", "environment-secret")
	const path = "settings/web-search"
	for _, method := range []string{"GET", "PUT"} {
		if w := proxyAdminRequest(router, method, path, `{}`, false); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s returned %d", method, w.Code)
		}
	}
	call := func(method, body string, status int) map[string]any {
		t.Helper()
		w := proxyAdminRequest(router, method, path, body, true)
		if w.Code != status || strings.Contains(w.Body.String(), "-secret") {
			t.Fatalf("%s status=%d body=%s", method, w.Code, w.Body.String())
		}
		m, err := decodeProtocolObject(w.Body.Bytes())
		if err != nil || m["api_key"] != nil {
			t.Fatalf("admin response is invalid or exposed credentials: %v", err)
		}
		return m
	}
	task := &relayTask{format: FormatOpenAIResponse, body: []byte(responsesSearchInput)}
	checkEffective := func(provider, key string, enabled bool) {
		t.Helper()
		s, search := task.responsesSearchFallback()
		if (search != nil) != enabled || s.Provider != provider || s.APIKey != key {
			t.Fatalf("unexpected effective provider=%q enabled=%v", s.Provider, search != nil)
		}
	}
	if m := call("GET", "", 200); m["provider"] != "environment" || m["environment_key_set"] != true || m["key_saved"] != false {
		t.Fatalf("incorrect initial state: %v", m)
	}
	checkEffective("brave", "environment-secret", true)
	if m := call("PUT", `{"provider":"tavily","api_key":"tavily-secret"}`, 200); m["key_saved"] != true || m["provider"] != "tavily" {
		t.Fatalf("incorrect save response: %v", m)
	}
	call("PUT", `{"provider":"tavily"}`, 200)
	checkEffective("tavily", "tavily-secret", true)
	call("PUT", `{"provider":"exa"}`, 400)
	call("PUT", `{"provider":"tavily","api_key":"invalid\nkey"}`, 400)
	call("PUT", `{"provider":"https://example.invalid","api_key":"invalid-secret"}`, 400)
	checkEffective("tavily", "tavily-secret", true)
	call("PUT", `{"provider":"exa","api_key":"exa-secret"}`, 200)
	checkEffective("exa", "exa-secret", true)
	call("PUT", `{"provider":"disabled"}`, 200)
	checkEffective("", "", false)
	if m := call("GET", "", 200); m["provider"] != "disabled" || m["key_saved"] != false {
		t.Fatalf("incorrect disabled state: %v", m)
	}
	call("PUT", `{"provider":"environment"}`, 200)
	checkEffective("brave", "environment-secret", true)
}

func TestAdditionalWebSearchProviders(t *testing.T) {
	for _, tc := range []struct{ provider, endpoint, header, credential, fixture string }{
		{"tavily", tavilySearchEndpoint, "Authorization", "Bearer test-key", `{"results":[{"url":"https://go.dev/","title":"Go","content":"Search snippet"}]}`},
		{"exa", exaSearchEndpoint, "X-API-Key", "test-key", `{"results":[{"url":"https://go.dev/","title":"Go","highlights":["Search","snippet"]}]}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			original := webSearchHTTPClient.Transport
			t.Cleanup(func() { webSearchHTTPClient.Transport = original })
			webSearchHTTPClient.Transport = jobRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.String() != tc.endpoint || req.Header.Get(tc.header) != tc.credential || req.Header.Get("Content-Type") != "application/json" {
					t.Error("incorrect provider endpoint, HTTP method or credentials")
				}
				body, _ := io.ReadAll(req.Body)
				m, err := decodeProtocolObject(body)
				if err != nil || m["query"] != "Go documentation" || strings.Contains(string(body), "test-key") {
					t.Errorf("unexpected search payload: %q", body)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.fixture))}, nil
			})
			result, err := searchWeb(context.Background(), webSearchSettings{Provider: tc.provider, APIKey: "test-key"}, "Go documentation")
			if err != nil || len(result) != 1 || result[0] != (webSearchResult{URL: "https://go.dev/", Title: "Go", Description: "Search snippet"}) {
				t.Fatalf("invalid normalized search response: %+v / %v", result, err)
			}
		})
	}
}
