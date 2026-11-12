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
	"syscall"
	"time"
)

// Manager creates worktrees of Root under Dir. Only the operations that change the shared worktree registry of
// the main checkout (worktree add/remove/prune) take the global lock, and only for their own duration; network
// fetches run outside it, so a slow or unreachable remote never blocks the preparation of other runs. Runs on the
// same worktree path are serialised by a per-path lock.
type Manager struct {
	Root       string // main project checkout (shares .git with the worktrees)
	Dir        string // where worktrees live (inside the dashboard's runtime dir)
	BaseBranch string // e.g. develop

	mu     sync.Mutex             // worktree registry of the main checkout
	pathMu sync.Mutex             // guards paths
	paths  map[string]*sync.Mutex // per worktree path
}

// FetchTimeout bounds one `git fetch`: an unreachable GitLab (VPN off) must not hang a run for minutes.
var FetchTimeout = 60 * time.Second

// WarnPrefix marks log lines the UI should surface as warnings (the run continues).
const WarnPrefix = "warning: "

// ErrUnreachable means the remote could not be fetched and the code the run needs is not available locally.
var ErrUnreachable = errors.New("GitLab недоступен — включён ли VPN?")

// sshCommand fails fast instead of waiting for a dead host or prompting for a password nobody will type.
const sshCommand = "ssh -o ConnectTimeout=10 -o BatchMode=yes"

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

// IsReview reports whether path is a review worktree (under Dir/review): disposable, recreated on demand.
func (m *Manager) IsReview(path string) bool {
	prefix := filepath.Join(filepath.Clean(m.Dir), "review") + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), prefix) && filepath.Clean(path) != filepath.Clean(prefix)
}

// ListReview returns the review worktrees only.
func (m *Manager) ListReview(ctx context.Context) ([]Entry, error) {
	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if m.IsReview(e.Path) {
			out = append(out, e)
		}
	}
	return out, nil
}

// lockPath serialises the work on one worktree path; it returns the unlock function.
func (m *Manager) lockPath(path string) func() {
	m.pathMu.Lock()
	if m.paths == nil {
		m.paths = map[string]*sync.Mutex{}
	}
	key := filepath.Clean(path)
	l := m.paths[key]
	if l == nil {
		l = &sync.Mutex{}
		m.paths[key] = l
	}
	m.pathMu.Unlock()
	l.Lock()
	return l.Unlock
}

// RemoveIf removes the worktree when keep() returns false, both under the path lock, so a run that starts
// preparing the same worktree meanwhile is not raced: it either finds the worktree gone (and recreates it) or keeps it.
func (m *Manager) RemoveIf(ctx context.Context, path string, keep func() bool) (bool, error) {
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(m.Dir)) {
		return false, errors.New("refusing to remove a directory outside the worktree dir")
	}
	defer m.lockPath(path)()
	if keep != nil && keep() {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.git(ctx, m.Root, "worktree", "remove", "--force", path)
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	if err != nil && !m.Exists(path) {
		err = nil // already gone (deleted by hand): prune was all that was needed
	}
	return err == nil, err
}

// PrepareDetached creates (or moves) a detached worktree at exactly `sha` for read-only work on an MR head.
// branch is fetched first when the commit is not known locally; the main checkout is untouched. When the commit
// cannot be obtained the error wraps ErrUnreachable (fetch failed) — the caller must not run elsewhere silently.
func (m *Manager) PrepareDetached(ctx context.Context, name, branch, sha string, log func(string)) (string, error) {
	if strings.TrimSpace(sha) == "" {
		return "", errors.New("head SHA is empty")
	}
	path := m.ReviewPath(name)
	defer m.lockPath(path)()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	short := sha[:min(10, len(sha))]
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		if head, err := m.git(ctx, path, "rev-parse", "HEAD"); err == nil && strings.TrimSpace(head) == sha {
			log("reusing review worktree " + path + " at " + short)
			m.linkClaudeConfig(path, log)
			return path, nil
		}
		if err := m.fetchHead(ctx, branch, sha, log); err != nil {
			return "", err
		}
		if _, err := m.git(ctx, path, "checkout", "--quiet", "--detach", sha); err != nil {
			return "", err
		}
		log("review worktree " + path + " moved to " + short)
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	if err := m.fetchHead(ctx, branch, sha, log); err != nil {
		return "", err
	}
	m.mu.Lock()
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	_, err := m.git(ctx, m.Root, "worktree", "add", "--quiet", "--detach", path, sha)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	log("review worktree " + path + " at " + short)
	m.linkClaudeConfig(path, log)
	return path, nil
}

