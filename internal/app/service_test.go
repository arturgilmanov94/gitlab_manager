package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mr-review/internal/config"
	"mr-review/internal/db"
	"mr-review/internal/runner"
	"mr-review/internal/skill"
	"mr-review/internal/testutil"
)

func newService(t *testing.T) (*Service, *testutil.FakeGitLab, *testutil.FakeRunner) {
	t.Helper()
	settings, _ := testutil.Settings(t)
	return newServiceWith(t, settings)
}

func newServiceWith(t *testing.T, settings *config.Settings) (*Service, *testutil.FakeGitLab, *testutil.FakeRunner) {
	t.Helper()
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
	// Every action is mapped; only the review ones are backed by the fixture project.
	if m := svc.SkillMap(); len(m) != len(skill.Actions) || !m[0].Found() || m[0].Action.Kind != db.KindReviewFull {
		t.Fatalf("%+v", m)
	}
	if svc.SkillFor(db.KindReviewQuick) == nil || svc.SkillFor(db.KindPlan) != nil {
		t.Fatal("quick review must fall back to the review agent; plan has no project skill in the fixture")
	}
}

func TestSyncScopesMRs(t *testing.T) {
	svc, gl, fr := newService(t)
	// !42: alice is author+assignee. !43 and !44 are bob's MRs where alice is a reviewer; !45 is bob's MR without
	// any role for alice, added by hand.
	for _, iid := range []int64{43, 44, 45} {
		p := testutil.MRPayload(iid, "sha")
		p["author"] = map[string]any{"username": "bob"}
		p["assignees"] = []any{}
		p["reviewers"] = []any{map[string]any{"username": "alice"}}
		if iid == 45 {
			p["reviewers"] = []any{}
		}
		gl.MRs[iid] = p
	}
	gl.Listed = []int64{42, 43, 44}
	added, _ := svc.AddMR("!45")
	if !added.Manual || added.MyRoles != "" || !added.Relevant() {
		t.Fatalf("manual MR must stay relevant: %+v", added)
	}
	res, err := svc.SyncMRs()
	if err != nil || res.Synced != 4 || res.Archived != 0 || res.Pruned != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	mr42 := findMR(t, svc, 42)
	if mr42.MyRoles != "author,assignee" || mr42.ApprovedByMe || !mr42.Relevant() {
		t.Fatalf("%+v", mr42)
	}
	if mr42.Labels != "High, backend" {
		t.Fatalf("GitLab labels must be stored on the MR: %q", mr42.Labels)
	}
	if findMR(t, svc, 43).MyRoles != "reviewer" {
		t.Fatal("reviewer role expected")
	}
	// Alice is removed from the reviewers of 43 and 44: the reviewed one goes to the history, the untouched one is pruned.
	mr43 := findMR(t, svc, 43)
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha")}
	runID, _ := svc.StartReview(mr43.ID, db.KindReviewQuick, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	gl.MRs[43]["reviewers"] = []any{}
	gl.MRs[44]["reviewers"] = []any{}
	gl.Listed = []int64{42}
	res, _ = svc.SyncMRs()
	if res.Synced != 2 || res.Archived != 1 || res.Pruned != 1 {
		t.Fatalf("synced %d archived %d pruned %d", res.Synced, res.Archived, res.Pruned)
	}
	all, _ := svc.DB.ListMRs()
	seen := map[int64]bool{}
	for _, mr := range all {
		seen[mr.IID] = true
	}
	if !seen[42] || !seen[43] || seen[44] || !seen[45] {
		t.Fatalf("expected 42, 43 (history), 45 (manual) to stay and 44 to be pruned: %v", seen)
	}
	if findMR(t, svc, 43).Relevant() || !findMR(t, svc, 45).Relevant() {
		t.Fatal("reviewed foreign MR is history; manual MR stays relevant")
	}
	// Approving my own MR moves it out of the main list too.
	gl.ApprovedBy[42] = []string{"alice"}
	if _, err := svc.RefreshMR(mr42.ID); err != nil {
		t.Fatal(err)
	}
	if findMR(t, svc, 42).Relevant() {
		t.Fatal("approved by me is not relevant")
	}
}

func findMR(t *testing.T, svc *Service, iid int64) *db.MergeRequest {
	t.Helper()
	all, _ := svc.DB.ListMRs()
	for _, mr := range all {
		if mr.IID == iid {
			m := mr.MergeRequest
			return &m
		}
	}
	t.Fatalf("MR !%d not found", iid)
	return nil
}

func TestDeriveVerdict(t *testing.T) {
	cases := []struct {
		agent    string
		findings []db.Finding
		want     string
	}{
		{"approve_with_comments", nil, "approve"},
		{"approve", []db.Finding{{Severity: "HIGH", Status: "open"}}, "request_changes"},
		{"approve", []db.Finding{{Severity: "CRITICAL", Status: "open"}, {Severity: "LOW", Status: "open"}}, "blocked"},
		{"request_changes", []db.Finding{{Severity: "LOW", Status: "open"}, {Severity: "MEDIUM", Status: "open"}}, "approve_with_comments"},
		{"blocked", []db.Finding{{Severity: "HIGH", Status: "fixed"}, {Severity: "MEDIUM", Status: "obsolete"}}, "approve"},
		{"request_changes", []db.Finding{{Severity: "HIGH", Status: "open"}}, "request_changes"},
	}
	for _, c := range cases {
		if got := DeriveVerdict(c.agent, c.findings); got != c.want {
			t.Errorf("%s + %d findings: got %s, want %s", c.agent, len(c.findings), got, c.want)
		}
	}
}

func TestParallelRunsOnDifferentMRs(t *testing.T) {
	settings, _ := testutil.Settings(t)
	settings.RunConcurrency = 2
	svc, gl, fr := newServiceWith(t, settings)
	gl.MRs[43] = testutil.MRPayload(43, "sha-43")
	a, _ := svc.AddMR("!42")
	b, _ := svc.AddMR("!43")
	fr.Block = make(chan struct{})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1"), testutil.FullReviewOutput("sha-43")}
	runA, err := svc.StartReview(a.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	runB, err := svc.StartReview(b.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Both agents are running at the same time: two requests reached the runner while both are blocked.
	testutil.WaitFor(t, func() bool { return status(svc, runA) == db.StatusRunning && status(svc, runB) == db.StatusRunning })
	fr.Lock()
	inFlight := len(fr.Requests)
	fr.Unlock()
	if inFlight != 2 {
		t.Fatalf("expected 2 concurrent agent invocations, got %d", inFlight)
	}
	// A second run on the same MR is still refused while the first is active.
	if _, err := svc.StartReview(a.ID, db.KindReviewQuick, "", 0); err == nil || !strings.Contains(err.Error(), "already queued or running") {
		t.Fatalf("%v", err)
	}
	close(fr.Block)
	testutil.WaitFor(t, func() bool { return status(svc, runA) == db.StatusDone && status(svc, runB) == db.StatusDone })
}

func TestPermissionPromptAnsweredFromDashboard(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")

	// Allowed with the agent's suggested rules: the decision reaches the runner and the run finishes.
	fr.AskWrite("/tmp/claude/a.txt")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusWaiting })
	pending, _ := svc.DB.PendingApproval(runID)
	if pending == nil || pending.ToolName != "Write" || pending.Description != "/tmp/claude/a.txt" || pending.SuggestionsJSON == "" {
		t.Fatalf("%+v", pending)
	}
	if all, _ := svc.DB.PendingApprovals(); len(all) != 1 {
		t.Fatalf("header badge source: %+v", all)
	}
	run, _ := svc.DB.GetRun(runID)
	if !run.Active() || !strings.Contains(run.Progress, "Нужен ваш ответ") {
		t.Fatalf("%+v", run)
	}
	// A second run on the same MR is still refused while this one waits.
	if _, err := svc.StartReview(mr.ID, db.KindReviewQuick, "", 0); err == nil {
		t.Fatal("waiting run must count as active")
	}
	if err := svc.Decide(pending.ID, "allow_always", ""); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	fr.Lock()
	decision := fr.Decisions[0]
	fr.Unlock()
	if !decision.Allow || !decision.ApplySuggestions {
		t.Fatalf("%+v", decision)
	}
	approvals, _ := svc.DB.ListApprovals(runID)
	if len(approvals) != 1 || approvals[0].Status != db.ApprovalAllowed || !approvals[0].Remember {
		t.Fatalf("%+v", approvals)
	}
	if err := svc.Decide(pending.ID, "allow", ""); err == nil {
		t.Fatal("answering twice must fail")
	}

	// Denied with a note: the agent gets the note, the run still completes, the denial is recorded on the run.
	fr.AskWrite("/tmp/claude/b.txt")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, _ = svc.StartReview(mr.ID, db.KindReviewQuick, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusWaiting })
	pending, _ = svc.DB.PendingApproval(runID)
	if err := svc.Decide(pending.ID, "deny", "use $TMPDIR instead"); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	fr.Lock()
	decision = fr.Decisions[1]
	fr.Unlock()
	if decision.Allow || decision.Message != "use $TMPDIR instead" {
		t.Fatalf("%+v", decision)
	}
	run, _ = svc.DB.GetRun(runID)
	if !strings.Contains(run.DenialsJSON, "/tmp/claude/b.txt") || run.Progress != "" {
		t.Fatalf("%+v", run)
	}

	// Cancelled while waiting: the prompt expires, the run is cancelled.
	fr.AskWrite("/tmp/claude/c.txt")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, _ = svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusWaiting })
	pending, _ = svc.DB.PendingApproval(runID)
	if !svc.Cancel(runID) {
		t.Fatal("cancel")
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusCancelled })
	if a, _ := svc.DB.GetApproval(pending.ID); a.Status != db.ApprovalExpired {
		t.Fatalf("%+v", a)
	}
	if err := svc.Decide(pending.ID, "allow", ""); err == nil {
		t.Fatal("an expired prompt cannot be answered")
	}
}

