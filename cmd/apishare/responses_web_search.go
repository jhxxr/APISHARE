package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	braveSearchEndpoint  = "https://api.search.brave.com/res/v1/web/search"
	tavilySearchEndpoint = "https://api.tavily.com/search"
	exaSearchEndpoint    = "https://api.exa.ai/search"
)

var webSearchHTTPClient = &http.Client{
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

type webSearchResult struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

func searchWeb(ctx context.Context, s webSearchSettings, query string) ([]webSearchResult, error) {
	method, endpoint, header, credential := http.MethodPost, "", "", s.APIKey
	var payload string
	switch s.Provider {
	case "brave":
		method, endpoint, header = http.MethodGet, braveSearchEndpoint, "X-Subscription-Token"
		endpoint += "?" + url.Values{"q": {query}, "count": {"5"}, "text_decorations": {"false"}}.Encode()
	case "tavily":
		endpoint, header, credential = tavilySearchEndpoint, "Authorization", "Bearer "+s.APIKey
		payload = jsonText(map[string]any{"query": query, "max_results": 5, "search_depth": "basic"})
	case "exa":
		endpoint, header = exaSearchEndpoint, "X-API-Key"
		payload = jsonText(map[string]any{"query": query, "numResults": 5, "contents": map[string]any{"highlights": true}})
	default:
		return nil, fmt.Errorf("unsupported web search provider")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("could not create %s search request", s.Provider)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(header, credential)
	resp, err := webSearchHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s search request failed", s.Provider) // No query or credentials in errors.
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s search returned HTTP %d", s.Provider, resp.StatusCode)
	}
	body, err := readProtocolBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("could not read %s search response", s.Provider)
	}
	var data struct {
		Type string `json:"type"`
		Web  struct {
			Results []webSearchResult `json:"results"`
		} `json:"web"`
		Results []struct {
			webSearchResult
			Content    string   `json:"content"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
	}
	if json.Unmarshal(body, &data) != nil || (s.Provider == "brave" && data.Type != "search") || (s.Provider != "brave" && data.Results == nil) {
		return nil, fmt.Errorf("invalid %s search response", s.Provider)
	}
	raw := data.Web.Results
	if s.Provider != "brave" {
		raw = make([]webSearchResult, 0, len(data.Results))
		for _, result := range data.Results {
			r := result.webSearchResult
			if s.Provider == "exa" {
				r.Description = strings.Join(result.Highlights, " ")
			} else {
				r.Description = result.Content
			}
			raw = append(raw, r)
		}
	}
	results := make([]webSearchResult, 0, 5)
	seen := map[string]bool{}
	for _, result := range raw {
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

func relayResponsesSearchFallback(c *gin.Context, task *relayTask, settings webSearchSettings, req *responsesSearchRequest) (info relayInfo, wrote bool, err error) {
	start := time.Now()
	info = relayInfo{UpstreamName: "gateway:" + settings.Provider + "-search", startedAt: start}
	defer func() { info.DurationMS = time.Since(start).Milliseconds() }()
	if task.requestRedaction != nil {
		receipt := task.requestRedactionReceipt
		info.requestRedaction = &receipt
	}
	query := req.query
	if task.requestRedaction != nil && task.requestRedaction.Config.Mode != "off" {
		payload, _ := json.Marshal(map[string]any{"input": query})
		processed, receipt, checkErr := task.requestRedaction.processStage(FormatOpenAIResponse, payload, 0, "search")
		info.requestRedaction = &receipt
		if checkErr != nil {
			return info, false, checkErr
		}
		var checked struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(processed.Body, &checked); err != nil {
			return info, false, err
		}
		query = checked.Input
	}
	results, err := searchWeb(c.Request.Context(), settings, query)
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
			map[string]any{"id": "ws_" + randHex(16), "type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": query, "sources": sources}},
			map[string]any{"id": "msg_" + randHex(16), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": answer.String(), "annotations": citations, "logprobs": []any{}}}},
		},
	}
	body, err := json.Marshal(response)
	if err != nil {
		return info, false, err
	}
	info.Status = http.StatusOK
	info.mergeUsage(upstreamUsage{Source: FormatOpenAIResponse, InKnown: true, OutKnown: true})
	c.Header("X-APISHARE-Web-Search", settings.Provider)
	if task.stream {
		info.markFirstToken()
		err = relayNativeJSONStream(c, FormatOpenAIResponse, body, false)
		return info, relayResponseCommitted(c), err
	}
	c.Data(http.StatusOK, "application/json", body)
	return info, true, nil
}

func (task *relayTask) responsesSearchFallback() (webSearchSettings, *responsesSearchRequest) {
	if task.format != FormatOpenAIResponse || task.nativeOnly || agentPurgeAppliesToModel(task) {
		return webSearchSettings{}, nil
	}
	settings, err := readWebSearchSettings()
	if err != nil {
		return webSearchSettings{}, nil
	}
	settings = settings.effective()
	if settings.APIKey == "" {
		return webSearchSettings{}, nil
	}
	return settings, parseResponsesSearchFallback(task.body)
}
