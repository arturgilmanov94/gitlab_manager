package app

import (
	"encoding/json"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/runner"
	"mr-review/internal/testutil"
)

func bashPrompt(command string) runner.PermissionRequest {
	input, _ := json.Marshal(map[string]string{"command": command})
	return runner.PermissionRequest{ToolName: "Bash", Description: command, Input: input, Reason: "not in the allow list"}
}

// A read-only run runs read-only shell commands without asking; anything else is still asked.
func TestReadOnlyCommandsAllowedWithoutAsking(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Ask = []runner.PermissionRequest{bashPrompt("git log --oneline | head -5 && glab api projects/1/merge_requests/2")}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	if approvals, _ := svc.DB.ListApprovals(id); len(approvals) != 0 || !fr.Decisions[0].Allow {
		t.Fatalf("read-only command must pass without a prompt: %+v %+v", approvals, fr.Decisions)
	}

	fr.Ask = []runner.PermissionRequest{bashPrompt("git push origin HEAD")}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ = svc.StartReview(mr.ID, db.KindReviewQuick, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusWaiting })
	pending, _ := svc.DB.PendingApproval(id)
	if pending == nil || BashCommand(*pending) != "git push origin HEAD" {
		t.Fatalf("a write must be asked: %+v", pending)
	}
	_ = svc.Decide(pending.ID, "deny", "no")
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
}

// «Разрешать такие до конца запуска»: the next call of the same kind passes without a prompt, another kind is
// still asked.
func TestAllowSimilarUntilEndOfRun(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	fr.Ask = []runner.PermissionRequest{bashPrompt("php bin/console cache:clear"), bashPrompt("php bin/console lint:yaml"), bashPrompt("make build")}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusWaiting })
	first, _ := svc.DB.PendingApproval(id)
	if SimilarRule(*first) != "Bash: php bin/console" {
		t.Fatalf("rule: %q", SimilarRule(*first))
	}
	if err := svc.Decide(first.ID, "allow_run", ""); err != nil {
		t.Fatal(err)
	}
	// The second php bin/console passes by the rule; make build is asked.
	testutil.WaitFor(t, func() bool {
		p, _ := svc.DB.PendingApproval(id)
		return p != nil && p.ID != first.ID
	})
	next, _ := svc.DB.PendingApproval(id)
	if BashCommand(*next) != "make build" {
		t.Fatalf("another kind must be asked: %+v", next)
	}
	_ = svc.Decide(next.ID, "allow", "")
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	approvals, _ := svc.DB.ListApprovals(id)
	if len(approvals) != 2 || approvals[0].Note == "" {
		t.Fatalf("two prompts, the first noted with its rule: %+v", approvals)
	}
	fr.Lock()
	defer fr.Unlock()
	if len(fr.Decisions) != 3 || !fr.Decisions[1].Allow {
		t.Fatalf("%+v", fr.Decisions)
	}
	if len(svc.runRules) != 0 {
		t.Fatal("rules end with the run")
	}
}