func TestExportPlanWritesMarkdown(t *testing.T) {
	svc, _, fr := newService(t)
	svc.Settings.PlansDir = filepath.Join(t.TempDir(), "plans")
	issue, _ := svc.AddIssue("#7")
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	planID, _ := svc.StartPlan(issue.ID, "", "be careful", 0, "")
	testutil.WaitFor(t, func() bool { return status(svc, planID) == db.StatusDone })
	path, err := svc.ExportPlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, svc.Settings.PlansDir) || !strings.HasSuffix(path, "-group__sub__project-7-run"+strconv.FormatInt(planID, 10)+".md") {
		t.Fatalf("path %s", path)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"# group/sub/project#7 — Task 7", "Plan summary", "1. one", "2. two", "| `a.php` | x |", "be careful", "Оценка: S"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan file missing %q:\n%s", want, text)
		}
	}
	run, _ := svc.DB.GetRun(planID)
	if run.PlanPath != path {
		t.Fatalf("plan_path not stored: %+v", run)
	}
	// Only finished plans can be exported.
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	reviewID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusDone })
	if _, err := svc.ExportPlan(reviewID); err == nil {
		t.Fatal("a review is not a plan")
	}
}

func TestConcurrencyOneQueuesSecondMR(t *testing.T) {
	settings, _ := testutil.Settings(t)
	settings.RunConcurrency = 1
	svc, gl, fr := newServiceWith(t, settings)
	gl.MRs[43] = testutil.MRPayload(43, "sha-43")
	a, _ := svc.AddMR("!42")
	b, _ := svc.AddMR("!43")
	fr.Block = make(chan struct{})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1"), testutil.FullReviewOutput("sha-43")}
	runA, _ := svc.StartReview(a.ID, db.KindReviewFull, "", 0)
	runB, _ := svc.StartReview(b.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, runA) == db.StatusRunning })
	if status(svc, runB) != db.StatusQueued {
		t.Fatalf("second MR must wait in the queue, got %s", status(svc, runB))
	}
	close(fr.Block)
	testutil.WaitFor(t, func() bool { return status(svc, runA) == db.StatusDone && status(svc, runB) == db.StatusDone })
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
	runID, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	run, _ := svc.DB.GetRun(runID)
	if run.Verdict != "request_changes" || run.SkillIdentifier != "agent:mr-review" || run.CostUSD != 0.1 || run.SessionID != "sess-1" || run.TotalTokens() != 54200 {
		t.Fatalf("%+v", run)
	}
	req := fr.Requests[0]
	if req.Agent != "mr-review" || req.Mode != runner.ModeReadOnly || req.Dir != svc.Settings.ProjectRoot {
		t.Fatalf("request: %+v", req)
	}
	for _, want := range []string{"Mode: FULL REVIEW", "merge_requests/42", "Head SHA to review: sha-1", "READ-ONLY", "LANGUAGE: write every human-readable field", "in Russian"} {
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
	verifyID, err := svc.StartReview(mr.ID, db.KindReviewVerify, "claude", 0)
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
	// Verify continues the review's agent session (same runner): resume instead of a fresh --agent start.
	if fr.Requests[1].ResumeSessionID != "sess-1" || fr.Requests[1].Agent != "" {
		t.Fatalf("verify must resume the base run's session: %+v", fr.Requests[1])
	}
	if chain, _ := svc.DB.ListRunsBySession("sess-1"); len(chain) < 2 || chain[0].ID != runID {
		t.Fatalf("session chain: %+v", chain)
	}
	if mr.PipelineStatus != "failed" || mr.Diverged != 3 || mr.ApprovalsGiven != 1 || mr.ApprovalsRequired != 2 || mr.ChangesCount != "7" {
		t.Fatalf("GitLab state not stored: %+v", mr)
	}
	items, _ := svc.DB.ListMRs()
	if items[0].Last.OpenFindings != 2 || items[0].Stale {
		t.Fatalf("%+v", items[0])
	}

	// Quick review prompt is different.
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-2")}
	quickID, _ := svc.StartReview(mr.ID, db.KindReviewQuick, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, quickID) == db.StatusDone })
	if !strings.Contains(fr.Requests[2].Prompt, "QUICK REVIEW") {
		t.Fatal("quick prompt")
	}
}

