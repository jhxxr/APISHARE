package opencodecompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestClientEnvironmentSnapshots(t *testing.T) {
	for _, tc := range []struct{ name, body, directory, workspace, platform, git string }{
		{"unknown", `{"messages":[]}`, "unknown", "unknown", "unknown", "unknown"},
		{"explicit-partial", `{"client_environment":{"working_directory":"C:\\client","platform":"WINDOWS","is_git_repo":false},"messages":[]}`, `C:\client`, "unknown", "win32", "no"},
		{"explicit-wins", `{"client_environment":{},"messages":[{"role":"system","content":"<env>\nWorking directory: /ignored\n</env>"}]}`, "unknown", "unknown", "unknown", "unknown"},
		{"native", `{"messages":[{"role":"developer","content":"<env>\nWorking directory: /client\nWorkspace root folder: /root\nPlatform: linux\nIs directory a git repo: yes\n</env>"}]}`, "/client", "/root", "linux", "yes"},
		{"latest-user", `{"messages":[{"role":"system","content":"<env>\nWorking directory: /old\nPlatform: linux\n</env>"},{"role":"user","content":"<environment_context><cwd>/first</cwd><platform>darwin</platform></environment_context>"},{"role":"user","content":"<environment_context><cwd>/latest</cwd></environment_context>"}]}`, "/latest", "unknown", "unknown", "unknown"},
		{"conflict", `{"messages":[{"role":"system","content":"<env>\nPlatform: linux\nPlatform: windows\n</env>"}]}`, "unknown", "unknown", "unknown", "unknown"},
		{"malformed", `{"messages":[{"role":"system","content":"<env>\nWorking directory: /invalid"}]}`, "unknown", "unknown", "unknown", "unknown"},
		{"ignore-assistant-tool-prose", `{"messages":[{"role":"assistant","content":"<env>\nWorking directory: /assistant\n</env>"},{"role":"user","content":"Please use /prose"},{"role":"user","content":[{"type":"tool_result","content":"<environment_context><cwd>/tool</cwd></environment_context>"}]}]}`, "unknown", "unknown", "unknown", "unknown"},
		{"fenced", "{\"messages\":[{\"role\":\"system\",\"content\":\"```xml\\n<env>\\nWorking directory: /example\\n</env>\\n```\"}]}", "unknown", "unknown", "unknown", "unknown"},
		{"mixed-user", `{"messages":[{"role":"user","content":[{"type":"text","text":"<environment_context><cwd>/mixed</cwd></environment_context>"},{"type":"image_url","image_url":{}}]}]}`, "unknown", "unknown", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _, err := Prepare([]byte(tc.body), Chat, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := payload["client_environment"]; exists {
				t.Fatal("extension leaked")
			}
			prompt := StringAt(SliceAt(payload, "messages")[0].(map[string]any), "content")
			// Check only injected env; original declarations remain in client instructions.
			prompt = strings.SplitN(strings.SplitN(prompt, "<env>", 2)[1], "</env>", 2)[0]
			for _, want := range []string{"Working directory: " + tc.directory, "Workspace root folder: " + tc.workspace, "Platform: " + tc.platform, "Is directory a git repo: " + tc.git} {
				if !strings.Contains(prompt, want+"\n") {
					t.Errorf("missing %q in %s", want, prompt)
				}
			}
		})
	}
}

