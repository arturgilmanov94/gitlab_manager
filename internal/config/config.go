// Package config loads settings from the environment and an optional .env file.
// No machine-specific value is ever compiled into the binary.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mr-review/internal/projectroot"
	"mr-review/internal/skill"
)

// Settings is the effective runtime configuration.
type Settings struct {
	BaseDir           string // directory the binary lives in (dashboard directory)
	ProjectRoot       string // "" when unresolved
	ProjectRootSource string
	ProjectRootError  string

	Host        string
	Port        int
	OpenBrowser bool

	DatabasePath string
	LogDir       string
	RuntimeDir   string
	WorktreeDir  string

	ReviewSkill  string            // full-review skill name override (REVIEW_SKILL / SKILL_REVIEW_FULL)
	SkillNames   map[string]string // action kind → project skill name override (SKILL_* variables, then the UI)
	CustomSkills map[string]string // action kind → instructions written in the dashboard (UI); replace the project skill

	ClaudeBin          string
	ClaudeModels       []string
	ClaudeMaxBudgetUSD string
	ClaudeExtraTools   []string
	ClaudePermissions  string // auto (classifier + sandbox) | manual (rules + sandbox, ask the rest) | strict (fixed tool lists)
	ApprovalTimeoutSec int    // how long a run waits for the developer's answer to a permission prompt
	PlansDir           string // where "save plan" writes markdown; "" = <project>/.claude/plans
	ReportLanguage     string // language of the agent's human-readable output (ru by default)
	CodexBin           string
	CodexModels        []string
	DefaultRunner      string
	RunTimeoutSec      int

	GlabBin               string
	GitLabHost            string
	GitLabProject         string // override when the origin remote cannot be parsed
	GitLabSyncRoles       []string
	GitLabSyncOnlyProject bool
	// GitLabIssueProjects lists the project paths whose issues are synchronised (tasks often live in other trackers
	// than the code). Empty = every project where I am the assignee. GITLAB_ISSUE_PROJECTS=tn/project/eu/eu,tn/core/tradernet
	GitLabIssueProjects []string

	BaseBranch     string
	RunConcurrency int

	// ReviewWorktree runs reviews in a read-only worktree at the MR head SHA (REVIEW_WORKTREE, default on).
	ReviewWorktree bool
	// ReviewWorktreeTTLMin is how long an idle review worktree is kept after its last session before the janitor
	// removes it (REVIEW_WORKTREE_TTL_MIN, default 60; 0 = keep until the MR leaves the list or by hand).
	ReviewWorktreeTTLMin int
	// PrefetchMaxDiffChars caps the MR diff inlined into review prompts (PREFETCH_MAX_DIFF_CHARS; 0 = never inline).
	PrefetchMaxDiffChars int

	// StandSkill names the project skill that explains how to reach the developer's stand ("" = detect by
	// "стенд/stand" in a skill's name or description). Used by «Проверить на стенде».
	StandSkill string

	// TerminalCmd is the TERMINAL_CMD template for «Открыть в терминале» ("" = detect gnome-terminal, konsole, ...).
	TerminalCmd string

	// HighlightLabels are the GitLab labels worth showing (and filtering by) in the lists, with their colours.
	// Every other label stays out of the way. Configured by HIGHLIGHT_LABELS=high:red,product:yellow,bug:orange.
	HighlightLabels []LabelStyle
}

// LabelStyle is a GitLab label shown as a coloured badge.
type LabelStyle struct {
	Name  string // label text as in GitLab (matched case-insensitively)
	Color string // red | orange | yellow | green | blue | gray
}

// DefaultHighlightLabels is the built-in HIGHLIGHT_LABELS value.
const DefaultHighlightLabels = "high:red,product:yellow,bug:orange"

// labelColors are the colours a highlighted label may use (CSS classes .label-<color>).
var labelColors = map[string]bool{"red": true, "orange": true, "yellow": true, "green": true, "blue": true, "gray": true}

// ParseHighlightLabels reads "name:color,name:color"; a missing or unknown colour becomes gray, duplicates are dropped.
func ParseHighlightLabels(value string) []LabelStyle {
	var out []LabelStyle
	seen := map[string]bool{}
	for _, item := range splitList(value) {
		name, color, _ := strings.Cut(item, ":")
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		color = strings.ToLower(strings.TrimSpace(color))
		if !labelColors[color] {
			color = "gray"
		}
		seen[strings.ToLower(name)] = true
		out = append(out, LabelStyle{Name: name, Color: color})
	}
	return out
}

// EnvFile returns the path of the local .env file.
func (s *Settings) EnvFile() string { return filepath.Join(s.BaseDir, ".env") }

// RunLogDir is where per-run logs are written.
func (s *Settings) RunLogDir() string { return filepath.Join(s.LogDir, "runs") }

// PidFile is the background server pid file.
func (s *Settings) PidFile() string { return filepath.Join(s.RuntimeDir, "server.pid") }

// Address is host:port.
func (s *Settings) Address() string { return s.Host + ":" + strconv.Itoa(s.Port) }

// URL is the browser URL.
func (s *Settings) URL() string { return "http://" + s.Address() }

// ParseEnvFile reads KEY=VALUE lines (comments, blank lines, quotes and `export` allowed).
func ParseEnvFile(path string) map[string]string {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if key != "" {
			values[key] = value
		}
	}
	return values
}

// Lookup resolves a key from the process environment first, then the .env file.
type Lookup func(key string) string

// NewLookup builds a Lookup over the given environment map and .env values.
func NewLookup(env map[string]string, envFile map[string]string) Lookup {
	return func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return envFile[key]
	}
}