func TestVerifyNeedsBaseAndSingleActive(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	if _, err := svc.StartReview(mr.ID, db.KindReviewVerify, "", 0); err == nil || !strings.Contains(err.Error(), "full review first") {
		t.Fatalf("%v", err)
	}
	fr.Block = make(chan struct{})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusRunning })
	if _, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0); err == nil || !strings.Contains(err.Error(), "already queued or running") {
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
	runID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
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
	planID, err := svc.StartPlan(issue.ID, "", "be careful", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, planID) == db.StatusDone })
	plan, _ := svc.DB.GetRun(planID)
	if plan.Summary != "Plan summary" || !strings.Contains(plan.Prompt, "be careful") || fr.Requests[0].Mode != runner.ModeReadOnly {
		t.Fatalf("%+v", plan)
	}
	// No project skill for "plan" in the fixture: the prompt says so and no agent is selected.
	if plan.SkillIdentifier != "" || fr.Requests[0].Agent != "" || !strings.Contains(plan.Prompt, "No dedicated project skill") {
		t.Fatalf("plan without project skill: %+v", plan)
	}

	// The project provides an implementation agent: the run is handed to it.
	testutil.AddProjectAgent(t, svc.Settings.ProjectRoot, "task-implement", "Implement a GitLab task")
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	implID, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	impl, _ := svc.DB.GetRun(implID)
	if impl.Branch != "group/sub/project#7" || impl.WorkDir == "" || fr.Requests[1].Mode != runner.ModeEdit || fr.Requests[1].Dir != impl.WorkDir {
		t.Fatalf("%+v", impl)
	}
	if impl.SkillIdentifier != "agent:task-implement" || fr.Requests[1].Agent != "task-implement" || !strings.Contains(impl.Prompt, "`task-implement` agent") {
		t.Fatalf("implement must run as the project's task-implement agent: %+v", impl)
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
	// A project skill (slash command) for comment fixes: invoked inside the prompt, no --agent.
	testutil.AddProjectSkill(t, svc.Settings.ProjectRoot, "mr-fix-comments", "Address reviewer comments")
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	// The MR branch does not exist on origin in this fixture; Prepare must create it from origin/develop.
	runID, err := svc.StartFixComments(mr.ID, "", "only file A", 0)
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
	if run.SkillIdentifier != "skill:mr-fix-comments" || !strings.HasPrefix(run.Prompt, "/mr-fix-comments https://") || fr.Requests[0].Agent != "" {
		t.Fatalf("fix-comments must invoke the project skill as a slash command: %+v", run)
	}
}

