package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const braveSearchEndpoint = "https://api.search.brave.com/res/v1/web/search"

var braveSearchClient = &http.Client{
	Timeout: 10 * time.Second,
	// Never carry the provider credential to a redirected host.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type responsesSearchRequest struct {
	query  string
	tool   map[string]any
	choice any
}

// Like sub2api's Messages emulation, this handles a search-only request, not
// a model turn that can choose between search, other tools and an answer.
func parseResponsesSearchFallback(body []byte) *responsesSearchRequest {
	m, err := decodeProtocolObject(body)
	if err != nil {
		return nil
	}
	for _, field := range []string{"previous_response_id", "conversation", "context_management", "max_output_tokens"} {
		if m[field] != nil {
			return nil
		}
	}
	for _, field := range []string{"background", "store"} {
		if v := m[field]; v != nil && v != false {
			return nil
		}
	}
	if text := m["text"]; text != nil {
		cfg := object(text)
		if cfg == nil || (cfg["format"] != nil && str(object(cfg["format"])["type"]) != "text") {
			return nil
		}
	}
	tools := array(m["tools"])
	if len(tools) != 1 {
		return nil
	}
	tool := object(tools[0])
	typ := str(tool["type"])
	if !isResponsesWebSearch(typ) {
		return nil
	}
	for k, v := range tool {
		switch k {
		case "type":
		case "external_web_access":
			if v != true {
				return nil // Never substitute live search for cached-only mode.
			}
		case "search_context_size":
			if s := str(v); s != "low" && s != "medium" && s != "high" {
				return nil
			}
		default:
			return nil // In particular, do not drop domain filters or user location.
		}
	}
	choice := m["tool_choice"]
	if choice == nil {
		choice = "auto"
	}
	if choice != "auto" && choice != "required" {
		cfg := object(choice)
		if len(cfg) != 1 || !isResponsesWebSearch(str(cfg["type"])) {
			return nil
		}
	}
	if v := m["include"]; v != nil {
		includes, ok := v.([]any)
		if !ok {
			return nil
		}
		for _, include := range includes {
			if str(include) != "web_search_call.action.sources" {
				return nil
			}
		}
	}
	query := str(m["input"])
	if items, ok := m["input"].([]any); ok {
		for i, item := range items {
			j := object(item)
			if typ := str(j["type"]); j == nil || typ != "" && typ != "message" {
				return nil
			}
			if i != len(items)-1 {
				continue
			}
			if str(j["role"]) != "user" {
				return nil
			}
			query = str(j["content"])
			if parts, ok := j["content"].([]any); ok {
				var texts []string
				for _, part := range parts {
					p := object(part)
					if str(p["type"]) != "input_text" {
						return nil
					}
					texts = append(texts, str(p["text"]))
				}
				query = strings.Join(texts, " ")
			}
		}
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 1024 {
		return nil
	}
	return &responsesSearchRequest{query: query, tool: tool, choice: choice}
}

type braveSearchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

func searchBrave(ctx context.Context, key, query string) ([]braveSearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, braveSearchEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = url.Values{"q": {query}, "count": {"5"}, "text_decorations": {"false"}}.Encode()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	resp, err := braveSearchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Brave Search request failed") // Do not expose query, URL or credentials.
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Brave Search returned HTTP %d", resp.StatusCode)
	}
	body, err := readProtocolBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("could not read Brave Search response")
	}
	var data struct {
		Type string `json:"type"`
		Web  struct {
			Results []braveSearchResult `json:"results"`
		} `json:"web"`
	}
	if json.Unmarshal(body, &data) != nil || data.Type != "search" {
		return nil, fmt.Errorf("invalid Brave Search response")
	}
	results := make([]braveSearchResult, 0, 5)
	seen := map[string]bool{}
	for _, result := range data.Web.Results {
		u, err := url.Parse(result.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || len(result.URL) > 2048 || seen[result.URL] {
			continue
		}
		seen[result.URL] = true
		result.Title = truncate(strings.Join(strings.Fields(result.Title), " "), 200)
		result.Description = truncate(strings.Join(strings.Fields(result.Description), " "), 500)
		if result.Title == "" {
			result.Title = u.Hostname()
		}
		results = append(results, result)
		if len(results) == 5 {
			break
		}
	}
	return results, nil
}

func relayResponsesSearchFallback(c *gin.Context, task *relayTask, key string, req *responsesSearchRequest) (info relayInfo, wrote bool, err error) {
	start := time.Now()
	info = relayInfo{UpstreamName: "gateway:brave-search", startedAt: start}
	defer func() { info.DurationMS = time.Since(start).Milliseconds() }()
	results, err := searchBrave(c.Request.Context(), key, req.query)
	info.LatencyMS = info.elapsedMS()
	if err != nil || c.Request.Context().Err() != nil {
		if err == nil {
			err = c.Request.Context().Err()
		}
		return info, false, err
	}

	sources, citations := []any{}, []any{}
	var answer strings.Builder
	for _, result := range results {
		sources = append(sources, map[string]any{"type": "url", "url": result.URL})
		if answer.Len() > 0 {
			answer.WriteString("\n\n")
		}
		startIndex := utf8.RuneCountInString(answer.String())
		answer.WriteString(result.Title)
		if result.Description != "" {
			answer.WriteString(" — " + result.Description)
		}
		citations = append(citations, map[string]any{"type": "url_citation", "start_index": startIndex, "end_index": utf8.RuneCountInString(answer.String()), "url": result.URL, "title": result.Title})
	}
	if len(results) == 0 {
		answer.WriteString("No web search results found.")
	}
	// Only the search provider ran. Report zero model tokens; never estimate
	// tokens from search snippets or charge model inference for this response.
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
		"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
	response := map[string]any{
		"id": "resp_" + randHex(16), "object": "response", "model": task.model,
		"created_at": start.Unix(), "completed_at": time.Now().Unix(), "status": "completed",
		"error": nil, "incomplete_details": nil, "usage": usage, "store": false,
		"tool_choice": req.choice, "tools": []any{req.tool}, "parallel_tool_calls": false,
		"output": []any{
			map[string]any{"id": "ws_" + randHex(16), "type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": req.query, "sources": sources}},
			map[string]any{"id": "msg_" + randHex(16), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer.String(), "annotations": citations, "logprobs": []any{}}}},
		},
	}
	body, err := json.Marshal(response)
	if err != nil {
		return info, false, err
	}
	info.Status = http.StatusOK
	info.mergeUsage(upstreamUsage{Source: FormatOpenAIResponse, InKnown: true, OutKnown: true})
	c.Header("X-APISHARE-Web-Search", "brave")
	if task.stream {
		info.markFirstToken()
		err = relayNativeJSONStream(c, FormatOpenAIResponse, body, false)
		return info, relayResponseCommitted(c), err
	}
	c.Data(http.StatusOK, "application/json", body)
	return info, true, nil
}

func (task *relayTask) responsesSearchFallback() (string, *responsesSearchRequest) {
	if task.format != FormatOpenAIResponse || task.nativeOnly || agentPurgeAppliesToModel(task) {
		return "", nil
	}
	key := strings.TrimSpace(os.Getenv("RESPONSES_BRAVE_SEARCH_API_KEY"))
	if key == "" {
		return "", nil
	}
	return key, parseResponsesSearchFallback(task.body)
}
