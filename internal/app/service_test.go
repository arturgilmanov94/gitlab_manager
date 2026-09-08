package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/runner"
	"mr-review/internal/testutil"
)

func newService(t *testing.T) (*Service, *testutil.FakeGitLab, *testutil.FakeRunner) {
	t.Helper()
	settings, _ := testutil.Settings(t)
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Migrate(); err != nil {
		t.Fatal(err)
	}
	gl := testutil.NewFakeGitLab()
	fr := &testutil.FakeRunner{}
	svc := New(settings, database, gl, []runner.Runner{fr})
	t.Cleanup(func() { svc.Shutdown(); database.Close() })
	return svc, gl, fr
}

func status(svc *Service, id int64) string {
	run, _ := svc.DB.GetRun(id)
	if run == nil {
		return ""
	}
	return run.Status
}

func TestDefaultProjectAndUser(t *testing.T) {
	svc, _, _ := newService(t)
	// The fixture's origin is a local bare path, so host/project come from GITLAB_HOST / GITLAB_PROJECT.
	if h, p := svc.DefaultProject(); h != "gitlab.example.com" || p != "group/sub/project" {
		t.Fatalf("host %q project %q", h, p)
	}
	if svc.CurrentUser() != "alice" {
		t.Fatal("user")
	}
	if svc.Skill() == nil || svc.Skill().Name != "mr-review" {
		t.Fatal("skill not resolved from project")
	}
}

func TestAddSyncReviewVerify(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, err := svc.AddMR("https://gitlab.example.com/group/sub/project/-/merge_requests/42")
	if err != nil {
		t.Fatal(err)
	}
	if mr.HeadSHA != "sha-1" || mr.Author != "alice" || mr.Unresolved != 2 {
		t.Fatalf("%+v", mr)
	}
	res, err := svc.SyncMRs()
	if err != nil || res.Synced != 1 || res.Username != "alice" {
		t.Fatalf("%+v %v", res, err)
	}

	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, err := svc.StartReview(mr.ID, db.KindReviewFull, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	run, _ := svc.DB.GetRun(runID)
	if run.Verdict != "request_changes" || run.SkillIdentifier != "agent:mr-review" || run.CostUSD != 0.1 || run.SessionID != "sess-1" {
		t.Fatalf("%+v", run)
	}
	req := fr.Requests[0]
	if req.Agent != "mr-review" || req.Mode != runner.ModeReadOnly || req.Dir != svc.Settings.ProjectRoot {
		t.Fatalf("request: %+v", req)
	}
	for _, want := range []string{"Mode: FULL REVIEW", "merge_requests/42", "Head SHA to review: sha-1", "READ-ONLY"} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	findings, _ := svc.DB.ListFindings(runID)
	if len(findings) != 2 || findings[0].Severity != "HIGH" {
		t.Fatalf("%+v", findings)
	}
	if d, _ := svc.DB.ListDiscussions(runID); len(d) != 1 || d[0].Author != "bob" {
		t.Fatal("discussions")
	}

	// New commits arrive; verify.
	gl.MRs[42] = testutil.MRPayload(42, "sha-2")
	fr.Outputs = []map[string]any{testutil.VerifyOutput(findings[0].ID, findings[1].ID, "sha-2")}
	verifyID, err := svc.StartReview(mr.ID, db.KindReviewVerify, "claude")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, verifyID) == db.StatusDone })
	verify, _ := svc.DB.GetRun(verifyID)
	if verify.BaseRunID == nil || *verify.BaseRunID != runID || verify.HeadSHA != "sha-2" {
		t.Fatalf("%+v", verify)
	}
	merged, _ := svc.DB.ListFindings(verifyID)
	got := []string{}
	for _, f := range merged {
		got = append(got, f.Status+":"+f.Title)
	}
	if strings.Join(got, ",") != "fixed:Null deref,open:Naming,open:New" {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(fr.Requests[1].Prompt, "Previously reviewed SHA: sha-1") {
		t.Fatal("verify prompt")
	}
	items, _ := svc.DB.ListMRs()
	if items[0].Last.OpenFindings != 2 || items[0].Stale {
		t.Fatalf("%+v", items[0])
	}

	// Quick review prompt is different.
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-2")}
	quickID, _ := svc.StartReview(mr.ID, db.KindReviewQuick, "")
	testutil.WaitFor(t, func() bool { return status(svc, quickID) == db.StatusDone })
	if !strings.Contains(fr.Requests[2].Prompt, "QUICK REVIEW") {
		t.Fatal("quick prompt")
	}
}

