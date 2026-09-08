package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Read-only tool surface for review/plan runs. Project settings may widen it further.
var claudeReadOnlyAllowed = []string{
	"Read", "Grep", "Glob", "WebFetch",
	"Bash(glab api *)", "Bash(glab mr view *)", "Bash(glab mr diff *)", "Bash(glab issue view *)", "Bash(jq *)",
	"Bash(git log *)", "Bash(git diff *)", "Bash(git show *)", "Bash(git fetch *)", "Bash(git ls-files *)",
	"Bash(git rev-parse *)", "Bash(git branch *)", "Bash(git merge-base *)", "Bash(git blame *)", "Bash(git status *)",
	"Bash(git remote *)", "Bash(cat *)", "Bash(head *)", "Bash(tail *)", "Bash(wc *)", "Bash(ls *)", "Bash(grep *)",
	"Bash(rg *)", "Bash(find *)",
}

var claudeReadOnlyDenied = []string{
	"Edit", "Write", "MultiEdit", "NotebookEdit",
	"Bash(git checkout *)", "Bash(git switch *)", "Bash(git reset *)", "Bash(git stash *)", "Bash(git commit *)",
	"Bash(git push *)", "Bash(git merge *)", "Bash(git rebase *)", "Bash(git clean *)", "Bash(git restore *)",
	"Bash(git worktree *)", "Bash(glab api *--method*)", "Bash(glab api *-X *)", "Bash(glab mr approve *)",
	"Bash(glab mr note *)", "Bash(glab mr merge *)", "Bash(glab mr close *)", "Bash(glab mr update *)",
}

// Edit mode (worktree): file edits allowed, git state changes and GitLab writes still denied.
var claudeEditDenied = []string{
	"Bash(git checkout *)", "Bash(git switch *)", "Bash(git reset *)", "Bash(git stash *)", "Bash(git commit *)",
	"Bash(git push *)", "Bash(git merge *)", "Bash(git rebase *)", "Bash(git clean *)", "Bash(git restore *)",
	"Bash(git worktree *)", "Bash(git branch -D *)", "Bash(glab api *--method*)", "Bash(glab api *-X *)",
	"Bash(glab mr create *)", "Bash(glab mr approve *)", "Bash(glab mr note *)", "Bash(glab mr merge *)",
	"Bash(glab mr close *)", "Bash(glab mr update *)", "Bash(rm -rf *)",
}

// Claude runs Claude Code non-interactively.
type Claude struct{ Bin string }

// Name is "claude".
func (c *Claude) Name() string { return "claude" }

// Detect reports the executable and version.
func (c *Claude) Detect() (string, string, bool) {
	path, err := exec.LookPath(c.Bin)
	if err != nil {
		return "", "", false
	}
	out, _ := exec.Command(path, "--version").CombinedOutput()
	return path, strings.TrimSpace(strings.Split(string(out), "\n")[0]), true
}

// BuildArgs assembles the command line (exported for tests and logs).
func (c *Claude) BuildArgs(req Request) []string {
	args := []string{"-p", "--output-format", "json", "--permission-prompts", "none"}
	if len(req.Schema) > 0 {
		args = append(args, "--json-schema", compactJSON(req.Schema))
	}
	if req.Mode == ModeEdit {
		args = append(args, "--permission-mode", "acceptEdits")
		args = append(args, "--disallowedTools")
		args = append(args, claudeEditDenied...)
	} else {
		args = append(args, "--permission-mode", "dontAsk")
		args = append(args, "--allowedTools")
		args = append(args, claudeReadOnlyAllowed...)
		args = append(args, req.ExtraTools...)
		args = append(args, "--disallowedTools")
		args = append(args, claudeReadOnlyDenied...)
	}
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	} else if req.Agent != "" {
		args = append(args, "--agent", req.Agent)
	}
	if !IsDefaultModel(req.Model) {
		args = append(args, "--model", req.Model)
	}
	if req.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", req.MaxBudgetUSD)
	}
	if req.SessionName != "" && req.ResumeSessionID == "" {
		args = append(args, "--name", req.SessionName)
	}
	return args
}

