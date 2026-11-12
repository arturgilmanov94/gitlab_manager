package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/notify"
	"mr-review/internal/testutil"
)

func countCalls(gl *testutil.FakeGitLab, prefix string) int {
	n := 0
	for _, c := range gl.CallsCopy() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// The discussions of an MR are stored with every note; a sync updates them in place, drops the ones GitLab no
// longer has and keeps the counter in step. A background sync does not re-read them while nothing changed.
func TestDiscussionsSyncedAndStored(t *testing.T) {
	svc, gl, _ := newService(t)
	mr, err := svc.AddMR("!42")
	if err != nil || mr.Unresolved != 2 {
		t.Fatalf("%+v %v", mr, err)
	}
	threads := svc.ThreadsOf(mr)
	if len(threads.Open) != 2 || len(threads.Resolved) != 1 || len(threads.Comments) != 1 || threads.SyncedAt == "" {
		t.Fatalf("%+v", threads)
	}
	// alice (the current user) answered d1 last: it does not wait for her; d2 does and goes first.
	if first := threads.Open[0]; first.GitLabID != "d2" || !first.WaitsForMe || threads.Open[1].WaitsForMe || threads.Waiting() != 1 {
		t.Fatalf("%+v", threads.Open)
	}
	d1 := threads.Open[1]
	if len(d1.People()) != 2 || d1.Replies()[0].Body != "because" || d1.URL != mr.WebURL+"#note_100" || d1.File != "src/A.php" || d1.Line != 12 {
		t.Fatalf("%+v", d1)
	}
	keptID := d1.ID

	// GitLab: d1 resolved, d2 gone, a new thread from carol.
	gl.SetThreads(42, []gitlab.Discussion{
		{ID: "d1", Resolvable: true, Resolved: true, ResolvedBy: "bob", File: "src/A.php", Line: 12, Notes: []gitlab.Note{{ID: 100, Author: "bob", Body: "why?"}, {ID: 199, Author: "alice", Body: "because"}}},
		{ID: "d9", Resolvable: true, File: "src/B.php", OldLine: 4, BaseSHA: "base-1", Notes: []gitlab.Note{{ID: 400, Author: "carol", Body: "this was fine"}}},
	})
	if changed, err := svc.RefreshThreads(mr.ID); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	mr, _ = svc.DB.GetMR(mr.ID)
	threads = svc.ThreadsOf(mr)
	if mr.Unresolved != 1 || len(threads.Open) != 1 || threads.Open[0].GitLabID != "d9" || len(threads.Resolved) != 1 || threads.Resolved[0].ID != keptID || threads.Resolved[0].ResolvedBy != "bob" || len(threads.Comments) != 0 {
		t.Fatalf("%d %+v", mr.Unresolved, threads)
	}
	if changed, _ := svc.RefreshThreads(mr.ID); changed {
		t.Fatal("nothing changed in GitLab")
	}
	// A comment on a removed line shows the old file at the base commit.
	code, err := svc.ThreadCode(threads.Open[0].ID)
	if err != nil || code.SHA != "base-1" || code.Line != 4 || !strings.Contains(code.Note, "удалена") {
		t.Fatalf("%+v %v", code, err)
	}

	// Background sync: with an age limit the unchanged MR is not asked for its discussions again…
	svc.Settings.DiscussionsMaxAgeSec = 3600
	before := countCalls(gl, "discussions 42")
	if _, err := svc.SyncMRs(); err != nil {
		t.Fatal(err)
	}
	if got := countCalls(gl, "discussions 42"); got != before {
		t.Fatalf("unchanged MR must not re-read discussions: %d → %d", before, got)
	}
	// …a new comment (user_notes_count) makes it read them, and a failure keeps the stored state.
	gl.MRs[42]["user_notes_count"] = 7.0
	gl.FailUnresolved = true
	res, _ := svc.SyncMRs()
	if mr, _ = svc.DB.GetMR(mr.ID); mr.Unresolved != 1 || len(res.Problems) != 1 || len(svc.ThreadsOf(mr).Open) != 1 {
		t.Fatalf("%d %v", mr.Unresolved, res.Problems)
	}
	gl.FailUnresolved = false
	gl.SetThreads(42, nil)
	gl.MRs[42]["user_notes_count"] = 8.0
	_, _ = svc.SyncMRs()
	if mr, _ = svc.DB.GetMR(mr.ID); mr.Unresolved != 0 || svc.ThreadsOf(mr).Total() != 0 {
		t.Fatalf("%d", mr.Unresolved)
	}
}

// A review gets the threads as they are now, with ids and replies, and its assessment lands on the live thread:
// by the id it returned, or by author and place when it returned none.
func TestReviewAssessesLiveThreads(t *testing.T) {
	svc, gl, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	out := testutil.FullReviewOutput("sha-1")
	out["unresolved_discussions"] = []any{
		map[string]any{"discussion_id": "d2", "author": "someone", "file": "", "line": nil, "body": "why?", "assessment": "fixed in the last commit", "addressed": true},
		map[string]any{"discussion_id": "", "author": "@bob", "file": "src/A.php", "line": 12.0, "body": "why?", "assessment": "still open", "addressed": false},
	}
	fr.Outputs = []map[string]any{out}
	// The thread list changes after the MR was added and before the run: the prompt must show the fresh one.
	gl.SetThreads(42, []gitlab.Discussion{
		{ID: "d1", Resolvable: true, File: "src/A.php", Line: 12, Notes: []gitlab.Note{{ID: 100, Author: "bob", Body: "why?"}, {ID: 199, Author: "alice", Body: "because\nof reasons"}}},
		{ID: "d2", Resolvable: true, File: "src/A.php", Line: 13, Notes: []gitlab.Note{{ID: 101, Author: "bob", Body: "and this?"}}},
		{ID: "d3", Resolvable: true, Resolved: true, Notes: []gitlab.Note{{ID: 102, Author: "bob", Body: "resolved one"}}},
	})
	id, err := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	prompt := fr.Requests[0].Prompt
	for _, want := range []string{"Unresolved reviewer discussions (2)", "[d1] @bob at src/A.php:12: why?", "reply @alice: because of reasons", "[d2] @bob at src/A.php:13: and this?", "as `discussion_id`"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "resolved one") {
		t.Fatal("resolved threads are not review input")
	}
	threads := svc.ThreadsOf(mr)
	byID := map[string]ThreadView{}
	for _, v := range threads.Open {
		byID[v.GitLabID] = v
	}
	if a := byID["d2"].Assessment; a == nil || !a.Addressed || a.Text != "fixed in the last commit" || a.RunID != id {
		t.Fatalf("d2: %+v", a)
	}
	if a := byID["d1"].Assessment; a == nil || a.Addressed || a.Text != "still open" {
		t.Fatalf("d1: %+v", a)
	}
}

// Selected live threads go to the fix run with their replies; a resolved thread cannot be selected.
func TestFixSelectedThreads(t *testing.T) {
	svc, _, fr := newService(t)
	mr, _ := svc.AddMR("!42")
	threads := svc.ThreadsOf(mr)
	if _, err := svc.StartFixFindings(mr.ID, "", "", db.Selection{Threads: []int64{threads.Resolved[0].ID}}, 0); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("%v", err)
	}
	if _, err := svc.StartFixFindings(mr.ID, "", "", db.Selection{Threads: []int64{9999}}, 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("%v", err)
	}
	fr.Outputs = []map[string]any{{"summary": "Done.", "results": []any{}, "addressed": []any{}, "changes": []any{}, "tests": "none", "todo": []any{}, "self_review": "ok", "commit_message": "", "ask": []any{}}}
	var d1 ThreadView
	for _, v := range threads.Open {
		if v.GitLabID == "d1" {
			d1 = v
		}
	}
	id, err := svc.StartFixFindings(mr.ID, "", "", db.Selection{Threads: []int64{d1.ID}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return status(svc, id) == db.StatusDone })
	if prompt := fr.Requests[0].Prompt; !strings.Contains(prompt, `"body": "why?\n\nreply @alice: because"`) || !strings.Contains(prompt, `"line": 12`) {
		t.Fatalf("%s", prompt)
	}
}

