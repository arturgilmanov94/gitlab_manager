// Package doctor checks the environment. It never prints secrets.
package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"mr-review/internal/config"
	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/projectroot"
	rn "mr-review/internal/runner"
	"mr-review/internal/skill"
	"mr-review/internal/terminal"
)

// Statuses.
const (
	OK   = "OK"
	WARN = "WARN"
	FAIL = "FAIL"
)

// Check is one diagnostic line.
type Check struct {
	Name   string
	Status string
	Detail string
	Fix    string
}

// Report is the full result.
type Report struct {
	Version string
	Checks  []Check
}

// Ready is true when nothing FAILed.
func (r Report) Ready() bool {
	for _, c := range r.Checks {
		if c.Status == FAIL {
			return false
		}
	}
	return true
}

// Render formats the report for the terminal.
func (r Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mr-review %s — environment check\n\n", r.Version)
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-*s  %-5s %s\n", width, c.Name, c.Status, c.Detail)
		if c.Status != OK && c.Fix != "" {
			for _, line := range strings.Split(c.Fix, "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				fmt.Fprintf(&b, "%-*s        -> %s\n", width, "", line)
			}
		}
	}
	b.WriteString("\n")
	if r.Ready() {
		b.WriteString("Ready to run.\n")
	} else {
		b.WriteString("Not ready: fix the FAIL items above and re-run `./mr-review doctor`.\n")
	}
	return b.String()
}

// Run performs all checks.
func Run(s *config.Settings, version string, gl gitlab.Client, runners []rn.Runner) Report {
	report := Report{Version: version}
	add := func(c Check) { report.Checks = append(report.Checks, c) }

	add(Check{"Binary", OK, fmt.Sprintf("%s %s/%s (%s)", version, runtime.GOOS, runtime.GOARCH, runtime.Version()), ""})

	root := s.ProjectRoot
	if root == "" {
		add(Check{"Project root", FAIL, "not resolved", s.ProjectRootError})
	} else {
		add(Check{"Project root", OK, fmt.Sprintf("%s  (via %s)", root, s.ProjectRootSource), ""})
		if projectroot.IsGitRepository(root) {
			add(Check{"Git repository", OK, "", ""})
		} else {
			add(Check{"Git repository", FAIL, root + " has no .git", "PROJECT_ROOT must point at the main project's git working copy."})
		}
		resolver := skill.NewWithNames(root, s.SkillNames)
		var instructions []string
		for _, f := range resolver.InstructionFiles() {
			if f.Kind == "claude-md" || f.Kind == "agents-md" {
				instructions = append(instructions, f.RelPath)
			}
		}
		if len(instructions) > 0 {
			add(Check{"Project instructions", OK, strings.Join(instructions, ", "), ""})
		} else {
			add(Check{"Project instructions", WARN, "no CLAUDE.md / AGENTS.md found", "Runs will work without project rules. Make sure the checkout contains its Claude instructions."})
		}
		claudeDir := filepath.Join(root, ".claude")
		if info, err := os.Stat(claudeDir); err == nil && info.IsDir() {
			agents, _ := filepath.Glob(filepath.Join(claudeDir, "agents", "*.md"))
			skills, _ := filepath.Glob(filepath.Join(claudeDir, "skills", "*", "SKILL.md"))
			add(Check{"Project Claude config", OK, fmt.Sprintf(".claude/  (agents: %d, skills: %d)", len(agents), len(skills)), ""})
		} else {
			add(Check{"Project Claude config", WARN, ".claude/ not found", "Project-local agents/skills live in .claude/. Sync it to this machine if the project keeps it out of git."})
		}
		for _, res := range resolver.Map() {
			add(SkillCheck(resolver, res))
		}
		if host, path, ok := gitlab.ProjectFromGitRemote(root); ok {
			add(Check{"GitLab project", OK, path + " @ " + host, ""})
		} else {
			add(Check{"GitLab project", WARN, "cannot derive from `git remote get-url origin`", "Set GITLAB_HOST in .env and add MRs/issues by full URL."})
		}
		if err := writable(s.WorktreeDir); err != nil {
			add(Check{"Worktree directory", FAIL, err.Error(), "Fix WORKTREE_DIR in .env or directory permissions."})
		} else {
			add(Check{"Worktree directory", OK, s.WorktreeDir + fmt.Sprintf("  (branches start from origin/%s)", s.BaseBranch), ""})
		}
	}

	switch s.ClaudePermissions {
	case "strict":
		add(Check{"Permissions", OK, "strict: fixed tool lists, anything else is denied without asking", ""})
	case "manual":
		add(Check{"Permissions", OK, fmt.Sprintf("manual: project rules + sandbox, everything else is asked in the dashboard (timeout %ds)", s.ApprovalTimeoutSec), ""})
	default:
		add(Check{"Permissions", OK, fmt.Sprintf("auto: agent classifier + sandbox; what it cannot decide is asked in the dashboard (timeout %ds)", s.ApprovalTimeoutSec), ""})
	}

	found := 0
	for _, r := range runners {
		name := "Agent: " + r.Name()
		if path, version, ok := r.Detect(); ok {
			found++
			add(Check{name, OK, strings.TrimSpace(path + "  " + version), ""})
		} else {
			hint := map[string]string{
				"claude": "Install Claude Code (https://docs.claude.com/en/docs/claude-code) and run `claude` once to log in, or set CLAUDE_BIN in .env.",
				"codex":  "Optional: install the Codex CLI (`npm i -g @openai/codex`) and run `codex login`.",
				"cursor": "Optional: install the Cursor CLI agent (cursor-agent).",
			}[r.Name()]
			status := WARN
			if r.Name() == "claude" && found == 0 {
				status = WARN
			}
			add(Check{name, status, "not found in PATH", hint})
		}
	}
	if found == 0 {
		add(Check{"Agents", FAIL, "no coding agent available", "At least one of claude / codex / cursor-agent must be installed and logged in."})
	}

	if path, version, ok := gl.Detect(); !ok {
		add(Check{"GitLab CLI", FAIL, fmt.Sprintf("`%s` not found in PATH", s.GlabBin), "Install glab (https://gitlab.com/gitlab-org/cli) or set GLAB_BIN in .env."})
	} else {
		add(Check{"GitLab CLI", OK, strings.TrimSpace(path + "  " + version), ""})
		host := s.GitLabHost
		if host == "" && root != "" {
			if h, _, ok := gitlab.ProjectFromGitRemote(root); ok {
				host = h
			}
		}
		ok, summary := gl.AuthStatus(host)
		status := OK
		if !ok {
			status = FAIL
		}
		fix := "Run `glab auth login` once on this machine (credentials are never copied by the dashboard)."
		if host != "" {
			fix = fmt.Sprintf("Run `glab auth login --hostname %s` once on this machine (credentials are never copied by the dashboard).", host)
		}
		add(Check{"GitLab access", status, summary, fix})
	}

	if err := writable(filepath.Dir(s.DatabasePath)); err != nil {
		add(Check{"Database", FAIL, err.Error(), "Fix DATABASE_PATH in .env or directory permissions."})
	} else if database, err := db.Open(s.DatabasePath); err != nil {
		add(Check{"Database", FAIL, err.Error(), ""})
	} else {
		applied := database.SchemaVersion()
		database.Close()
		if len(applied) == 0 {
			add(Check{"Database", WARN, s.DatabasePath + "  (not initialised — created on first start)", "Run `./mr-review init-db` or just `./mr-review start`."})
		} else {
			add(Check{"Database", OK, fmt.Sprintf("%s  (migrations: %d)", s.DatabasePath, len(applied)), ""})
		}
	}
	if err := writable(s.LogDir); err != nil {
		add(Check{"Logs", FAIL, err.Error(), "Fix LOG_DIR in .env."})
	} else {
		add(Check{"Logs", OK, s.LogDir, ""})
	}
	if em, problem := terminal.Detect(s.TerminalCmd); em == nil {
		add(Check{"Terminal", WARN, problem, "Optional: «Открыть в терминале» needs a terminal emulator on this machine; set TERMINAL_CMD in .env (e.g. `gnome-terminal --working-directory={dir} -- bash -lc {cmd}`)."})
	} else if !terminal.GraphicalSession() {
		add(Check{"Terminal", WARN, em.Name + " found, but no DISPLAY / WAYLAND_DISPLAY in this process", "Start the dashboard from the desktop session (double-click or a graphical terminal) to open terminal windows from the UI."})
	} else {
		add(Check{"Terminal", OK, fmt.Sprintf("%s (%s)", em.Name, em.Path), ""})
	}
	add(Check{"Server", OK, s.URL(), ""})
	return report
}