// Run executes Claude Code with cwd = req.Dir and parses the JSON result.
func (c *Claude) Run(ctx context.Context, req Request) (*Result, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := c.BuildArgs(req)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = req.Dir
	cmd.Env = cleanEnv()
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	logf(req.Log, "$ cwd=%s\n$ %s %s\n--- prompt ---\n%s\n--- end prompt ---\n", req.Dir, c.Bin, quoteArgs(args), req.Prompt)
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started).Milliseconds()
	if stderr.Len() > 0 {
		logf(req.Log, "--- stderr ---\n%s\n", stderr.String())
	}
	logf(req.Log, "--- stdout ---\n%s\n--- exit in %d ms ---\n", stdout.String(), elapsed)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("claude timed out after %s", timeout)
	}
	if ctx.Err() == context.Canceled {
		return nil, ErrCancelled
	}
	payload, perr := ParseClaudeJSON(stdout.String())
	if perr != nil {
		detail := lastNonEmpty(stderr.String())
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		return nil, fmt.Errorf("%v (%s)", perr, detail)
	}
	res := &Result{
		SessionID:  str(payload["session_id"]),
		CostUSD:    num(payload["total_cost_usd"]),
		Usage:      ClaudeUsage(payload),
		DurationMs: int64(num(payload["duration_ms"])),
		NumTurns:   int(num(payload["num_turns"])),
		Raw:        stdout.Bytes(),
	}
	if res.DurationMs == 0 {
		res.DurationMs = elapsed
	}
	if isErr, _ := payload["is_error"].(bool); isErr || runErr != nil {
		msg := str(payload["result"])
		if msg == "" {
			msg = str(payload["error"])
		}
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		return res, errors.New(strings.TrimSpace(msg))
	}
	res.Text = str(payload["result"])
	if len(req.Schema) > 0 {
		structured := ExtractStructured(payload)
		if structured == nil {
			return res, errors.New("claude did not return the structured JSON result")
		}
		res.Structured = structured
	}
	return res, nil
}

// ClaudeUsage sums token usage over every model listed in modelUsage (main agent plus subagents);
// it falls back to the top-level usage block of older CLI versions.
func ClaudeUsage(payload map[string]any) Usage {
	var u Usage
	if models, ok := payload["modelUsage"].(map[string]any); ok && len(models) > 0 {
		for _, v := range models {
			m, _ := v.(map[string]any)
			u.Input += int64(num(m["inputTokens"]))
			u.Output += int64(num(m["outputTokens"]))
			u.CacheRead += int64(num(m["cacheReadInputTokens"]))
			u.CacheWrite += int64(num(m["cacheCreationInputTokens"]))
		}
		return u
	}
	if usage, ok := payload["usage"].(map[string]any); ok {
		u.Input = int64(num(usage["input_tokens"]))
		u.Output = int64(num(usage["output_tokens"]))
		u.CacheRead = int64(num(usage["cache_read_input_tokens"]))
		u.CacheWrite = int64(num(usage["cache_creation_input_tokens"]))
	}
	return u
}

// ParseClaudeJSON parses `claude -p --output-format json` output, tolerating leading noise.
func ParseClaudeJSON(stdout string) (map[string]any, error) {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return nil, errors.New("claude produced no output")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload, nil
	}
	for idx := 0; idx < len(text); idx++ {
		if text[idx] != '{' {
			continue
		}
		if err := json.Unmarshal([]byte(text[idx:]), &payload); err == nil {
			return payload, nil
		}
	}
	return nil, errors.New("claude output is not JSON")
}

// ExtractStructured returns structured_output, or parses the result text as JSON.
func ExtractStructured(payload map[string]any) json.RawMessage {
	if so, ok := payload["structured_output"].(map[string]any); ok {
		raw, _ := json.Marshal(so)
		return raw
	}
	switch result := payload["result"].(type) {
	case map[string]any:
		raw, _ := json.Marshal(result)
		return raw
	case string:
		text := strings.TrimSpace(result)
		if strings.HasPrefix(text, "```") {
			text = strings.Trim(text, "`")
			if idx := strings.Index(text, "{"); idx >= 0 {
				text = text[idx:]
			}
		}
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) == nil {
			raw, _ := json.Marshal(obj)
			return raw
		}
	}
	return nil
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		// A run launched from inside another Claude Code session must not inherit its nesting markers.
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func quoteArgs(args []string) string {
	var parts []string
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \"'()*{}[]$&|;<>") {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

func logf(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format, args...)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func lastNonEmpty(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
