package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Read-only tool surface for the strict policy. Project settings may widen it further.
var claudeReadOnlyAllowed = []string{
	"Read", "Grep", "Glob", "WebFetch",
	"Bash(glab api *)", "Bash(glab mr view *)", "Bash(glab mr diff *)", "Bash(glab issue view *)", "Bash(jq *)",
	"Bash(git log *)", "Bash(git diff *)", "Bash(git show *)", "Bash(git fetch *)", "Bash(git ls-files *)",
	"Bash(git rev-parse *)", "Bash(git branch *)", "Bash(git merge-base *)", "Bash(git blame *)", "Bash(git status *)",
	"Bash(git remote *)", "Bash(cat *)", "Bash(head *)", "Bash(tail *)", "Bash(wc *)", "Bash(ls *)", "Bash(grep *)",
	"Bash(rg *)", "Bash(find *)",
}

// Read-only runs never edit files, never change git state and never write to GitLab — under both policies.
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

// Claude runs Claude Code non-interactively over the stream-json protocol: the dashboard sees every tool
// call as it happens and answers permission prompts (`--permission-prompt-tool stdio`).
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
	args := []string{"-p", "--output-format", "stream-json", "--input-format", "stream-json", "--verbose",
		"--permission-prompts", "host", "--permission-prompt-tool", "stdio"}
	if len(req.Schema) > 0 {
		args = append(args, "--json-schema", compactJSON(req.Schema))
	}
	switch {
	case req.Policy == PolicyStrict && req.Mode == ModeEdit:
		args = append(args, "--permission-mode", "acceptEdits", "--disallowedTools")
		args = append(args, claudeEditDenied...)
	case req.Policy == PolicyStrict:
		args = append(args, "--permission-mode", "dontAsk", "--allowedTools")
		args = append(args, claudeReadOnlyAllowed...)
		args = append(args, req.ExtraTools...)
		args = append(args, "--disallowedTools")
		args = append(args, claudeReadOnlyDenied...)
	case req.Mode == ModeEdit:
		args = append(args, "--permission-mode", permissionMode(req.Policy), "--settings", SandboxSettings(nil), "--disallowedTools")
		args = append(args, claudeEditDenied...)
	default:
		args = append(args, "--permission-mode", permissionMode(req.Policy), "--settings", SandboxSettings(req.ProtectDirs))
		if len(req.ExtraTools) > 0 {
			args = append(args, "--allowedTools")
			args = append(args, req.ExtraTools...)
		}
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

// permissionMode maps the sandboxed policies onto Claude Code's --permission-mode.
func permissionMode(p Policy) string {
	if p == PolicyManual {
		return "manual"
	}
	return "auto"
}

// SandboxSettings is the `--settings` JSON that runs the agent's shell in its sandbox (no prompts for
// sandboxed commands) and, for read-only runs, makes the given directories read-only for the shell.
func SandboxSettings(protect []string) string {
	sandbox := map[string]any{"enabled": true, "autoAllowBashIfSandboxed": true}
	if len(protect) > 0 {
		sandbox["filesystem"] = map[string]any{"denyWrite": protect}
	}
	raw, _ := json.Marshal(map[string]any{"sandbox": sandbox})
	return string(raw)
}

// Run executes Claude Code with cwd = req.Dir, drives the stream-json protocol and parses the result.
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
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	logf(req.Log, "$ cwd=%s\n$ %s %s\n--- prompt ---\n%s\n--- end prompt ---\n", req.Dir, c.Bin, quoteArgs(args), req.Prompt)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	session := &streamSession{req: req, stdin: stdin, log: req.Log}
	session.send(map[string]any{"type": "control_request", "request_id": "init-1", "request": map[string]any{"subtype": "initialize", "hooks": map[string]any{}}})
	session.send(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": req.Prompt}})
	payload := session.consume(ctx, stdout)
	session.closeStdin()
	runErr := cmd.Wait()
	elapsed := time.Since(started).Milliseconds()
	if stderr.Len() > 0 {
		logf(req.Log, "--- stderr ---\n%s\n", stderr.String())
	}
	logf(req.Log, "--- exit in %d ms ---\n", elapsed)

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("claude timed out after %s", timeout)
	}
	if ctx.Err() == context.Canceled {
		return nil, ErrCancelled
	}
	if payload == nil {
		detail := lastNonEmpty(stderr.String())
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if detail == "" {
			detail = "claude produced no result event"
		}
		return nil, fmt.Errorf("claude did not finish: %s", detail)
	}
	raw, _ := json.Marshal(payload)
	res := &Result{
		SessionID:  str(payload["session_id"]),
		Models:     ClaudeModels(payload),
		CostUSD:    num(payload["total_cost_usd"]),
		Usage:      ClaudeUsage(payload),
		DurationMs: int64(num(payload["duration_ms"])),
		NumTurns:   int(num(payload["num_turns"])),
		Context:    ContextUsage{Tokens: session.ctx, Window: ClaudeContextWindow(payload, session.model), Model: session.model},
		Raw:        raw,
	}
	if denials, ok := payload["permission_denials"].([]any); ok && len(denials) > 0 {
		res.Denials, _ = json.Marshal(denials)
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

// streamSession drives one stream-json conversation: it forwards permission prompts to req.Permission,
// reports tool calls through req.Progress and returns the final result event.
type streamSession struct {
	req    Request
	stdin  io.WriteCloser
	log    io.Writer
	mu     sync.Mutex
	closed bool
	model  string // model of the session from the init event
	ctx    int64  // tokens in the context at the last turn of the main agent
}

func (s *streamSession) send(msg map[string]any) {
	raw, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		_, _ = s.stdin.Write(append(raw, '\n'))
	}
}

// closeStdin ends the conversation: Claude Code keeps waiting for further user messages while stdin is open,
// so the pipe is closed as soon as the result event has arrived (and again, harmlessly, after the process exits).
func (s *streamSession) closeStdin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		_ = s.stdin.Close()
	}
}

