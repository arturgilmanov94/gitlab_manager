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

func TestBuildArgsReadOnlyAndEdit(t *testing.T) {
	c := &Claude{Bin: "claude"}
	args := strings.Join(c.BuildArgs(Request{Schema: []byte(`{"type":"object"}`), Agent: "mr-review", Model: "opus", MaxBudgetUSD: "5", ExtraTools: []string{"Bash(php *)"}, SessionName: "s"}), " ")
	for _, want := range []string{"-p", "--output-format json", "--json-schema {\"type\":\"object\"}", "--permission-mode dontAsk", "--permission-prompts none", "--agent mr-review", "--model opus", "--max-budget-usd 5", "Bash(php *)", "Edit", "Bash(git checkout *)", "--name s"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	edit := strings.Join(c.BuildArgs(Request{Mode: ModeEdit, ResumeSessionID: "abc", Agent: "mr-review"}), " ")
	if !strings.Contains(edit, "--permission-mode acceptEdits") || strings.Contains(edit, "--allowedTools") || !strings.Contains(edit, "--resume abc") || strings.Contains(edit, "--agent") {
		t.Fatalf("edit args: %s", edit)
	}
	if strings.Contains(strings.Join(c.BuildArgs(Request{Model: "default"}), " "), "--model") {
		t.Fatal("default model must not pass --model")
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

func TestRunParsesOutputAndUsesDir(t *testing.T) {
	bin := fakeClaude(t, "#!/bin/sh\ncat >/dev/null\nprintf '{\"type\":\"result\",\"is_error\":false,\"total_cost_usd\":0.5,\"duration_ms\":10,\"session_id\":\"s1\",\"structured_output\":{\"summary\":\"ok\",\"cwd\":\"'\"$PWD\"'\"}}'\n")
	dir := t.TempDir()
	var log bytes.Buffer
	res, err := (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "prompt text", Schema: []byte(`{}`), Dir: dir, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(res.Structured, &out)
	if out["cwd"] != dir || res.CostUSD != 0.5 || res.SessionID != "s1" {
		t.Fatalf("%+v %v", res, out)
	}
	if !strings.Contains(log.String(), "prompt text") {
		t.Fatal("prompt not logged")
	}
}

func TestRunErrorsAndTimeout(t *testing.T) {
	bin := fakeClaude(t, "#!/bin/sh\ncat >/dev/null\necho boom >&2\nexit 3\n")
	if _, err := (&Claude{Bin: bin}).Run(context.Background(), Request{Prompt: "p", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom, got %v", err)
	}
	slow := fakeClaude(t, "#!/bin/sh\ncat >/dev/null\nsleep 30\n")
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
