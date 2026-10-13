package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkoukk/tiktoken-go"
)

func TestTokenEncoderColdLoadIsNonblockingAndShared(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	encoding := &tiktoken.Tiktoken{}
	cache := &tokenEncodingCache{load: func(string) (*tiktoken.Tiktoken, error) {
		calls.Add(1)
		close(started)
		<-release
		return encoding, nil
	}}
	if cache.get("shared-encoding") != nil {
		t.Fatal("cold load unexpectedly returned an encoding")
	}
	<-started
	for i := 0; i < 100; i++ {
		if cache.get("shared-encoding") != nil {
			t.Fatal("unfinished load was published")
		}
	}
	cache.RLock()
	ready := cache.entries["shared-encoding"].ready
	cache.RUnlock()
	close(release)
	<-ready
	if cache.get("shared-encoding") != encoding || calls.Load() != 1 {
		t.Fatal("load was duplicated or cache never became ready")
	}
	if tokenEncodingName("gpt-4o") != tokenEncodingName("gpt-4o-2024-08-06") || tokenEncodingName("unknown-future-model") != "" {
		t.Fatal("model variants do not share the known encoding")
	}
}

func TestTokenEncoderFailuresBackOff(t *testing.T) {
	var calls atomic.Int32
	cache := &tokenEncodingCache{load: func(string) (*tiktoken.Tiktoken, error) { calls.Add(1); return nil, errors.New("offline") }}
	cache.get("encoding")
	cache.RLock()
	ready := cache.entries["encoding"].ready
	cache.RUnlock()
	<-ready
	for i := 0; i < 100; i++ {
		cache.get("encoding")
	}
	if calls.Load() != 1 {
		t.Fatal("offline requests repeatedly initialized tokenizers")
	}
	cache.Lock()
	cache.entries["encoding"].retryAt = time.Now().Add(-time.Second)
	cache.Unlock()
	cache.get("encoding")
	cache.RLock()
	ready = cache.entries["encoding"].ready
	cache.RUnlock()
	<-ready
	if calls.Load() != 2 {
		t.Fatal("expired failure was not retried")
	}
}

func TestKnownUsageSkipsTokenEstimation(t *testing.T) {
	publicTestDB(t)
	gin.SetMode(gin.TestMode)
	key := publicTestKey(t, "lazy-known", 0, 0, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	tokenEncodings.RLock()
	before := tokenEncodings.entries[tiktoken.MODEL_CL100K_BASE]
	tokenEncodings.RUnlock()
	task := &relayTask{format: FormatOpenAIChat, model: "gpt-4", canon: &canonReq{InputText: strings.Repeat("a", 100000)}}
	finalizeCall(c, *key, task, relayInfo{Status: 200, PromptTok: 7, CompletionTok: 0, InKnown: true, OutKnown: true}, true, nil, -1)
	tokenEncodings.RLock()
	after := tokenEncodings.entries[tiktoken.MODEL_CL100K_BASE]
	tokenEncodings.RUnlock()
	logs, err := dbListLogs(1, 0)
	if err != nil || len(logs) != 1 || logs[0].PromptTokens != 7 || logs[0].CompletionTokens != 0 || before != after {
		t.Fatal("real usage triggered estimation or zero usage was lost")
	}
}

func TestPromptEstimationIncludesLaterMessagesWithoutChangingAffinity(t *testing.T) {
	body := []byte(`{"model":"review-model","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"` + strings.Repeat("later ", 1000) + `"},{"role":"user","content":"follow-up"}]}`)
	cr, err := parseClientRequest(FormatOpenAIChat, body, "")
	if err != nil {
		t.Fatal(err)
	}
	if cr.ConversionError != nil {
		t.Fatalf("bad fixture: %v", cr.ConversionError)
	}
	prefix := cr.InputText
	tokens := estimatePromptTokens(&relayTask{model: cr.Model, canon: cr})
	if tokens <= 1000 || cr.InputText != prefix || strings.Contains(prefix, "later") {
		t.Fatalf("accounting used a truncated session prefix: %d %q", tokens, prefix)
	}
}

func TestTokenVocabularyLoaderCachesAndRejectsMalformedData(t *testing.T) {
	t.Setenv("TIKTOKEN_CACHE_DIR", t.TempDir())
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "YQ== 0\nYg== 1\n") }))
	defer server.Close()
	loader := &boundedTokenLoader{client: &http.Client{Timeout: time.Second}}
	for i := 0; i < 2; i++ {
		ranks, err := loader.LoadTiktokenBpe(server.URL)
		if err != nil || len(ranks) != 2 || ranks["b"] != 1 {
			t.Fatalf("cache/load failed: %v %v", ranks, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("cached vocabulary was downloaded twice")
	}
	if _, err := os.Stat(tokenVocabularyCachePath(server.URL)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "\n", "invalid", "! 0", "YQ== -1", "YQ== x", "YQ== 0\nYQ== 1", strings.Repeat("a", maxTokenVocabularyBytes+1)} {
		if _, err := parseTokenVocabulary([]byte(body)); err == nil {
			t.Fatal("malformed vocabulary accepted")
		}
	}
}

func TestTokenVocabularyDownloadHasDeadline(t *testing.T) {
	t.Setenv("TIKTOKEN_CACHE_DIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	loader := &boundedTokenLoader{client: &http.Client{Timeout: 30 * time.Millisecond}}
	if _, err := loader.LoadTiktokenBpe(server.URL); err == nil {
		t.Fatal("stalled vocabulary download had no deadline")
	}
}