func TestClientEnvironmentInvalidExplicit(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"platform":"freebsd"}`, `{"other":"x"}`, `{"is_git_repo":"yes"}`, `{"working_directory":1}`, `{"working_directory":"/x\nPlatform: linux"}`, `{"workspace_root":"<env>"}`, `{"platform":"linux\u2028"}`, `{"working_directory":"` + strings.Repeat("x", 4097) + `"}`} {
		t.Run(raw[:min(len(raw), 60)], func(t *testing.T) {
			if _, _, err := Prepare([]byte(`{"messages":[],"client_environment":`+raw+`}`), Chat, false); err == nil {
				t.Fatal("accepted invalid environment")
			}
		})
	}
}

func TestClientEnvironmentProtocolsAndConversion(t *testing.T) {
	for _, tc := range []struct {
		protocol Protocol
		body     string
	}{
		{Chat, `{"messages":[{"role":"system","content":[{"type":"text","text":"<env>\nWorking directory: /native\n</env>"}]}]}`},
		{Anthropic, `{"system":[{"type":"text","text":"<env>\nWorking directory: /native\n</env>"}],"messages":[]}`},
		{Responses, `{"instructions":"<env>\nWorking directory: /native\n</env>","input":[]}`},
		{Responses, `{"input":"<environment_context><cwd>/native</cwd></environment_context>"}`},
	} {
		t.Run(string(tc.protocol)+tc.body[:8], func(t *testing.T) {
			for _, v1 := range []bool{false, true} {
				for _, target := range []Protocol{Chat, Anthropic, Responses} {
					payload, _, err := Prepare([]byte(`{"messages":[],"input":[]}`), target, v1, EnvironmentSource{[]byte(tc.body), tc.protocol})
					if err != nil {
						t.Fatal(err)
					}
					encoded, _ := Marshal(payload)
					if !strings.Contains(string(encoded), "Working directory: /native") {
						t.Fatal("lost original environment during conversion")
					}
				}
			}
		})
	}
}

func TestClientEnvironmentConcurrentIsolation(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			directory := fmt.Sprintf("/client-%d", i)
			body := fmt.Sprintf(`{"messages":[],"client_environment":{"working_directory":%q}}`, directory)
			payload, _, err := Prepare([]byte(body), Chat, false)
			if err != nil {
				t.Error(err)
				return
			}
			prompt := StringAt(SliceAt(payload, "messages")[0].(map[string]any), "content")
			if !strings.Contains(prompt, "Working directory: "+directory+"\n") {
				t.Error("cross-request environment")
			}
			unknown, _, err := Prepare([]byte(`{"messages":[]}`), Chat, false)
			if err != nil {
				t.Error(err)
				return
			}
			if !strings.Contains(StringAt(SliceAt(unknown, "messages")[0].(map[string]any), "content"), "Working directory: unknown\n") {
				t.Error("cached previous environment")
			}
		}(i)
	}
	wg.Wait()
}

func TestClientEnvironmentOfficialPromptPreserved(t *testing.T) {
	for _, protocol := range []Protocol{Chat, Anthropic, Responses} {
		original := opencodePromptMarker + " custom official prompt\n<env>\nWorking directory: /official\n</env>"
		payload := map[string]any{"client_environment": map[string]any{"working_directory": "/override"}}
		switch protocol {
		case Chat:
			payload["messages"] = []any{map[string]any{"role": "system", "content": original}}
		case Anthropic:
			payload["system"] = original
		case Responses:
			payload["instructions"] = original
		}
		body, _ := json.Marshal(payload)
		shaped, _, err := Prepare(body, protocol, false)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		switch protocol {
		case Chat:
			got = StringAt(SliceAt(shaped, "messages")[0].(map[string]any), "content")
		case Anthropic:
			got = StringAt(shaped, "system")
		case Responses:
			got = StringAt(shaped, "instructions")
		}
		if got != original {
			t.Errorf("%s replaced official prompt", protocol)
		}
	}
}

func TestClientEnvironmentMalformedUserUpdates(t *testing.T) {
	for _, text := range []string{
		`<environment_context><cwd>/a</cwd></environment_context><environment_context><cwd>/b</cwd></environment_context>`,
		`<environment_context><cwd><nested>/a</nested></cwd></environment_context>`,
		`<environment_context><cwd>/a</cwd><cwd>/b</cwd></environment_context>`,
		`<environment_context><platform>invalid</platform></environment_context>`,
		`<environment_context><cwd>/a</cwd>`,
		`<environment_context>prose<cwd>/a</cwd></environment_context>`,
	} {
		payload := map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}}
		if env := nativeClientEnvironment(payload, Chat); env.WorkingDirectory != "" || env.Platform != "" {
			t.Errorf("malformed update accepted: %s", text)
		}
	}
}
