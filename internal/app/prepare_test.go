package app

import (
	"strings"
	"testing"
	"time"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// GitLab unreachable: an implementation starts from the local origin/develop within seconds and the run page
// carries the warning that the base may be stale.
func TestImplementOfflineStartsFromLocalBase(t *testing.T) {
	svc, _, fr := newService(t)
	issue, err := svc.AddIssue("#7")
	if err != nil {
		t.Fatal(err)
	}
	testutil.DeadRemote(t, svc.Settings.ProjectRoot)
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	start := time.Now()
	id, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("offline implement took %s", elapsed)
	}
	warnings := svc.DB.RunWarnings(id)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "локальной копии origin/develop") {
		t.Fatalf("warning expected: %v", warnings)
	}
}

// The run is "running" with started_at and the note «готовлю код» while git works; stopping it then ends it as
// cancelled and the agent is never started.
func TestCancelWhilePreparingCode(t *testing.T) {
	svc, _, fr := newService(t)
	issue, err := svc.AddIssue("#7")
	if err != nil {
		t.Fatal(err)
	}
	testutil.HangingRemote(t, svc.Settings.ProjectRoot)
	id, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool {
		run, _ := svc.DB.GetRun(id)
		return run.Status == db.StatusRunning && run.Progress == progressPreparing && run.StartedAt != ""
	})
	if !svc.Cancel(id) {
		t.Fatal("cancel refused")
	}
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusCancelled })
	run, _ := svc.DB.GetRun(id)
	fr.Lock()
	defer fr.Unlock()
	if len(fr.Requests) != 0 || !strings.Contains(run.Error, "preparing") {
		t.Fatalf("agent must not start: %d requests, error %q", len(fr.Requests), run.Error)
	}
}

// The session id reaches the run while the agent works; a stopped run keeps it and the tokens used so far, and
// is offered for continuation.
func TestCancelledRunKeepsSessionAndTokens(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Block = make(chan struct{})
	defer close(fr.Block)
	id, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool {
		run, _ := svc.DB.GetRun(id)
		return run.Status == db.StatusRunning && run.SessionID == "sess-1" && run.InputTokens == 100
	})
	svc.Cancel(id)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusCancelled })
	run, _ := svc.DB.GetRun(id)
	if run.SessionID != "sess-1" || run.TotalTokens() != 120 {
		t.Fatalf("cancelled run must keep session and tokens: %+v", run)
	}
	runs, _ := svc.DB.ListRunsForMR(mr.ID)
	if len(svc.Resumable(runs, svc.Settings.ProjectRoot)) != 1 {
		t.Fatal("the stopped session must be resumable")
	}
}