// EnvironMap converts os.Environ() into a map.
func EnvironMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// Load builds Settings for the dashboard located in baseDir.
func Load(baseDir string, env map[string]string, readEnvFile bool) *Settings {
	baseDir, _ = filepath.Abs(baseDir)
	envFile := map[string]string{}
	if readEnvFile {
		envFile = ParseEnvFile(filepath.Join(baseDir, ".env"))
	}
	get := NewLookup(env, envFile)
	str := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(get(key))); err == nil {
			return v
		}
		return def
	}
	boolean := func(key string, def bool) bool {
		switch strings.ToLower(strings.TrimSpace(get(key))) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return def
	}
	path := func(key, def string) string {
		return resolvePath(str(key, def), baseDir)
	}

	s := &Settings{
		BaseDir:               baseDir,
		Host:                  str("HOST", "127.0.0.1"),
		Port:                  num("PORT", 8765),
		OpenBrowser:           boolean("OPEN_BROWSER", true),
		DatabasePath:          path("DATABASE_PATH", "./data/reviews.sqlite"),
		LogDir:                path("LOG_DIR", "./logs"),
		RuntimeDir:            path("RUNTIME_DIR", "./runtime"),
		WorktreeDir:           path("WORKTREE_DIR", "./runtime/worktrees"),
		SkillNames:            skillNames(get),
		ClaudeBin:             str("CLAUDE_BIN", "claude"),
		ClaudeModels:          splitList(str("CLAUDE_MODELS", "default,fable,opus,sonnet")),
		ClaudeMaxBudgetUSD:    strings.TrimSpace(get("CLAUDE_MAX_BUDGET_USD")),
		ClaudeExtraTools:      SplitTools(get("CLAUDE_EXTRA_ALLOWED_TOOLS")),
		ClaudePermissions:     strings.ToLower(str("CLAUDE_PERMISSIONS", "auto")),
		ApprovalTimeoutSec:    num("APPROVAL_TIMEOUT_SEC", 1800),
		ReportLanguage:        strings.ToLower(str("REPORT_LANGUAGE", "ru")),
		CodexBin:              str("CODEX_BIN", "codex"),
		CodexModels:           splitList(str("CODEX_MODELS", "default,gpt-5-codex")),
		DefaultRunner:         str("DEFAULT_RUNNER", "claude"),
		RunTimeoutSec:         num("RUN_TIMEOUT_SEC", 1800),
		GlabBin:               str("GLAB_BIN", "glab"),
		GitLabHost:            strings.TrimSpace(get("GITLAB_HOST")),
		GitLabProject:         strings.TrimSpace(get("GITLAB_PROJECT")),
		GitLabSyncRoles:       splitList(str("GITLAB_SYNC_ROLES", "reviewer,assignee,author")),
		GitLabSyncOnlyProject: boolean("GITLAB_SYNC_ONLY_PROJECT", true),
		GitLabIssueProjects:   splitList(get("GITLAB_ISSUE_PROJECTS")),
		BaseBranch:            str("BASE_BRANCH", "develop"),
		RunConcurrency:        num("RUN_CONCURRENCY", 4),
		HighlightLabels:       ParseHighlightLabels(str("HIGHLIGHT_LABELS", DefaultHighlightLabels)),
		TerminalCmd:           strings.TrimSpace(get("TERMINAL_CMD")),
		StandSkill:            strings.TrimSpace(get("STAND_SKILL")),
		ReviewWorktree:        boolean("REVIEW_WORKTREE", true),
		ReviewWorktreeTTLMin:  num("REVIEW_WORKTREE_TTL_MIN", 60),
		PrefetchMaxDiffChars:  num("PREFETCH_MAX_DIFF_CHARS", 60000),
	}
	if s.RunConcurrency < 1 {
		s.RunConcurrency = 1
	}
	if s.ClaudePermissions != "strict" && s.ClaudePermissions != "manual" {
		s.ClaudePermissions = "auto"
	}
	if s.ApprovalTimeoutSec < 60 {
		s.ApprovalTimeoutSec = 60
	}
	if v := strings.TrimSpace(get("PLANS_DIR")); v != "" {
		s.PlansDir = resolvePath(v, baseDir)
	}
	s.ReviewSkill = s.SkillNames[skill.ActionReviewFull]

	root, err := projectroot.Resolve(baseDir, get("PROJECT_ROOT"), "")
	if err != nil {
		s.ProjectRootError = err.Error()
	} else {
		s.ProjectRoot = root.Path
		s.ProjectRootSource = root.Source
	}
	return s
}

// skillNames reads the per-action skill name overrides (SKILL_REVIEW_FULL, SKILL_PLAN, ...).
// REVIEW_SKILL is the legacy alias of SKILL_REVIEW_FULL.
func skillNames(get Lookup) map[string]string {
	names := map[string]string{}
	for _, action := range skill.Actions {
		if name := strings.TrimSpace(get(action.EnvKey)); name != "" {
			names[action.Kind] = name
		}
	}
	if legacy := strings.TrimSpace(get("REVIEW_SKILL")); legacy != "" && names[skill.ActionReviewFull] == "" {
		names[skill.ActionReviewFull] = legacy
	}
	return names
}

func resolvePath(value, base string) string {
	if strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			value = filepath.Join(home, value[2:])
		}
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(base, value))
}

func splitList(value string) []string {
	var out []string
	for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// SplitTools splits "Read Bash(git log *) Grep" keeping parenthesised groups intact.
func SplitTools(value string) []string {
	var out []string
	var current strings.Builder
	depth := 0
	for _, r := range value {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if (r == ' ' || r == '\t' || r == '\n') && depth == 0 {
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}
