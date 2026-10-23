package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoutingSnapshotTracksEditsAndKeepsAdmittedCopies(t *testing.T) {
	modelsTestDB(t)
	up := &Upstream{Type: "openai", OpenAIProtocol: "chat", APIKey: "old-key", Models: "allowed", ModelMap: `{"allowed":"old-target"}`, HeaderOverrides: `{"X-Config":"old"}`, Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	admitted, err := pickUpstream(FormatOpenAIChat, "allowed", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.config == nil || admitted.mapModel("allowed") != "old-target" {
		t.Fatal("runtime configuration was not compiled")
	}
	up.APIKey, up.ModelMap, up.HeaderOverrides = "new-key", `{"allowed":"new-target"}`, `{"X-Config":"new"}`
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	fresh, err := cachedRoutingUpstream(up.ID)
	if err != nil || fresh.APIKey != "new-key" || fresh.mapModel("allowed") != "new-target" {
		t.Fatalf("edit did not replace snapshot: %+v %v", fresh, err)
	}
	if admitted.APIKey != "old-key" || admitted.mapModel("allowed") != "old-target" {
		t.Fatal("edit changed a request's admitted configuration")
	}
	// Mutating a returned copy (including unsaved test/probe configuration)
	// must neither mutate the cache nor use stale parsed fields.
	fresh.Models, fresh.ModelMap, fresh.HeaderOverrides = "other", `{"other":"changed"}`, `{"X-Config":"unsaved"}`
	if fresh.servesModel("allowed") || fresh.mapModel("other") != "changed" {
		t.Fatal("mutated copies reused stale compiled fields")
	}
	req, _ := http.NewRequest("POST", "http://upstream.invalid", nil)
	if err := fresh.applyHeaders(req, nil); err != nil || req.Header.Get("X-Config") != "unsaved" {
		t.Fatal("unsaved headers reused cached rules")
	}
	up.Enabled = false
	if err := dbUpdateUpstream(up); err != nil {
		t.Fatal(err)
	}
	if _, err := pickUpstream(FormatOpenAIChat, "allowed", nil, false); err == nil {
		t.Fatal("disabled upstream remained routable")
	}
	if err := dbDeleteUpstream(up.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedRoutingUpstream(up.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted upstream remained cached: %v", err)
	}
}

func TestRoutingManyCandidatesReturnIndependentCopies(t *testing.T) {
	modelsTestDB(t)
	const count = 65
	for i := 0; i < count; i++ {
		up := &Upstream{Type: "openai", Models: "model", Weight: 1, Enabled: true}
		if err := dbInsertUpstream(up); err != nil {
			t.Fatal(err)
		}
	}
	excluded := make(map[int64]bool, count)
	for i := 0; i < count; i++ {
		up, err := pickUpstream(FormatOpenAIChat, "model", excluded, false)
		if err != nil {
			t.Fatal(err)
		}
		id := up.ID
		if excluded[id] || up.Weight != 1 || !up.Enabled || up.Models != "model" {
			t.Fatalf("invalid candidate on selection %d: %+v", i, up)
		}
		excluded[id] = true
		up.ID, up.Weight, up.Enabled, up.Models = -1, 0, false, "changed"
		cached, err := cachedRoutingUpstream(id)
		if err != nil || cached.ID != id || cached.Weight != 1 || !cached.Enabled || cached.Models != "model" {
			t.Fatalf("selected configuration modified the snapshot: %+v / %v", cached, err)
		}
	}
	if _, err := pickUpstream(FormatOpenAIChat, "model", excluded, false); err == nil {
		t.Fatal("exhausted candidates remained routable")
	}
}

func TestCompiledHeadersKeepVariablesPerRequest(t *testing.T) {
	up := &Upstream{APIKey: "upstream-secret", HeaderOverrides: `{"*":true,"re:^X-Private-.*$":false,"Authorization":"Bearer {api_key}","X-Tenant":"{client_header:X-Tenant}"}`}
	compileUpstreamConfig(up)
	for _, tenant := range []string{"first", "second"} {
		req, _ := http.NewRequest("POST", "http://upstream.invalid", nil)
		incoming := http.Header{"Authorization": {"Bearer client-secret"}, "X-Tenant": {tenant}, "X-Private-Id": {"private"}}
		if err := up.applyHeaders(req, incoming); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("X-Tenant") != tenant || req.Header.Get("Authorization") != "Bearer upstream-secret" || req.Header.Get("X-Private-Id") != "" {
			t.Fatal("compiled rules leaked or reused request headers")
		}
	}
}

func TestRoutingSnapshotExpires(t *testing.T) {
	modelsTestDB(t)
	up := &Upstream{Type: "openai", Models: "model", Weight: 1, Enabled: true}
	if err := dbInsertUpstream(up); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedRoutingUpstream(up.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE upstreams SET enabled=0 WHERE id=?`, up.ID); err != nil {
		t.Fatal(err)
	}
	routingCache.Lock()
	routingCache.snapshot.expires = time.Now().Add(-time.Second)
	routingCache.Unlock()
	if _, err := pickUpstream(FormatOpenAIChat, "model", nil, false); err == nil {
		t.Fatal("expired snapshot was reused")
	}
}

func TestWildcardCatalogFetchesAreParallelAndBounded(t *testing.T) {
	modelsTestDB(t)
	started, release := make(chan struct{}, 8), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var active, maximum atomic.Int32
	for i := 0; i < 6; i++ {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := active.Add(1)
			defer active.Add(-1)
			for previous := maximum.Load(); n > previous && !maximum.CompareAndSwap(previous, n); previous = maximum.Load() {
			}
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"shared-model"}]}`)
		}))
		t.Cleanup(server.Close)
		typeName := "openai"
		if i == 0 {
			typeName = "anthropic"
		}
		if err := dbInsertUpstream(&Upstream{Type: typeName, BaseURL: server.URL, APIKey: fmt.Sprintf("key-%d", i), Weight: 1, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		models []catalogModel
		err    error
	}
	done := make(chan result, 1)
	go func() { models, err := availableModels(context.Background()); done <- result{models, err} }()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			<-done
			t.Fatal("catalog fetching remained sequential")
		}
	}
	releaseOnce.Do(func() { close(release) })
	got := <-done
	if got.err != nil || len(got.models) != 1 || got.models[0].Owner != "anthropic" || maximum.Load() > 4 {
		t.Fatalf("parallel catalog changed ordering/bound: %+v %v concurrency=%d", got.models, got.err, maximum.Load())
	}
}