func TestAskResumesSession(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	runID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
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

// Phase C: the developer chooses to start a run inside an earlier agent session of the same object.
func TestContinueSessionByChoice(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	gl.MRs[43] = testutil.MRPayload(43, "sha-x")
	other, _ := svc.AddMR("!43")

	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	first, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, first) == db.StatusDone })
	if got := svc.Resumable(mustRuns(t, svc, mr.ID), svc.Settings.ProjectRoot); len(got) != 1 || got[0].ID != first {
		t.Fatalf("the finished review must be offered as a context: %+v", got)
	}

	// Wrong choices are rejected with a clear message.
	if _, err := svc.StartReview(other.ID, db.KindReviewFull, "", first); err == nil || !strings.Contains(err.Error(), "another merge request") {
		t.Fatalf("session of another MR must be refused: %v", err)
	}
	if _, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 999); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := svc.StartFixComments(mr.ID, "", "", first); err == nil || !strings.Contains(err.Error(), "another directory") {
		t.Fatalf("a project-root session cannot continue in a worktree: %v", err)
	}

	// A new full review inside the first session: resume instead of --agent, the prompt says it is a continuation.
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	second, err := svc.StartReview(mr.ID, db.KindReviewFull, "", first)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, second) == db.StatusDone })
	run, _ := svc.DB.GetRun(second)
	if run.ContinueRunID == nil || *run.ContinueRunID != first || run.SessionID != "sess-1" {
		t.Fatalf("%+v", run)
	}
	req := fr.Requests[len(fr.Requests)-1]
	if req.ResumeSessionID != "sess-1" || req.Agent != "" || !strings.Contains(req.Prompt, "continuing your own earlier session") || !strings.Contains(req.Prompt, "Mode: FULL REVIEW") {
		t.Fatalf("request must resume the chosen session: %+v", req)
	}
	if chain, _ := svc.DB.ListRunsBySession("sess-1"); len(chain) != 2 || chain[0].ID != first || chain[1].ID != second {
		t.Fatalf("chain: %+v", chain)
	}
	// Retry keeps the chosen context.
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	third, err := svc.Retry(second)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, third) == db.StatusDone })
	if r, _ := svc.DB.GetRun(third); r.ContinueRunID == nil || *r.ContinueRunID != first {
		t.Fatalf("retry must keep the context: %+v", r)
	}

	// An agent that reports no session id leaves nothing to continue.
	fr.Session = "-"
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	fourth, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, fourth) == db.StatusDone })
	if got := svc.Resumable(mustRuns(t, svc, mr.ID), svc.Settings.ProjectRoot); len(got) != 3 {
		t.Fatalf("runs without a session id are not offered: %+v", got)
	}
}

