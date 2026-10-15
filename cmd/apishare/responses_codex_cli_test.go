package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// Opt-in integration against the installed CLI and a deterministic local model.
// No user configuration, credentials or external inference service is used.
func TestCodexCLIResponsesToolLoop(t *testing.T) {
	binary := os.Getenv("APISHARE_CODEX_BINARY")
	if binary == "" {
		t.Skip("set APISHARE_CODEX_BINARY to run the installed Codex CLI smoke test")
	}
	dir, home := t.TempDir(), t.TempDir()
	var smokeEnv []string
	for _, env := range os.Environ() {
		upper := strings.ToUpper(env)
		if !strings.HasPrefix(upper, "CODEX_HOME=") && !strings.HasPrefix(upper, "OPENAI_API_KEY=") && !strings.HasPrefix(upper, "OPENAI_BASE_URL=") {
			smokeEnv = append(smokeEnv, env)
		}
	}
	smokeEnv = append(smokeEnv, "CODEX_HOME="+home, "APISHARE_CODEX_SMOKE_KEY=local-fixture-key")
	catalogCtx, catalogCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer catalogCancel()
	catalogCmd := exec.CommandContext(catalogCtx, binary, "debug", "models")
	catalogCmd.Env = smokeEnv
	catalogRaw, err := catalogCmd.Output()
	if err != nil {
		t.Fatalf("read CLI's bundled model metadata: %v", err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(catalogRaw, &catalog); err != nil {
		t.Fatal(err)
	}
	var smokeModel map[string]any
	for _, raw := range array(catalog["models"]) {
		candidate := object(raw)
		if str(candidate["apply_patch_tool_type"]) == "freeform" {
			smokeModel = candidate
			break
		}
	}
	if smokeModel == nil {
		t.Fatal("CLI has no bundled freeform patch model")
	}
	smokeModel["slug"] = "gpt-5.4"
	smokeModel["multi_agent_version"] = "v1"
	catalogPath := filepath.Join(home, "local-smoke-models.json")
	if err := os.WriteFile(catalogPath, []byte(jsonText(map[string]any{"models": []any{smokeModel}})), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	stage := 0
	var seenTools []string
	var patchResult, firstTestResult, commandResult string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var wire map[string]any
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		tools := array(wire["tools"])
		for _, tool := range tools {
			seenTools = append(seenTools, str(object(object(tool)["function"])["name"]))
		}
		result := &canonResponse{Finish: "stop", Usage: upstreamUsage{Prompt: 10, Completion: 10, InKnown: true, OutKnown: true}}
		call := func(name, id, args string) {
			result.Finish = "tool_calls"
			result.Blocks = []responseBlock{{Kind: "tool", Call: canonCall{id, name, args}}}
		}
		lastToolOutput := func() string {
			msgs := array(wire["messages"])
			for i := len(msgs) - 1; i >= 0; i-- {
				j := object(msgs[i])
				if str(j["role"]) == "tool" {
					return contentText(j["content"])
				}
			}
			return ""
		}
		switch stage {
		case 0, 2:
			patch := "*** Begin Patch\n*** Add File: go.mod\n+module smoke\n*** Add File: sum.go\n+package smoke\n+func Sum(a, b int) int { return a - b }\n*** Add File: sum_test.go\n+package smoke\n+import \"testing\"\n+func TestSum(t *testing.T) { if Sum(2, 3) != 5 { t.Fatal(\"sum should be 5\") } }\n*** End Patch"
			if stage == 2 {
				firstTestResult = lastToolOutput()
				patch = "*** Begin Patch\n*** Update File: sum.go\n@@\n-func Sum(a, b int) int { return a - b }\n+func Sum(a, b int) int { return a + b }\n*** End Patch"
			}
			for _, raw := range tools {
				tool := object(object(raw)["function"])
				name := str(tool["name"])
				if name == "apply_patch" || strings.HasSuffix(name, "__apply_patch") {
					call(name, fmt.Sprintf("call_smoke_patch_%d", stage), jsonText(map[string]any{"input": patch}))
					break
				}
				if name == "exec" || strings.HasSuffix(name, "__exec") {
					call(name, fmt.Sprintf("call_smoke_patch_%d", stage), jsonText(map[string]any{"input": "text(await tools.apply_patch(" + jsonText(patch) + "));"}))
					break
				}
			}
		case 1, 3:
			patchResult = lastToolOutput()
			for _, raw := range tools {
				tool := object(object(raw)["function"])
				name := str(tool["name"])
				command := "go test -v ./..."
				if name == "exec" || strings.HasSuffix(name, "__exec") {
					code := "let result = await tools.exec_command(" + jsonText(map[string]any{"cmd": command, "workdir": dir, "yield_time_ms": 10000}) + "); while (result.session_id) { result = await tools.write_stdin({session_id: result.session_id, chars: '', yield_time_ms: 1000}); } text(result);"
					call(name, fmt.Sprintf("call_smoke_test_%d", stage), jsonText(map[string]any{"input": code}))
					break
				}
				if !strings.Contains(name, "exec_command") && !strings.Contains(name, "shell_command") && name != "shell" {
					continue
				}
				props := object(object(tool["parameters"])["properties"])
				args := map[string]any{}
				if props["cmd"] != nil {
					args["cmd"] = command
				} else if props["command"] != nil {
					if str(object(props["command"])["type"]) == "array" {
						if runtime.GOOS == "windows" {
							args["command"] = []string{"powershell", "-NoProfile", "-Command", command}
						} else {
							args["command"] = []string{"sh", "-lc", command}
						}
					} else {
						args["command"] = command
					}
				} else {
					continue
				}
				call(name, fmt.Sprintf("call_smoke_test_%d", stage), jsonText(args))
				break
			}
		default:
			commandResult = lastToolOutput()
			result.Blocks = []responseBlock{{Kind: "text", Text: "codex-smoke-ok"}}
		}
		if len(result.Blocks) == 0 {
			t.Errorf("no suitable CLI tool at stage %d; names: %v", stage, seenTools)
			result.Blocks = []responseBlock{{Kind: "text", Text: "fixture could not find the requested tool"}}
		}
		stage++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(renderProtocolResponse(FormatOpenAIChat, "gpt-5.4", "", result))
	}))
	defer upstream.Close()
	up := &Upstream{ID: 901, Name: "CLI smoke", Type: "openai", OpenAIProtocol: "chat", BaseURL: upstream.URL, APIKey: "local-test-key"}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		cr, err := parseClientRequest(FormatOpenAIResponse, body, "")
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid request", 400)
			return
		}
		if cr.ConversionError != nil {
			t.Errorf("CLI request conversion: %v", cr.ConversionError)
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		_, wrote, err := doRelay(c, up, &relayTask{apiKeyID: 901, format: FormatOpenAIResponse, endpoint: "/v1/responses", body: body, canon: cr, model: cr.Model, stream: cr.Stream})
		if err != nil {
			t.Errorf("CLI relay: %v", err)
			if !wrote {
				writeFormatError(c, FormatOpenAIResponse, 400, err.Error())
			}
		}
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--json", "-C", dir, "-m", "gpt-5.4", "-s", "danger-full-access",
		"-c", "approval_policy=\"never\"", "-c", "web_search=\"disabled\"", "-c", "model_provider=\"apishare_smoke\"", "-c", "model_providers.apishare_smoke.name=\"API Share local smoke\"",
		"-c", fmt.Sprintf("model_catalog_json=%q", catalogPath),
		"-c", fmt.Sprintf("model_providers.apishare_smoke.base_url=%q", gateway.URL+"/v1"), "-c", "model_providers.apishare_smoke.wire_api=\"responses\"", "-c", "model_providers.apishare_smoke.env_key=\"APISHARE_CODEX_SMOKE_KEY\"", "-c", "model_providers.apishare_smoke.requires_openai_auth=false",
		"Create a Sum function and test in a Go module, run the test, repair any failure, and run it again. Finish with codex-smoke-ok.")
	cmd.Env = smokeEnv
	output, err := cmd.CombinedOutput()
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		t.Fatalf("Codex CLI: %v\n%s", err, output)
	}
	if stage != 5 || !strings.Contains(firstTestResult, "FAIL") || !strings.Contains(commandResult, "PASS") || !strings.Contains(string(output), "codex-smoke-ok") {
		t.Fatalf("incomplete CLI loop: stages=%d patch=%s first_test=%s final_test=%s\n%s", stage, patchResult, firstTestResult, commandResult, output)
	}
	content, err := os.ReadFile(filepath.Join(dir, "sum.go"))
	if err != nil || !strings.Contains(string(content), "return a + b") {
		t.Fatalf("CLI did not apply the patch: %v / %q", err, content)
	}
}
