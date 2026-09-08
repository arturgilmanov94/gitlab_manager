package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Cursor runs the Cursor CLI agent (`cursor-agent`) non-interactively. Offered only when found in PATH.
// It follows the project's .cursor rules; structured output is requested through the prompt.
type Cursor struct{ Bin string }

// Name is "cursor".
func (c *Cursor) Name() string { return "cursor" }

// Detect reports the executable and version.
func (c *Cursor) Detect() (string, string, bool) {
	path, err := exec.LookPath(c.Bin)
	if err != nil {
		return "", "", false
	}
	out, _ := exec.Command(path, "--version").CombinedOutput()
	return path, strings.TrimSpace(strings.Split(string(out), "\n")[0]), true
}

// BuildArgs assembles the cursor-agent command line.
func (c *Cursor) BuildArgs(req Request) []string {
	args := []string{"--print", "--output-format", "text"}
	if req.Mode == ModeEdit {
		args = append(args, "--force")
	}
	if !IsDefaultModel(req.Model) {
		args = append(args, "--model", req.Model)
	}
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	}
	return args
}

// Run executes cursor-agent with the prompt on stdin.
func (c *Cursor) Run(ctx context.Context, req Request) (*Result, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt := req.Prompt
	if len(req.Schema) > 0 {
		prompt += "\n\nReturn ONLY a JSON object that validates against this JSON schema:\n" + compactJSON(req.Schema) + "\n"
	}
	args := c.BuildArgs(req)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = req.Dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	logf(req.Log, "$ cwd=%s\n$ %s %s\n--- prompt ---\n%s\n--- end prompt ---\n", req.Dir, c.Bin, quoteArgs(args), prompt)
	started := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(started).Milliseconds()
	logf(req.Log, "--- stderr ---\n%s\n--- stdout ---\n%s\n--- exit in %d ms ---\n", stderr.String(), stdout.String(), elapsed)
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("cursor-agent timed out after %s", timeout)
	}
	if ctx.Err() == context.Canceled {
		return nil, ErrCancelled
	}
	if runErr != nil {
		return nil, fmt.Errorf("cursor-agent failed: %s", firstNonEmptyStr(lastNonEmpty(stderr.String()), runErr.Error()))
	}
	res := &Result{Text: strings.TrimSpace(stdout.String()), DurationMs: elapsed, Raw: stdout.Bytes()}
	if len(req.Schema) > 0 {
		text := res.Text
		if idx := strings.LastIndex(text, "\n{"); idx >= 0 {
			text = text[idx+1:]
		} else if idx := strings.Index(text, "{"); idx > 0 {
			text = text[idx:]
		}
		text = strings.Trim(text, "` \n")
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) != nil {
			return res, errors.New("cursor-agent did not return the structured JSON result")
		}
		res.Structured, _ = json.Marshal(obj)
	}
	return res, nil
}