// consume reads events until the result event or EOF. It returns the result payload (nil when none).
func (s *streamSession) consume(ctx context.Context, stdout io.Reader) map[string]any {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var result map[string]any
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			logf(s.log, "%s\n", line)
			continue
		}
		switch str(event["type"]) {
		case "control_request":
			s.handleControlRequest(ctx, event)
		case "assistant":
			s.noteToolUse(event)
			s.noteContext(event)
		case "result":
			result = event
			logf(s.log, "--- result --- status=%s turns=%v duration=%vms cost=$%.2f (full JSON is stored with the run)\n%s\n",
				str(event["subtype"]), event["num_turns"], event["duration_ms"], num(event["total_cost_usd"]), truncate(str(event["result"]), 4000))
			s.closeStdin()
		case "system":
			switch str(event["subtype"]) {
			case "init":
				s.model = str(event["model"])
				logf(s.log, "[init] session=%s model=%s permissions=%s claude=%s cwd=%s\n",
					str(event["session_id"]), str(event["model"]), str(event["permissionMode"]), str(event["claude_code_version"]), str(event["cwd"]))
			case "permission_denied":
				logf(s.log, "[denied] %s\n", ToolNote(str(event["tool_name"]), event["tool_input"]))
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		logf(s.log, "--- stream read error: %v ---\n", err)
	}
	return result
}

