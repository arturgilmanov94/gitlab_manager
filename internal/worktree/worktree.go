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

// ReviewPath returns the read-only review worktree directory for an object name (e.g. "project!123").
func (m *Manager) ReviewPath(name string) string { return filepath.Join(m.Dir, "review", Slug(name)) }

// PrepareDetached creates (or moves) a detached worktree at exactly `sha` for read-only work on an MR head.
// branch is fetched first so a commit never seen by this checkout is available; the main checkout is untouched.
func (m *Manager) PrepareDetached(ctx context.Context, name, branch, sha string, log func(string)) (string, error) {
	if strings.TrimSpace(sha) == "" {
		return "", errors.New("head SHA is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	path := m.ReviewPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		if head, err := m.git(ctx, path, "rev-parse", "HEAD"); err == nil && strings.TrimSpace(head) == sha {
			log("reusing review worktree " + path + " at " + sha[:min(10, len(sha))])
			m.linkClaudeConfig(path, log)
			return path, nil
		}
		m.fetchHead(ctx, branch, sha, log)
		if _, err := m.git(ctx, path, "checkout", "--quiet", "--detach", sha); err != nil {
			return "", err
		}
		log("review worktree " + path + " moved to " + sha[:min(10, len(sha))])
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	m.fetchHead(ctx, branch, sha, log)
	if _, err := m.git(ctx, m.Root, "worktree", "add", "--quiet", "--detach", path, sha); err != nil {
		return "", err
	}
	log("review worktree " + path + " at " + sha[:min(10, len(sha))])
	m.linkClaudeConfig(path, log)
	return path, nil
}

// fetchHead makes sha available locally: fetch the branch, then the commit itself (servers may refuse the latter).
func (m *Manager) fetchHead(ctx context.Context, branch, sha string, log func(string)) {
	if _, err := m.git(ctx, m.Root, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return
	}
	if branch != "" {
		if _, err := m.git(ctx, m.Root, "fetch", "--quiet", "origin", branch); err != nil {
			log("git fetch origin " + branch + ": " + err.Error())
		}
	}
	if _, err := m.git(ctx, m.Root, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := m.git(ctx, m.Root, "fetch", "--quiet", "origin", sha); err != nil {
			log("git fetch origin " + sha + ": " + err.Error())
		}
	}
}

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

// linkedNames are the instruction files that may be linked into a worktree from the main checkout.
var linkedNames = []string{".claude", "CLAUDE.md", "CLAUDE.local.md", "AGENTS.md"}

// linkClaudeConfig symlinks untracked Claude instruction files from the main checkout. The links are left out
// of status/diff/commit through pathspecs (see linkedPathspec): nothing is written into the repository's config.
func (m *Manager) linkClaudeConfig(path string, log func(string)) {
	for _, name := range linkedNames {
		src := filepath.Join(m.Root, name)
		dst := filepath.Join(path, name)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue // tracked in git or already linked
		}
		if err := os.Symlink(src, dst); err == nil {
			log("linked " + name + " from the main checkout")
		}
	}
}

// linkedPathspec is the pathspec that covers the worktree except the symlinked instruction files.
func (m *Manager) linkedPathspec(path string) []string {
	spec := []string{"--", "."}
	for _, name := range linkedNames {
		if info, err := os.Lstat(filepath.Join(path, name)); err == nil && info.Mode()&os.ModeSymlink != 0 {
			spec = append(spec, ":(exclude)"+name)
		}
	}
	return spec
}

// Status returns `git status --short` of the worktree.
func (m *Manager) Status(ctx context.Context, path string) (string, error) {
	return m.git(ctx, path, append([]string{"status", "--short"}, m.linkedPathspec(path)...)...)
}

// Diff returns the working tree diff (staged + unstaged + untracked as intent-to-add).
func (m *Manager) Diff(ctx context.Context, path string) (string, error) {
	_, _ = m.git(ctx, path, append([]string{"add", "--intent-to-add", "--all"}, m.linkedPathspec(path)...)...)
	out, err := m.git(ctx, path, append([]string{"diff", "--no-color"}, m.linkedPathspec(path)...)...)
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
	if _, err := m.git(ctx, path, append([]string{"add", "--all"}, m.linkedPathspec(path)...)...); err != nil {
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
