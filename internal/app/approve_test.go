package app

import (
	"strings"
	"testing"

	"mr-review/internal/db"
)

// Approve goes to GitLab on the head the developer saw; an MR that moved on is refused before GitLab is asked.
// Approved MRs leave the main list (as before), unapprove brings them back. People and readiness are stored.
func TestApproveAndDetails(t *testing.T) {
	svc, gl, _ := newService(t)
	gl.MRs[42]["author"] = map[string]any{"username": "bob"}
	gl.MRs[42]["reviewers"] = []any{map[string]any{"username": "alice"}, map[string]any{"username": "dave"}}
	gl.MRs[42]["detailed_merge_status"] = "not_approved"
	mr, _ := svc.AddMR("!42")
	details := svc.DB.MRDetailsOf(mr.ID)
	if strings.Join(details.ApprovedBy, ",") != "carol" || strings.Join(details.Reviewers, ",") != "alice,dave" || strings.Join(details.Assignees, ",") != "alice" {
		t.Fatalf("%+v", details)
	}
	if text, tone := MergeReadiness(details); text != "ждёт approvals" || tone != "warning" {
		t.Fatal(text, tone)
	}
	if text, tone := MergeReadiness(db.MRDetails{HasConflicts: true, MergeStatus: "mergeable"}); tone != "danger" || !strings.Contains(text, "конфликты") {
		t.Fatal(text, tone)
	}
	if _, err := svc.ApproveMR(mr.ID, true, "sha-0"); err == nil || !strings.Contains(err.Error(), "новые коммиты") || len(gl.Writes) != 0 {
		t.Fatalf("%v %v", err, gl.Writes)
	}
	gl.FailWrite = true
	if _, err := svc.ApproveMR(mr.ID, true, "sha-1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("%v", err)
	}
	gl.FailWrite = false
	got, err := svc.ApproveMR(mr.ID, true, "sha-1")
	if err != nil || !got.ApprovedByMe || got.ApprovalsGiven != 2 || got.Relevant() || gl.Writes[0] != "approve 42 sha-1" {
		t.Fatalf("%+v %v %v", got, err, gl.Writes)
	}
	if by := svc.DB.MRDetailsOf(mr.ID).ApprovedBy; strings.Join(by, ",") != "carol,alice" {
		t.Fatalf("%v", by)
	}
	// Approvals unreadable on the next sync: the names stay.
	gl.FailApprovals = true
	_, _ = svc.RefreshMR(mr.ID)
	if by := svc.DB.MRDetailsOf(mr.ID).ApprovedBy; strings.Join(by, ",") != "carol,alice" {
		t.Fatalf("names lost on a failed approvals call: %v", by)
	}
	gl.FailApprovals = false
	if got, err = svc.ApproveMR(mr.ID, false, ""); err != nil || got.ApprovedByMe || !got.Relevant() {
		t.Fatalf("%+v %v", got, err)
	}
}

// Retry restarts the failed head pipeline and the MR shows it running; a pipeline that did not fail is not retried.
func TestRetryPipeline(t *testing.T) {
	svc, gl, _ := newService(t)
	mr, _ := svc.AddMR("!42")
	got, err := svc.RetryPipeline(mr.ID)
	if err != nil || got.PipelineStatus != "running" || gl.Writes[0] != "retry 9001" {
		t.Fatalf("%+v %v %v", got, err, gl.Writes)
	}
	if _, err := svc.RetryPipeline(mr.ID); err == nil || !strings.Contains(err.Error(), "не упал") || len(gl.Writes) != 1 {
		t.Fatalf("%v %v", err, gl.Writes)
	}
}
