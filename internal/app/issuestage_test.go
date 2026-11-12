package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/testutil"
)

// The stage follows the runs and the linked MRs; a stage set by hand holds until «авто».
func TestIssueStage(t *testing.T) {
	svc, gl, fr := newService(t)
	issue, _ := svc.AddIssue("#7")
	if issue.Stage != "new" {
		t.Fatalf("%q", issue.Stage)
	}
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	id, _ := svc.StartPlan(issue.ID, "", "", 0, "")
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	testutil.WaitFor(t, func() bool { i, _ := svc.DB.GetIssue(issue.ID); return i.Stage == "planned" })

	if err := svc.SetIssueStage(issue.ID, "review"); err != nil {
		t.Fatal(err)
	}
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	id, _ = svc.StartImplement(issue.ID, "", "", "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	if i, _ := svc.DB.GetIssue(issue.ID); i.Stage != "review" || !i.StageManual {
		t.Fatalf("a hand-set stage holds: %+v", i)
	}
	if err := svc.SetIssueStage(issue.ID, "nonsense"); err == nil {
		t.Fatal("unknown stage")
	}
	if err := svc.SetIssueStage(issue.ID, "auto"); err != nil {
		t.Fatal(err)
	}
	if i, _ := svc.DB.GetIssue(issue.ID); i.Stage != "implemented" || i.StageManual {
		t.Fatalf("%+v", i)
	}

	// GitLab links an open MR that closes the task → review; merged → merged; the task closed → done.
	gl.IssueMRs = map[int64][]gitlab.LinkedMR{7: {{ProjectPath: "group/sub/project", IID: 42, Title: "MR 42", State: "opened", SourceBranch: "feature", Closes: true}}}
	if i, _ := svc.RefreshIssue(issue.ID); i.Stage != "review" {
		t.Fatalf("%q", i.Stage)
	}
	gl.IssueMRs[7][0].State = "merged"
	if i, _ := svc.RefreshIssue(issue.ID); i.Stage != "merged" {
		t.Fatalf("%q", i.Stage)
	}
	gl.Issues[7]["state"] = "closed"
	if i, _ := svc.RefreshIssue(issue.ID); i.Stage != "done" {
		t.Fatalf("%q", i.Stage)
	}
}

// Linked MRs come from GitLab and from the task's branch; the MR page finds its task; comments reach the prompt.
func TestIssueLinksAndComments(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42") // source branch "feature"
	gl.IssueMRs = map[int64][]gitlab.LinkedMR{7: {{ProjectPath: "group/sub/project", IID: 42, Title: "MR 42", State: "opened", SourceBranch: "feature", Closes: true}}}
	gl.IssueNotes = map[int64][]gitlab.IssueNote{7: {{Author: "pm", Body: "Scope changed:\nonly the API part", CreatedAt: "2026-11-01T10:00:00Z"}}}
	issue, _ := svc.AddIssue("#7")
	mrs, notes := svc.DB.IssueLinks(issue.ID)
	if len(mrs) != 1 || mrs[0].MRID != mr.ID || mrs[0].Via != "closes" || len(notes) != 1 || notes[0].Author != "pm" {
		t.Fatalf("%+v %+v", mrs, notes)
	}
	if tasks := svc.IssuesOfMR(mr); len(tasks) != 1 || tasks[0].ID != issue.ID {
		t.Fatalf("%+v", tasks)
	}

	// Not linked in GitLab, but opened from the task's branch: still a linked MR.
	gl.IssueMRs = nil
	gl.MRs[43] = testutil.MRPayload(43, "sha-9")
	gl.MRs[43]["source_branch"] = issue.Ref()
	other, _ := svc.AddMR("!43")
	svc.RefreshIssue(issue.ID)
	if mrs, _ = svc.DB.IssueLinks(issue.ID); len(mrs) != 1 || mrs[0].IID != 43 || mrs[0].Via != "branch" || mrs[0].MRID != other.ID {
		t.Fatalf("%+v", mrs)
	}
	if tasks := svc.IssuesOfMR(other); len(tasks) != 1 {
		t.Fatal("the MR page finds the task of its branch")
	}

	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	id, _ := svc.StartPlan(issue.ID, "", "", 0, "")
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	prompt := fr.Requests[0].Prompt
	for _, want := range []string{"--- comments on the issue", "@pm, 2026-11-01T10:00:00Z:\nScope changed: only the API part", "Merge requests linked to this issue:", "group/sub/project!43 (opened"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}
