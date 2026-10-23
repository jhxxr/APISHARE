package main

import (
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/gin-gonic/gin"
)

const requestClientRuleVersion = 1
const maxRecordedUserAgent = 2048
const requestClientContextKey = "requestClient"

// Identity is self-reported. In particular, Qwen's Anthropic proxy mode can
// present exactly the same UA and x-app header as Claude Code.
type requestClientIdentity struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	SDKID       string   `json:"sdk_id,omitempty"`
	SDKName     string   `json:"sdk_name,omitempty"`
	SDKVersion  string   `json:"sdk_version,omitempty"`
	UserAgent   string   `json:"user_agent,omitempty"`
	Source      string   `json:"source"`
	RuleVersion int      `json:"rule_version"`
	Ambiguous   bool     `json:"ambiguous,omitempty"`
	Conflict    bool     `json:"conflict,omitempty"`
	Candidates  []string `json:"candidates,omitempty"`
}

var requestClientNames = map[string]string{
	"unknown":       "未知客户端",
	"codex":         "Codex",
	"claude_code":   "Claude Code",
	"opencode":      "OpenCode",
	"gemini_cli":    "Gemini CLI",
	"cline":         "Cline",
	"roo_code":      "Roo Code",
	"kilo_code":     "Kilo Code",
	"kimi_cli":      "Kimi CLI",
	"qwen_code":     "Qwen Code",
	"lobehub":       "LobeHub",
	"cherry_studio": "Cherry Studio",
	"aider":         "Aider",
	"open_webui":    "Open WebUI",
	"sillytavern":   "SillyTavern",
}

type clientUARule struct {
	id      string
	name    string
	pattern *regexp.Regexp
	version int
}

func clientUA(id, name, pattern string, version int) clientUARule {
	return clientUARule{id, name, regexp.MustCompile(pattern), version}
}

// Source snapshots and provider-specific limits: docs/CLIENT_USER_AGENTS.md.
// More specific application rules run before the independent SDK fallback.
var requestClientUARules = []clientUARule{
	clientUA("codex", "", `(?i)^(?:codex_cli_rs|codex-tui|codex_vscode)/([^/\s]+)(?:\s|$)`, 1),
	clientUA("claude_code", "", `(?i)^claude-cli/([^/\s]+)(?:\s|$)`, 1),
	clientUA("opencode", "", `(?i)^opencode/([^/\s]+)/([0-9][^/\s]*|unknown)/([^/\s]+)(?:\s|$)`, 2),
	clientUA("opencode", "", `(?i)^opencode/([0-9][^/\s]*)(?:\s|$)`, 1),
	clientUA("gemini_cli", "", `(?i)^GeminiCLI(?:-[^/\s]+)?/([^/\s]+)/[^\s]+\s+\(`, 1),
	clientUA("gemini_cli", "", `(?i)^CloudCodeVSCode/([^/\s]+)\s+\([^\r\n]*proxy_client=geminicli\)`, 1),
	clientUA("cline", "", `(?i)^Cline/([^/\s]+)(?:\s|$)`, 1),
	clientUA("roo_code", "", `(?i)^RooCode/([^/\s]+)(?:\s|$)`, 1),
	clientUA("kilo_code", "", `(?i)^kilo/([^/\s]+)/([^/\s]+)/([^/\s]+)(?:\s|$)`, 2),
	clientUA("kilo_code", "", `(?i)^opencode-kilo-provider(?:/([^/\s]+))?(?:\s|$)`, 1),
	clientUA("kimi_cli", "", `(?i)^KimiCLI/([^/\s]+)(?:\s|$)`, 1),
	clientUA("qwen_code", "", `(?i)^QwenCode/([^/\s]+)(?:\s|$)`, 1),
	clientUA("lobehub", "", `(?i)^LobeHub Desktop/([^/\s]+)(?:\s|$)`, 1),
}

var requestSDKUARules = []clientUARule{
	clientUA("openai_python", "OpenAI Python SDK", `(?i)^(?:Async)?(?:Azure)?OpenAI/Python\s+(\S+)(?:\s|$)`, 1),
	clientUA("openai_js", "OpenAI JavaScript SDK", `(?i)^(?:Azure)?OpenAI/JS\s+(\S+)(?:\s|$)`, 1),
	clientUA("anthropic_python", "Anthropic Python SDK", `(?i)^(?:Async)?Anthropic/Python\s+(\S+)(?:\s|$)`, 1),
	clientUA("anthropic_js", "Anthropic JavaScript SDK", `(?i)^Anthropic/JS\s+(\S+)(?:\s|$)`, 1),
	clientUA("google_genai_python", "Google Gen AI Python SDK", `(?i)(?:^|\s)google-genai-sdk/([^\s]+)(?:\s|$)`, 1),
}

func boundedClientText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "")
	}
	return strings.TrimSpace(value)
}

func normalizeRequestClient(client requestClientIdentity) requestClientIdentity {
	if _, ok := requestClientNames[client.ID]; !ok {
		client.ID = "unknown"
	}
	client.Name = requestClientNames[client.ID]
	if client.Source == "" {
		client.Source = "unknown"
	}
	return client
}

type clientAttribution struct {
	id, title, host, path string
}

