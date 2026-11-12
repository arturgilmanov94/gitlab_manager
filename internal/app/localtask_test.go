package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// A local task lives without GitLab: numbered on its own, untouched by sync, worked on in its own branch, closed
// by its stage; the prompt says there is no issue to look for.
func TestLocalTask(t *testing.T) {
	svc, gl, fr := newService(t)
	if _, err := svc.CreateLocalTask(0, "  ", ""); err == nil {
		t.Fatal("a title is required")
	}
	a, err := svc.CreateLocalTask(0, "Speed up the export", "Only the CSV path")
	if err != nil || !a.Local() || a.IID != 1 || a.Ref() != "task-1" || a.Stage != "new" || a.Closed() {
		t.Fatalf("%+v %v", a, err)
	}
	b, _ := svc.CreateLocalTask(0, "Second", "")
	if b.IID != 2 {
		t.Fatalf("%d", b.IID)
	}
	if svc.DB.ProjectOfIssue(a.ID) != svc.root.ProjectID {
		t.Fatal("a local task belongs to the default project")
	}

	before := len(gl.CallsCopy())
	if _, err := svc.SyncIssues(); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.DB.GetIssue(a.ID); got == nil || got.Title != "Speed up the export" {
		t.Fatal("sync keeps local tasks")
	}
	for _, c := range gl.CallsCopy()[before:] {
		if strings.Contains(c, "task-") || strings.HasPrefix(c, "issue local") {
			t.Fatalf("no GitLab call for a local task: %s", c)
		}
	}
	if _, err := svc.RefreshIssue(a.ID); err != nil {
		t.Fatalf("refresh of a local task needs no GitLab: %v", err)
	}

	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	id, err := svc.StartPlan(a.ID, "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	prompt := fr.Requests[0].Prompt
	if !strings.Contains(prompt, "local task #1") || !strings.Contains(prompt, "NO GitLab issue") || !strings.Contains(prompt, "Only the CSV path") || !strings.Contains(prompt, "branch `task-1`") {
		t.Fatalf("%s", prompt)
	}
	if fr.Requests[0].Dir != svc.Worktrees.Path("task-1") {
		t.Fatalf("%s", fr.Requests[0].Dir)
	}

	if err := svc.EditLocalTask(a.ID, "Speed up the CSV export", "edited"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.DB.GetIssue(a.ID); got.Title != "Speed up the CSV export" || got.Description != "edited" {
		t.Fatalf("%+v", got)
	}
	gitlabTask, _ := svc.AddIssue("#7")
	if err := svc.EditLocalTask(gitlabTask.ID, "x", ""); err == nil {
		t.Fatal("a GitLab task is edited in GitLab")
	}

	a, _ = svc.DB.GetIssue(a.ID)
	if draft := svc.MRDraftFor(&db.Run{ResultJSON: `{"summary":"done"}`}, a); strings.Contains(draft.Description, "Closes") || draft.Title != "task-1 Speed up the CSV export" {
		t.Fatalf("%+v", draft)
	}
	if err := svc.SetIssueStage(a.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.DB.GetIssue(a.ID); !got.Closed() {
		t.Fatal("a local task is closed by its stage")
	}
}
