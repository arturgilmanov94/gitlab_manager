package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Codex runs the OpenAI Codex CLI (`codex exec`). It is offered only when the binary is found.
// Codex reads AGENTS.md from the working directory, so project rules kept there still apply.
type Codex struct{ Bin string }

// Name is "codex".
func (c *Codex) Name() string { return "codex" }

// Detect reports the executable and version.
func (c *Codex) Detect() (string, string, bool) {
	path, err := exec.LookPath(c.Bin)
	if err != nil {
		return "", "", false
	}
	out, _ := exec.Command(path, "--version").CombinedOutput()
	return path, strings.TrimSpace(strings.Split(string(out), "\n")[0]), true
}

// BuildArgs assembles `codex exec` arguments. schemaFile/outFile are temp files.
func (c *Codex) BuildArgs(req Request, schemaFile, outFile string) []string {
	args := []string{"exec", "--skip-git-repo-check", "-C", req.Dir, "--output-last-message", outFile}
	if req.ResumeSessionID != "" {
		args = []string{"exec", "resume", req.ResumeSessionID, "--skip-git-repo-check", "--output-last-message", outFile}
	}
	if req.Mode == ModeEdit {
		args = append(args, "--sandbox", "workspace-write", "--full-auto")
	} else {
		args = append(args, "--sandbox", "read-only")
	}
	if !IsDefaultModel(req.Model) {
		args = append(args, "-m", req.Model)
	}
	if schemaFile != "" {
		args = append(args, "--output-schema", schemaFile)
	}
	return append(args, "-") // prompt on stdin
}

// Run executes Codex and reads the last message (JSON when a schema was given).
func (c *Codex) Run(ctx context.Context, req Request) (*Result, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tmp, err := os.MkdirTemp("", "mr-review-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	outFile := filepath.Join(tmp, "last-message.txt")
	schemaFile := ""
	if len(req.Schema) > 0 {
		schemaFile = filepath.Join(tmp, "schema.json")
		if err := os.WriteFile(schemaFile, req.Schema, 0o600); err != nil {
			return nil, err
		}
	}
	args := c.BuildArgs(req, schemaFile, outFile)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = req.Dir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	logf(req.Log, "$ cwd=%s\n$ %s %s\n--- prompt ---\n%s\n--- end prompt ---\n", req.Dir, c.Bin, quoteArgs(args), req.Prompt)
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started).Milliseconds()
	logf(req.Log, "--- stderr ---\n%s\n--- stdout ---\n%s\n--- exit in %d ms ---\n", stderr.String(), stdout.String(), elapsed)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("codex timed out after %s", timeout)
	}
	if ctx.Err() == context.Canceled {
		return nil, ErrCancelled
	}
	if runErr != nil {
		return nil, fmt.Errorf("codex failed: %s", firstNonEmptyStr(lastNonEmpty(stderr.String()), runErr.Error()))
	}
	last, _ := os.ReadFile(outFile)
	res := &Result{Text: strings.TrimSpace(string(last)), DurationMs: elapsed, Raw: stdout.Bytes(), SessionID: codexSessionID(stdout.String(), stderr.String())}
	res.Usage.Input = codexTokensUsed(stdout.String(), stderr.String()) // codex reports one total; keep it as "input"
	if len(req.Schema) > 0 {
		text := res.Text
		if idx := strings.Index(text, "{"); idx > 0 {
			text = text[idx:]
		}
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) != nil {
			return res, errors.New("codex did not return the structured JSON result")
		}
		res.Structured, _ = json.Marshal(obj)
	}
	return res, nil
}

var codexTokensRe = regexp.MustCompile(`(?i)tokens used[:\s]+([\d,]+)`)

// codexTokensUsed parses the "tokens used: N" summary line codex prints (best effort).
func codexTokensUsed(outputs ...string) int64 {
	for _, out := range outputs {
		if m := codexTokensRe.FindStringSubmatch(out); m != nil {
			n, _ := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
			return n
		}
	}
	return 0
}

// codexSessionID extracts a session/thread id from codex output when present (best effort).
func codexSessionID(outputs ...string) string {
	for _, out := range outputs {
		for _, line := range strings.Split(out, "\n") {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "session id:") || strings.Contains(lower, "thread id:") {
				parts := strings.Fields(line)
				if len(parts) > 0 {
					return parts[len(parts)-1]
				}
			}
		}
	}
	return ""
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
