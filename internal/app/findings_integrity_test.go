package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// Verify carries a finding's GitLab comment link and agent check over to the newer head; a finding the developer
// rejected is neither re-checked nor lost, and every later review is told not to raise it again.
func TestVerifyKeepsFindingStateAndRejections(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	reviewID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusDone })
	findings, _ := svc.DB.ListFindings(reviewID)
	high, low := findings[0], findings[1]
	_ = svc.DB.SetFindingGitLabNote(high.ID, "https://gitlab.example.com/note/1")
	_ = svc.DB.SetFindingCheck(high.ID, "confirmed", "real", reviewID)
	if err := svc.DB.SetFindingStatus(low.ID, "false_positive"); err != nil {
		t.Fatal(err)
	}

	gl.MRs[42] = testutil.MRPayload(42, "sha-2")
	fr.Outputs = []map[string]any{testutil.VerifyOutput(high.ID, low.ID, "sha-2")}
	verifyID, err := svc.StartReview(mr.ID, db.KindReviewVerify, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, verifyID) == db.StatusDone })
	verify, _ := svc.DB.GetRun(verifyID)
	if strings.Contains(verify.Prompt, `"title": "Naming"`) {
		t.Fatal("a rejected finding must not be re-checked")
	}
	if !strings.Contains(verify.Prompt, "already REJECTED") || !strings.Contains(verify.Prompt, "Naming at src/B.php (false positive)") {
		t.Fatalf("the rejected finding must be listed: %s", verify.Prompt)
	}
	carried, _ := svc.DB.ListFindings(verifyID)
	byTitle := map[string]db.Finding{}
	for _, f := range carried {
		byTitle[f.Title] = f
	}
	if f := byTitle["Null deref"]; f.GitLabNoteURL != "https://gitlab.example.com/note/1" || f.CheckStatus != "confirmed" || f.Status != "fixed" {
		t.Fatalf("GitLab link and check must travel with the finding: %+v", f)
	}
	if f := byTitle["Naming"]; f.Status != "false_positive" {
		t.Fatalf("the rejection must stand: %+v", f)
	}

	// A new full review hears about the rejection too (once, although two runs hold the finding).
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-2")}
	fullID, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, fullID) == db.StatusDone })
	full, _ := svc.DB.GetRun(fullID)
	if strings.Count(full.Prompt, "Naming at src/B.php") != 1 {
		t.Fatalf("rejected list in a new review: %s", full.Prompt)
	}
}
