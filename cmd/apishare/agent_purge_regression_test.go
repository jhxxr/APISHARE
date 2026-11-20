package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"apishare/internal/agentpurge"
	"github.com/gin-gonic/gin"
)

func TestAgentPurgeLongTemplateBudget(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	original := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	body, changed := agentpurge.RewriteWithPersona(original, string(FormatOpenAIChat), "strict", "qwen-37")
	if !changed {
		t.Fatal("long original template was not selected")
	}
	task := &relayTask{modelContextLimits: map[string]int{model: 4096}}
	_, err := boundAgentPurgeBody(body, FormatOpenAIChat, model, task)
	var budget *agentPurgeBudgetError
	if !errors.As(err, &budget) || budget.Input <= budget.Limit {
		t.Fatalf("long template exceeded the configured context without a clear error: %v", err)
	}
}

func TestAgentPurgeRegressionStrictAnthropicSystemMessages(t *testing.T) {
	body := anthropicSystemFixture(t, false)
	rewritten, changed := agentpurge.Rewrite(body, string(FormatAnthropic), "strict")
	if !changed {
		t.Fatal("request was not rewritten")
	}
	cr, err := parseClientRequest(FormatAnthropic, rewritten, "")
	if err != nil || cr.ConversionError != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if cr.System != agentpurge.Prompt() {
		t.Errorf("strict retains original messages[].role=system instructions: explicitTypes=%t, concise=%t", strings.Contains(cr.System, "Use explicit types."), strings.Contains(cr.System, "Keep answers concise."))
	}
}

func TestAgentPurgeRegressionReplaceReintroducesEmptySystemBlock(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","max_tokens":128,"system":[{"type":"text","text":"You are Claude.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"Hello"}]}`)
	normalized, err := normalizeInternAIAnthropicRequest(body, FormatAnthropic, "https://discovery-api.intern-ai.org.cn/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	rewritten, changed := agentpurge.Rewrite(normalized, string(FormatAnthropic), "replace")
	if !changed {
		t.Fatal("request was not rewritten")
	}
	payload, err := decodeProtocolObject(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if _, needsRepair := cleanInternAITextBlocks(payload["system"]); needsRepair {
		t.Errorf("replace reintroduced an empty system text block after the provider normalization pass")
	}
}

func TestAgentPurgeRegressionDeepSeekContextLimitAfterInjection(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	const contextLimit = 4096
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":2048,"messages":[{"role":"user","content":%q}]}`, model, strings.Repeat("a", 12000)))
	cr, err := parseClientRequest(FormatOpenAIChat, body, "")
	if err != nil {
		t.Fatal(err)
	}
	before := estimateRequestContextTokens(cr)
	var after, output int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded, e := parseClientRequest(FormatOpenAIChat, b, "")
		if e != nil {
			t.Error(e)
			return
		}
		after, output = estimateRequestContextTokens(forwarded), forwarded.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	pair := []agentPurgePair{{Model: model, UpstreamID: 7}}
	task := &relayTask{format: FormatOpenAIChat, endpoint: "/v1/chat/completions", model: model, body: body, canon: cr,
		candidateRawModel: model + "(high)", estimatedInputTokens: before, modelContextLimits: map[string]int{model: contextLimit},
		userSub: "review-user", client: &requestClientState{identity: detectRequestClient(http.Header{"User-Agent": {"codex_cli_rs/1.2.3"}})},
		agentPurgeState: &agentPurgeRequestState{policy: agentPurgePolicy{Enabled: true, Pairs: pair}, user: userAgentPurgeConfig{Enabled: true, Mode: "append", Pairs: pair, Clients: []string{"codex"}}},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	up := &Upstream{ID: 7, Type: "openai", OpenAIProtocol: "chat", BaseURL: server.URL, Models: model, Enabled: true, Weight: 1}
	info, wrote, e := doRelay(c, up, task)
	if e != nil || !wrote || info.Status != 200 {
		t.Fatalf("relay failed %v", e)
	}
	t.Logf("model=%s before_input=%d actual_input=%d max_tokens=%d context_limit=%d", model, before, after, output, contextLimit)
	if before+output <= contextLimit && after+output > contextLimit {
		t.Errorf("late persona injection makes a previously clamped request exceed the configured context limit by %d tokens", after+output-contextLimit)
	}
}

type agentPurgeBlockingBody struct {
	reader io.Reader
	began  chan struct{}
	once   sync.Once
}

func (b *agentPurgeBlockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.began) })
	return b.reader.Read(p)
}

func TestAgentPurgeRegressionSlowBodyHoldsGlobalConfigLock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/user", func(c *gin.Context) {
		c.Set("xauthUser", xauthUserRecord{Sub: "review-user"})
		c.Set("xauthKey", ApiKey{ID: 1})
	})
	registerUserAgentPurgeRoutes(g)
	pr, pw := io.Pipe()
	began := make(chan struct{})
	done := make(chan struct{})
	req := httptest.NewRequest("PUT", "/api/user/agent-purge", &agentPurgeBlockingBody{reader: pr, began: began})
	req.Header.Set("Content-Type", "application/json")
	go func() { defer close(done); r.ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("handler never reached request-body reader")
	}
	got := agentPurgeConfigMu.TryLock()
	if got {
		agentPurgeConfigMu.Unlock()
	}
	_ = pw.Close()
	<-done
	_ = pr.Close()
	if !got {
		t.Errorf("a stalled authenticated request body holds the global mutex, blocking every user's and administrator's policy save")
	}
}

func TestAgentPurgeRegressionConflictingTitlesMustNotMatch(t *testing.T) {
	h := make(http.Header)
	h.Set("User-Agent", "codex_cli_rs/1.2.3")
	h.Set("HTTP-Referer", "https://cline.bot")
	h.Set("X-Title", "Cline")
	h.Set("X-OpenRouter-Title", "Codex")
	client := detectRequestClient(h)
	if !client.Conflict {
		t.Errorf("conflicting X-Title evidence was discarded rather than flagged: id=%s source=%s conflict=%t", client.ID, client.Source, client.Conflict)
	}
}