// noteContext reads the usage of an assistant turn of the main agent (subagent turns carry parent_tool_use_id):
// input + cache read + cache write of one request is the size of the context at that turn.
func (s *streamSession) noteContext(event map[string]any) {
	if str(event["parent_tool_use_id"]) != "" {
		return
	}
	message, _ := event["message"].(map[string]any)
	usage, _ := message["usage"].(map[string]any)
	if usage == nil {
		return
	}
	tokens := int64(num(usage["input_tokens"]) + num(usage["cache_read_input_tokens"]) + num(usage["cache_creation_input_tokens"]))
	if tokens <= 0 || tokens == s.ctx {
		return
	}
	s.ctx = tokens
	if s.req.Context != nil {
		s.req.Context(ContextUsage{Tokens: tokens, Model: firstNonEmpty(str(message["model"]), s.model)})
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ClaudeContextWindow returns the context window size of the session's main model from modelUsage
// (the entry matching model, else the model with most output tokens); 0 when the CLI does not report it.
func ClaudeContextWindow(payload map[string]any, model string) int64 {
	models, ok := payload["modelUsage"].(map[string]any)
	if !ok {
		return 0
	}
	if m, ok := models[model].(map[string]any); ok && model != "" {
		return int64(num(m["contextWindow"]))
	}
	for _, name := range ClaudeModels(payload) {
		m, _ := models[name].(map[string]any)
		if w := int64(num(m["contextWindow"])); w > 0 {
			return w
		}
	}
	return 0
}

func (s *streamSession) noteToolUse(event map[string]any) {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, item := range content {
		block, _ := item.(map[string]any)
		if str(block["type"]) != "tool_use" {
			continue
		}
		name := str(block["name"])
		note := ToolNote(name, block["input"])
		logf(s.log, "[tool] %s\n", note)
		if s.req.Progress != nil {
			s.req.Progress(note)
		}
		if s.req.Event != nil {
			s.req.Event(ToolEvent{Tool: name, Detail: strings.TrimPrefix(note, name+": ")})
		}
	}
}

func (s *streamSession) handleControlRequest(ctx context.Context, event map[string]any) {
	request, _ := event["request"].(map[string]any)
	requestID := str(event["request_id"])
	if str(request["subtype"]) != "can_use_tool" {
		// Unknown request kinds (hooks, mcp) are answered with an error so the agent does not wait forever.
		s.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": "not supported by mr-review"}})
		return
	}
	input, _ := json.Marshal(request["input"])
	var suggestions json.RawMessage
	if request["permission_suggestions"] != nil {
		suggestions, _ = json.Marshal(request["permission_suggestions"])
	}
	permission := PermissionRequest{
		ToolName:    str(request["tool_name"]),
		Description: str(request["description"]),
		Input:       input,
		Reason:      str(request["decision_reason"]),
		Suggestions: suggestions,
		ToolUseID:   str(request["tool_use_id"]),
	}
	logf(s.log, "[permission?] %s — %s (%s)\n", permission.ToolName, permission.Description, permission.Reason)
	decision := PermissionDecision{Message: "mr-review: permission prompts are not answered for this run"}
	if s.req.Permission != nil {
		decision = s.req.Permission(ctx, permission)
	}
	var response map[string]any
	if decision.Allow {
		response = map[string]any{"behavior": "allow", "updatedInput": request["input"]}
		if decision.ApplySuggestions && len(suggestions) > 0 {
			response["updatedPermissions"] = request["permission_suggestions"]
			logf(s.log, "[permission] allowed, rules remembered for this session\n")
		} else {
			logf(s.log, "[permission] allowed\n")
		}
	} else {
		response = map[string]any{"behavior": "deny", "message": firstNonEmptyStr(decision.Message, "denied by the developer")}
		logf(s.log, "[permission] denied: %s\n", response["message"])
	}
	s.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": response}})
}

// ToolNote renders one tool call as a short human-readable progress line.
func ToolNote(name string, input any) string {
	in, _ := input.(map[string]any)
	detail := ""
	switch name {
	case "Bash":
		detail = firstNonEmptyStr(str(in["description"]), str(in["command"]))
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		detail = str(in["file_path"])
	case "Grep", "Glob":
		detail = str(in["pattern"])
	case "Agent", "Task":
		detail = firstNonEmptyStr(str(in["description"]), str(in["subagent_type"]))
	case "WebFetch":
		detail = str(in["url"])
	default:
		if len(in) > 0 {
			raw, _ := json.Marshal(in)
			detail = string(raw)
		}
	}
	detail = strings.ReplaceAll(detail, "\n", " ")
	if len(detail) > 140 {
		detail = detail[:140] + "…"
	}
	if detail == "" {
		return name
	}
	return name + ": " + detail
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

// ClaudeModels lists the models in modelUsage, the one with most output tokens first.
func ClaudeModels(payload map[string]any) []string {
	models, ok := payload["modelUsage"].(map[string]any)
	if !ok {
		return nil
	}
	type mu struct {
		name string
		out  float64
	}
	var list []mu
	for name, v := range models {
		m, _ := v.(map[string]any)
		list = append(list, mu{name, num(m["outputTokens"])})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].out != list[j].out {
			return list[i].out > list[j].out
		}
		return list[i].name < list[j].name
	})
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.name)
	}
	return out
}

// ParseClaudeJSON parses a single-JSON `claude -p` output (stored raw results), tolerating leading noise.
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
