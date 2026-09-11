package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeJSON(t *testing.T) {
	p, err := ParseClaudeJSON("warning: x\n{\"type\":\"result\",\"result\":\"x\"}")
	if err != nil || p["type"] != "result" {
		t.Fatalf("%v %v", p, err)
	}
	for _, bad := range []string{"", "not json"} {
		if _, err := ParseClaudeJSON(bad); err == nil {
			t.Fatal("expected error")
		}
	}
}

func TestExtractStructured(t *testing.T) {
	if string(ExtractStructured(map[string]any{"structured_output": map[string]any{"a": 1.0}})) != `{"a":1}` {
		t.Fatal("structured_output")
	}
	if string(ExtractStructured(map[string]any{"result": "```json\n{\"a\": 3}\n```"})) != `{"a":3}` {
		t.Fatal("fenced")
	}
	if ExtractStructured(map[string]any{"result": "prose"}) != nil {
		t.Fatal("prose")
	}
}

func TestBuildArgsAutoPolicy(t *testing.T) {
	c := &Claude{Bin: "claude"}
	args := strings.Join(c.BuildArgs(Request{Schema: []byte(`{"type":"object"}`), Agent: "mr-review", Model: "opus", MaxBudgetUSD: "5",
		ExtraTools: []string{"Bash(php *)"}, SessionName: "s", ProtectDirs: []string{"/proj"}}), " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--input-format stream-json", "--permission-prompts host", "--permission-prompt-tool stdio",
		"--json-schema {\"type\":\"object\"}", "--permission-mode auto", `"denyWrite":["/proj"]`, `"autoAllowBashIfSandboxed":true`,
		"--agent mr-review", "--model opus", "--max-budget-usd 5", "Bash(php *)", "--disallowedTools Edit Write", "Bash(git checkout *)", "--name s"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if strings.Contains(args, "--allowedTools Read") {
		t.Fatal("auto policy must not pin the read-only allowlist")
	}
	edit := strings.Join(c.BuildArgs(Request{Mode: ModeEdit, ResumeSessionID: "abc", Agent: "mr-review"}), " ")
	if !strings.Contains(edit, "--permission-mode auto") || strings.Contains(edit, "denyWrite") || !strings.Contains(edit, "--resume abc") ||
		strings.Contains(edit, "--agent") || !strings.Contains(edit, "Bash(git push *)") || strings.Contains(edit, "--disallowedTools Edit") {
		t.Fatalf("edit args: %s", edit)
	}
	if strings.Contains(strings.Join(c.BuildArgs(Request{Model: "default"}), " "), "--model") {
		t.Fatal("default model must not pass --model")
	}
}

func TestBuildArgsManualPolicy(t *testing.T) {
	c := &Claude{Bin: "claude"}
	ro := strings.Join(c.BuildArgs(Request{Policy: PolicyManual, ProtectDirs: []string{"/proj"}}), " ")
	if !strings.Contains(ro, "--permission-mode manual") || !strings.Contains(ro, `"denyWrite":["/proj"]`) || !strings.Contains(ro, "--disallowedTools Edit Write") {
		t.Fatalf("%s", ro)
	}
	edit := strings.Join(c.BuildArgs(Request{Policy: PolicyManual, Mode: ModeEdit}), " ")
	if !strings.Contains(edit, "--permission-mode manual") || strings.Contains(edit, "denyWrite") || strings.Contains(edit, "--disallowedTools Edit") {
		t.Fatalf("%s", edit)
	}
}

func TestBuildArgsStrictPolicy(t *testing.T) {
	c := &Claude{Bin: "claude"}
	ro := strings.Join(c.BuildArgs(Request{Policy: PolicyStrict, ExtraTools: []string{"Bash(php *)"}}), " ")
	for _, want := range []string{"--permission-mode dontAsk", "--allowedTools Read Grep Glob", "Bash(php *)", "--disallowedTools Edit Write"} {
		if !strings.Contains(ro, want) {
			t.Fatalf("missing %q in %s", want, ro)
		}
	}
	if strings.Contains(ro, "--settings") {
		t.Fatal("strict policy does not configure the sandbox")
	}
	edit := strings.Join(c.BuildArgs(Request{Policy: PolicyStrict, Mode: ModeEdit}), " ")
	if !strings.Contains(edit, "--permission-mode acceptEdits") || strings.Contains(edit, "--allowedTools") || !strings.Contains(edit, "Bash(git commit *)") {
		t.Fatalf("edit args: %s", edit)
	}
}

func TestToolNote(t *testing.T) {
	if got := ToolNote("Bash", map[string]any{"command": "git status", "description": "Show status"}); got != "Bash: Show status" {
		t.Fatal(got)
	}
	if got := ToolNote("Read", map[string]any{"file_path": "/a/b.php"}); got != "Read: /a/b.php" {
		t.Fatal(got)
	}
	if got := ToolNote("Bash", map[string]any{"command": strings.Repeat("x", 200)}); !strings.HasSuffix(got, "…") || len(got) > 160 {
		t.Fatal(got)
	}
	if got := ToolNote("TodoWrite", nil); got != "TodoWrite" {
		t.Fatal(got)
	}
}

func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// streamScript is a fake Claude Code speaking stream-json: it acknowledges the initialize request, reports one
// tool call, asks for a permission and reflects the decision in the structured result.
const streamScript = `#!/bin/sh
read init
printf '{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}\n'
read user
printf '{"type":"system","subtype":"init","cwd":"%s","model":"m"}\n' "$PWD"
printf '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status --short","description":"Show status"}}],"usage":{"input_tokens":10,"cache_read_input_tokens":15000,"cache_creation_input_tokens":4990,"output_tokens":4}}}\n'
printf '{"type":"assistant","parent_tool_use_id":"sub-1","message":{"content":[{"type":"text","text":"subagent"}],"usage":{"input_tokens":90000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":4}}}\n'
printf '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/tmp/x.txt","content":"hi"},"description":"/tmp/x.txt","decision_reason":"outside working dir","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}\n'
read resp
case "$resp" in
  *'"behavior":"allow"'*'updatedPermissions'*) d=allowed-remembered;;
  *'"behavior":"allow"'*) d=allowed;;
  *) d=denied;;
esac
printf '{"type":"result","is_error":false,"total_cost_usd":0.5,"duration_ms":10,"session_id":"s1","num_turns":2,"modelUsage":{"m":{"inputTokens":7,"outputTokens":3,"contextWindow":200000}},"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"rm -rf x"}}],"structured_output":{"summary":"%s","cwd":"%s"}}\n' "$d" "$PWD"
cat >/dev/null
`

func TestRunStreamsPermissionsAndProgress(t *testing.T) {
	bin := fakeClaude(t, streamScript)
	dir := t.TempDir()
	var log bytes.Buffer
	var notes []string
	var asked []PermissionRequest
	var contexts []ContextUsage
	res, err := (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "prompt text", Schema: []byte(`{}`), Dir: dir, Log: &log,
		Progress: func(note string) { notes = append(notes, note) },
		Context:  func(c ContextUsage) { contexts = append(contexts, c) },
		Permission: func(ctx context.Context, req PermissionRequest) PermissionDecision {
			asked = append(asked, req)
			return PermissionDecision{Allow: true, ApplySuggestions: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Structured, &out)
	if out["cwd"] != dir || out["summary"] != "allowed-remembered" || res.CostUSD != 0.5 || res.SessionID != "s1" || res.Usage.Input != 7 || res.NumTurns != 2 {
		t.Fatalf("%+v %v", res, out)
	}
	if len(asked) != 1 || asked[0].ToolName != "Write" || asked[0].Description != "/tmp/x.txt" || asked[0].Reason != "outside working dir" || !strings.Contains(string(asked[0].Input), "hi") || len(asked[0].Suggestions) == 0 {
		t.Fatalf("permission request: %+v", asked)
	}
	if len(notes) != 1 || notes[0] != "Bash: Show status" {
		t.Fatalf("progress notes: %v", notes)
	}
	// Context fill: the main agent's last turn (subagent turns are skipped), the window from modelUsage.
	if len(contexts) != 1 || contexts[0].Tokens != 20000 || contexts[0].Window != 0 || contexts[0].Model != "m" {
		t.Fatalf("live context: %+v", contexts)
	}
	if res.Context.Tokens != 20000 || res.Context.Window != 200000 || res.Context.Percent() != 10 {
		t.Fatalf("result context: %+v", res.Context)
	}
	if !strings.Contains(string(res.Denials), "rm -rf x") {
		t.Fatalf("denials: %s", res.Denials)
	}
	if !strings.Contains(log.String(), "prompt text") || !strings.Contains(log.String(), "[permission?] Write") {
		t.Fatalf("log: %s", log.String())
	}

	// Denied without a Permission callback.
	res, err = (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "p", Schema: []byte(`{}`), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(res.Structured, &out)
	if out["summary"] != "denied" {
		t.Fatalf("expected denied, got %v", out["summary"])
	}
}

func TestRunErrorsAndTimeout(t *testing.T) {
	bin := fakeClaude(t, "#!/bin/sh\nread init\necho boom >&2\nexit 3\n")
	if _, err := (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "p", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom, got %v", err)
	}
	slow := fakeClaude(t, "#!/bin/sh\nread init\nsleep 30\n")
	_, err := (&Claude{Bin: slow}).Run(context.Background(), Request{Prompt: "p", Dir: t.TempDir(), Timeout: 500 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if _, err := (&Claude{Bin: slow}).Run(ctx, Request{Prompt: "p", Dir: t.TempDir()}); err != ErrCancelled {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}
}

func TestRunIsErrorResult(t *testing.T) {
	// A real Claude keeps stdin open until the dashboard closes it after the result: the fake mimics that.
	bin := fakeClaude(t, "#!/bin/sh\nread init\nprintf '{\"type\":\"result\",\"is_error\":true,\"result\":\"budget exceeded\",\"session_id\":\"s2\"}\\n'\ncat >/dev/null\n")
	res, err := (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "p", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") || res == nil || res.SessionID != "s2" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestClaudeUsageSumsModels(t *testing.T) {
	payload := map[string]any{
		"modelUsage": map[string]any{
			"a": map[string]any{"inputTokens": 1.0, "outputTokens": 2.0, "cacheReadInputTokens": 3.0, "cacheCreationInputTokens": 4.0},
			"b": map[string]any{"inputTokens": 10.0, "outputTokens": 20.0, "cacheReadInputTokens": 30.0, "cacheCreationInputTokens": 40.0},
		},
	}
	u := ClaudeUsage(payload)
	if u.Input != 11 || u.Output != 22 || u.CacheRead != 33 || u.CacheWrite != 44 || u.Total() != 110 {
		t.Fatalf("%+v", u)
	}
	fallback := ClaudeUsage(map[string]any{"usage": map[string]any{"input_tokens": 5.0, "output_tokens": 6.0}})
	if fallback.Input != 5 || fallback.Output != 6 {
		t.Fatalf("%+v", fallback)
	}
}
