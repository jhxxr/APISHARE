package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Both pools deliberately use identical provider/model names. Inference can
// succeed only through each credential's proxy because the target has no DNS.
func TestPoolsIsolateRegistriesCredentialsAndEgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pools := make([]*Manager, 0, 2)
	failures := make([]*atomic.Bool, 0, 2)
	for _, identity := range []string{"pool-a", "pool-b"} {
		fail := &atomic.Bool{}
		failures = append(failures, fail)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Host != "pool-upstream.invalid" || r.Header.Get("Authorization") != "Bearer "+identity {
				t.Errorf("%s received another pool's request", identity)
			}
			w.Header().Set("Content-Type", "application/json")
			if fail.Load() {
				var input struct {
					Stream bool `json:"stream"`
				}
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input.Stream {
					payload := "data: " + `{"id":"broken","object":"chat.completion.chunk","model":"shared-model","choices":[{"index":0,"delta":{"content":"partial output"},"finish_reason":null}]}` + "\n\n"
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)+100))
					fmt.Fprint(w, payload) // Ending early forces an upstream read error.
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"error":{"message":"test provider unavailable"}}`)
				return
			}
			fmt.Fprintf(w, `{"id":"%s","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, identity, identity)
		}))
		defer proxy.Close()
		listener := httptest.NewUnstartedServer(nil)
		port := listener.Listener.Addr().(*net.TCPAddr).Port
		listener.Listener.Close()
		manager, err := NewManager(filepath.Join(t.TempDir(), identity), port)
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		if err := manager.ensureConfig(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(manager.root, "config.yaml")
		raw, _ := os.ReadFile(path)
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatal(err)
		}
		document["api-keys"] = map[string]any{"openai-compatibility": []any{map[string]any{
			"name": "same-provider", "base-url": "http://pool-upstream.invalid/v1",
			"keys":   []any{map[string]any{"api-key": identity, "proxy-url": proxy.URL}},
			"models": []any{map[string]any{"name": "shared-model", "alias": "shared-model"}, map[string]any{"name": identity + "-only", "alias": identity + "-only"}},
		}}}
		document["requests"] = map[string]any{"proxy-url": "http://127.0.0.1:1"}
		raw, _ = json.Marshal(document)
		if err := writePrivate(path, raw); err != nil {
			t.Fatal(err)
		}
		if err := manager.Start(ctx); err != nil {
			t.Fatal(err)
		}
		pools = append(pools, manager)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	request := func(manager *Manager, method, path, body string) []byte {
		t.Helper()
		base, key := manager.Connection()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("pool response: %d %s", res.StatusCode, raw)
		}
		return raw
	}
	for i, identity := range []string{"pool-a", "pool-b"} {
		catalog := string(request(pools[i], "GET", "/v1/models", ""))
		other := []string{"pool-b", "pool-a"}[i]
		if !strings.Contains(catalog, identity+"-only") || strings.Contains(catalog, other+"-only") {
			t.Fatalf("model registry crossed pools: %s", catalog)
		}
		for n := 0; n < 2; n++ {
			raw := request(pools[i], "POST", "/v1/chat/completions", `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)
			if !bytes.Contains(raw, []byte(identity)) {
				t.Fatalf("wrong account/proxy: %s", raw)
			}
		}
	}
	failures[0].Store(true)
	base, key := pools[0].Connection()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"shared-model","messages":[{"role":"user","content":"expected failure"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	failed, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, failed.Body)
	failed.Body.Close()
	if failed.StatusCode < 400 {
		t.Fatal("test failure unexpectedly succeeded")
	}
	if err := pools[0].Stop(); err != nil {
		t.Fatal(err)
	}
	raw := request(pools[1], "POST", "/v1/chat/completions", `{"model":"shared-model","messages":[{"role":"user","content":"still running"}]}`)
	if !bytes.Contains(raw, []byte("pool-b")) {
		t.Fatalf("stopping one pool affected another: %s", raw)
	}
	failures[1].Store(true)
	streamBase, streamKey := pools[1].Connection()
	streamRequest, _ := http.NewRequestWithContext(ctx, "POST", streamBase+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"shared-model","stream":true,"messages":[{"role":"user","content":"expected interrupted stream"}]}`))
	streamRequest.Header.Set("Authorization", "Bearer "+streamKey)
	streamRequest.Header.Set("Content-Type", "application/json")
	streamResponse, err := client.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, streamResponse.Body)
	streamResponse.Body.Close()
	for i, pool := range pools {
		history, err := pool.AccountUptimes(ctx, time.Now())
		if err != nil || len(history) != 1 {
			t.Fatalf("real SDK outcomes were not captured: %+v %v", history, err)
		}
		for _, item := range history {
			wantSuccesses := int64(2 + i)
			if item.Calls != int64(3+i) || item.Successes != wantSuccesses || item.Failures != 1 {
				t.Fatalf("pool %d outcomes crossed accounts or missed SDK errors: %+v", i, item)
			}
		}
	}
}
