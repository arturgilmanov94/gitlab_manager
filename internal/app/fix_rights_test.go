package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// Runs that change an MR branch (fix comments, fix findings, fix CI) are for its author or an assignee only;
// a reviewer is refused by the server, whatever the UI showed.
func TestFixRightsAuthorOrAssignee(t *testing.T) {
	svc, gl, fr := newService(t)
	// !43 by bob, alice is an assignee: allowed. !44 by bob, alice only reviews: refused.
	assigned := testutil.MRPayload(43, "sha-43")
	assigned["author"] = map[string]any{"username": "bob"}
	gl.MRs[43] = assigned
	reviewing := testutil.MRPayload(44, "sha-44")
	reviewing["author"] = map[string]any{"username": "bob"}
	reviewing["assignees"] = []any{}
	reviewing["reviewers"] = []any{map[string]any{"username": "alice"}}
	gl.MRs[44] = reviewing
	gl.Unresolved[43], gl.Unresolved[44] = 1, 1
	mine, _ := svc.AddMR("!42")
	asAssignee, _ := svc.AddMR("!43")
	asReviewer, _ := svc.AddMR("!44")

	if !mine.CanFix("alice") || !asAssignee.CanFix("alice") || asReviewer.CanFix("alice") || mine.CanFix("") {
		t.Fatalf("CanFix: %v %v %v", mine.CanFix("alice"), asAssignee.CanFix("alice"), asReviewer.CanFix("alice"))
	}
	refused := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "only the author or an assignee")
	}
	if _, err := svc.StartFixComments(asReviewer.ID, "", "", 0); !refused(err) {
		t.Fatalf("fix comments: %v", err)
	}
	if _, err := svc.StartCIFix(asReviewer.ID, "", 0, 0); !refused(err) {
		t.Fatalf("ci fix: %v", err)
	}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-44")}
	reviewID, _ := svc.StartReview(asReviewer.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, reviewID) == db.StatusDone })
	findings, _ := svc.DB.ListFindings(reviewID)
	if _, err := svc.StartFixFindings(asReviewer.ID, "", "", []int64{findings[0].ID}, nil, 0); !refused(err) {
		t.Fatalf("fix findings: %v", err)
	}
	// The assignee may.
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	id, err := svc.StartFixComments(asAssignee.ID, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { s := status(svc, id); return s != db.StatusQueued && s != db.StatusRunning })
}
