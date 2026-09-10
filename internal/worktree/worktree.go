// Package worktree manages per-task git worktrees so the developer's checkout is never touched.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Manager creates worktrees of Root under Dir. Operations that touch the shared .git of the main checkout
// (fetch, worktree add/remove/prune) are serialised: runs on different branches may prepare in parallel.
type Manager struct {
	Root       string // main project checkout (shares .git with the worktrees)
	Dir        string // where worktrees live (inside the dashboard's runtime dir)
	BaseBranch string // e.g. develop

	mu sync.Mutex
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._#/-]+`)

// Slug turns a branch name into a directory name.
func Slug(branch string) string {
	s := unsafe.ReplaceAllString(branch, "-")
	s = strings.ReplaceAll(s, "/", "__")
	s = strings.ReplaceAll(s, "#", "-")
	return strings.Trim(s, "-.")
}

// Path returns the worktree directory for a branch.
func (m *Manager) Path(branch string) string { return filepath.Join(m.Dir, Slug(branch)) }

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// Prepare creates (or reuses) a worktree on `branch`: an existing local branch, else the branch on origin
// (fetched first, so an MR branch never seen by this checkout is still picked up), else a new branch from
// origin/BaseBranch. The main checkout's branch is not changed. .claude/ is linked in when the project keeps it out of git.
func (m *Manager) Prepare(ctx context.Context, branch string, log func(string)) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("branch name is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	path := m.Path(branch)
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		log("reusing existing worktree " + path)
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	log("git fetch origin " + m.BaseBranch)
	if _, err := m.git(ctx, m.Root, "fetch", "--quiet", "origin", m.BaseBranch); err != nil {
		return "", err
	}
	// Remove stale registrations (directory deleted by hand) before adding.
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	if _, err := m.git(ctx, m.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		log("branch exists locally; checking it out into " + path)
		if _, err := m.git(ctx, m.Root, "worktree", "add", path, branch); err != nil {
			return "", err
		}
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	// The branch may exist on origin without ever having been fetched here (an MR branch of a colleague).
	if _, err := m.git(ctx, m.Root, "fetch", "--quiet", "origin", branch); err != nil {
		log("git fetch origin " + branch + ": " + err.Error() + " (assuming the branch does not exist on origin)")
	}
	if _, err := m.git(ctx, m.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		log("branch exists on origin; tracking it in " + path)
		if _, err := m.git(ctx, m.Root, "worktree", "add", "--track", "-b", branch, path, "origin/"+branch); err != nil {
			return "", err
		}
	} else {
		log(fmt.Sprintf("creating branch %s from origin/%s in %s", branch, m.BaseBranch, path))
		if _, err := m.git(ctx, m.Root, "worktree", "add", "-b", branch, path, "origin/"+m.BaseBranch); err != nil {
			return "", err
		}
	}
	m.linkClaudeConfig(path, log)
	return path, nil
}

// linkClaudeConfig symlinks untracked Claude instruction files from the main checkout and excludes the links
// from git in this worktree (info/exclude), so `git status` stays clean and Commit never adds a local symlink.
func (m *Manager) linkClaudeConfig(path string, log func(string)) {
	var linked []string
	for _, name := range []string{".claude", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"} {
		src := filepath.Join(m.Root, name)
		dst := filepath.Join(path, name)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if info, err := os.Lstat(dst); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				linked = append(linked, name)
			}
			continue // tracked in git or already linked
		}
		if err := os.Symlink(src, dst); err == nil {
			log("linked " + name + " from the main checkout")
			linked = append(linked, name)
		}
	}
	if len(linked) > 0 {
		m.exclude(path, linked)
	}
}

