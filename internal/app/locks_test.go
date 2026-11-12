package app

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// Ten simultaneous clicks start exactly one review of an MR.
func TestConcurrentStartsCreateOneRun(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Block = make(chan struct{})
	defer close(fr.Block)
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0); err == nil {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if started != 1 {
		t.Fatalf("%d runs started", started)
	}
}

// Two edit runs never share a workspace, even for different objects (an MR and a task on one branch).
func TestEditRunsDoNotShareWorkspace(t *testing.T) {
	svc, _, _ := newService(t)
	issue, _ := svc.AddIssue("#7")
	mr, _ := svc.AddMR("!42")
	dir := svc.Worktrees.Path("feature")
	if _, err := svc.DB.CreateRun(db.Run{Kind: db.KindImplement, IssueID: &issue.ID, Runner: "claude", WorkDir: dir, Branch: "feature"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.DB.CreateRun(db.Run{Kind: db.KindFixComments, MRID: &mr.ID, Runner: "claude", WorkDir: dir, Branch: "feature"})
	var active *db.ActiveRunError
	if !errors.As(err, &active) {
		t.Fatalf("a second edit run in the same workspace must be refused: %v", err)
	}
	// Read-only runs in the project root are not edit runs: several may run at once.
	if _, err := svc.DB.CreateRun(db.Run{Kind: db.KindReviewFull, MRID: &mr.ID, Runner: "claude", WorkDir: svc.Settings.ProjectRoot}); err != nil {
		t.Fatal(err)
	}
}

// A follow-up question is a queued run: it can be stopped, and while it holds the agent session no other run may
// resume that session.
func TestAskIsQueuedAndHoldsTheSession(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	reviewID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusDone })

	fr.Block = make(chan struct{})
	askID, err := svc.Ask(reviewID, "why HIGH?")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return svc.sessionUser("sess-1") == askID })
	if _, err := svc.Ask(reviewID, "and?"); err == nil {
		t.Fatal("one question at a time")
	}
	svc.mu.Lock()
	svc.sessions["other-session"] = askID
	svc.mu.Unlock()
	if holder := svc.claimSession("other-session", 999); holder != askID {
		t.Fatal("a session being resumed cannot be claimed by another run")
	}
	svc.releaseSession("other-session", askID)

	if !svc.Cancel(askID) {
		t.Fatal("cancel")
	}
	testutil.WaitFor(t, func() bool { return status(svc, askID) == db.StatusCancelled })
	close(fr.Block)
	msgs, _ := svc.DB.ListMessages(reviewID)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "error" || !strings.Contains(msgs[1].Content, "остановлен") {
		t.Fatalf("%+v", msgs)
	}
	if svc.sessionUser("sess-1") != 0 {
		t.Fatal("the session is released when the question ends")
	}
}