// SkillCheck describes how one dashboard action is backed by the project: its own skill (OK), the fallback
// action's skill or no skill at all (WARN: the run works on CLAUDE.md plus the dashboard prompt), or a skill
// file that Claude Code cannot use (FAIL).
func SkillCheck(resolver *skill.Resolver, res skill.Resolution) Check {
	name := "Skill: " + res.Action.Kind
	howTo := fmt.Sprintf("Create .claude/skills/%s/SKILL.md or .claude/agents/%s.md in the project (or point %s at an existing one). Expected behaviour: %s",
		res.Wanted, res.Wanted, res.Action.EnvKey, res.Action.Contract)
	switch {
	case res.Skill == nil:
		return Check{name, WARN, fmt.Sprintf("%s not found; runs use CLAUDE.md / AGENTS.md + dashboard prompt", res.Wanted), howTo}
	case res.Via != "":
		return Check{name, WARN, fmt.Sprintf("%s not found; using the %s skill %s (%s)", res.Wanted, res.Via, res.Skill.Identifier(), res.Skill.RelPath), howTo}
	}
	v := resolver.Validate(*res.Skill)
	status := OK
	if !v.OK {
		status = FAIL
	}
	return Check{name, status, fmt.Sprintf("%s  (%s; run as `%s`)", res.Skill.Identifier(), res.Skill.RelPath, res.Skill.Invocation()), strings.Join(v.Problems, "; ")}
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%s: %v", dir, err)
	}
	probe := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("%s not writable: %v", dir, err)
	}
	return os.Remove(probe)
}
