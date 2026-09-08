// Package projectroot finds the main project the dashboard operates on.
//
// Order: explicit PROJECT_ROOT; git toplevel of the dashboard dir; git toplevel of cwd;
// parent directories containing .git; a single sibling git repo with Claude instructions.
package projectroot

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// HowToFix is shown when nothing could be resolved.
const HowToFix = `Could not determine the main project root.
Either place the dashboard inside the project (e.g. <project>/tools/mr-review),
next to it (a single sibling git repository with CLAUDE.md or .claude/ is picked up automatically),
or pass the path explicitly:

    PROJECT_ROOT=/path/to/project ./mr-review doctor

or set PROJECT_ROOT in the dashboard's .env file (relative paths are allowed, e.g. ../tradernet).`

// Resolved is the detected root and how it was found.
type Resolved struct {
	Path   string
	Source string // env | git:dashboard | git:cwd | parents | sibling
}

// IsGitRepository reports whether path contains a .git entry.
func IsGitRepository(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// HasClaudeInstructions reports whether path carries project instructions for coding agents:
// CLAUDE.md, AGENTS.md, .claude/CLAUDE.md or .claude/{agents,commands,skills,rules}. A bare .claude/
// holding only settings does not count (every repo touched by Claude Code gets one).
func HasClaudeInstructions(path string) bool {
	for _, file := range []string{"CLAUDE.md", "AGENTS.md", filepath.Join(".claude", "CLAUDE.md")} {
		if info, err := os.Stat(filepath.Join(path, file)); err == nil && !info.IsDir() {
			return true
		}
	}
	for _, dir := range []string{"agents", "commands", "skills", "rules"} {
		if info, err := os.Stat(filepath.Join(path, ".claude", dir)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// GitToplevel runs `git rev-parse --show-toplevel` in dir.
func GitToplevel(dir string) string {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		return resolved
	}
	return top
}

// SiblingCandidates lists sibling git repos with Claude instructions.
func SiblingCandidates(dashboardDir string) []string {
	parent := filepath.Dir(dashboardDir)
	if parent == dashboardDir {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		if samePath(path, dashboardDir) {
			continue
		}
		if IsGitRepository(path) && HasClaudeInstructions(path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Resolve applies the resolution order. explicit is the PROJECT_ROOT value ("" = unset).
func Resolve(dashboardDir, explicit, cwd string) (Resolved, error) {
	dashboardDir = canonical(dashboardDir)
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		if strings.HasPrefix(explicit, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				explicit = filepath.Join(home, explicit[2:])
			}
		}
		if !filepath.IsAbs(explicit) {
			explicit = filepath.Join(dashboardDir, explicit)
		}
		explicit = canonical(explicit)
		if info, err := os.Stat(explicit); err != nil || !info.IsDir() {
			return Resolved{}, fmt.Errorf("PROJECT_ROOT points to a missing directory: %s\n\n%s", explicit, HowToFix)
		}
		return Resolved{explicit, "env"}, nil
	}

	if top := GitToplevel(dashboardDir); top != "" && !samePath(top, dashboardDir) {
		return Resolved{top, "git:dashboard"}, nil
	}
	searchFrom := dashboardDir
	if top := GitToplevel(dashboardDir); samePath(top, dashboardDir) {
		searchFrom = filepath.Dir(dashboardDir) // the dashboard is its own repo; look around it
	}
	if top := GitToplevel(searchFrom); top != "" && !samePath(top, dashboardDir) {
		return Resolved{top, "git:dashboard"}, nil
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if top := GitToplevel(cwd); top != "" && !samePath(top, dashboardDir) {
		return Resolved{top, "git:cwd"}, nil
	}
	for dir := searchFrom; ; dir = filepath.Dir(dir) {
		if IsGitRepository(dir) && !samePath(dir, dashboardDir) {
			return Resolved{canonical(dir), "parents"}, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	siblings := SiblingCandidates(dashboardDir)
	switch len(siblings) {
	case 1:
		return Resolved{canonical(siblings[0]), "sibling"}, nil
	case 0:
		return Resolved{}, errors.New(HowToFix)
	default:
		var b strings.Builder
		b.WriteString("Several sibling git repositories with Claude instructions were found; pick one explicitly:\n\n")
		for _, s := range siblings {
			fmt.Fprintf(&b, "    PROJECT_ROOT=%s ./mr-review doctor\n", s)
		}
		b.WriteString("\nor set PROJECT_ROOT in the dashboard's .env file.")
		return Resolved{}, errors.New(b.String())
	}
}

func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return canonical(a) == canonical(b)
}
