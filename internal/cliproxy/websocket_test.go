package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCodexResponsesWebSocketTransportReusesProviderConnection(t *testing.T) {
	var connections, requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Error("Codex account did not select its WebSocket executor")
			w.WriteHeader(500)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		connections.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for i := 1; i <= 2; i++ {
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			var request struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(body, &request) != nil || request.Type != "response.create" {
				t.Errorf("invalid native request: %s", body)
				return
			}
			requests.Add(1)
			for _, event := range []string{
				fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_core_%d","status":"in_progress"}}`, i),
				fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello","response_id":"resp_core_%d"}`, i),
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_core_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}}`, i),
			} {
				if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
					t.Error(err)
					return
				}
			}
		}
		_, _, _ = conn.Read(ctx)
	}))
	defer provider.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	manager, err := NewManager(t.TempDir(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.ensureConfig(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.root, "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["api-keys"] = map[string]any{"codex": []any{map[string]any{
		"name": "local-ws", "base-url": provider.URL + "/v1", "proxy-url": "direct",
		"models": []any{map[string]any{"name": "gpt-5.4", "alias": "local-codex-ws"}},
		"keys":   []any{map[string]any{"api-key": "local-fixture-token", "websockets": true}},
	}}}
	raw, _ = json.Marshal(document)
	if err := writePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(map[string]any{
		"type": "codex", "email": "websocket@example.invalid", "access_token": "local-fixture-token",
		"expired": "2099-01-01T00:00:00Z", "disabled": true, "websockets": true,
	})
	if err := manager.SeedCredentials(map[string]json.RawMessage{"ws.json": credential}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.RefreshRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	if !manager.Ready("local-codex-ws") {
		t.Fatalf("Codex fixture has no registered model: %+v", manager.Runtime())
	}
	base, key := manager.Connection()
	conn, _, err := websocket.Dial(ctx, base+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(map[string]any{"type": "response.create", "model": "local-codex-ws",
			"instructions": "Local test", "input": []any{map[string]any{"role": "user", "content": "hello"}}})
		if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
			t.Fatal(err)
		}
		for {
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(body, &event)
			if event.Type == "error" {
				t.Fatalf("SDK Responses WebSocket failed: %s", body)
			}
			if event.Type == "response.completed" {
				break
			}
		}
	}
	if connections.Load() != 1 || requests.Load() != 2 {
		t.Fatalf("SDK did not reuse native provider transport: connections=%d requests=%d", connections.Load(), requests.Load())
	}
	credentials, err := manager.Credentials(ctx)
	found := false
	for _, credential := range credentials {
		found = found || credential.Name == "ws.json" && credential.Websockets != nil && *credential.Websockets
	}
	if err != nil || !found {
		t.Fatalf("WebSocket capability was not projected: %+v / %v", credentials, err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestStoppedCredentialAcceptsLegacyWebsocketsFlag(t *testing.T) {
	manager, err := NewManager(t.TempDir(), 18317)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`true`, `"true"`, `false`, `"false"`, `null`, `123`} {
		body := json.RawMessage(`{"type":"codex","disabled":true,"websockets":` + raw + `}`)
		if err := manager.SeedCredentials(map[string]json.RawMessage{"legacy.json": body}); err != nil {
			t.Fatal(err)
		}
		credentials, err := manager.Credentials(context.Background())
		if err != nil || len(credentials) != 1 {
			t.Fatalf("legacy flag broke credential listing: flag=%s err=%v", raw, err)
		}
		flag := credentials[0].Websockets
		if raw == "null" || raw == "123" {
			if flag != nil {
				t.Fatalf("invalid or absent capability became configured: %s", raw)
			}
		} else if flag == nil || *flag != strings.Contains(raw, "true") {
			t.Fatalf("legacy capability changed: %s / %v", raw, flag)
		}
	}
}