var requestClientAttributions = []clientAttribution{
	{"cline", "Cline", "cline.bot", ""},
	{"roo_code", "Roo Code", "github.com", "/RooVetGit/Roo-Cline"},
	{"kilo_code", "Kilo Code", "kilo.ai", ""},
	{"cherry_studio", "Cherry Studio", "cherry-ai.com", ""},
	{"aider", "Aider", "aider.chat", ""},
	{"open_webui", "Open WebUI", "openwebui.com", ""},
	{"sillytavern", "SillyTavern", "sillytavern.app", ""},
	{"lobehub", "LobeHub", "lobehub.com", ""},
}

func attributedRequestClient(header http.Header) string {
	referer := header.Get("HTTP-Referer")
	if len(referer) > maxRecordedUserAgent {
		return ""
	}
	parsed, err := url.Parse(referer)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return ""
	}
	title := strings.TrimSpace(header.Get("X-Title"))
	for _, entry := range requestClientAttributions {
		if strings.EqualFold(title, entry.title) && strings.EqualFold(parsed.Hostname(), entry.host) &&
			(entry.path == "" || strings.EqualFold(strings.TrimRight(parsed.Path, "/"), entry.path)) {
			return entry.id
		}
	}
	return ""
}

func detectRequestClient(header http.Header) requestClientIdentity {
	result := requestClientIdentity{
		ID: "unknown", Source: "unknown", RuleVersion: requestClientRuleVersion,
		UserAgent: boundedClientText(header.Get("User-Agent"), maxRecordedUserAgent),
	}
	for _, rule := range requestClientUARules {
		if match := rule.pattern.FindStringSubmatch(result.UserAgent); match != nil {
			result.ID, result.Version, result.Source = rule.id, boundedClientText(match[rule.version], 96), "user_agent"
			break
		}
	}
	for _, rule := range requestSDKUARules {
		if match := rule.pattern.FindStringSubmatch(result.UserAgent); match != nil {
			result.SDKID, result.SDKName, result.SDKVersion = rule.id, rule.name, boundedClientText(match[rule.version], 96)
			break
		}
	}
	// Never infer an application from generic x-app, x-client-type or session IDs.
	originator := ""
	switch strings.ToLower(strings.TrimSpace(header.Get("originator"))) {
	case "codex_cli_rs", "codex-tui", "codex_vscode":
		originator = "codex"
	case "cline":
		originator = "cline"
	}
	for _, evidence := range []struct{ id, source string }{
		{attributedRequestClient(header), "auxiliary_headers"},
		{originator, "originator"},
	} {
		if evidence.id == "" {
			continue
		}
		if result.ID == "unknown" {
			result.ID, result.Source = evidence.id, evidence.source
		} else if result.ID != evidence.id {
			result.Conflict = true
			result.Candidates = append(result.Candidates, result.ID, evidence.id)
		}
	}
	if result.ID == "claude_code" {
		result.Ambiguous = true
		result.Candidates = append(result.Candidates, "claude_code", "qwen_code")
	}
	if result.ID == "unknown" && result.SDKID != "" {
		result.Source = "sdk_only"
	}
	if len(result.Candidates) > 0 {
		sort.Strings(result.Candidates)
		unique := result.Candidates[:0]
		for _, id := range result.Candidates {
			if len(unique) == 0 || unique[len(unique)-1] != id {
				unique = append(unique, id)
			}
		}
		result.Candidates = unique
	}
	return normalizeRequestClient(result)
}

type requestClientState struct {
	identity requestClientIdentity
	model    string
	recorded atomic.Bool
}

func requestClientFor(c *gin.Context) *requestClientState {
	if value, exists := c.Get(requestClientContextKey); exists {
		return value.(*requestClientState)
	}
	header := http.Header{}
	if c.Request != nil {
		header = c.Request.Header
	}
	state := &requestClientState{identity: detectRequestClient(header)}
	c.Set(requestClientContextKey, state)
	return state
}

func (task *relayTask) clientIdentity() requestClientIdentity {
	if task.client != nil {
		return task.client.identity
	}
	return normalizeRequestClient(requestClientIdentity{})
}

func (task *relayTask) markClientRecorded() {
	if task.client != nil {
		task.client.recorded.Store(true)
	}
}

func requestClientEndpoint(c *gin.Context) clientFormat {
	switch c.FullPath() {
	case "/v1/chat/completions", "/v1/embeddings":
		return FormatOpenAIChat
	case "/v1/responses", "/v1/responses/compact":
		return FormatOpenAIResponse
	case "/v1/messages":
		return FormatAnthropic
	case "/v1beta/models/*action":
		return FormatGemini
	case "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations":
		return clientFormat("openai-audio")
	case "/v1/realtime", "/v1/realtime/calls":
		return FormatRealtime
	}
	return ""
}

// Runs after authentication and before rate limiting. Capture once before any
// upstream can rewrite headers; fallback records only otherwise unlogged errors.
// Model discovery and unauthenticated traffic do not become inference calls.
func trackRequestClient() gin.HandlerFunc {
	return func(c *gin.Context) {
		endpoint := requestClientEndpoint(c)
		if endpoint == "" || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		state := requestClientFor(c)
		c.Next()
		status := c.Writer.Status()
		message := http.StatusText(status)
		if c.Request.Context().Err() != nil {
			status, message = http.StatusBadGateway, "client disconnected"
		}
		if status < 400 || state.recorded.Swap(true) {
			return
		}
		ak := c.MustGet("apiKey").(ApiKey)
		if err := dbInsertLog(&CallLog{KeyID: ak.ID, KeyName: ak.Name, Model: state.model,
			Endpoint: string(endpoint), Status: status, Error: message, Client: state.identity}); err != nil {
			log.Printf("[client-log] key_id=%d: %v", ak.ID, err)
		}
	}
}