func TestNativeRequestRewritesKeepNumberPrecision(t *testing.T) {
	body := []byte(`{"model":"old","stream":true,"custom":{"integer":9007199254740993,"decimal":1.234567890123456789},"messages":[{"role":"user","content":"hello"}]}`)
	rewritten := injectStreamUsage(replaceModelInBody(body, "new"))
	if !strings.Contains(string(rewritten), "9007199254740993") || !strings.Contains(string(rewritten), "1.234567890123456789") || !strings.Contains(string(rewritten), `"model":"new"`) || !hasIncludeUsage(rewritten) {
		t.Fatalf("rewrite corrupted untouched fields: %s", rewritten)
	}
}

func TestCompiledInvalidModelMapMatchesUncachedBehavior(t *testing.T) {
	up := &Upstream{ModelMap: `{"model":"target","broken":123}`}
	compileUpstreamConfig(up)
	if up.mapModel("model") != "model" {
		t.Fatal("partially decoded invalid model map was used")
	}
}

func TestCanceledCatalogRequestDoesNotStartDiscovery(t *testing.T) {
	modelsTestDB(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"data":[]}`) }))
	defer server.Close()
	up := &Upstream{ID: -91, Type: "openai", BaseURL: server.URL, APIKey: "key"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cachedUpstreamModels(ctx, up); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	modelsCache.Lock()
	_, pending := modelsCache.pending[upstreamModelCacheKeyForProxy(up, nil)]
	modelsCache.Unlock()
	if calls.Load() != 0 || pending {
		t.Fatal("canceled catalog request started work")
	}
}
