package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// Scenario «без сети» (wave 0): the VPN goes off while the dashboard runs. Nothing hangs, nothing is silently
// wrong: the header says GitLab is unreachable, the sync keeps the stored data, a review of a commit that never
// reached this machine fails with a clear reason, and an implementation still starts from the local base.
func TestScenarioOffline(t *testing.T) {
	settings, _ := testutil.Settings(t)
	settings.ReviewWorktree = true
	svc, gl, fr := newServiceWith(t, settings)
	mr, err := svc.AddMR("!42")
	if err != nil {
		t.Fatal(err)
	}
	issue, err := svc.AddIssue("#7")
	if err != nil {
		t.Fatal(err)
	}

	// The VPN goes off: git cannot fetch, the API does not answer, TCP dials fail.
	testutil.DeadRemote(t, svc.Settings.ProjectRoot)
	gl.FailUnresolved, gl.FailApprovals = true, true
	svc.Settings.GitLabReachCheck = true
	svc.reach.dial = func(addr string, timeout time.Duration) error { return errors.New("connect: no route to host") }

	svc.autoSyncOnce()
	if _, msg := svc.AutoSyncState(); !strings.HasPrefix(msg, OfflineHint) || svc.GitLabOffline() == "" {
		t.Fatalf("offline must be reported: %q", msg)
	}
	if after, _ := svc.DB.GetMR(mr.ID); after.Unresolved != mr.Unresolved || after.ApprovalsGiven != mr.ApprovalsGiven {
		t.Fatalf("stored data must survive: %+v", after)
	}

	// A review of a head commit this machine has never seen: failed, with the reason, without starting the agent.
	start := time.Now()
	reviewID, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusFailed })
	review, _ := svc.DB.GetRun(reviewID)
	if !strings.Contains(review.Error, "VPN") || len(fr.Requests) != 0 {
		t.Fatalf("review must fail clearly before the agent: %q (%d agent calls)", review.Error, len(fr.Requests))
	}

	// An implementation starts from the local origin/develop with a warning.
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	implID, err := svc.StartImplement(issue.ID, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, implID) == db.StatusDone })
	if warnings := svc.DB.RunWarnings(implID); len(warnings) == 0 {
		t.Fatal("the stale base must be pointed out")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("offline scenario took %s", elapsed)
	}
}