func mustRuns(t *testing.T, svc *Service, mrID int64) []db.RunSummary {
	t.Helper()
	runs, err := svc.DB.ListRunsForMR(mrID)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// «Проверить замечание»: a read-only mini run re-examines one finding and records the outcome on it.
func TestVerifyFinding(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	reviewID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusDone })
	findings, _ := svc.DB.ListFindings(reviewID)

	if _, err := svc.StartVerifyFinding(mr.ID, 999, "", 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown finding: %v", err)
	}
	gl.MRs[43] = testutil.MRPayload(43, "sha-x")
	other, _ := svc.AddMR("!43")
	if _, err := svc.StartVerifyFinding(other.ID, findings[0].ID, "", 0); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("finding of another MR: %v", err)
	}

	fr.Outputs = []map[string]any{{"status": "false_positive", "evidence": "The null check exists three lines above; `src/A.php:7` guards the call.", "severity": "", "suggestion": ""}}
	checkID, err := svc.StartVerifyFinding(mr.ID, findings[0].ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, checkID) == db.StatusDone })
	req := fr.Requests[len(fr.Requests)-1]
	if req.Mode != runner.ModeReadOnly || req.Agent != "mr-review" || !strings.Contains(req.Prompt, "VERIFY ONE FINDING") || !strings.Contains(req.Prompt, "Null deref") || strings.Contains(req.Prompt, "Naming") {
		t.Fatalf("prompt must carry only the checked finding and use the review skill as fallback: %+v", req)
	}
	run, _ := svc.DB.GetRun(checkID)
	if run.Kind != db.KindVerifyFinding || run.FindingID == nil || *run.FindingID != findings[0].ID || run.Verdict != "false_positive" || run.SkillIdentifier != "agent:mr-review" {
		t.Fatalf("%+v", run)
	}
	f, _ := svc.DB.GetFinding(findings[0].ID)
	if f.Status != "false_positive" || f.CheckStatus != "false_positive" || f.CheckRunID == nil || *f.CheckRunID != checkID || !strings.Contains(f.VerifyNote, "guards the call") {
		t.Fatalf("finding must record the check: %+v", f)
	}
	if _, err := svc.StartVerifyFinding(mr.ID, findings[0].ID, "", 0); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Fatalf("closed finding cannot be checked again: %v", err)
	}
	// A confirmed finding stays open; the verdict of the review follows the remaining open findings.
	fr.Outputs = []map[string]any{{"status": "confirmed", "evidence": "Inconsistent naming remains in `src/B.php` (camelCase next to snake_case).", "severity": "INFO", "suggestion": ""}}
	checkID, _ = svc.StartVerifyFinding(mr.ID, findings[1].ID, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, checkID) == db.StatusDone })
	if f, _ := svc.DB.GetFinding(findings[1].ID); f.Status != "open" || f.CheckStatus != "confirmed" {
		t.Fatalf("%+v", f)
	}
	items, _ := svc.DB.ListMRs()
	var item db.MRListItem
	for _, it := range items {
		if it.IID == 42 {
			item = it
		}
	}
	if item.Last == nil || item.Last.Kind != db.KindReviewFull || item.Done == nil || item.Done.Open() != 1 {
		t.Fatalf("finding checks must not become the MR's last run; open findings: %+v", item)
	}
	// What changed since the reviewed SHA (compare API) is cached per SHA pair.
	gl.MRs[42] = testutil.MRPayload(42, "sha-2")
	fresh, _ := svc.RefreshMR(mr.ID)
	if c := svc.Changes(fresh, "sha-1"); c == nil || c.Commits != 2 || c.Additions != 10 {
		t.Fatalf("%+v", c)
	}
	svc.Changes(fresh, "sha-1")
	if n := strings.Count(strings.Join(gl.Calls, "\n"), "compare sha-1..sha-2"); n != 1 {
		t.Fatalf("compare must be cached: %d calls", n)
	}
	if svc.Changes(fresh, "sha-2") != nil {
		t.Fatal("same SHA: nothing changed")
	}
}

// Tasks are tracked in other GitLab projects than the code: issue sync spans every project by default and can be
// narrowed with GITLAB_ISSUE_PROJECTS.
func TestSyncIssuesAcrossProjects(t *testing.T) {
	svc, gl, _ := newService(t)
	other := testutil.IssuePayload(8)
	other["references"] = map[string]any{"full": "group/tracker/eu#8"}
	other["web_url"] = "https://gitlab.example.com/group/tracker/eu/-/issues/8"
	gl.Issues[8] = other
	res, err := svc.SyncIssues()
	if err != nil || res.Synced != 2 || res.Project != "*" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(gl.Calls) == 0 || gl.Calls[len(gl.Calls)-1] != "issues project=" {
		t.Fatalf("issues must be listed across projects: %v", gl.Calls)
	}
	issues, _ := svc.DB.ListIssues()
	projects := map[string]bool{}
	for _, i := range issues {
		projects[i.ProjectPath] = true
	}
	if !projects["group/tracker/eu"] || !projects["group/sub/project"] {
		t.Fatalf("%v", projects)
	}

	settings, _ := testutil.Settings(t)
	settings.GitLabIssueProjects = []string{"group/tracker/eu"}
	svc2, gl2, _ := newServiceWith(t, settings)
	gl2.Issues[8] = other
	res, _ = svc2.SyncIssues()
	if res.Synced != 1 || res.Project != "group/tracker/eu" {
		t.Fatalf("single project goes into the API filter: %+v", res)
	}
	settings.GitLabIssueProjects = []string{"group/tracker/eu", "group/sub/project"}
	svc3, gl3, _ := newServiceWith(t, settings)
	gl3.Issues[8] = other
	gl3.Issues[9] = map[string]any{"iid": 9.0, "title": "elsewhere", "web_url": "https://gitlab.example.com/x/y/-/issues/9", "references": map[string]any{"full": "x/y#9"}, "author": map[string]any{"username": "bob"}, "state": "opened", "labels": []any{}}
	res, _ = svc3.SyncIssues()
	if res.Synced != 2 {
		t.Fatalf("several projects are filtered client-side: %+v", res)
	}
}