// exclude appends names to the worktree's private ignore list (.git/info/exclude of this worktree).
func (m *Manager) exclude(path string, names []string) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	out, err := m.git(ctx, path, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return
	}
	file := strings.TrimSpace(out)
	if !filepath.IsAbs(file) {
		file = filepath.Join(path, file)
	}
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	existing, _ := os.ReadFile(file)
	var add []string
	for _, name := range names {
		if !strings.Contains(string(existing), "\n/"+name+"\n") && !strings.HasPrefix(string(existing), "/"+name+"\n") {
			add = append(add, "/"+name)
		}
	}
	if len(add) == 0 {
		return
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("# mr-review: instruction files linked from the main checkout\n" + strings.Join(add, "\n") + "\n")
}

// Status returns `git status --short` of the worktree.
func (m *Manager) Status(ctx context.Context, path string) (string, error) {
	return m.git(ctx, path, "status", "--short")
}

// Diff returns the working tree diff (staged + unstaged + untracked as intent-to-add).
func (m *Manager) Diff(ctx context.Context, path string) (string, error) {
	_, _ = m.git(ctx, path, "add", "--intent-to-add", "--all")
	out, err := m.git(ctx, path, "diff", "--no-color")
	if err != nil {
		return "", err
	}
	return out, nil
}

// Log returns commits on the branch that are not on origin/BaseBranch.
func (m *Manager) Log(ctx context.Context, path string) (string, error) {
	out, err := m.git(ctx, path, "log", "--oneline", "--no-color", "origin/"+m.BaseBranch+"..HEAD")
	if err != nil {
		return "", nil
	}
	return out, nil
}

// Commit stages everything and commits with message.
func (m *Manager) Commit(ctx context.Context, path, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", errors.New("commit message is empty")
	}
	if _, err := m.git(ctx, path, "add", "--all"); err != nil {
		return "", err
	}
	return m.git(ctx, path, "commit", "--quiet", "-m", message)
}

// Push pushes the branch to origin with upstream tracking.
func (m *Manager) Push(ctx context.Context, path, branch string) (string, error) {
	return m.git(ctx, path, "push", "--set-upstream", "origin", branch)
}

// Remove deletes the worktree directory (keeps the branch).
func (m *Manager) Remove(ctx context.Context, path string) error {
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(m.Dir)) {
		return errors.New("refusing to remove a directory outside the worktree dir")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.git(ctx, m.Root, "worktree", "remove", "--force", path)
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	return err
}

// Entry is one worktree registered in the main checkout under Dir.
type Entry struct {
	Path   string
	Branch string
	Head   string
}

// List returns the worktrees the dashboard created (those under Dir), from `git worktree list --porcelain`.
func (m *Manager) List(ctx context.Context) ([]Entry, error) {
	out, err := m.git(ctx, m.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []Entry
	var current *Entry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, Entry{Path: strings.TrimPrefix(line, "worktree ")})
			current = &entries[len(entries)-1]
		case current != nil && strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case current != nil && strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	dir := filepath.Clean(m.Dir) + string(filepath.Separator)
	var mine []Entry
	for _, e := range entries {
		if strings.HasPrefix(filepath.Clean(e.Path)+string(filepath.Separator), dir) {
			mine = append(mine, e)
		}
	}
	return mine, nil
}

// Unpushed reports how many commits of the worktree branch are not on origin/<branch>; hasUpstream is false when
// the branch itself was never pushed (a fresh branch tracks origin/<base>, which does not count).
func (m *Manager) Unpushed(ctx context.Context, path string) (count int, hasUpstream bool) {
	branch, err := m.git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return 0, false
	}
	remote := "refs/remotes/origin/" + strings.TrimSpace(branch)
	if _, err := m.git(ctx, path, "rev-parse", "--verify", "--quiet", remote); err != nil {
		return 0, false
	}
	out, err := m.git(ctx, path, "rev-list", "--count", remote+"..HEAD")
	if err != nil {
		return 0, true
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n, true
}

// Exists reports whether the worktree directory is present.
func (m *Manager) Exists(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// Timeout used for git operations.
const Timeout = 5 * time.Minute
