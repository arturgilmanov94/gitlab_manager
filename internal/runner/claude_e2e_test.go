//go:build e2e

package runner

// End-to-end check against the real Claude Code CLI (costs tokens; run by hand):
//
//	go test -tags e2e -run TestE2E -v ./internal/runner
//
// It verifies the assumptions the dashboard relies on: the stream-json protocol with --json-schema returns a
// structured result, the sandbox settings make the project root read-only for the shell, and a permission
// prompt reaches Request.Permission and is honoured.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestE2EClaudeStreamProtocol(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("probe\n"), 0o644)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	var log bytes.Buffer
	var asked []PermissionRequest
	var notes []string
	schema := []byte(`{"type":"object","properties":{"inside_write":{"type":"string"},"outside_write":{"type":"string"},"subagent":{"type":"string"}},"required":["inside_write","outside_write","subagent"],"additionalProperties":false}`)
	prompt := "Do exactly these steps with Bash and report each as WORKED or DENIED plus the error text:\n" +
		"1. `echo x >> README.md` (inside the working directory) -> field inside_write\n" +
		"2. `echo x > " + outside + "` -> field outside_write\n" +
		"3. call the Agent tool with subagent_type Explore to list the files in the working directory -> field subagent\n" +
		"Answer only with the JSON object."
	res, err := (&Claude{Bin: "claude"}).Run(context.Background(), Request{
		Prompt: prompt, Schema: schema, Dir: dir, Model: "sonnet", Timeout: 4 * time.Minute, Log: &log,
		Mode: ModeReadOnly, Policy: PolicyAuto, ProtectDirs: []string{dir},
		Progress: func(note string) { notes = append(notes, note) },
		Permission: func(ctx context.Context, req PermissionRequest) PermissionDecision {
			asked = append(asked, req)
			return PermissionDecision{Allow: true}
		},
	})
	t.Logf("log:\n%s", log.String())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		t.Fatalf("structured: %v (%s)", err, res.Structured)
	}
	t.Logf("result: %v; asked: %d; notes: %d; usage: %+v", out, len(asked), len(notes), res.Usage)
	if !strings.HasPrefix(out["inside_write"], "DENIED") {
		t.Errorf("the project root must be read-only for the shell: %q", out["inside_write"])
	}
	if !strings.HasPrefix(out["subagent"], "WORKED") {
		t.Errorf("subagents must be available: %q", out["subagent"])
	}
	if len(notes) == 0 || res.SessionID == "" || res.Usage.Total() == 0 {
		t.Errorf("progress notes, session id and usage must be reported: %d %q %+v", len(notes), res.SessionID, res.Usage)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(data) != "probe\n" {
		t.Errorf("README.md in the protected dir was modified: %q %v", data, err)
	}
}

// In edit mode the Write tool is available; under the manual policy writing outside the worktree is exactly
// what Claude Code asks about, so the prompt must reach Request.Permission and the answer must decide the
// outcome. (Under the auto policy the classifier approves such writes on its own, without a prompt.)
func TestE2EClaudePermissionPrompt(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed")
	}
	dir := t.TempDir()
	allowed := filepath.Join(t.TempDir(), "allowed.txt")
	denied := filepath.Join(t.TempDir(), "denied.txt")
	var log bytes.Buffer
	var asked []PermissionRequest
	prompt := "Use the Write tool twice and report each result in one line: (1) create " + allowed + " with content ok; (2) create " + denied + " with content no. Plain text answer."
	res, err := (&Claude{Bin: "claude"}).Run(context.Background(), Request{
		Prompt: prompt, Dir: dir, Model: "sonnet", Timeout: 4 * time.Minute, Log: &log, Mode: ModeEdit, Policy: PolicyManual,
		Permission: func(ctx context.Context, req PermissionRequest) PermissionDecision {
			asked = append(asked, req)
			if strings.Contains(string(req.Input), "allowed.txt") {
				return PermissionDecision{Allow: true}
			}
			return PermissionDecision{Message: "the developer denied this write"}
		},
	})
	t.Logf("log:\n%s", log.String())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("answer: %s; asked: %d", res.Text, len(asked))
	if len(asked) != 2 || asked[0].ToolName != "Write" || asked[0].Reason == "" {
		t.Fatalf("both writes must prompt: %+v", asked)
	}
	if _, err := os.Stat(allowed); err != nil {
		t.Errorf("allowed write must have happened: %v", err)
	}
	if _, err := os.Stat(denied); err == nil {
		t.Errorf("denied write must not have happened")
	}
	if !strings.Contains(string(res.Denials), "denied.txt") {
		t.Errorf("the denial must be reported in permission_denials: %s", res.Denials)
	}
}