// hasCommit reports whether the commit is in the local object store.
func (m *Manager) hasCommit(ctx context.Context, sha string) bool {
	_, err := m.git(ctx, m.Root, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// fetchHead makes sha available locally: fetch the branch, then the commit itself (servers may refuse the latter).
// An error means the commit is still missing; it wraps ErrUnreachable when the remote could not be reached.
func (m *Manager) fetchHead(ctx context.Context, branch, sha string, log func(string)) error {
	if m.hasCommit(ctx, sha) {
		return nil
	}
	var fetchErr error
	if branch != "" {
		if fetchErr = m.fetch(ctx, branch); fetchErr != nil {
			log("git fetch origin " + branch + ": " + fetchErr.Error())
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.hasCommit(ctx, sha) {
		return nil
	}
	// The remote answered (or no branch is known): ask for the commit itself — the branch may have moved on after a
	// force push. Servers often refuse fetching by SHA; that refusal is not a connectivity problem.
	if fetchErr == nil {
		reached := branch != ""
		if err := m.fetch(ctx, sha); err != nil {
			log("git fetch origin " + sha + ": " + err.Error())
			if !reached {
				fetchErr = err
			}
		} else if m.hasCommit(ctx, sha) {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	short := sha[:min(10, len(sha))]
	if fetchErr != nil {
		return fmt.Errorf("коммита %s нет в локальном репозитории, а git fetch не удался (%v): %w", short, fetchErr, ErrUnreachable)
	}
	return fmt.Errorf("коммит %s не найден ни локально, ни в origin (ветку перезаписали?)", short)
}

// fetch runs `git fetch origin <refs>` in the main checkout, bounded by FetchTimeout.
func (m *Manager) fetch(ctx context.Context, refs ...string) error {
	fctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	_, err := m.git(fctx, m.Root, append([]string{"fetch", "--quiet", "origin"}, refs...)...)
	if err != nil && ctx.Err() == nil && errors.Is(fctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("git fetch origin %s: нет ответа за %s", strings.Join(refs, " "), FetchTimeout)
	}
	return err
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+sshCommand)
	}
	// On cancel/timeout kill git's whole process group: ssh (a child of git) would otherwise keep running and hold
	// the output pipes. WaitDelay is the backstop for anything that escaped the group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
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

// hasRef reports whether the full ref exists in the main checkout.
func (m *Manager) hasRef(ctx context.Context, ref string) bool {
	_, err := m.git(ctx, m.Root, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// Prepare creates (or reuses) a worktree on `branch`: an existing local branch, else the branch on origin
// (fetched first, so an MR branch never seen by this checkout is still picked up), else a new branch from
// origin/BaseBranch. The main checkout's branch is not changed. .claude/ is linked in when the project keeps it out of git.
// When origin cannot be fetched the local origin/BaseBranch is used with a warning; without it the error wraps
// ErrUnreachable.
func (m *Manager) Prepare(ctx context.Context, branch string, log func(string)) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("branch name is empty")
	}
	path := m.Path(branch)
	defer m.lockPath(path)()
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		log("reusing existing worktree " + path)
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	log("git fetch origin " + m.BaseBranch)
	offline := false
	if err := m.fetch(ctx, m.BaseBranch); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !m.hasRef(ctx, "refs/remotes/origin/"+m.BaseBranch) {
			return "", fmt.Errorf("%v: %w", err, ErrUnreachable)
		}
		offline = true
		log(WarnPrefix + fmt.Sprintf("не удалось обновить origin/%s (%v) — работаю от локальной копии origin/%s, она может отставать от GitLab",
			m.BaseBranch, err, m.BaseBranch))
	}
	if m.hasRef(ctx, "refs/heads/"+branch) {
		log("branch exists locally; checking it out into " + path)
		if err := m.addWorktree(ctx, path, branch); err != nil {
			return "", err
		}
		m.linkClaudeConfig(path, log)
		return path, nil
	}
	// The branch may exist on origin without ever having been fetched here (an MR branch of a colleague).
	// Offline it is not retried: the base fetch has just failed, another attempt would only wait again.
	if !offline {
		if err := m.fetch(ctx, branch); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			log("git fetch origin " + branch + ": " + err.Error() + " (assuming the branch does not exist on origin)")
		}
	}
	if m.hasRef(ctx, "refs/remotes/origin/"+branch) {
		log("branch exists on origin; tracking it in " + path)
		if err := m.addWorktree(ctx, path, "--track", "-b", branch, path, "origin/"+branch); err != nil {
			return "", err
		}
	} else {
		log(fmt.Sprintf("creating branch %s from origin/%s in %s", branch, m.BaseBranch, path))
		if err := m.addWorktree(ctx, path, "-b", branch, path, "origin/"+m.BaseBranch); err != nil {
			return "", err
		}
	}
	m.linkClaudeConfig(path, log)
	return path, nil
}

// addWorktree prunes stale registrations (a directory deleted by hand) and runs `git worktree add`. With a single
// argument it checks out that existing branch at path; otherwise args are passed through after "add".
func (m *Manager) addWorktree(ctx context.Context, path string, args ...string) error {
	if len(args) == 1 {
		args = []string{path, args[0]}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.git(ctx, m.Root, "worktree", "prune")
	_, err := m.git(ctx, m.Root, append([]string{"worktree", "add"}, args...)...)
	return err
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

// linkedPathspec is the pathspec that covers the worktree except the symlinked instruction files. Links the
// project already ignores through .gitignore are left out of the exclude list: git never picks them up anyway,
// and naming an ignored path in a pathspec (even an exclude one) makes `git add` complain and exit non-zero.
func (m *Manager) linkedPathspec(path string) []string {
	spec := []string{"--", "."}
	var linked []string
	for _, name := range linkedNames {
		if info, err := os.Lstat(filepath.Join(path, name)); err == nil && info.Mode()&os.ModeSymlink != 0 {
			linked = append(linked, name)
		}
	}
	if len(linked) == 0 {
		return spec
	}
	ignored := m.ignored(path, linked)
	for _, name := range linked {
		if !ignored[name] {
			spec = append(spec, ":(exclude)"+name)
		}
	}
	return spec
}

// ignored reports which of names the repository's ignore rules (.gitignore, info/exclude, core.excludesFile) cover.
func (m *Manager) ignored(path string, names []string) map[string]bool {
	out := map[string]bool{}
	// Exit status 1 only means "none of them is ignored"; stdout lists the ignored ones either way.
	stdout, _ := m.git(context.Background(), path, append([]string{"check-ignore", "--"}, names...)...)
	for _, line := range strings.Split(stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out
}

// HasBranch reports whether the local branch exists in the main checkout (worktrees share its refs).
func (m *Manager) HasBranch(ctx context.Context, branch string) bool {
	_, err := m.git(ctx, m.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// FileHunks returns the hunk headers of one file between two commits (`git diff -U0`), the map from line
// numbers of `from` to line numbers of `to`. Runs in the main checkout: worktrees share its object database,
// so both commits are there as long as the run that fetched them did so into this repository.
func (m *Manager) FileHunks(ctx context.Context, from, to, file string) (string, error) {
	if from == "" || to == "" || file == "" {
		return "", errors.New("from, to and file are required")
	}
	return m.git(ctx, m.Root, "diff", "--unified=0", "--no-color", "--no-ext-diff", from, to, "--", file)
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

// CommitPaths commits the given paths (relative to the worktree) with message; paths without changes are skipped.
// It returns false when nothing of them had changed. Never touches files outside the list.
func (m *Manager) CommitPaths(ctx context.Context, path, message string, paths []string) (bool, error) {
	if strings.TrimSpace(message) == "" {
		return false, errors.New("commit message is empty")
	}
	var clean []string
	for _, p := range paths {
		p = filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
		if p == "" || p == "." || strings.HasPrefix(p, "../") || filepath.IsAbs(p) {
			continue
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 {
		return false, nil
	}
	if _, err := m.git(ctx, path, append([]string{"add", "--all", "--"}, clean...)...); err != nil {
		return false, err
	}
	if _, err := m.git(ctx, path, "diff", "--cached", "--quiet"); err == nil {
		return false, nil // nothing staged
	}
	if _, err := m.git(ctx, path, "commit", "--quiet", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// CommitLog returns the commits of the branch that origin/BaseBranch does not have, oldest first, with full messages.
func (m *Manager) CommitLog(ctx context.Context, path string) []Commit {
	out, err := m.git(ctx, path, "log", "--reverse", "--no-color", "--format=%H%x1f%s%x1f%b%x1e", "origin/"+m.BaseBranch+"..HEAD")
	if err != nil {
		return nil
	}
	var list []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		parts := strings.SplitN(strings.TrimSpace(rec), "\x1f", 3)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		c := Commit{SHA: parts[0], Subject: parts[1]}
		if len(parts) == 3 {
			c.Body = strings.TrimSpace(parts[2])
		}
		list = append(list, c)
	}
	return list
}

// Commit is one commit of a branch.
type Commit struct{ SHA, Subject, Body string }

// Push pushes the branch to origin with upstream tracking.
func (m *Manager) Push(ctx context.Context, path, branch string) (string, error) {
	return m.git(ctx, path, "push", "--set-upstream", "origin", branch)
}

// Remove deletes the worktree directory (keeps the branch).
func (m *Manager) Remove(ctx context.Context, path string) error {
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(m.Dir)) {
		return errors.New("refusing to remove a directory outside the worktree dir")
	}
	defer m.lockPath(path)()
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

// CheckedOutAt returns the directory where branch is checked out right now ("" = nowhere): git lets a branch be
// checked out in one worktree at a time, the main checkout included.
func (m *Manager) CheckedOutAt(ctx context.Context, branch string) string {
	out, err := m.git(ctx, m.Root, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	path := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return path
		}
	}
	return ""
}

// BranchState is where a worktree's branch stands against origin/BaseBranch (as last fetched) and its own files.
type BranchState struct {
	Ahead  int // commits of the branch that origin/BaseBranch does not have
	Behind int // commits of origin/BaseBranch the branch does not have
	Dirty  int // files with uncommitted changes
}

// State reports the branch state of the worktree at path. No network: origin/BaseBranch is what the last fetch saw.
func (m *Manager) State(ctx context.Context, path string) BranchState {
	var st BranchState
	if out, err := m.git(ctx, path, "rev-list", "--left-right", "--count", "origin/"+m.BaseBranch+"...HEAD"); err == nil {
		fmt.Sscanf(strings.TrimSpace(out), "%d %d", &st.Behind, &st.Ahead)
	}
	if out, err := m.Status(ctx, path); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) != "" {
				st.Dirty++
			}
		}
	}
	return st
}

// Head returns the commit the worktree is on ("" when unknown).
func (m *Manager) Head(ctx context.Context, path string) string {
	out, err := m.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ErrConflict marks a rebase that stopped on conflicts and was rolled back.
var ErrConflict = errors.New("rebase conflict")

// Rebase brings the worktree's branch on top of a freshly fetched origin/BaseBranch. Uncommitted changes are
// refused (nothing is stashed behind the developer's back); on a conflict the rebase is aborted — the branch stays
// exactly as it was — and the error (wrapping ErrConflict) names the conflicting files.
func (m *Manager) Rebase(ctx context.Context, path string, log func(string)) error {
	defer m.lockPath(path)()
	if st := m.State(ctx, path); st.Dirty > 0 {
		return fmt.Errorf("в workspace есть незакоммиченные изменения (%d файлов): сначала закоммитьте их", st.Dirty)
	}
	log("git fetch origin " + m.BaseBranch)
	if err := m.fetch(ctx, m.BaseBranch); err != nil {
		return fmt.Errorf("%v: %w", err, ErrUnreachable)
	}
	log("git rebase origin/" + m.BaseBranch)
	if _, err := m.git(ctx, path, "rebase", "origin/"+m.BaseBranch); err != nil {
		files, _ := m.git(ctx, path, "diff", "--name-only", "--diff-filter=U")
		_, _ = m.git(ctx, path, "rebase", "--abort")
		files = strings.Join(strings.Fields(files), ", ")
		if files == "" {
			return fmt.Errorf("rebase не прошёл и отменён, ветка не изменена: %v", err)
		}
		return fmt.Errorf("конфликты в %s — rebase отменён, ветка не изменена: %w", files, ErrConflict)
	}
	return nil
}

// Exists reports whether the worktree directory is present.
func (m *Manager) Exists(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// Timeout used for git operations.
const Timeout = 5 * time.Minute