// Skill overrides from the UI: another project skill name, or dashboard-written instructions that replace the skill.
func TestSkillSettingsFromUI(t *testing.T) {
	svc, _, fr := newService(t)
	if svc.SkillFor(db.KindPlan) != nil {
		t.Fatal("fixture has no plan skill")
	}
	if err := svc.SaveSkillSetting(db.KindPlan, "", true, ""); err == nil {
		t.Fatal("empty custom instructions must be refused")
	}
	if err := svc.SaveSkillSetting("nope", "", false, ""); err == nil {
		t.Fatal("unknown action must be refused")
	}
	if err := svc.SaveSkillSetting(db.KindPlan, "", true, "Plan in three phases. Mention the DB migrations first."); err != nil {
		t.Fatal(err)
	}
	sk := svc.SkillFor(db.KindPlan)
	if sk == nil || sk.Kind != skill.KindCustom || sk.Identifier() != "custom:plan" {
		t.Fatalf("%+v", sk)
	}
	settings := svc.SkillSettings()
	if !settings[db.KindPlan].Custom || settings[db.KindPlan].Text == "" || settings[db.KindReviewFull].Custom {
		t.Fatalf("%+v", settings)
	}
	issue, _ := svc.AddIssue("#7")
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	runID, err := svc.StartPlan(issue.ID, "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	req := fr.Requests[len(fr.Requests)-1]
	if req.Agent != "" || !strings.Contains(req.Prompt, "--- developer instructions ---") || !strings.Contains(req.Prompt, "Mention the DB migrations first.") || !strings.Contains(req.Prompt, "READ-ONLY") {
		t.Fatalf("custom instructions must be in the prompt, constraints kept: %+v", req)
	}
	if run, _ := svc.DB.GetRun(runID); run.SkillIdentifier != "custom:plan" {
		t.Fatalf("%+v", run)
	}
	// Pointing the full review at another project skill by name, from the UI, wins over the .env default.
	testutil.AddProjectSkill(t, svc.Settings.ProjectRoot, "php-style", "PHP style rules")
	if err := svc.SaveSkillSetting(db.KindReviewFull, "php-style", false, ""); err != nil {
		t.Fatal(err)
	}
	if sk := svc.SkillFor(db.KindReviewFull); sk == nil || sk.Name != "php-style" {
		t.Fatalf("%+v", sk)
	}
	if err := svc.SaveSkillSetting(db.KindReviewFull, "", false, ""); err != nil {
		t.Fatal(err)
	}
	if sk := svc.SkillFor(db.KindReviewFull); sk == nil || sk.Name != "mr-review" {
		t.Fatalf("clearing the override restores the default: %+v", sk)
	}
	// Overrides survive a restart: a new service on the same database applies them at start.
	svc2 := New(svc.Settings, svc.DB, svc.GitLab, []runner.Runner{fr})
	if sk := svc2.SkillFor(db.KindPlan); sk == nil || sk.Kind != skill.KindCustom {
		t.Fatalf("%+v", sk)
	}
}

// «Проверить на стенде»: needs a stand-access skill in the project; runs in the MR worktree in edit mode with the
// stand skill named in the prompt; the report is stored like other edit runs.
func TestStandTest(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	if _, err := svc.StartStandTest(mr.ID, "", "", 0); err == nil || !strings.Contains(err.Error(), "no stand skill") {
		t.Fatalf("without a stand skill the run must be refused: %v", err)
	}
	testutil.AddProjectSkill(t, svc.Settings.ProjectRoot, "gilmanov-stand", "Подключение к dev-стенду по SSH для проверки изменений")
	if sk := svc.StandSkill(); sk == nil || sk.Name != "gilmanov-stand" {
		t.Fatalf("stand skill must be detected by description: %+v", sk)
	}
	fr.Outputs = []map[string]any{{"summary": "Deployed 2 files, tests green, emulation script ran without errors on the stand.", "deployed": []any{"src/A.php"},
		"tests": "phpunit ok", "script_path": "scripts/onerun/mr_42_check.php", "script_output": "OK", "problems": []any{}, "changes": []any{}, "todo": []any{}, "commit_message": ""}}
	runID, err := svc.StartStandTest(mr.ID, "", "mock the payment API", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) != db.StatusQueued && status(svc, runID) != db.StatusRunning })
	run, _ := svc.DB.GetRun(runID)
	if run.Status != db.StatusDone {
		t.Fatalf("%s: %s", run.Status, run.Error)
	}
	req := fr.Requests[len(fr.Requests)-1]
	if req.Mode != runner.ModeEdit || req.Dir != svc.Worktrees.Path("feature") || req.Agent != "" {
		t.Fatalf("%+v", req)
	}
	for _, want := range []string{"Mode: STAND TEST", "`gilmanov-stand`", "mock the payment API", "git diff --name-status origin/develop...HEAD", "Never run `git push`"} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if run.Kind != db.KindStandTest || run.Branch != "feature" || !run.IsEdit() || !strings.Contains(run.Summary, "Deployed 2 files") {
		t.Fatalf("%+v", run)
	}
	// STAND_SKILL pins the skill by name; an unknown name means "no stand skill".
	svc.Settings.StandSkill = "nope"
	if svc.StandSkill() != nil {
		t.Fatal("unknown STAND_SKILL must not fall back to detection")
	}
}