// Telegram: a new thread somebody opened in my MR is reported once; my own threads and the first sync are not news.
func TestTelegramNewThread(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		texts = append(texts, body["text"].(string))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), texts...) }

	svc, gl, _ := newService(t)
	svc.Settings.DashboardURL = "http://dash.lan:8765"
	svc.telegram = notify.New("tok", "chat")
	svc.telegram.API = srv.URL
	mr, _ := svc.AddMR("!42") // first sync: four discussions, no messages
	gl.SetThreads(42, []gitlab.Discussion{
		{ID: "d1", Resolvable: true, File: "src/A.php", Line: 12, Notes: []gitlab.Note{{ID: 100, Author: "bob", Body: "why?"}}},
		{ID: "n1", Resolvable: true, File: "src/C.php", Line: 5, Notes: []gitlab.Note{{ID: 500, Author: "carol", Body: "Please rename this\nsecond line"}}},
		{ID: "n2", Resolvable: true, Notes: []gitlab.Note{{ID: 501, Author: "alice", Body: "note to self"}}},
		{ID: "n3", Notes: []gitlab.Note{{ID: 502, Author: "carol", Body: "plain comment"}}},
	})
	if err := svc.SyncDiscussions(mr.ID); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return len(got()) == 1 })
	if msg := got()[0]; !strings.Contains(msg, "💬 Новый тред в моём MR !42 «MR 42»") || !strings.Contains(msg, "@carol · src/C.php:5") || !strings.Contains(msg, "Please rename this") ||
		strings.Contains(msg, "second line") || !strings.Contains(msg, "http://dash.lan:8765/mr/1#discussions") {
		t.Fatalf("%q", msg)
	}
	_ = svc.SyncDiscussions(mr.ID)
	if len(got()) != 1 {
		t.Fatalf("a known thread is not news: %q", got())
	}
}

