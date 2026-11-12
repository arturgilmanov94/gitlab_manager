package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// Two projects: the first comes from .env at start, the second is added. MRs and tasks are assigned by their
// GitLab project / tracker, and every run works in its own project's repository.
func TestTwoProjectsRunInTheirRepos(t *testing.T) {
	svc, gl, fr := newService(t)
	projects := svc.Projects()
	if len(projects) != 1 || projects[0].RepoRoot != svc.Settings.ProjectRoot || projects[0].ProjectPath != "group/sub/project" || svc.DefaultProjectID() != projects[0].ID {
		t.Fatalf("the default project comes from .env: %+v", projects)
	}
	otherRoot := testutil.GitRepo(t, filepath.Join(t.TempDir(), "other"))
	other, err := svc.DB.CreateProject(db.Project{Name: "other", GitLabHost: "gitlab.example.com", ProjectPath: "group/other", RepoRoot: otherRoot, BaseBranch: "develop"})
	if err != nil {
		t.Fatal(err)
	}

	payload := testutil.MRPayload(50, "sha-50")
	payload["web_url"] = "https://gitlab.example.com/group/other/-/merge_requests/50"
	payload["references"] = map[string]any{"full": "group/other!50"}
	payload["source_branch"] = "other-feature"
	gl.MRs[50] = payload
	issue := testutil.IssuePayload(9)
	issue["web_url"] = "https://gitlab.example.com/group/other/-/issues/9"
	issue["references"] = map[string]any{"full": "group/other#9"}
	gl.Issues[9] = issue

	mine, _ := svc.AddMR("!42")
	theirs, err := svc.AddMR("group/other!50")
	if err != nil {
		t.Fatal(err)
	}
	task, err := svc.AddIssue("https://gitlab.example.com/group/other/-/issues/9")
	if err != nil {
		t.Fatal(err)
	}
	if svc.DB.ProjectOfMR(mine.ID) != projects[0].ID || svc.DB.ProjectOfMR(theirs.ID) != other.ID || svc.DB.ProjectOfIssue(task.ID) != other.ID {
		t.Fatal("objects must be assigned to their projects")
	}

	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1"), testutil.FullReviewOutput("sha-50")}
	a, _ := svc.StartReview(mine.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, a) == db.StatusDone })
	b, _ := svc.StartReview(theirs.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, b) == db.StatusDone })
	if fr.Requests[0].Dir != svc.Settings.ProjectRoot || fr.Requests[1].Dir != otherRoot {
		t.Fatalf("reviews must run in their repositories: %s, %s", fr.Requests[0].Dir, fr.Requests[1].Dir)
	}
	if svc.DB.ProjectOfRun(a) != projects[0].ID || svc.DB.ProjectOfRun(b) != other.ID {
		t.Fatal("runs carry their project")
	}

	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	impl, err := svc.StartImplement(task.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, impl) == db.StatusDone })
	run, _ := svc.DB.GetRun(impl)
	if !strings.Contains(run.WorkDir, "project-") || fr.Requests[2].Dir != run.WorkDir {
		t.Fatalf("the worktree of another project lives in its own directory: %s", run.WorkDir)
	}
	out, _ := exec.Command("git", "-C", otherRoot, "worktree", "list").Output()
	if !strings.Contains(string(out), run.WorkDir) {
		t.Fatalf("the worktree must belong to the other repository:\n%s", out)
	}
	if strings.Contains(fr.Requests[2].Prompt, "group/sub/project") {
		t.Fatal("the prompt must name the other project")
	}
}
