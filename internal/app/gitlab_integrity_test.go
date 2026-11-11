package app

import (
	"strings"
	"testing"
)

// A failed discussions/approvals call keeps the stored counters (a hiccup is not "0 threads") and shows up in
// the sync indicator.
func TestSyncKeepsCountersOnErrors(t *testing.T) {
	svc, gl, _ := newService(t)
	mr, err := svc.AddMR("!42")
	if err != nil {
		t.Fatal(err)
	}
	if mr.Unresolved != 2 || mr.ApprovalsGiven != 1 || mr.ApprovalsRequired != 2 || mr.MyRoles == "" {
		t.Fatalf("precondition: %+v", mr)
	}
	gl.FailUnresolved, gl.FailApprovals = true, true
	res, err := svc.SyncMRs()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := svc.DB.GetMR(mr.ID)
	if after.Unresolved != 2 || after.ApprovalsGiven != 1 || after.ApprovalsRequired != 2 {
		t.Fatalf("errors must not change the counters: %+v", after)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "!42") || !strings.Contains(res.Problems[0], "обсуждения") {
		t.Fatalf("problems: %v", res.Problems)
	}
	svc.autoSyncOnce()
	if _, msg := svc.AutoSyncState(); !strings.Contains(msg, "не все MR обновились (1)") {
		t.Fatalf("sync indicator: %q", msg)
	}
	// GitLab answers again: fresh values are taken.
	gl.FailUnresolved, gl.FailApprovals = false, false
	gl.Unresolved[42] = 5
	if _, err := svc.SyncMRs(); err != nil {
		t.Fatal(err)
	}
	if after, _ := svc.DB.GetMR(mr.ID); after.Unresolved != 5 {
		t.Fatalf("%+v", after)
	}
}

// A failed username lookup is retried later instead of being cached as "nobody" for the life of the process.
func TestCurrentUserCachesOnlySuccess(t *testing.T) {
	svc, gl, _ := newService(t)
	gl.FailUser = true
	if svc.CurrentUser() != "" {
		t.Fatal("no user while GitLab fails")
	}
	svc.CurrentUser() // within the retry pause: not asked again
	if gl.UserCalls != 1 {
		t.Fatalf("calls: %d", gl.UserCalls)
	}
	gl.FailUser = false
	svc.userTried = svc.userTried.Add(-userRetry)
	if svc.CurrentUser() != "alice" {
		t.Fatal("the lookup must be retried after the pause")
	}
	svc.CurrentUser()
	if gl.UserCalls != 2 {
		t.Fatalf("a known user is cached: %d calls", gl.UserCalls)
	}
}