// Reply and resolve go to GitLab with the developer's text and the page state follows: the reply is in the thread,
// «Ответить и закрыть» resolves it, a refusal of GitLab changes nothing here.
func TestReplyAndResolveThread(t *testing.T) {
	svc, gl, _ := newService(t)
	mr, _ := svc.AddMR("!42")
	find := func(id string) ThreadView {
		th := svc.ThreadsOf(mr)
		for _, v := range append(append(th.Open, th.Resolved...), th.Comments...) {
			if v.GitLabID == id {
				return v
			}
		}
		t.Fatalf("thread %s not found", id)
		return ThreadView{}
	}
	d2 := find("d2")
	if err := svc.ReplyThread(d2.ID, "   ", false); err == nil {
		t.Fatal("an empty reply is refused before GitLab")
	}
	gl.FailWrite = true
	if err := svc.ReplyThread(d2.ID, "ok", true); err == nil || !strings.Contains(err.Error(), "403") || len(gl.Replies) != 0 {
		t.Fatalf("%v %v", err, gl.Replies)
	}
	gl.FailWrite = false
	if err := svc.ReplyThread(d2.ID, " Done in abc123 ", false); err != nil {
		t.Fatal(err)
	}
	if d2 = find("d2"); gl.Replies[0] != "d2: Done in abc123" || !d2.Open() || d2.WaitsForMe || d2.Replies()[0].Body != "Done in abc123" {
		t.Fatalf("%v %+v", gl.Replies, d2)
	}
	if err := svc.ReplyThread(d2.ID, "closing", true); err != nil {
		t.Fatal(err)
	}
	if d2 = find("d2"); !d2.Resolved || len(gl.Resolves) != 1 || gl.Resolves[0] != "d2=true" {
		t.Fatalf("%+v %v", d2, gl.Resolves)
	}
	if mr, _ = svc.DB.GetMR(mr.ID); mr.Unresolved != 1 {
		t.Fatalf("the counter follows: %d", mr.Unresolved)
	}
	if err := svc.ResolveThread(d2.ID, false); err != nil || !find("d2").Open() {
		t.Fatal(err)
	}
	if err := svc.ResolveThread(find("dc").ID, true); err == nil {
		t.Fatal("a plain comment cannot be resolved")
	}
	if err := svc.ReplyThread(find("dc").ID, "thanks", false); err != nil || gl.Replies[2] != "dc: thanks" {
		t.Fatalf("%v %v", err, gl.Replies)
	}

	// The draft: only for an own MR and only when the review found the thread addressed.
	addressed := ThreadView{MRDiscussion: db.MRDiscussion{Resolvable: true}, Assessment: &db.Assessment{Addressed: true, Text: "guard added"}}
	if got := addressed.ReplyDraft(true); got != "Исправлено. guard added" {
		t.Fatal(got)
	}
	addressed.Assessment.Addressed = false
	if addressed.ReplyDraft(true) != "" || (ThreadView{}).ReplyDraft(true) != "" {
		t.Fatal("no draft without an «addressed» assessment")
	}
	addressed.Assessment.Addressed = true
	if addressed.ReplyDraft(false) != "" {
		t.Fatal("no draft in someone else's MR")
	}
}
