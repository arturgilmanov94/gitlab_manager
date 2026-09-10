package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mr-review/internal/testutil"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tmp := t.TempDir()
	root := testutil.GitRepo(t, filepath.Join(tmp, "project"))
	return &Manager{Root: root, Dir: filepath.Join(tmp, "worktrees"), BaseBranch: "develop"}, root
}

// An MR branch that exists on origin but was never fetched into this checkout must be picked up from
// origin, not silently recreated from develop.
func TestPrepareFetchesUnknownOriginBranch(t *testing.T) {
	m, root := newManager(t)
	// Publish a branch with an extra commit straight into the bare origin through a throwaway clone.
	clone := filepath.Join(t.TempDir(), "clone")
	git(t, filepath.Dir(clone), "clone", "-q", root+".git-remote", clone)
	git(t, clone, "checkout", "-q", "-b", "mr/feature-x")
	_ = os.WriteFile(filepath.Join(clone, "X.txt"), []byte("x\n"), 0o644)
	git(t, clone, "add", "X.txt")
	git(t, clone, "commit", "-q", "-m", "feature x")
	want := git(t, clone, "rev-parse", "HEAD")
	git(t, clone, "push", "-q", "origin", "mr/feature-x")

	if out, _ := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/mr/feature-x").Output(); len(out) > 0 {
		t.Fatal("precondition: the main checkout must not know the branch yet")
	}
	path, err := m.Prepare(context.Background(), "mr/feature-x", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, path, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree must start at the origin head %s, got %s", want, got)
	}
	if _, err := os.Stat(filepath.Join(path, "X.txt")); err != nil {
		t.Fatal("the MR commit is missing from the worktree")
	}
	if upstream := git(t, path, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); upstream != "origin/mr/feature-x" {
		t.Fatalf("upstream %s", upstream)
	}
}

func TestPrepareUnknownBranchStartsFromBase(t *testing.T) {
	m, _ := newManager(t)
	path, err := m.Prepare(context.Background(), "group/project#7", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if git(t, path, "rev-parse", "HEAD") != git(t, path, "rev-parse", "origin/develop") {
		t.Fatal("a new task branch must start at origin/develop")
	}
	if _, err := os.Lstat(filepath.Join(path, ".claude")); err != nil {
		t.Fatal(".claude must be linked from the main checkout")
	}
}

// Several runs prepare different worktrees at once; the shared .git is touched under a lock, so all succeed.
func TestPrepareConcurrentBranches(t *testing.T) {
	m, _ := newManager(t)
	branches := []string{"task/a", "task/b", "task/c", "task/d"}
	var wg sync.WaitGroup
	errs := make(chan error, len(branches))
	for _, b := range branches {
		wg.Add(1)
		go func(branch string) {
			defer wg.Done()
			if _, err := m.Prepare(context.Background(), branch, func(string) {}); err != nil {
				errs <- err
			}
		}(b)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, b := range branches {
		if !m.Exists(m.Path(b)) {
			t.Fatalf("worktree for %s missing", b)
		}
	}
}

// Instruction files linked from the main checkout never show up as changes or get committed; a fresh branch does
// not count as pushed just because it tracks origin/<base>.
func TestLinkedConfigExcludedAndUnpushed(t *testing.T) {
	m, root := newManager(t)
	_ = os.MkdirAll(filepath.Join(root, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".claude", "note.md"), []byte("x"), 0o644)
	ctx := context.Background()
	path, err := m.Prepare(ctx, "feature/x", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(path, ".claude")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a linked .claude in the worktree: %v", err)
	}
	if status, _ := m.Status(ctx, path); strings.TrimSpace(status) != "" {
		t.Fatalf("linked files must be excluded from git status: %q", status)
	}
	if n, has := m.Unpushed(ctx, path); has || n != 0 {
		t.Fatalf("fresh branch is not on origin: %d %v", n, has)
	}
	_ = os.WriteFile(filepath.Join(path, "a.txt"), []byte("a"), 0o644)
	if _, err := m.Commit(ctx, path, "add a"); err != nil {
		t.Fatal(err)
	}
	if tree := git(t, path, "ls-tree", "--name-only", "HEAD"); strings.Contains(tree, ".claude") || !strings.Contains(tree, "a.txt") {
		t.Fatalf("commit must contain a.txt and not the symlink: %s", tree)
	}
	if _, err := m.Push(ctx, path, "feature/x"); err != nil {
		t.Fatal(err)
	}
	if n, has := m.Unpushed(ctx, path); !has || n != 0 {
		t.Fatalf("after push: %d %v", n, has)
	}
	_ = os.WriteFile(filepath.Join(path, "b.txt"), []byte("b"), 0o644)
	_, _ = m.Commit(ctx, path, "add b")
	if n, has := m.Unpushed(ctx, path); !has || n != 1 {
		t.Fatalf("one commit ahead: %d %v", n, has)
	}
}
