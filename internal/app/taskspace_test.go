package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/runner"
	"mr-review/internal/testutil"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Every stage of a task works in the task's own worktree (G1): the investigation is read-only there, the
// implementation reuses the same worktree, and the prompt tells the real state of the branch each time.
func TestTaskWorktreeForEveryStage(t *testing.T) {
	svc, _, fr := newService(t)
	issue, err := svc.AddIssue("https://gitlab.example.com/group/sub/project/-/issues/7")
	if err != nil {
		t.Fatal(err)
	}
	branch := issue.Ref()
	path := svc.Worktrees.Path(branch)

	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	planID, err := svc.StartPlan(issue.ID, "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, planID) == db.StatusDone })
	req := fr.Requests[0]
	if req.Dir != path || req.Mode != runner.ModeReadOnly || len(req.ProtectDirs) != 2 || req.ProtectDirs[1] != path {
		t.Fatalf("plan must read the task worktree, read-only: %+v", req)
	}
	if !strings.Contains(req.Prompt, "created just now from the current `origin/develop`") || !strings.Contains(req.Prompt, "READ-ONLY") {
		t.Fatalf("%s", req.Prompt)
	}
	if plan, _ := svc.DB.GetRun(planID); plan.Branch != branch || plan.WorkDir != path {
		t.Fatalf("%+v", plan)
	}
	if gitT(t, svc.Settings.ProjectRoot, "rev-parse", "--abbrev-ref", "HEAD") != "develop" {
		t.Fatal("the developer's checkout must stay on its branch")
	}

	// The implementation finds the same worktree; work left uncommitted there is named in the prompt.
	_ = os.WriteFile(filepath.Join(path, "draft.txt"), []byte("wip\n"), 0o644)
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	implID, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	req = fr.Requests[1]
	if req.Dir != path || req.Mode != runner.ModeEdit {
		t.Fatalf("%+v", req)
	}
	if !strings.Contains(req.Prompt, "already carries work from earlier runs") || !strings.Contains(req.Prompt, "uncommitted changes in 1 file(s)") || strings.Contains(req.Prompt, "created just now") {
		t.Fatalf("%s", req.Prompt)
	}
}

// A task branch open in the developer's own checkout is refused before anything is queued, with a way out.
func TestTaskBranchOpenInCheckout(t *testing.T) {
	svc, _, _ := newService(t)
	issue, _ := svc.AddIssue("https://gitlab.example.com/group/sub/project/-/issues/7")
	gitT(t, svc.Settings.ProjectRoot, "checkout", "-q", "-b", issue.Ref())
	for _, start := range []func() (int64, error){
		func() (int64, error) { return svc.StartPlan(issue.ID, "", "", 0, "") },
		func() (int64, error) { return svc.StartImplement(issue.ID, "", "", "", 0) },
	} {
		if _, err := start(); err == nil || !strings.Contains(err.Error(), "открыта в вашей рабочей копии") || !strings.Contains(err.Error(), "git switch develop") {
			t.Fatalf("%v", err)
		}
	}
	if runs, _ := svc.DB.ListRunsForIssue(issue.ID); len(runs) != 0 {
		t.Fatalf("nothing is queued: %d", len(runs))
	}
	// Another branch for the task is fine.
	if _, err := svc.StartImplement(issue.ID, "", "", "feature/other", 0); err != nil {
		t.Fatal(err)
	}
}

// «Обновить» rebases the task branch on a fresh origin/develop; a conflict aborts and leaves the branch as it was;
// uncommitted changes are refused.
func TestUpdateTaskWorkspace(t *testing.T) {
	svc, _, fr := newService(t)
	issue, _ := svc.AddIssue("https://gitlab.example.com/group/sub/project/-/issues/7")
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	implID, _ := svc.StartImplement(issue.ID, "", "", "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	path := svc.Worktrees.Path(issue.Ref())
	_ = os.WriteFile(filepath.Join(path, "task.txt"), []byte("mine\n"), 0o644)
	gitT(t, path, "add", "task.txt", "CHANGED.txt") // the fake agent leaves CHANGED.txt
	gitT(t, path, "commit", "-q", "-m", "task work")

	// Someone pushes to develop: a new file (no conflict) — the branch is behind by one, then rebased.
	other := filepath.Join(t.TempDir(), "other")
	remote := gitT(t, svc.Settings.ProjectRoot, "remote", "get-url", "origin")
	push := func(file, text string) {
		if _, err := os.Stat(other); err != nil {
			gitT(t, filepath.Dir(other), "clone", "-q", "-b", "develop", remote, other)
		}
		gitT(t, other, "pull", "-q", "origin", "develop")
		_ = os.WriteFile(filepath.Join(other, file), []byte(text), 0o644)
		gitT(t, other, "add", file)
		gitT(t, other, "commit", "-q", "-m", "upstream "+file)
		gitT(t, other, "push", "-q", "origin", "develop")
	}
	push("upstream.txt", "x\n")
	msg, err := svc.UpdateWorkspace(implID)
	if err != nil || !strings.Contains(msg, "перенесена") {
		t.Fatalf("%q %v", msg, err)
	}
	if st := svc.Worktrees.State(t.Context(), path); st.Behind != 0 || st.Ahead != 1 {
		t.Fatalf("%+v", st)
	}
	if _, err := os.Stat(filepath.Join(path, "upstream.txt")); err != nil {
		t.Fatal("the rebased branch has the upstream commit")
	}

	// A conflicting upstream change: the rebase is aborted, the branch keeps its commit as it was.
	push("task.txt", "theirs\n")
	before := gitT(t, path, "rev-parse", "HEAD")
	if _, err := svc.UpdateWorkspace(implID); err == nil || !strings.Contains(err.Error(), "task.txt") || !strings.Contains(err.Error(), "отменён") {
		t.Fatalf("%v", err)
	}
	if gitT(t, path, "rev-parse", "HEAD") != before || strings.Trim(strings.ReplaceAll(gitT(t, path, "status", "--short"), "?? .claude", ""), "\n") != "" {
		t.Fatal("after an aborted rebase the branch and the files are as they were")
	}
	if st := svc.Worktrees.State(t.Context(), path); st.Behind != 1 {
		t.Fatalf("the page still shows the branch behind: %+v", st)
	}

	_ = os.WriteFile(filepath.Join(path, "task.txt"), []byte("edited\n"), 0o644)
	if _, err := svc.UpdateWorkspace(implID); err == nil || !strings.Contains(err.Error(), "незакоммиченные") {
		t.Fatalf("%v", err)
	}
}

// The agent's `commits` become local commits: in order, only the listed paths; a step without changes or with a
// path outside the worktree makes nothing; what no step covers stays uncommitted. Nothing is pushed.
func TestAgentStepsBecomeLocalCommits(t *testing.T) {
	svc, _, fr := newService(t)
	issue, _ := svc.AddIssue("#7")
	out := testutil.ImplementOutput()
	out["commits"] = []any{
		map[string]any{"message": "feat: first step", "paths": []any{"CHANGED.txt"}},
		map[string]any{"message": "chore: nothing here", "paths": []any{"missing.txt", "../outside.txt"}},
	}
	fr.Outputs = []map[string]any{out}
	id, _ := svc.StartImplement(issue.ID, "", "", "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	path := svc.Worktrees.Path(issue.Ref())
	if log := gitT(t, path, "log", "--format=%s", "origin/develop..HEAD"); log != "feat: first step" {
		t.Fatalf("%q", log)
	}
	p, err := svc.PushPreviewFor(id)
	if err != nil || len(p.Commits) != 1 || !p.Ready || p.Title != "Draft: group/sub/project#7 Task 7" || p.HasUpstream {
		t.Fatalf("%+v %v", p, err)
	}

	// A file nobody committed blocks the push until it is committed.
	_ = os.WriteFile(filepath.Join(path, "left.txt"), []byte("x"), 0o644)
	if p, _ = svc.PushPreviewFor(id); p.Ready || !strings.Contains(p.Why, "незакоммиченные") {
		t.Fatalf("%+v", p)
	}
	if _, err := svc.PushAndDraftMR(id, "", ""); err == nil {
		t.Fatal("no push with uncommitted files")
	}
}
