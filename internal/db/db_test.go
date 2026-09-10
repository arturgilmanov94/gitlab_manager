package db

import (
	"path/filepath"
	"testing"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "x", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMigrateIdempotent(t *testing.T) {
	d := open(t)
	applied, err := d.Migrate()
	if err != nil || len(applied) != 0 || len(d.SchemaVersion()) != 7 {
		t.Fatalf("%v %v %v", applied, err, d.SchemaVersion())
	}
}

func TestApprovalsAndWaitingRuns(t *testing.T) {
	d := open(t)
	mr, _ := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "g/p", IID: 1, WebURL: "u"})
	runID, _ := d.CreateRun(Run{Kind: KindReviewFull, MRID: &mr.ID, Runner: "claude"})
	_ = d.UpdateRun(runID, map[string]any{"status": StatusWaiting, "progress": "Нужен ваш ответ"})
	if active, _ := d.ActiveRunForMR(mr.ID); active == nil || !active.Active() || active.Progress == "" {
		t.Fatal("a waiting run is active")
	}
	id, err := d.CreateApproval(Approval{RunID: runID, ToolName: "Write", Description: "/x", InputJSON: `{"file_path":"/x"}`, Reason: "outside"})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := d.PendingApproval(runID); p == nil || p.ID != id || !p.Pending() {
		t.Fatalf("%+v", p)
	}
	if all, _ := d.PendingApprovals(); len(all) != 1 {
		t.Fatalf("%+v", all)
	}
	_ = d.DecideApproval(id, ApprovalAllowed, true, "")
	if p, _ := d.PendingApproval(runID); p != nil {
		t.Fatal("answered prompt is not pending")
	}
	list, _ := d.ListApprovals(runID)
	if len(list) != 1 || list[0].Status != ApprovalAllowed || !list[0].Remember || list[0].DecidedAt == "" {
		t.Fatalf("%+v", list)
	}
	// A restart fails the waiting run and expires whatever was still pending.
	second, _ := d.CreateApproval(Approval{RunID: runID, ToolName: "Bash"})
	if n, _ := d.FailStaleRuns("restart"); n != 1 {
		t.Fatal("waiting run must be failed on restart")
	}
	if a, _ := d.GetApproval(second); a.Status != ApprovalExpired {
		t.Fatalf("%+v", a)
	}
}

func TestMRRunFindingsFlow(t *testing.T) {
	d := open(t)
	mr, err := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "g/p", IID: 1, WebURL: "u", Title: "T", HeadSHA: "abc", Unresolved: 2})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "g/p", IID: 1, WebURL: "u", Title: "T2", HeadSHA: "def"})
	if again.ID != mr.ID || again.Title != "T2" || again.HeadSHA != "def" || again.Unresolved != 0 {
		t.Fatalf("%+v", again)
	}
	runID, err := d.CreateRun(Run{Kind: KindReviewFull, MRID: &mr.ID, HeadSHA: "def", Runner: "claude", SkillIdentifier: "agent:mr-review"})
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := d.ActiveRunForMR(mr.ID); active == nil || active.ID != runID {
		t.Fatal("active run expected")
	}
	_ = d.UpdateRun(runID, map[string]any{"status": StatusDone, "summary": "s", "verdict": "approve", "input_tokens": 10, "cache_read_tokens": 90})
	line := int64(3)
	_ = d.ReplaceFindings(runID, []Finding{{Severity: "high", Title: "x", File: "a.php", Line: &line}, {Severity: "LOW", Title: "y", Status: "fixed"}})
	_ = d.ReplaceDiscussions(runID, []Discussion{{Author: "bob", Body: "why?", Addressed: true}})
	items, _ := d.ListMRs()
	if len(items) != 1 || items[0].Last == nil || items[0].Last.Status != StatusDone || items[0].Last.OpenFindings != 1 || items[0].Last.Tokens != 100 {
		t.Fatalf("%+v", items)
	}
	findings, _ := d.ListFindings(runID)
	if findings[0].Severity != "HIGH" || *findings[0].Line != 3 || findings[1].Status != "fixed" {
		t.Fatalf("%+v", findings)
	}
	if latest, _ := d.LatestDoneReview(mr.ID); latest == nil || latest.ID != runID {
		t.Fatal("latest done review")
	}
	runs, _ := d.ListRunsForMR(mr.ID)
	if len(runs) != 1 || runs[0].TotalFindings != 2 {
		t.Fatalf("%+v", runs)
	}
	if _, err := d.AddMessage(runID, "user", "q", 0, 0); err != nil {
		t.Fatal(err)
	}
	// The fixture MR has no role of mine: it counts as history, not as the main list.
	if c := d.Counts(); c["merge_requests"] != 0 || c["history"] != 1 || c["runs"] != 1 || c["findings"] != 2 || c["tokens"] != 100 {
		t.Fatalf("%v", c)
	}
	_, _ = d.UpsertMR(MergeRequest{GitLabHost: "gl", ProjectPath: "g/p", IID: 1, WebURL: "u", State: "opened", MyRoles: "reviewer"})
	if c := d.Counts(); c["merge_requests"] != 1 || c["history"] != 0 {
		t.Fatalf("relevant MR must be counted in the main list: %v", c)
	}
	_ = d.SetMRHidden(mr.ID, true)
	if c := d.Counts(); c["merge_requests"] != 0 || c["history"] != 1 {
		t.Fatalf("hidden MR moves to the history count: %v", c)
	}
	_ = d.DeleteMR(mr.ID)
	if run, _ := d.GetRun(runID); run != nil {
		t.Fatal("cascade delete expected")
	}
}

func TestIssuesAndStale(t *testing.T) {
	d := open(t)
	issue, err := d.UpsertIssue(Issue{GitLabHost: "gl", ProjectPath: "g/p", IID: 9, WebURL: "u", Title: "task"})
	if err != nil || issue.Ref() != "g/p#9" {
		t.Fatalf("%+v %v", issue, err)
	}
	runID, _ := d.CreateRun(Run{Kind: KindPlan, IssueID: &issue.ID, Runner: "claude"})
	if n, _ := d.FailStaleRuns("restart"); n != 1 {
		t.Fatal("stale run")
	}
	list, _ := d.ListIssues()
	if len(list) != 1 || list[0].Last == nil || list[0].Last.ID != runID || list[0].Last.Status != StatusFailed {
		t.Fatalf("%+v", list)
	}
}
