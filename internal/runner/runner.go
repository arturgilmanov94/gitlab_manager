// Package runner abstracts the coding agents that execute a prompt (Claude Code, Codex CLI).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Mode controls what the agent may do.
type Mode string

const (
	ModeReadOnly Mode = "read-only"
	ModeEdit     Mode = "edit"
)

// Policy selects how permissions are decided for a run.
type Policy string

const (
	// PolicyAuto lets the agent's own permission classifier decide, runs the shell in the agent's sandbox and
	// forwards everything that would prompt to Request.Permission (the dashboard user). Default.
	PolicyAuto Policy = "auto"
	// PolicyManual is the classic Claude Code mode: project/user permission rules apply, the shell runs in the
	// sandbox, and everything else is asked — the prompt goes to Request.Permission (the dashboard user).
	PolicyManual Policy = "manual"
	// PolicyStrict is the fixed allow/deny list of tools; anything else is denied without asking.
	PolicyStrict Policy = "strict"
)

// ErrCancelled is returned when the run was cancelled through the context.
var ErrCancelled = errors.New("cancelled")

// PermissionRequest is one tool call the agent wants to make but is not allowed to without approval.
type PermissionRequest struct {
	ToolName    string
	Description string          // agent's short description of the call (command, path, ...)
	Input       json.RawMessage // tool input as JSON
	Reason      string          // why the agent could not decide itself
	Suggestions json.RawMessage // agent-proposed permission rules that would make similar calls automatic (opaque)
	ToolUseID   string
}

// PermissionDecision is the answer to a PermissionRequest.
type PermissionDecision struct {
	Allow            bool
	Message          string // shown to the agent when denied
	ApplySuggestions bool   // also apply Suggestions for the rest of the session ("allow and remember")
}

// PermissionFunc decides a permission request; it must return promptly once ctx is done.
type PermissionFunc func(ctx context.Context, req PermissionRequest) PermissionDecision

// Request describes one agent invocation.
type Request struct {
	Prompt          string
	Schema          []byte // JSON schema for the structured result; nil = plain text answer
	Dir             string // cwd — the project root or a worktree
	Model           string // "" or "default" = runner/project default
	Agent           string // Claude agent name (kind=agent skills); ignored by other runners
	ResumeSessionID string // continue a previous session (follow-up questions)
	Mode            Mode
	Policy          Policy   // "" = PolicyAuto
	ProtectDirs     []string // directories the sandboxed shell must not write to (read-only runs: the project root)
	ExtraTools      []string
	MaxBudgetUSD    string
	Timeout         time.Duration
	SessionName     string
	Log             io.Writer
	Permission      PermissionFunc     // nil: anything that would prompt is denied
	Progress        func(note string)  // optional: short notes about what the agent is doing right now
	Event           func(ev ToolEvent) // optional: every tool call, for the structured timeline
}

// ToolEvent is one tool call of the agent.
type ToolEvent struct {
	Tool   string // Read, Edit, Bash, Agent, ...
	Detail string // file, pattern, command description (already shortened)
}

// Usage is the token consumption of a run, summed over every model the agent used.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Total is the sum of all token kinds.
func (u Usage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Result is what an agent returned.
type Result struct {
	Structured json.RawMessage // parsed structured output (when Schema was given)
	Text       string          // final plain-text answer
	SessionID  string
	CostUSD    float64
	Usage      Usage
	DurationMs int64
	NumTurns   int
	Denials    json.RawMessage // tool calls the agent was refused (Claude Code permission_denials), for the run page
	Raw        json.RawMessage
}

// Runner is implemented per agent.
type Runner interface {
	Name() string
	Detect() (path, version string, ok bool)
	Run(ctx context.Context, req Request) (*Result, error)
}

// Info describes an available runner for the UI.
type Info struct {
	Name    string
	Path    string
	Version string
	Models  []string
}

// IsDefaultModel reports whether the model selector means "use the default".
func IsDefaultModel(model string) bool { return model == "" || model == "default" }