func TestVerifyNeedsBaseAndSingleActive(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	if _, err := svc.StartReview(mr.ID, db.KindReviewVerify, ""); err == nil || !strings.Contains(err.Error(), "full review first") {
		t.Fatalf("%v", err)
	}
	fr.Block = make(chan struct{})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, err := svc.StartReview(mr.ID, db.KindReviewFull, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusRunning })
	if _, err := svc.StartReview(mr.ID, db.KindReviewFull, ""); err == nil || !strings.Contains(err.Error(), "already queued or running") {
		t.Fatalf("%v", err)
	}
	if !svc.Cancel(runID) {
		t.Fatal("cancel")
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusCancelled })
	if svc.Cancel(runID) {
		t.Fatal("second cancel must be false")
	}
}

func TestFailureAndRecover(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.FailWith = "claude timed out after 1s"
	runID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "")
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusFailed })
	run, _ := svc.DB.GetRun(runID)
	if !strings.Contains(run.Error, "timed out") {
		t.Fatal(run.Error)
	}
	_, _ = svc.DB.CreateRun(db.Run{Kind: db.KindReviewFull, MRID: &mr.ID, Runner: "claude"})
	if svc.Recover() != 1 {
		t.Fatal("recover")
	}
}

func TestPlanImplementAndWorktreeActions(t *testing.T) {
	svc, gl, fr := newService(t)
	issue, err := svc.AddIssue("https://gitlab.example.com/group/sub/project/-/issues/7")
	if err != nil {
		t.Fatal(err)
	}
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	planID, err := svc.StartPlan(issue.ID, "", "be careful")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, planID) == db.StatusDone })
	plan, _ := svc.DB.GetRun(planID)
	if plan.Summary != "Plan summary" || !strings.Contains(plan.Prompt, "be careful") || fr.Requests[0].Mode != runner.ModeReadOnly {
		t.Fatalf("%+v", plan)
	}

	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	implID, err := svc.StartImplement(issue.ID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	impl, _ := svc.DB.GetRun(implID)
	if impl.Branch != "group/sub/project#7" || impl.WorkDir == "" || fr.Requests[1].Mode != runner.ModeEdit || fr.Requests[1].Dir != impl.WorkDir {
		t.Fatalf("%+v", impl)
	}
	if _, err := os.Stat(filepath.Join(impl.WorkDir, "CHANGED.txt")); err != nil {
		t.Fatal("agent edit not in worktree")
	}
	if _, err := os.Lstat(filepath.Join(impl.WorkDir, ".claude")); err != nil {
		t.Fatal(".claude not linked into the worktree")
	}
	// The main checkout is untouched and still on develop.
	if _, err := os.Stat(filepath.Join(svc.Settings.ProjectRoot, "CHANGED.txt")); err == nil {
		t.Fatal("main checkout was modified")
	}
	state := svc.Worktree(impl)
	if !state.Exists || !strings.Contains(state.Status, "CHANGED.txt") || !strings.Contains(state.Diff, "changed by fake agent") {
		t.Fatalf("%+v", state)
	}
	if _, err := svc.Commit(implID, "group/sub/project#7 do the thing"); err != nil {
		t.Fatal(err)
	}
	if state = svc.Worktree(impl); strings.TrimSpace(state.Status) != "" || !strings.Contains(state.Log, "do the thing") {
		t.Fatalf("after commit: %+v", state)
	}
	if _, err := svc.Push(implID); err != nil {
		t.Fatal(err)
	}
	url, err := svc.CreateMR(implID, "", "")
	if err != nil || url == "" || !strings.Contains(gl.CreatedMR, "|group/sub/project#7|develop|group/sub/project#7 Task 7|Closes ") {
		t.Fatalf("%s %v %s", url, err, gl.CreatedMR)
	}
	if err := svc.RemoveWorktree(implID); err != nil {
		t.Fatal(err)
	}
	if svc.Worktree(impl).Exists {
		t.Fatal("worktree should be gone")
	}
}

func TestFixCommentsUsesMRBranchWorktree(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	// The MR branch does not exist on origin in this fixture; Prepare must create it from origin/develop.
	runID, err := svc.StartFixComments(mr.ID, "", "only file A")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) != db.StatusQueued && status(svc, runID) != db.StatusRunning })
	run, _ := svc.DB.GetRun(runID)
	if run.Status != db.StatusDone {
		t.Fatalf("%s: %s", run.Status, run.Error)
	}
	if run.Branch != "feature" || !strings.Contains(run.Prompt, "FIX REVIEW COMMENTS") || !strings.Contains(run.Prompt, "only file A") {
		t.Fatalf("%+v", run)
	}
}

func TestAskResumesSession(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "")
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	fr.Texts = []string{"Because it can be nil."}
	answer, err := svc.Ask(runID, "why HIGH?")
	if err != nil || answer != "Because it can be nil." {
		t.Fatalf("%q %v", answer, err)
	}
	last := fr.Requests[len(fr.Requests)-1]
	if last.ResumeSessionID != "sess-1" || len(last.Schema) != 0 || last.Agent != "" {
		t.Fatalf("%+v", last)
	}
	msgs, _ := svc.DB.ListMessages(runID)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("%+v", msgs)
	}
}
