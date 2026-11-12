package db

import "testing"

// Rows from before projects existed get their project; nothing else changes and assigned rows stay as they are.
func TestAssignProjectsBackfill(t *testing.T) {
	d := open(t)
	mine, _ := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "tn/core/tradernet", IID: 1, Title: "a", State: "opened"})
	other, _ := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "other/repo", IID: 2, Title: "b", State: "opened"})
	elsewhere, _ := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "nobody/here", IID: 3, Title: "c", State: "opened"})
	task, _ := d.UpsertIssue(Issue{GitLabHost: "gl", ProjectPath: "tn/project/eu/eu", IID: 7, Title: "t", State: "opened"})
	otherTask, _ := d.UpsertIssue(Issue{GitLabHost: "gl", ProjectPath: "other/tracker", IID: 8, Title: "u", State: "opened"})
	reviewRun, _ := d.CreateRun(Run{Kind: KindReviewFull, MRID: &other.ID, Runner: "claude"})
	planRun, _ := d.CreateRun(Run{Kind: KindPlan, IssueID: &task.ID, Runner: "claude"})

	tn, _ := d.CreateProject(Project{Name: "tradernet", GitLabHost: "gl", ProjectPath: "tn/core/tradernet", RepoRoot: "/r/tn", IssueProjects: "tn/project/eu/eu"})
	rp, _ := d.CreateProject(Project{Name: "repo", GitLabHost: "gl", ProjectPath: "other/repo", RepoRoot: "/r/other", IssueProjects: "other/tracker"})
	projects, _ := d.ListProjects()
	mrs, issues, runs, err := d.AssignProjects(projects)
	if err != nil || mrs != 2 || issues != 2 || runs != 2 {
		t.Fatalf("assigned %d MRs, %d issues, %d runs: %v", mrs, issues, runs, err)
	}
	for _, c := range []struct {
		got, want int64
	}{
		{d.ProjectOfMR(mine.ID), tn.ID}, {d.ProjectOfMR(other.ID), rp.ID}, {d.ProjectOfMR(elsewhere.ID), 0},
		{d.ProjectOfIssue(task.ID), tn.ID}, {d.ProjectOfIssue(otherTask.ID), rp.ID},
		{d.ProjectOfRun(reviewRun), rp.ID}, {d.ProjectOfRun(planRun), tn.ID},
	} {
		if c.got != c.want {
			t.Fatalf("project %d, want %d", c.got, c.want)
		}
	}
	// A second pass changes nothing; data is intact.
	if mrs, issues, runs, _ := d.AssignProjects(projects); mrs+issues+runs != 0 {
		t.Fatal("assigned rows must not be touched again")
	}
	if got, _ := d.GetMR(mine.ID); got.Title != "a" {
		t.Fatalf("%+v", got)
	}
	if err := d.DeleteProject(rp.ID); err == nil {
		t.Fatal("a project with data cannot be deleted")
	}
}
