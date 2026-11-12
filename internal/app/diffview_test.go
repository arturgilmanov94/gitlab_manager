package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/testutil"
)

// The diff of an MR: the whole MR by default; «since my view» and «since the AI review» appear once there is a
// commit to count from and something arrived after it. Findings and threads sit on their lines.
func TestMRDiffModesAndMarks(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	view, err := svc.MRDiff(mr.ID, "")
	if err != nil || view.Mode != DiffAll || len(view.Files) != 1 || view.Files[0].Path != "src/A.php" || view.Additions != 2 || view.Deletions != 1 {
		t.Fatalf("%+v %v", view, err)
	}
	if view.Modes[1].Available || view.Modes[2].Available || !strings.Contains(view.Modes[1].Hint, "Просмотрено") {
		t.Fatalf("%+v", view.Modes)
	}
	if v, _ := svc.MRDiff(mr.ID, DiffSeen); v.Note == "" || len(v.Files) != 0 {
		t.Fatalf("an unavailable mode explains itself: %+v", v)
	}

	// A review with a finding on a line the MR added (src/A.php:2) and one outside the diff; threads at 12, 13 and 3.
	out := testutil.FullReviewOutput("sha-1")
	out["findings"] = []any{
		map[string]any{"severity": "HIGH", "category": "bug", "file": "src/A.php", "line": 2.0, "title": "Null deref", "description": "d", "suggestion": ""},
		map[string]any{"severity": "LOW", "category": "style", "file": "src/A.php", "line": 90.0, "title": "Naming", "description": "d", "suggestion": ""},
	}
	fr.Outputs = []map[string]any{out}
	id, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	body, err := svc.MRDiffFile(mr.ID, DiffAll, 0)
	if err != nil || len(body.Rows) != 5 || body.Rows[0].Kind != "hunk" || body.Rows[1].Kind != "del" || body.Rows[1].Old != 1 || body.Rows[3].Kind != "add" || body.Rows[3].New != 2 || body.Rows[4].Kind != "ctx" {
		t.Fatalf("%+v %v", body, err)
	}
	if marks := body.Rows[3].Marks; len(marks) != 1 || marks[0].Kind != "finding" || marks[0].Text != "Null deref" {
		t.Fatalf("the finding sits on the added line 2: %+v", body.Rows)
	}
	if marks := body.Rows[4].Marks; len(marks) != 1 || marks[0].Kind != "thread" || marks[0].Status != "resolved" {
		t.Fatalf("the resolved thread sits on the context line 3: %+v", body.Rows[4])
	}
	if len(body.Loose) != 3 || !strings.Contains(body.Loose[0].Text, "строка 90") {
		t.Fatalf("marks outside the shown lines are listed above the file: %+v", body.Loose)
	}
	if v, _ := svc.MRDiff(mr.ID, DiffAll); v.Files[0].Marks != 5 {
		t.Fatalf("%+v", v.Files[0])
	}

	// Seen at sha-1, then a new head: both ranges start at sha-1 and come from the compare API.
	_ = svc.SetMRSeen(mr.ID, true)
	gl.MRs[42] = testutil.MRPayload(42, "sha-2")
	mr, _ = svc.RefreshMR(mr.ID)
	view, _ = svc.MRDiff(mr.ID, DiffSeen)
	if view.From != "sha-1" || view.To != "sha-2" || len(view.Files) != 1 || view.Files[0].Path != "src/B.php" || !view.Modes[2].Available {
		t.Fatalf("%+v", view)
	}
	if body, err = svc.MRDiffFile(mr.ID, DiffReview, 0); err != nil || body.Rows[2].Text != "since sha-1" || body.Rows[2].New != 11 {
		t.Fatalf("%+v %v", body, err)
	}
	if _, err := svc.MRDiffFile(mr.ID, DiffReview, 7); err == nil {
		t.Fatal("a file index out of range is an error, not a panic")
	}
	if n := countCalls(gl, "compare-diffs sha-1..sha-2"); n != 1 {
		t.Fatalf("a range is fetched once and cached: %d", n)
	}
}
