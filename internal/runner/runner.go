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

// ErrCancelled is returned when the run was cancelled through the context.
var ErrCancelled = errors.New("cancelled")

// Request describes one agent invocation.
type Request struct {
	Prompt          string
	Schema          []byte // JSON schema for the structured result; nil = plain text answer
	Dir             string // cwd — the project root or a worktree
	Model           string // "" or "default" = runner/project default
	Agent           string // Claude agent name (kind=agent skills); ignored by other runners
	ResumeSessionID string // continue a previous session (follow-up questions)
	Mode            Mode
	ExtraTools      []string
	MaxBudgetUSD    string
	Timeout         time.Duration
	SessionName     string
	Log             io.Writer
}

// Result is what an agent returned.
type Result struct {
	Structured json.RawMessage // parsed structured output (when Schema was given)
	Text       string          // final plain-text answer
	SessionID  string
	CostUSD    float64
	DurationMs int64
	NumTurns   int
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