// Phase 4: the agent stops with questions instead of guessing; the developer answers in the dashboard and the same
// session continues. Counters accumulate over both stages; a restart keeps the waiting run.
func TestAgentQuestionsAndAnswers(t *testing.T) {
	svc, _, fr := newService(t)
	issue, _ := svc.AddIssue("#7")
	fr.Outputs = []map[string]any{{"summary": "", "steps": []any{}, "files": []any{}, "risks": []any{}, "questions": []any{}, "estimate": "",
		"ask": []any{map[string]any{"question": "Which currency for the fee?", "options": []any{"USD", "EUR"}, "why": "The ticket does not say"},
			map[string]any{"question": "Keep the old endpoint?", "options": []any{}, "why": "Clients may still call it"}}}}
	runID, err := svc.StartPlan(issue.ID, "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusWaiting })
	run, _ := svc.DB.GetRun(runID)
	if !strings.Contains(run.Progress, "2 вопрос") || run.SessionID != "sess-1" || run.FinishedAt != "" || run.InputTokens != 1000 {
		t.Fatalf("%+v", run)
	}
	if !strings.Contains(fr.Requests[0].Prompt, "do NOT guess: stop and return the structured result with `ask` filled") {
		t.Fatal("prompt must carry the questions rule")
	}
	pending, _ := svc.DB.PendingQuestions(runID)
	if len(pending) != 2 || pending[0].Options[1] != "EUR" || pending[1].Why == "" {
		t.Fatalf("%+v", pending)
	}
	if ids, _ := svc.DB.RunsWaitingForAnswers(); len(ids) != 1 || ids[0] != runID {
		t.Fatalf("%v", ids)
	}
	// A restart must not fail a run that only waits for the developer.
	if n, _ := svc.DB.FailStaleRuns("restart"); n != 0 || status(svc, runID) != db.StatusWaiting {
		t.Fatal("waiting-for-answers run must survive a restart")
	}
	if err := svc.Answer(runID, map[int64]string{pending[0].ID: "EUR"}); err == nil || !strings.Contains(err.Error(), "has no answer") {
		t.Fatalf("every question needs an answer: %v", err)
	}
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	if err := svc.Answer(runID, map[int64]string{pending[0].ID: "EUR", pending[1].ID: "Yes, keep it for one release"}); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	req := fr.Requests[len(fr.Requests)-1]
	if req.ResumeSessionID != "sess-1" || req.Agent != "" || !strings.Contains(req.Prompt, "A: EUR") || !strings.Contains(req.Prompt, "keep it for one release") || len(req.Schema) == 0 {
		t.Fatalf("continuation must resume the session with the answers and the schema: %+v", req)
	}
	run, _ = svc.DB.GetRun(runID)
	if run.Summary != "Plan summary" || run.InputTokens != 2000 || run.CostUSD < 0.19 || run.Progress != "" {
		t.Fatalf("result stored, counters accumulated: %+v", run)
	}
	if ids, _ := svc.DB.RunsWaitingForAnswers(); len(ids) != 0 {
		t.Fatal("nothing waits any more")
	}
	if err := svc.Answer(runID, nil); err == nil {
		t.Fatal("a finished run takes no answers")
	}

	// Cancelling a run that waits for answers works without a process to stop.
	fr.Outputs = []map[string]any{{"summary": "", "steps": []any{}, "files": []any{}, "risks": []any{}, "questions": []any{}, "estimate": "",
		"ask": []any{map[string]any{"question": "Q?", "options": []any{}, "why": ""}}}}
	second, _ := svc.StartPlan(issue.ID, "", "", 0, "")
	testutil.WaitFor(t, func() bool { return status(svc, second) == db.StatusWaiting })
	if !svc.Cancel(second) || status(svc, second) != db.StatusCancelled {
		t.Fatalf("%s", status(svc, second))
	}
	// Without a session id the questions cannot be continued: the run fails with a clear message.
	fr.Session = "-"
	fr.Outputs = []map[string]any{{"summary": "", "steps": []any{}, "files": []any{}, "risks": []any{}, "questions": []any{}, "estimate": "",
		"ask": []any{map[string]any{"question": "Q?", "options": []any{}, "why": ""}}}}
	third, _ := svc.StartPlan(issue.ID, "", "", 0, "")
	testutil.WaitFor(t, func() bool { return status(svc, third) == db.StatusFailed })
	if r, _ := svc.DB.GetRun(third); !strings.Contains(r.Error, "no session id") {
		t.Fatalf("%+v", r)
	}
}

// Structural progress: tool calls of a run are stored as events and grouped into phases; edit runs start with the workspace phase.
func TestRunTimeline(t *testing.T) {
	svc, _, fr := newService(t)
	issue, _ := svc.AddIssue("#7")
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	runID, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	tl := svc.DB.TimelineFor(runID)
	labels := []string{}
	for _, p := range tl {
		labels = append(labels, p.Phase)
	}
	if strings.Join(labels, ",") != "workspace,analysis,commands,implement,tests" || !tl[4].Current {
		t.Fatalf("%v", labels)
	}
}

// Phase 5: «Исследовать» has a bug mode (root cause instead of a plan); the MR draft is built from the report.
func TestBugModeAndMRDraft(t *testing.T) {
	svc, gl, fr := newService(t)
	bug := testutil.IssuePayload(9)
	bug["title"] = "Fix crash when saving a profile"
	bug["labels"] = []any{"Bug"}
	gl.Issues[9] = bug
	issue, _ := svc.AddIssue("#9")
	if !issue.LooksLikeBug() {
		t.Fatal("bug heuristics")
	}
	if _, err := svc.StartPlan(issue.ID, "", "", 0, "weird"); err == nil {
		t.Fatal("unknown mode must be refused")
	}
	fr.Outputs = []map[string]any{{"summary": "Null profile id.", "expected": "saves", "actual": "500", "reproduction": []any{"open profile", "save"}, "path": []any{"ProfileController::save"},
		"root_cause": "`Profile::save()` dereferences `$this->id` before it is set", "evidence": []any{"src/Profile.php:42"}, "fix": "guard the id", "risks": []any{}, "questions": []any{}, "estimate": "S", "ask": []any{}}}
	runID, err := svc.StartPlan(issue.ID, "", "check the migration too", 0, "bug")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, runID) == db.StatusDone })
	run, _ := svc.DB.GetRun(runID)
	req := fr.Requests[len(fr.Requests)-1]
	if run.Mode != "bug" || !strings.Contains(req.Prompt, "Mode: BUG ANALYSIS") || !strings.Contains(string(req.Schema), "root_cause") || !strings.Contains(req.Prompt, "check the migration too") {
		t.Fatalf("%+v", run)
	}
	fr.Outputs = []map[string]any{{"summary": "Null profile id.", "expected": "saves", "actual": "500", "reproduction": []any{}, "path": []any{},
		"root_cause": "x", "evidence": []any{}, "fix": "y", "risks": []any{}, "questions": []any{}, "estimate": "S", "ask": []any{}}}
	retry, _ := svc.Retry(runID)
	testutil.WaitFor(t, func() bool { return status(svc, retry) == db.StatusDone })
	if r, _ := svc.DB.GetRun(retry); r.Mode != "bug" {
		t.Fatal("retry keeps the mode")
	}

	// Implementation report → MR draft with the sections the team expects.
	fr.Outputs = []map[string]any{{"summary": "Guarded the id.", "changes": []any{map[string]any{"path": "src/Profile.php", "description": "guard"}}, "tests": "phpunit: 3 passed",
		"todo": []any{"backfill old rows"}, "self_review": "Checked null paths; nothing else.", "commit_message": "group/sub/project#9 guard id", "ask": []any{}}}
	implID, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	impl, _ := svc.DB.GetRun(implID)
	if !strings.Contains(fr.Requests[len(fr.Requests)-1].Prompt, "(4) SELF-REVIEW") {
		t.Fatal("implement prompt must describe the phases")
	}
	draft := svc.MRDraftFor(impl, issue)
	for _, want := range []string{"group/sub/project#9 Fix crash when saving a profile", "Closes https://gitlab.example.com/group/sub/project/-/issues/9", "## Что сделано", "Guarded the id.", "## Проверки", "phpunit: 3 passed", "## Известные ограничения", "- backfill old rows", "## Self-review", "Checked null paths"} {
		if !strings.Contains(draft.Title+"\n"+draft.Description, want) {
			t.Fatalf("draft missing %q:\n%s", want, draft.Description)
		}
	}
	state := svc.Worktree(impl)
	if !state.Exists || state.Clean() || state.Pushed() {
		t.Fatalf("fresh workspace has uncommitted changes and no upstream: %+v", state)
	}
	if _, err := svc.Commit(implID, "guard id"); err != nil {
		t.Fatal(err)
	}
	if state := svc.Worktree(impl); !state.Clean() || state.Pushed() {
		t.Fatalf("%+v", state)
	}
}
