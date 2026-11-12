package web

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"mr-review/internal/usage"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mr-review/internal/app"
	"mr-review/internal/db"
	"mr-review/internal/runner"
	"mr-review/internal/testutil"
)

func newServer(t *testing.T) (*httptest.Server, *app.Service, *testutil.FakeRunner) {
	t.Helper()
	settings, _ := testutil.Settings(t)
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = database.Migrate()
	fr := &testutil.FakeRunner{}
	svc := app.New(settings, database, testutil.NewFakeGitLab(), []runner.Runner{fr})
	srv, err := New(svc, "test", []runner.Runner{fr})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); svc.Shutdown(); database.Close() })
	return ts, svc, fr
}

func postJSON(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestPagesAndFlow(t *testing.T) {
	ts, svc, fr := newServer(t)
	if code, body := get(t, ts.URL+"/mrs"); code != 200 || !strings.Contains(body, "Пока нет merge requests") || !strings.Contains(body, "skill: mr-review") {
		t.Fatalf("%d %s", code, body[:200])
	}
	if code, body := get(t, ts.URL+"/"); code != 200 || !strings.Contains(body, "Сейчас ничего не требует вашего внимания") || !strings.Contains(body, "Мои MR") {
		t.Fatalf("overview %d", code)
	}
	if code, body := get(t, ts.URL+"/api/health"); code != 200 || !strings.Contains(body, svc.Settings.ProjectRoot) {
		t.Fatalf("%d %s", code, body)
	}
	code, out := postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/merge_requests/42"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	redirect := out["redirect"].(string)
	if code, body := get(t, ts.URL+redirect); code != 200 || !strings.Contains(body, "Исправить замечания ревьюеров (2)") || !strings.Contains(body, "Запустить ревью") || !strings.Contains(body, "Ещё не проверен") {
		t.Fatalf("mr page: %d (own MR with 2 unresolved threads, never reviewed)", code)
	}
	if _, body := get(t, ts.URL+redirect); !strings.Contains(body, "Pipeline failed") || !strings.Contains(body, "approvals 1/2") || !strings.Contains(body, "отстаёт от develop на 3") {
		t.Fatal("mr page must show GitLab state: pipeline, approvals, divergence")
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, "Запустить ревью") || strings.Contains(body, ">Быстрое<") || !strings.Contains(body, "Pipeline failed") {
		t.Fatal("list must show a state-driven primary action and the GitLab state")
	}
	if code, body := get(t, ts.URL+"/mrs"); code != 200 || !strings.Contains(body, "MR 42") || !strings.Contains(body, "data-tip=") || !strings.Contains(body, `data-filter="#mrs-table"`) {
		t.Fatalf("%d", code)
	}
	// Highlighted GitLab labels are badges (case-insensitive match, configured colour) and row filter attributes;
	// the other labels stay out of the row. Filter chips: mine/others, draft, AI state, labels.
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, `label-red tip" data-tip="Метка GitLab «high»">high<`) || !strings.Contains(body, `data-labels="high"`) ||
		!strings.Contains(body, `data-mine="1" data-role="author assignee" data-draft="0" data-ai="never"`) || strings.Contains(body, ">backend<") ||
		!strings.Contains(body, `data-facet="mine"`) || !strings.Contains(body, `data-facet="ai"`) || !strings.Contains(body, `class="fchip label-orange" data-value="bug"`) {
		t.Fatal("list must show highlighted labels as badges, row facets and filter chips")
	}
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#7"})
	if _, body := get(t, ts.URL+"/issues"); !strings.Contains(body, `data-labels=""`) || !strings.Contains(body, `data-facet="labels"`) || !strings.Contains(body, ">backend<") {
		t.Fatal("issues list must carry label facets and chips and keep the other labels as text")
	}
	// The overview lists the own MR with a failed pipeline; the never-reviewed MR is not offered for review (own MR).
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "Pipeline failed") || !strings.Contains(body, "Разобрать ошибки") {
		t.Fatal("overview must surface the failed pipeline of my MR")
	}

	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	code, out = postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full", "runner": "claude"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	runURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(runURL))
		return strings.Contains(body, `"status":"done"`)
	})
	if code, body := get(t, ts.URL+runURL); code != 200 || !strings.Contains(body, "Null deref") || !strings.Contains(body, "Нужны правки") || !strings.Contains(body, "Продолжить сессию") {
		t.Fatalf("run page %d", code)
	}
	// Markdown rendering and file links to GitLab at the reviewed commit.
	if _, body := get(t, ts.URL+runURL); !strings.Contains(body, `<div class="md">`) || !strings.Contains(body, "/-/blob/sha-1/src/A.php#L10") || !strings.Contains(body, `data-format="log"`) {
		t.Fatal("run page must render markdown, link findings to GitLab and mark the log for formatting")
	}
	// A finding also links into the MR's «Изменения», anchored at the file (GitLab anchors it by SHA-1 of the path).
	if _, body := get(t, ts.URL+"/mr/1"); !strings.Contains(body, fmt.Sprintf("/-/merge_requests/42/diffs#%x", sha1.Sum([]byte("src/A.php")))) {
		t.Fatal("MR page must link a finding into the MR diff at its file")
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, "Открыть замечания") || !strings.Contains(body, "1 major") || !strings.Contains(body, "Проверен") {
		t.Fatal("list must show CURRENT state with findings summary and «Открыть замечания»")
	}
	// Manual finding statuses.
	findings, _ := svc.DB.ListFindings(1)
	if code, out := postJSON(t, ts.URL+"/api/findings/"+strconvI(findings[0].ID)+"/status", map[string]any{"status": "false_positive"}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := postJSON(t, ts.URL+"/api/findings/"+strconvI(findings[0].ID)+"/status", map[string]any{"status": "bogus"}); code != 400 {
		t.Fatal("unknown status must be rejected")
	}
	if _, body := get(t, ts.URL+redirect); !strings.Contains(body, "ложное срабатывание") || strings.Contains(body, "1 major") || !strings.Contains(body, "1 info") {
		t.Fatal("a false positive is not an open finding any more")
	}
	if code, body := get(t, ts.URL+"/runs"); code != 200 || !strings.Contains(body, "Полное ревью") || !strings.Contains(body, "MR 42") {
		t.Fatalf("sessions page %d", code)
	}
	if code, body := get(t, ts.URL+"/workspaces"); code != 200 || !strings.Contains(body, "Workspaces пока нет") {
		t.Fatalf("workspaces page %d", code)
	}
	if _, body := get(t, ts.URL+redirect); !strings.Contains(body, "Нет изменений после последнего ревью") {
		t.Fatal("disabled «Проверить изменения» must explain why")
	}
	if _, body := get(t, ts.URL+runURL+"/log"); !strings.Contains(body, "fake run") {
		t.Fatal("log")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "bogus"}); code != 400 || out["error"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/merge_requests/999"}); code != 400 || !strings.Contains(out["error"].(string), "404") {
		t.Fatalf("%d %v", code, out)
	}
	fr.Texts = []string{"Because."}
	code, out = postJSON(t, ts.URL+"/api/runs/1/ask", map[string]any{"question": "why?"})
	if code != 200 || out["run_id"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	askID := int64(out["run_id"].(float64))
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(askID); return r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/run/1"); !strings.Contains(body, "Because.") {
		t.Fatal("the answer shows in the asked run's dialog")
	}
	if code, body := get(t, ts.URL+"/doctor"); code != 200 || !strings.Contains(body, "Skill: review_full") || !strings.Contains(body, "Действия dashboard → skills проекта") || !strings.Contains(body, "task-plan") {
		t.Fatalf("doctor %d: must list the action → skill map", code)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/mrs/1", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatal("delete")
	}
	if code, _ := get(t, ts.URL+"/-/mr/1"); code != 404 {
		t.Fatal("expected 404 after delete")
	}
}

func TestPermissionPromptPage(t *testing.T) {
	ts, svc, fr := newServer(t)
	code, out := postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	fr.AskWrite("/tmp/claude/probe.txt")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	code, out = postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	runURL := out["redirect"].(string)
	runID := lastSeg(runURL)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+runID)
		return strings.Contains(body, `"status":"waiting"`) && strings.Contains(body, `"pending_approval"`)
	})
	if _, body := get(t, ts.URL+runURL); !strings.Contains(body, "Нужен ваш ответ: агент просит разрешение") || !strings.Contains(body, "/tmp/claude/probe.txt") || !strings.Contains(body, "Разрешить и не спрашивать") {
		t.Fatal("run page must show the permission prompt with allow/allow_always/deny")
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, "Ответить агенту") || !strings.Contains(body, "⚠ Нужен ваш ответ") {
		t.Fatal("list and header must point at the waiting run")
	}
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "агенту нужен ваш ответ") || !strings.Contains(body, ">Ответить<") {
		t.Fatal("overview inbox must lead with the pending approval")
	}
	pending, _ := svc.DB.PendingApproval(1)
	if code, out := postJSON(t, ts.URL+"/api/approvals/"+strings.TrimSpace(strconvI(pending.ID)), map[string]any{"decision": "bogus"}); code != 400 || out["error"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := postJSON(t, ts.URL+"/api/approvals/"+strconvI(pending.ID), map[string]any{"decision": "deny", "note": "not now"}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+runID)
		return strings.Contains(body, `"status":"done"`)
	})
	if _, body := get(t, ts.URL+runURL); !strings.Contains(body, "Разрешения в этой сессии") || !strings.Contains(body, "✕ отклонено: not now") || !strings.Contains(body, "Отклонено автоматически") {
		t.Fatal("run page must list the answered prompt and the automatic denials")
	}
}

func TestPlanFileExport(t *testing.T) {
	ts, svc, fr := newServer(t)
	svc.Settings.PlansDir = t.TempDir()
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#7"})
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	_, out := postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan"})
	runURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(runURL))
		return strings.Contains(body, `"status":"done"`)
	})
	if _, body := get(t, ts.URL+runURL); !strings.Contains(body, "Сохранить план в проект") {
		t.Fatal("plan page must offer the export")
	}
	code, out := postJSON(t, ts.URL+"/api/runs/"+lastSeg(runURL)+"/plan-file", map[string]any{})
	if code != 200 || !strings.HasPrefix(out["path"].(string), svc.Settings.PlansDir) {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+runURL); !strings.Contains(body, "файл плана:") || !strings.Contains(body, out["path"].(string)) {
		t.Fatal("plan page must show the exported file path")
	}
}

func strconvI(v int64) string { return strings.TrimSpace(fmtInt(v)) }

// lastSeg returns the id segment of a readable object URL like /-/review/12#approval.
func lastSeg(url string) string {
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url = url[:i]
	}
	return url[strings.LastIndex(url, "/")+1:]
}

func TestHistoryAndPrettyURLs(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	_, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	runURL := out["redirect"].(string)
	if !strings.HasPrefix(runURL, "/-/review/") {
		t.Fatalf("readable run URL expected, got %s", runURL)
	}
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(runURL))
		return strings.Contains(body, `"status":"done"`)
	})
	if code, body := get(t, ts.URL+runURL); code != 200 || !strings.Contains(body, "Результат ревью") {
		t.Fatalf("pretty run page %d", code)
	}
	if code, _ := get(t, ts.URL+"/-/bogus/1"); code != 404 {
		t.Fatal("unknown action slug must be 404")
	}
	// Old URLs redirect (the test client follows them).
	if code, body := get(t, ts.URL+"/run/1"); code != 200 || !strings.Contains(body, "Результат ревью") {
		t.Fatalf("legacy /run/1 must redirect: %d", code)
	}
	if code, body := get(t, ts.URL+"/mr/1"); code != 200 || !strings.Contains(body, "MR 42") {
		t.Fatalf("legacy /mr/1 must redirect: %d", code)
	}
	// I approve the MR: after a sync it leaves the main list and shows up in the history with its actions.
	gl := svc.GitLab.(*testutil.FakeGitLab)
	gl.ApprovedBy[42] = []string{"alice"}
	if code, out := postJSON(t, ts.URL+"/api/mrs/sync", map[string]any{}); code != 200 || out["archived"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/mrs"); strings.Contains(body, "MR 42") || !strings.Contains(body, "в истории: 1") {
		t.Fatal("approved MR must leave the main list")
	}
	if _, body := get(t, ts.URL+"/history"); !strings.Contains(body, "MR 42") || !strings.Contains(body, "одобрен мной") || !strings.Contains(body, "Полное ревью заново") {
		t.Fatal("history must list the MR with the review actions")
	}
}

func TestHideAndRestoreMR(t *testing.T) {
	ts, _, _ := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, "Merge requests <span class=\"pill\">1</span>") || !strings.Contains(body, "Убрать в историю") {
		t.Fatal("header counts the main list; the row offers to hide the MR")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/hide", map[string]any{}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/mrs"); strings.Contains(body, "MR 42") || !strings.Contains(body, "Merge requests <span class=\"pill\">0</span>") || !strings.Contains(body, "История <span class=\"pill\">1</span>") {
		t.Fatal("a hidden MR leaves the main list and the counters follow")
	}
	if _, body := get(t, ts.URL+"/history"); !strings.Contains(body, "MR 42") || !strings.Contains(body, "скрыт вами") || !strings.Contains(body, "Вернуть в список") {
		t.Fatal("history shows the hidden MR with a restore action")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/sync", map[string]any{}); code != 200 || out["archived"].(float64) != 1 || out["pruned"].(float64) != 0 {
		t.Fatalf("sync must keep a hidden MR: %d %v", code, out)
	}
	if code, _ := postJSON(t, ts.URL+"/api/mrs/1/unhide", map[string]any{}); code != 200 {
		t.Fatal("unhide")
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, "MR 42") {
		t.Fatal("restored MR is back in the main list")
	}
}

func TestSeenMRSinksAndResetsOnSync(t *testing.T) {
	ts, svc, _ := newServer(t)
	gl := svc.GitLab.(*testutil.FakeGitLab)
	gl.MRs[43] = testutil.MRPayload(43, "sha-43")
	gl.MRs[43]["updated_at"] = "2026-09-02T10:00:00+03:00" // newer than !42: listed first by default
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!43"})
	_, body := get(t, ts.URL+"/mrs")
	if strings.Index(body, "MR 43") > strings.Index(body, "MR 42") || !strings.Contains(body, ">Просмотрено</button>") || strings.Contains(body, `data-seen="1"`) {
		t.Fatal("default order is by GitLab activity; rows offer the seen mark and none is dimmed")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/2/seen", map[string]any{}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	_, body = get(t, ts.URL+"/mrs")
	if strings.Index(body, "MR 43") < strings.Index(body, "MR 42") || strings.Count(body, `data-seen="1"`) != 1 || !strings.Contains(body, ">просмотрено</span>") || !strings.Contains(body, "Снять отметку") {
		t.Fatal("a seen MR is dimmed, badged and sinks to the end of the list")
	}
	if _, page := get(t, ts.URL+"/-/mr/2"); !strings.Contains(page, `data-back href="/mrs"`) || !strings.Contains(page, "Снять отметку «просмотрено»</button>") || !strings.Contains(page, `>просмотрено</span>`) {
		t.Fatal("mr page has the back link and the seen toggle in the header")
	}
	if _, page := get(t, ts.URL+"/-/mr/1"); !strings.Contains(page, `backHref('/mrs')`) || !strings.Contains(page, ">Просмотрено</button>") {
		t.Fatal("mr page offers to mark the MR seen and return to the list")
	}
	// A sync without changes keeps the mark.
	postJSON(t, ts.URL+"/api/mrs/sync", map[string]any{})
	if _, body = get(t, ts.URL+"/mrs"); strings.Count(body, `data-seen="1"`) != 1 {
		t.Fatal("sync without activity must keep the mark")
	}
	// New commits on the MR: the next sync clears the mark and the MR returns to its place.
	gl.MRs[43] = testutil.MRPayload(43, "sha-43-new")
	gl.MRs[43]["updated_at"] = "2026-09-02T10:00:00+03:00"
	postJSON(t, ts.URL+"/api/mrs/sync", map[string]any{})
	_, body = get(t, ts.URL+"/mrs")
	if strings.Contains(body, `data-seen="1"`) || strings.Index(body, "MR 43") > strings.Index(body, "MR 42") {
		t.Fatal("new commits must clear the seen mark")
	}
	// New reviewer discussion: same.
	postJSON(t, ts.URL+"/api/mrs/2/seen", map[string]any{})
	gl.Unresolved[43] = 1
	postJSON(t, ts.URL+"/api/mrs/2/refresh", map[string]any{})
	if _, body = get(t, ts.URL+"/mrs"); strings.Contains(body, `data-seen="1"`) {
		t.Fatal("a new discussion must clear the seen mark")
	}
	postJSON(t, ts.URL+"/api/mrs/2/seen", map[string]any{})
	if code, _ := postJSON(t, ts.URL+"/api/mrs/2/unseen", map[string]any{}); code != 200 {
		t.Fatal("unseen")
	}
	if _, body = get(t, ts.URL+"/mrs"); strings.Contains(body, `data-seen="1"`) {
		t.Fatal("mark cleared by hand")
	}
}

func TestRunPageShowsContextFill(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	run, _ := svc.DB.GetRun(1)
	if run.ContextTokens != 20000 || run.ContextWindow != 200000 || run.ContextPct() != 10 {
		t.Fatalf("context stored with the run: %+v", run)
	}
	if _, body := get(t, ts.URL+"/-/review/1"); !strings.Contains(body, `<span class="k">контекст</span><span class="ctx-value">10%</span>`) {
		t.Fatal("run facts must show the context fill")
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "Продолжить сессию #1 · Полное ревью · claude · 54.2k токенов · контекст 10% ·") {
		t.Fatal("context picker must show the fill of the session")
	}
	if _, data := get(t, ts.URL+"/api/runs/1"); !strings.Contains(data, `"context_pct":10`) {
		t.Fatalf("api must report the fill: %s", data)
	}
	// The next run of the same model knows the window before its own result arrives (live percent).
	if w := svc.DB.KnownContextWindow("claude-opus-4-1"); w != 200000 {
		t.Fatalf("known window: %d", w)
	}
	if w := svc.DB.KnownContextWindow("claude-haiku-4-5"); w != 200000 {
		t.Fatalf("known window for a listed secondary model: %d", w)
	}
	if w := svc.DB.KnownContextWindow("other"); w != 0 {
		t.Fatalf("unknown model: %d", w)
	}
}

func fmtInt(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{digits[v%10]}, buf...)
		v /= 10
	}
	return string(buf)
}

func TestIssuesPages(t *testing.T) {
	ts, _, fr := newServer(t)
	code, out := postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/issues/7"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if code, body := get(t, ts.URL+"/issues"); code != 200 || !strings.Contains(body, "Task 7") {
		t.Fatalf("%d", code)
	}
	if code, body := get(t, ts.URL+"/issue/1"); code != 200 || !strings.Contains(body, "group/sub/project#7") || !strings.Contains(body, "Решить задачу") || !strings.Contains(body, "Исследовать") {
		t.Fatalf("%d", code)
	}
	if _, body := get(t, ts.URL+"/issues"); !strings.Contains(body, "Решить задачу") || !strings.Contains(body, "Новая") {
		t.Fatal("issues list must show state and primary action")
	}
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan", "notes": "n"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	runURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(runURL))
		return strings.Contains(body, `"status":"done"`)
	})
	if code, body := get(t, ts.URL+runURL); code != 200 || !strings.Contains(body, "План решения") || !strings.Contains(body, "Plan summary") || !strings.Contains(body, "Реализовать этот план") {
		t.Fatalf("plan page %d", code)
	}
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, "Есть план") || !strings.Contains(body, "Реализовать этот план") {
		t.Fatal("issue page must show planned state")
	}
	// Implement from the plan: the plan text must reach the agent's notes.
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "implement", "plan_run": lastSeg(runURL)})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	implURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(implURL))
		return strings.Contains(body, `"status":"done"`)
	})
	last := fr.Requests[len(fr.Requests)-1]
	if !strings.Contains(last.Prompt, "Plan from the investigation run") || !strings.Contains(last.Prompt, "1. one") {
		t.Fatalf("plan not passed to implementation prompt")
	}
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, "Готово к MR") || !strings.Contains(body, "Подготовить MR") || !strings.Contains(body, `class="btn btn-link ext tip" data-tip="Открыть задачу #7 в GitLab`) {
		t.Fatal("issue page must show ready state and a visible GitLab button")
	}
	if _, body := get(t, ts.URL+implURL); !strings.Contains(body, ">Задача в GitLab</a>") || !strings.Contains(body, "Забрать ветку в основную копию") || !strings.Contains(body, "git checkout group/sub/project#7") {
		t.Fatal("implement run page must link to the GitLab issue and explain where the branch lives")
	}
	// The issue page lists the worktree of the implementation with its state and the release action (disabled: dirty).
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, `id="workspaces"`) || !strings.Contains(body, "незакоммиченные изменения") || !strings.Contains(body, `<button class="btn btn-sm btn-primary" disabled>Забрать ветку в основную копию</button>`) {
		t.Fatal("issue page must list the task's workspaces with the release action")
	}
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "готово к MR") {
		t.Fatal("overview must list the implemented task")
	}
	if code, body := get(t, ts.URL+"/workspaces"); code != 200 || !strings.Contains(body, "group/sub/project#7") || !strings.Contains(body, "Есть незакоммиченные изменения") {
		t.Fatalf("workspaces page must list the dirty worktree: %d", code)
	}
	// Retry a failed run.
	fr.FailWith = "boom"
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan"})
	failedURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+lastSeg(failedURL))
		return strings.Contains(body, `"status":"failed"`)
	})
	if _, body := get(t, ts.URL+failedURL); !strings.Contains(body, "Не удалось исследовать задачу") || !strings.Contains(body, "Повторить") {
		t.Fatal("failed run page must show a human error and retry")
	}
	fr.FailWith = ""
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	code, out = postJSON(t, ts.URL+"/api/runs/"+lastSeg(failedURL)+"/retry", map[string]any{})
	if code != 200 || out["redirect"] == nil {
		t.Fatalf("retry: %d %v", code, out)
	}
	if code, out := postJSON(t, ts.URL+"/api/issues/sync", map[string]any{}); code != 200 || out["synced"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}

// Phase C in the UI: the MR page offers finished sessions as a context, the run page names the chosen one before the
// run finishes, and the sessions list shows the chain.
func TestContextPickerAndChain(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `<option value="">Новый чат · пустой контекст</option>`) || !strings.Contains(body, "пока нет завершённых сессий") {
		t.Fatal("mr page must show the context picker with only the new chat")
	}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `<option value="1" data-kinds="quick full verify verify_finding">Продолжить сессию #1 · Полное ревью · claude`) {
		t.Fatal("finished review must be offered as a context")
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, ">контекст #1<") {
		t.Fatal("list row must flag the available context")
	}
	fr.Block = make(chan struct{})
	code, out = postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "quick", "continue_run": "1"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/-/quick-review/2"); !strings.Contains(body, "Продолжение сессии #2") && !strings.Contains(body, "Продолжение сессии #1") || !strings.Contains(body, "по вашему выбору агент продолжает контекст «Полное ревью»") {
		t.Fatal("run page must name the chosen context while the run is still active")
	}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	close(fr.Block)
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/runs"); !strings.Contains(body, "сессия: <a href=\"/-/review/1\">#1</a> → <b>#2</b>") {
		t.Fatal("sessions list must show the conversation chain")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "fix", "continue_run": "1"}); code == 200 || !strings.Contains(out["error"].(string), "another directory") {
		t.Fatalf("fix comments cannot continue a project-root session: %d %v", code, out)
	}
}

// «Проверить замечание» in the UI: menu item on open findings, run page with the outcome, "what changed" on a stale review.
func TestVerifyFindingPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `startRun('/api/mrs/1/runs', 'verify_finding', this, {finding: '1'})`) || !strings.Contains(body, "Проверить замечание") {
		t.Fatal("open findings must offer the check")
	}
	fr.Outputs = []map[string]any{{"status": "obsolete", "evidence": "The method was removed in the latest commit; `src/A.php` no longer calls it at all.", "severity": "", "suggestion": ""}}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "verify_finding", "finding": "1"})
	if code != 200 || out["redirect"] != "/-/verify-finding/2" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/verify-finding/2"); !strings.Contains(body, "Проверяемое замечание") || !strings.Contains(body, "Итог проверки") || !strings.Contains(body, "неактуально") || !strings.Contains(body, "no longer calls it") {
		t.Fatal("run page must show the finding and the outcome")
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `class="fstatus fstatus-obsolete">неактуально`) || !strings.Contains(body, `href="/-/verify-finding/2">проверка #2</a>`) || strings.Contains(body, `{finding: '1'}`) {
		t.Fatal("mr page must show the check outcome on the finding and stop offering the check")
	}
	if _, body := get(t, ts.URL+"/runs"); !strings.Contains(body, "Проверка замечания") {
		t.Fatal("sessions list must name the run kind")
	}
	// New commits: the stale badge says what changed since the reviewed SHA.
	svc.GitLab.(*testutil.FakeGitLab).MRs[42] = testutil.MRPayload(42, "sha-2")
	postJSON(t, ts.URL+"/api/mrs/1/refresh", map[string]any{})
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "есть новые изменения: 2 коммитов, 1 файлов, <span class=\"text-success\">+10</span> <span class=\"text-danger\">−3</span>") {
		t.Fatal("stale badge must summarise the compare result")
	}
}

// «Открыть в терминале»: the dashboard starts a terminal emulator on its own machine in the run's directory,
// continuing the agent session; the page offers the button and the copyable command.
func TestOpenTerminal(t *testing.T) {
	settings, _ := testutil.Settings(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "term.sh")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\n"), 0o755)
	settings.TerminalCmd = script + " {dir} {cmd}"
	t.Setenv("DISPLAY", ":0")
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = database.Migrate()
	fr := &testutil.FakeRunner{}
	svc := app.New(settings, database, testutil.NewFakeGitLab(), []runner.Runner{fr})
	srv, err := New(svc, "test", []runner.Runner{fr})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); svc.Shutdown(); database.Close() })

	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	want := "cd '" + settings.ProjectRoot + "' && claude --resume sess-1"
	if _, body := get(t, ts.URL+"/-/review/1"); !strings.Contains(body, "Продолжить в терминале") || !strings.Contains(body, "openTerminal('1', true, this)") || !strings.Contains(body, template.HTMLEscapeString(want)) {
		t.Fatal("run page must offer the terminal and the copyable command")
	}
	code, out := postJSON(t, ts.URL+"/api/runs/1/terminal", map[string]any{"resume": "1"})
	if code != 200 || out["command"] != want {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool {
		data, err := os.ReadFile(record)
		return err == nil && strings.Contains(string(data), "claude --resume sess-1")
	})
	data, _ := os.ReadFile(record)
	if !strings.Contains(string(data), settings.ProjectRoot) || !strings.Contains(string(data), `exec "${SHELL:-bash}"`) {
		t.Fatalf("terminal must open in the run directory and keep a shell: %s", data)
	}
	if _, body := get(t, ts.URL+"/runs"); !strings.Contains(body, "openTerminal('1', true, this)") {
		t.Fatal("sessions list must offer the terminal too")
	}
	if code, out := postJSON(t, ts.URL+"/api/runs/999/terminal", map[string]any{}); code == 200 || !strings.Contains(out["error"].(string), "not found") {
		t.Fatalf("%d %v", code, out)
	}
}

// Without a terminal emulator the buttons are disabled with the reason instead of failing on click.
func TestTerminalUnavailable(t *testing.T) {
	ts, _, _ := newServer(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if code, out := postJSON(t, ts.URL+"/api/runs/1/terminal", map[string]any{"resume": "1"}); code == 200 {
		t.Fatalf("%d %v", code, out)
	}
}

// The doctor page carries the per-action override form and saves it through the API.
func TestSkillSettingsPage(t *testing.T) {
	ts, svc, _ := newServer(t)
	if _, body := get(t, ts.URL+"/doctor"); !strings.Contains(body, `saveSkill(event, 'plan')`) || !strings.Contains(body, "использовать свои инструкции") || !strings.Contains(body, `<option value="mr-review"`) {
		t.Fatal("doctor page must offer the override form with the project candidates")
	}
	code, out := postJSON(t, ts.URL+"/api/skills/plan", map[string]any{"name": "", "custom": "1", "text": "Check the acceptance criteria first."})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/doctor"); !strings.Contains(body, "✎ свои инструкции") || !strings.Contains(body, "Check the acceptance criteria first.") || !strings.Contains(body, "custom instructions from the dashboard") {
		t.Fatal("saved override must show in the table and in the doctor report")
	}
	if sk := svc.SkillFor(db.KindPlan); sk == nil || sk.Kind != "custom" {
		t.Fatalf("%+v", sk)
	}
	if code, out := postJSON(t, ts.URL+"/api/skills/nope", map[string]any{}); code == 200 {
		t.Fatalf("%d %v", code, out)
	}
}

// The MR page offers «Проверить на стенде» only when the project has a stand skill; the run page shows the stand report.
func TestStandTestPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if _, body := get(t, ts.URL+"/-/mr/1"); strings.Contains(body, `id="stand"`) || !strings.Contains(body, "не найден skill доступа к стенду") {
		t.Fatal("without a stand skill the card is absent and the menu item explains why")
	}
	testutil.AddProjectSkill(t, svc.Settings.ProjectRoot, "dev-stand", "Access to the dev stand")
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `id="stand"`) || !strings.Contains(body, "startRun('/api/mrs/1/runs', 'stand', this") {
		t.Fatal("stand card must appear")
	}
	fr.Outputs = []map[string]any{{"summary": "Deployed and exercised the MR on the stand; the emulation script passed.", "deployed": []any{"src/A.php"},
		"tests": "phpunit: 12 passed", "script_path": "scripts/onerun/mr_42.php", "script_output": "all good", "problems": []any{map[string]any{"severity": "MEDIUM", "title": "Slow query", "description": "took 4s"}}, "changes": []any{}, "todo": []any{}, "commit_message": ""}}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "stand", "notes": "x"})
	if code != 200 || out["redirect"] != "/-/stand-test/1" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/stand-test/1"); !strings.Contains(body, "Проверка на стенде") || !strings.Contains(body, "Залито на стенд") || !strings.Contains(body, "Slow query") || !strings.Contains(body, "scripts/onerun/mr_42.php") || !strings.Contains(body, `id="workspace"`) {
		t.Fatal("run page must show the stand report and the workspace")
	}
}

// The list toolbars are three blocks (filters, GitLab sync, agent controls with the usage slot); /api/usage serves
// the poller's snapshots and an empty set when polling is off.
func TestToolbarBlocksAndUsageAPI(t *testing.T) {
	ts, svc, _ := newServer(t)
	for _, path := range []string{"/mrs", "/issues", "/ci"} {
		_, body := get(t, ts.URL+path)
		if !strings.Contains(body, `class="tb-left"`) || !strings.Contains(body, `class="tb-agent"`) || !strings.Contains(body, `<span class="usage" data-usage hidden></span>`) {
			t.Fatalf("%s must have the two toolbar columns and the usage slot", path)
		}
	}
	if _, body := get(t, ts.URL+"/mrs"); !strings.Contains(body, `class="tb-row tb-filter-row"`) {
		t.Fatal("the MR list has the filter row")
	}
	code, body := get(t, ts.URL+"/api/usage")
	if code != 200 || !strings.Contains(body, `"agents":{}`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("%d %s", code, body)
	}
	svc.Usage = &usage.Poller{ClaudeCredentials: filepath.Join(t.TempDir(), "none.json")}
	svc.Usage.Refresh(context.Background(), []string{"claude"})
	if code, body = get(t, ts.URL+"/api/usage"); code != 200 || !strings.Contains(body, `"runner":"claude"`) || !strings.Contains(body, "не найдены") {
		t.Fatalf("%d %s", code, body)
	}
}

// The state block of an object carries data-ai-state: app.js paints «В очереди» there the moment an action is
// pressed, before the server (which first refreshes the object in GitLab) answers.
func TestStateBlocksAreMarkedForOptimisticUpdate(t *testing.T) {
	ts, _, _ := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/issues/7"})
	for _, path := range []string{"/mrs", "/issues", "/-/mr/1", "/-/issue/1"} {
		code, body := get(t, ts.URL+path)
		if code != 200 || !strings.Contains(body, "data-ai-state") {
			t.Fatalf("%s: %d, no data-ai-state hook for the optimistic state", path, code)
		}
	}
}

// Every page with the agent picker also offers the models configured for that agent; the API accepts "agent:model".
func TestModelPickerPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	for _, path := range []string{"/", "/mrs", "/issues", "/-/mr/1"} {
		if _, body := get(t, ts.URL+path); !strings.Contains(body, `class="segmented segmented-sm model-picker tip" data-runner="claude"`) || !strings.Contains(body, `<input type="radio" name="model_claude" value="opus" >Opus`) || !strings.Contains(body, `value="default" checked>авто`) {
			t.Fatalf("%s must render the model picker for claude", path)
		}
	}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "quick", "runner": "claude:fable"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if r, _ := svc.DB.GetRun(1); r.RequestedModel != "fable" || fr.Requests[0].Model != "fable" {
		t.Fatalf("%+v", r)
	}
	if _, body := get(t, ts.URL+"/-/quick-review/1"); !strings.Contains(body, "модель <span class=\"mono\">fable</span>") {
		t.Fatal("run page must show the requested model")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "quick", "runner": "claude:nope"}); code == 200 || !strings.Contains(fmt.Sprint(out["error"]), "not offered") {
		t.Fatalf("%d %v", code, out)
	}
}

// The task's stand check: card and button appear on the issue and implementation pages once there is an
// implementation and a stand skill; the run is bound to the task and reported in the «Довести до MR» checklist.
func TestStandTestIssuePages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/issues/7"})
	testutil.AddProjectSkill(t, svc.Settings.ProjectRoot, "dev-stand", "Access to the dev stand")
	if _, body := get(t, ts.URL+"/issue/1"); strings.Contains(body, `id="stand"`) {
		t.Fatal("no stand card before the task is implemented")
	}
	fr.Outputs = []map[string]any{testutil.ImplementOutput()}
	code, out := postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "implement"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	implURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, `id="stand"`) || !strings.Contains(body, "startRun('/api/issues/1/runs', 'stand', this") || !strings.Contains(body, `href="#stand">Проверить на стенде</a>`) {
		t.Fatal("issue page must offer the stand check for the implementation branch")
	}
	if _, body := get(t, ts.URL+implURL); !strings.Contains(body, `id="stand"`) || !strings.Contains(body, "Стенд · не проверялось") || !strings.Contains(body, `href="#stand">Проверить на стенде</a>`) {
		t.Fatal("implementation page must offer the stand check and list it in the MR checklist")
	}
	fr.Outputs = []map[string]any{{"summary": "The emulation script confirms the task works on the stand.", "deployed": []any{"CHANGED.txt"},
		"tests": "phpunit: 3 passed", "script_path": "scripts/onerun/task_7.php", "script_output": "all good", "problems": []any{}, "changes": []any{}, "todo": []any{}, "commit_message": ""}}
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "stand", "notes": "x"})
	if code != 200 || out["redirect"] != "/-/stand-test/2" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/stand-test/2"); !strings.Contains(body, "Проверка на стенде") || !strings.Contains(body, "← #7 Task 7") || !strings.Contains(body, "Залито на стенд") || !strings.Contains(body, "scripts/onerun/task_7.php") {
		t.Fatal("stand run page must be bound to the task and show the report")
	}
	if _, body := get(t, ts.URL+implURL); !strings.Contains(body, "Стенд · <a href=\"/-/stand-test/2\">сессия #2</a>") || !strings.Contains(body, "Запустить на стенде ещё раз") {
		t.Fatal("checklist must reference the stand run")
	}
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, "стенд: <a href=\"/-/stand-test/2\">#2</a>") {
		t.Fatal("issue state must mention the stand run")
	}
}

// Agent questions in the UI: header chip, overview inbox, the answer form on the run page and the API.
func TestAgentQuestionsPage(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#7"})
	fr.Outputs = []map[string]any{{"summary": "", "steps": []any{}, "files": []any{}, "risks": []any{}, "questions": []any{}, "estimate": "",
		"ask": []any{map[string]any{"question": "Which currency?", "options": []any{"USD", "EUR"}, "why": "unspecified"}}}}
	postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusWaiting })
	if _, body := get(t, ts.URL+"/-/task-plan/1"); !strings.Contains(body, "агент задаёт вопросы по задаче") || !strings.Contains(body, "Which currency?") || !strings.Contains(body, `value="EUR"`) || !strings.Contains(body, "answerQuestions(event, '1')") {
		t.Fatal("run page must show the questions form")
	}
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "⚠ Нужен ваш ответ") || !strings.Contains(body, "1 вопрос(ов) агента") {
		t.Fatal("header chip and overview inbox must point at the waiting run")
	}
	if _, body := get(t, ts.URL+"/issues"); !strings.Contains(body, "Ответить агенту") {
		t.Fatal("task row must offer to answer")
	}
	questions, _ := svc.DB.PendingQuestions(1)
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	code, out := postJSON(t, ts.URL+"/api/runs/1/answers", map[string]any{"answer_" + strconv.FormatInt(questions[0].ID, 10): "EUR — and log it"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/task-plan/1"); !strings.Contains(body, "Вопросы агента и ваши ответы (1)") || !strings.Contains(body, "EUR — and log it") || !strings.Contains(body, "План решения") {
		t.Fatal("finished run page must keep the Q&A and show the plan")
	}
}

// The run page shows the phases of the run; the JSON API carries them for live updates.
func TestTimelineOnRunPage(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/review/1"); !strings.Contains(body, `class="timeline done"`) || !strings.Contains(body, "Анализ <span class=\"tl-count\">1</span>") || !strings.Contains(body, "Команды") {
		t.Fatal("finished run page must show the phases")
	}
	if _, body := get(t, ts.URL+"/api/runs/1"); !strings.Contains(body, `"phase":"Команды"`) || !strings.Contains(body, `"Label":"Анализ"`) {
		t.Fatalf("api must carry the timeline: %s", body)
	}
}

// Task page preselects the bug mode for bug-like issues; the run page renders the bug analysis and the MR checklist.
func TestBugModeAndToMRPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	gl := svc.GitLab.(*testutil.FakeGitLab)
	bug := testutil.IssuePayload(9)
	bug["title"] = "Bug: export crashes"
	gl.Issues[9] = bug
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#9"})
	if _, body := get(t, ts.URL+"/-/issue/1"); !strings.Contains(body, `name="plan_mode" value="bug" checked`) || !strings.Contains(body, "похоже на баг") || !strings.Contains(body, "mode: planMode()") {
		t.Fatal("bug-like issue must preselect the bug mode")
	}
	fr.Outputs = []map[string]any{{"summary": "Export crashes on empty rows.", "expected": "file", "actual": "500", "reproduction": []any{"export with no rows"}, "path": []any{"Export::run"},
		"root_cause": "division by zero in `Export::avg()`", "evidence": []any{"src/Export.php:10"}, "fix": "guard zero", "risks": []any{}, "questions": []any{}, "estimate": "XS", "ask": []any{}}}
	code, out := postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan", "mode": "bug"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/task-plan/1"); !strings.Contains(body, "Разбор бага") || !strings.Contains(body, "Root cause") || !strings.Contains(body, "division by zero") || !strings.Contains(body, ">Исправить<") {
		t.Fatal("run page must render the bug analysis with the fix action")
	}
	if _, body := get(t, ts.URL+"/-/issue/1"); !strings.Contains(body, ">Исправить<") || !strings.Contains(body, "Открыть разбор") {
		t.Fatal("task page must offer the fix")
	}
	// Implementation with the fix handed over; the run page offers «Довести до MR» with the checklist and the prefilled form.
	fr.Outputs = []map[string]any{{"summary": "Guarded.", "changes": []any{map[string]any{"path": "src/Export.php", "description": "guard"}}, "tests": "none",
		"todo": []any{}, "self_review": "ok", "commit_message": "fix", "ask": []any{}}}
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "implement", "plan_run": "1"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if r, _ := svc.DB.GetRun(2); !strings.Contains(r.Prompt, "Root cause:") || !strings.Contains(r.Prompt, "division by zero") {
		t.Fatalf("bug analysis must be handed to the implementation: %s", r.Prompt)
	}
	_, body := get(t, ts.URL+"/-/task-implement/2")
	for _, want := range []string{`id="to-mr"`, "Тесты · не запускались", "Self-review · сделан агентом", "Чистый diff · есть незакоммиченные", "ветки ещё нет на origin", `createMRForm(event, '2')`, "## Что сделано", "Guarded."} {
		if !strings.Contains(body, want) {
			t.Fatalf("to-mr block missing %q", want)
		}
	}
	if strings.Contains(body, `<button type="submit" class="btn btn-primary tip"`) {
		t.Fatal("MR creation must be disabled until the branch is pushed")
	}
	if code, out := postJSON(t, ts.URL+"/api/runs/2/mr", map[string]any{"title": "T", "description": "D"}); code != 200 || out["url"] == "" {
		t.Fatalf("%d %v", code, out)
	}
}

// CI screen: failed pipelines with their jobs, the analysis flow and the fix action for own MRs.
func TestCIPage(t *testing.T) {
	ts, svc, fr := newServer(t)
	if _, body := get(t, ts.URL+"/ci"); !strings.Contains(body, "Нет известных упавших pipeline") {
		t.Fatal("empty state")
	}
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	_, body := get(t, ts.URL+"/ci")
	for _, want := range []string{`href="/ci" class="active"`, `<span class="pill hot">1</span>`, ">phpunit</a>", "allowed to fail", "не разбирался", `startRun('/api/mrs/1/runs', 'ci_analyze', this)`, `>Исправить CI без разбора<`} {
		if !strings.Contains(body, want) {
			t.Fatalf("ci page missing %q", want)
		}
	}
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "Pipeline failed") || !strings.Contains(body, ">Разобрать ошибки<") {
		t.Fatal("overview must lead to the CI analysis")
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `id="ci"`) || !strings.Contains(body, "хвост лога") || !strings.Contains(body, "Failed asserting") {
		t.Fatal("mr page must show the failed jobs with log tails")
	}
	fr.Outputs = []map[string]any{{"summary": "Fixture mismatch after the null-check change in AccountService.", "jobs": []any{map[string]any{"name": "phpunit", "kind": "code", "cause": "c", "fix": "f", "fixable_in_mr": true}}, "fixable_in_mr": true}}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "ci_analyze"})
	if code != 200 || out["redirect"] != "/-/ci-analyze/1" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/ci-analyze/1"); !strings.Contains(body, "Почему упал pipeline") || !strings.Contains(body, "можно исправить в MR") || !strings.Contains(body, `startRun('/api/mrs/1/runs', 'ci_fix', this, {analysis_run: '1'})`) {
		t.Fatal("analysis page must render the table and offer the fix")
	}
	if _, body := get(t, ts.URL+"/ci"); !strings.Contains(body, "Fixture mismatch") || !strings.Contains(body, `>Исправить CI<`) {
		t.Fatal("ci page must show the analysis and the fix for my MR")
	}
	fr.Outputs = []map[string]any{{"summary": "Fixed.", "changes": []any{}, "tests": "none", "todo": []any{}, "self_review": "ok", "unfixable": []any{map[string]any{"job": "lint", "reason": "flaky"}}, "commit_message": "", "ask": []any{}}}
	code, out = postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "ci_fix", "analysis_run": "1"})
	if code != 200 || out["redirect"] != "/-/ci-fix/2" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/ci-fix/2"); !strings.Contains(body, "Нельзя исправить в MR") || !strings.Contains(body, "flaky") || !strings.Contains(body, `id="workspace"`) {
		t.Fatal("fix page must list unfixable jobs and the workspace")
	}
}

// MR page of an own MR offers checkboxes and the «Исправить выбранные» bar; the run page shows per-finding results.
func TestFixSelectedPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `class="pick" data-finding="1"`) || !strings.Contains(body, `data-thread="1"`) || !strings.Contains(body, `fixSelected('1', this)`) {
		t.Fatal("own MR must offer the selection")
	}
	if _, body := get(t, ts.URL+"/-/review/1"); strings.Contains(body, `class="pick"`) {
		t.Fatal("the run page has no selection")
	}
	fr.Outputs = []map[string]any{{"summary": "Done.", "results": []any{map[string]any{"finding_id": 1.0, "status": "fixed", "note": "guarded"}}, "addressed": []any{}, "changes": []any{}, "tests": "none", "todo": []any{}, "self_review": "ok", "commit_message": "", "ask": []any{}}}
	code, out := postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "fix_findings", "findings": "1", "discussions": "", "notes": ""})
	if code != 200 || out["redirect"] != "/-/fix-findings/2" {
		t.Fatalf("%d %v", code, out)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/fix-findings/2"); !strings.Contains(body, "Выбранные замечания") || !strings.Contains(body, "Null deref") || !strings.Contains(body, ">исправлено<") || !strings.Contains(body, `id="workspace"`) {
		t.Fatal("fix page must list the per-finding results and the workspace")
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "исправлено агентом") || strings.Contains(body, `class="pick" data-finding="1"`) {
		t.Fatal("fixed finding shows the agent's verdict and is no longer selectable")
	}
}

// The MR page has the «Обсуждения» tab with the live threads: replies, resolved ones folded, the AI assessment of
// the latest review on its thread; the code under a thread and a re-read from GitLab are separate calls.
func TestDiscussionsTab(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	_, body := get(t, ts.URL+"/-/mr/1")
	for _, want := range []string{`data-tab="discussions"`, `data-pane="discussions" hidden`, `id="thread-1"`, "Открытые треды (2)", "Решённые треды (1)", "Комментарии без треда (1)",
		"because", "ждёт ответа", "Оценка AI:", "not addressed", `data-thread-code="1"`, `#note_100`, "LGTM overall", `<a href="#discussions">2 нерешённых обсуждений</a>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("MR page lacks %q", want)
		}
	}
	if strings.Contains(body, "Нерешённые обсуждения ревьюеров") {
		t.Fatal("the per-review snapshot of threads is replaced by the tab")
	}
	if _, run := get(t, ts.URL+"/-/review/1"); !strings.Contains(run, "Нерешённые обсуждения ревьюеров") {
		t.Fatal("the run page keeps what the review saw")
	}
	code, out := getJSON(t, ts.URL+"/api/threads/1/code")
	if code != 200 || out["code"].(map[string]any)["Line"].(float64) != 12 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/threads/sync", map[string]any{}); code != 200 || out["changed"] != false {
		t.Fatalf("%d %v", code, out)
	}
	// Reply with resolve from the page, the preview of the text, and a refused write shown as an error.
	gl := svc.GitLab.(*testutil.FakeGitLab)
	for _, want := range []string{`data-reply-thread="1"`, `replyThread('1', this, true)`, "Закрыть тред", "Открыть снова", "Превью"} {
		if !strings.Contains(body, want) {
			t.Fatalf("MR page lacks %q", want)
		}
	}
	if code, out := postJSON(t, ts.URL+"/api/markdown", map[string]any{"text": "**done** in `abc`"}); code != 200 || !strings.Contains(out["html"].(string), "<strong>done</strong>") && !strings.Contains(out["html"].(string), "<b>done</b>") {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := postJSON(t, ts.URL+"/api/threads/1/reply", map[string]any{"body": "fixed", "resolve": "1"}); code != 200 || len(gl.Replies) != 1 || len(gl.Resolves) != 1 {
		t.Fatalf("%d %v %v", code, gl.Replies, gl.Resolves)
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "Открытые треды (1)") || !strings.Contains(body, "Решённые треды (2)") || !strings.Contains(body, "fixed") {
		t.Fatal("the page follows the reply and the resolve")
	}
	gl.FailWrite = true
	if code, out := postJSON(t, ts.URL+"/api/threads/1/resolve", map[string]any{"resolved": "0"}); code != 400 || !strings.Contains(out["error"].(string), "403") {
		t.Fatalf("%d %v", code, out)
	}
	gl.FailWrite = false
	gl.SetThreads(42, nil)
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/threads/sync", map[string]any{}); code != 200 || out["changed"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "ещё никто ничего не написал") || !strings.Contains(body, "нет нерешённых обсуждений") {
		t.Fatal("empty state after the threads are gone")
	}
}

// The MR page shows who approved, reviewers, assignees and the merge readiness; Approve is offered in other
// people's MRs, Unapprove after it, Retry for a failed pipeline. The list has role badges and the role filter.
func TestApproveOnPages(t *testing.T) {
	ts, svc, _ := newServer(t)
	gl := svc.GitLab.(*testutil.FakeGitLab)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	_, body := get(t, ts.URL+"/-/mr/1")
	if strings.Contains(body, "approveMR(this)") || !strings.Contains(body, "Retry pipeline") || !strings.Contains(body, "@carol") || !strings.Contains(body, "<b>@alice (вы)</b>") {
		t.Fatal("own MR: no Approve, retry for the failed pipeline, people listed")
	}
	if code, _ := postJSON(t, ts.URL+"/api/mrs/1/pipeline/retry", map[string]any{}); code != 200 || gl.Writes[0] != "retry 9001" {
		t.Fatalf("%d %v", code, gl.Writes)
	}
	gl.MRs[42]["author"] = map[string]any{"username": "bob"}
	gl.MRs[42]["reviewers"] = []any{map[string]any{"username": "alice"}}
	gl.MRs[42]["detailed_merge_status"] = "discussions_not_resolved"
	postJSON(t, ts.URL+"/api/mrs/1/refresh", map[string]any{})
	if _, body = get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `data-sha="sha-1"`) || !strings.Contains(body, "approveMR(this)") || !strings.Contains(body, "есть нерешённые обсуждения") || strings.Contains(body, "Retry pipeline") {
		t.Fatal("someone else's MR offers Approve on the shown head and tells the readiness")
	}
	if _, list := get(t, ts.URL+"/mrs"); !strings.Contains(list, `data-role="assignee reviewer"`) || !strings.Contains(list, `data-facet="role"`) || !strings.Contains(list, ">reviewer</span>") {
		t.Fatal("the list carries roles and the role filter")
	}
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/approve", map[string]any{"sha": "old"}); code != 400 || !strings.Contains(out["error"].(string), "новые коммиты") {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := postJSON(t, ts.URL+"/api/mrs/1/approve", map[string]any{"sha": "sha-1"}); code != 200 {
		t.Fatal(code)
	}
	if _, body = get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, "Unapprove") || strings.Contains(body, "approveMR(this)") {
		t.Fatal("after the approval the page offers to take it back")
	}
	if code, _ := postJSON(t, ts.URL+"/api/mrs/1/unapprove", map[string]any{}); code != 200 || gl.Writes[len(gl.Writes)-1] != "unapprove 42" {
		t.Fatalf("%d %v", code, gl.Writes)
	}
}

func TestNoteText(t *testing.T) {
	got := noteText("see ![image](/uploads/abc/image.png){width=814 height=474} and [log](/uploads/d/log.txt)", "https://gl.example.com/g/p/-/merge_requests/5")
	if got != "see [вложение: image](https://gl.example.com/g/p/uploads/abc/image.png) and [вложение: log](https://gl.example.com/g/p/uploads/d/log.txt)" {
		t.Fatal(got)
	}
}

// Every result page states when the run started and finished, how long it took, agent + model, skill and tokens.
func TestRunFactsOnPages(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if run, _ := svc.DB.GetRun(1); run.Model != "claude-opus-4-1, claude-haiku-4-5" {
		t.Fatalf("models reported by the agent must be stored: %q", run.Model)
	}
	for _, path := range []string{"/-/mr/1", "/-/review/1"} {
		_, body := get(t, ts.URL+path)
		for _, want := range []string{`class="k">старт</span>`, `class="k">финиш</span>`, `class="k">длилось</span>`, `class="k">агент</span>claude · <span class="mono">claude-opus-4-1, claude-haiku-4-5</span>`, `class="k">skill</span><span class="mono">agent:mr-review</span>`, `class="k">токены</span>`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
	}
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#7"})
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(2); return r != nil && r.Status == db.StatusDone })
	if _, body := get(t, ts.URL+"/-/issue/1"); !strings.Contains(body, `class="k">длилось</span>`) || !strings.Contains(body, "CLAUDE.md + промпт") {
		t.Fatal("task page must show the facts of the plan run, including the missing skill")
	}
}

// The task list drops issues closed in GitLab; one an agent is still working on stays until the run ends.
func TestOpenIssuesFiltersClosed(t *testing.T) {
	item := func(iid int64, state string, last *db.LastRun) db.IssueListItem {
		return db.IssueListItem{Issue: db.Issue{IID: iid, State: state}, Last: last}
	}
	got := openIssues([]db.IssueListItem{
		item(1, "opened", nil),
		item(2, "closed", nil),
		item(3, "closed", &db.LastRun{Status: db.StatusRunning}),
		item(4, "closed", &db.LastRun{Status: db.StatusDone}),
		item(5, "", nil), // added by hand, not fetched yet
	})
	var iids []int64
	for _, issue := range got {
		iids = append(iids, issue.IID)
	}
	if fmt.Sprint(iids) != "[1 3 5]" {
		t.Fatalf("openIssues kept %v", iids)
	}
}

// The MR page: «на что обратить внимание» is stored and goes with the review; findings get a code excerpt, inline
// actions and «Отправить в GitLab»; the page carries its data version for the live refresh.
func TestReviewFocusFindingsAndLiveRefresh(t *testing.T) {
	ts, svc, fr := newServer(t)
	postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "!42"})
	if code, out := postJSON(t, ts.URL+"/api/mrs/1/focus", map[string]any{"focus": "только миграции"}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, `id="review-focus"`) || !strings.Contains(body, ">только миграции</textarea>") ||
		!strings.Contains(body, `data-autorefresh="1"`) || !strings.Contains(body, `data-scope="mr" data-scope-id="1"`) || !strings.Contains(body, "Ревью и действия") {
		t.Fatal("mr page: focus field, action block and live-refresh attributes")
	}
	_, v1 := getJSON(t, ts.URL+"/api/version?scope=mr&id=1")
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	// The focus sent with the start replaces the stored one.
	postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full", "focus": "API"})
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(1); return r != nil && r.Status == db.StatusDone })
	if r, _ := svc.DB.GetRun(1); r.Notes != "API" {
		t.Fatalf("run notes %q", r.Notes)
	}
	if _, v2 := getJSON(t, ts.URL+"/api/version?scope=mr&id=1"); v2["version"] == v1["version"] {
		t.Fatal("version must change when a run finishes")
	}
	_, body := get(t, ts.URL+"/-/mr/1")
	for _, want := range []string{`data-finding-code="1"`, `data-post-finding="1"`, "Отправить в GitLab", "ляжет на строку", "**Null deref**", ">Ложное</button>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mr page lacks %q", want)
		}
	}
	if code, out := getJSON(t, ts.URL+"/api/findings/1/code"); code != 200 || out["code"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	code, out := postJSON(t, ts.URL+"/api/findings/1/gitlab", map[string]any{"body": "Поправь, пожалуйста"})
	if code != 200 || out["on_line"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if _, body := get(t, ts.URL+"/-/mr/1"); !strings.Contains(body, ">в GitLab</a>") || strings.Contains(body, `data-post-finding="1"`) {
		t.Fatal("a posted finding links to its GitLab note instead of offering the editor")
	}
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	code, body := get(t, url)
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	return code, out
}

// Remote access: this machine is always let in; another device needs the key once (link / QR), then the cookie.
func TestRemoteAccessNeedsKey(t *testing.T) {
	settings, _ := testutil.Settings(t)
	settings.Host = "0.0.0.0"
	database, err := db.Open(settings.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = database.Migrate()
	svc := app.New(settings, database, testutil.NewFakeGitLab(), nil)
	t.Cleanup(func() { svc.Shutdown(); database.Close() })
	srv, err := New(svc, "test", nil)
	if err != nil || srv.accessKey == "" {
		t.Fatalf("key %q %v", srv.accessKey, err)
	}
	if again, _ := loadAccessKey(settings); again != srv.accessKey {
		t.Fatal("the generated key must persist")
	}
	h := srv.Handler()
	do := func(target, remote, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr = remote
		r.Host = "192.168.10.17:8765" // the browser addresses the dashboard by its IP
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: accessCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("/mrs", "127.0.0.1:5000", ""); w.Code != 200 {
		t.Fatalf("local: %d", w.Code)
	}
	if w := do("/mrs", "192.168.10.20:5000", ""); w.Code != 401 || !strings.Contains(w.Body.String(), "ещё не подключено") {
		t.Fatalf("phone without key: %d", w.Code)
	}
	if w := do("/api/version", "192.168.10.20:5000", ""); w.Code != 401 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("api without key: %d %s", w.Code, w.Body.String())
	}
	if w := do("/mrs?key=wrong", "192.168.10.20:5000", ""); w.Code != 401 {
		t.Fatalf("wrong key: %d", w.Code)
	}
	w := do("/mrs?key="+srv.accessKey+"&x=1", "192.168.10.20:5000", "")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mrs?x=1" || !strings.Contains(w.Header().Get("Set-Cookie"), accessCookie+"="+srv.accessKey) {
		t.Fatalf("key link: %d %v", w.Code, w.Header())
	}
	if w := do("/mrs", "192.168.10.20:5000", srv.accessKey); w.Code != 200 {
		t.Fatalf("phone with cookie: %d", w.Code)
	}
	if w := do("/static/style.css", "192.168.10.20:5000", ""); w.Code != 200 {
		t.Fatalf("static: %d", w.Code)
	}
	if w := do("/phone", "127.0.0.1:5000", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "/?key="+srv.accessKey) || !strings.Contains(w.Body.String(), `<svg class="qr"`) {
		t.Fatal("phone page must show the link with the key and a QR code")
	}
	if w := do("/phone", "192.168.10.20:5000", srv.accessKey); strings.Contains(w.Body.String(), "key=") {
		t.Fatal("the phone page must not show the key to another device")
	}
}

func TestPhonePageWhenRemoteAccessIsOff(t *testing.T) {
	ts, _, _ := newServer(t)
	if code, body := get(t, ts.URL+"/phone"); code != 200 || !strings.Contains(body, "REMOTE_ACCESS=1") {
		t.Fatalf("%d", code)
	}
}

// Another web site must not drive the dashboard through the browser (CSRF), nor reach it by a foreign host name
// (DNS rebinding); the dashboard's own pages and curl keep working.
func TestCrossSiteAndForeignHostRefused(t *testing.T) {
	ts, _, _ := newServer(t)
	h := ts.Config.Handler
	do := func(method, host string, headers map[string]string) int {
		r := httptest.NewRequest(method, "/api/mrs/sync", strings.NewReader(`{}`))
		r.Host = host
		r.RemoteAddr = "127.0.0.1:5000"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("POST", "127.0.0.1:8765", map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"}); c != 403 {
		t.Fatalf("cross-site POST: %d", c)
	}
	if c := do("POST", "127.0.0.1:8765", map[string]string{"Sec-Fetch-Site": "cross-site"}); c != 403 {
		t.Fatalf("sec-fetch cross-site: %d", c)
	}
	if c := do("GET", "evil.example:8765", nil); c != 403 {
		t.Fatalf("rebinding host: %d", c)
	}
	if c := do("POST", "127.0.0.1:8765", map[string]string{"Origin": "http://127.0.0.1:8765", "Sec-Fetch-Site": "same-origin"}); c == 403 {
		t.Fatal("same-origin POST refused")
	}
	if c := do("POST", "localhost:8765", nil); c == 403 {
		t.Fatal("curl-like POST refused")
	}
}

// GitLab does not answer (VPN off): the header says so and the MR page warns next to the launch buttons.
func TestOfflineChipAndLaunchWarning(t *testing.T) {
	ts, svc, _ := newServer(t)
	// The API host is a local port nobody listens on (set before the project is first resolved and cached).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svc.Settings.GitLabHost = l.Addr().String()
	l.Close()
	mr, err := svc.AddMR("!42")
	if err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, ts.URL+"/"); strings.Contains(body, "data-offline") {
		t.Fatal("no offline chip while the check is off")
	}
	svc.Settings.GitLabReachCheck = true
	if svc.CheckGitLab() == "" {
		t.Fatal("closed port must be offline")
	}
	_, body := get(t, ts.URL+mrPath(mr.ID))
	if !strings.Contains(body, "GitLab недоступен — VPN?") || !strings.Contains(body, "включён ли VPN? Новый код не скачается") {
		t.Fatal("offline chip and launch warning expected")
	}
}

// A Bash prompt shows the command as text (&& not escaped) and offers «Разрешать такие до конца запуска».
func TestBashPromptShowsCommand(t *testing.T) {
	ts, svc, fr := newServer(t)
	mr, _ := svc.AddMR("!42")
	input, _ := json.Marshal(map[string]string{"command": "make build && make test"})
	fr.Ask = []runner.PermissionRequest{{ToolName: "Bash", Description: "build", Input: input}}
	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	id, _ := svc.StartReview(mr.ID, db.KindReviewFull, "", 0)
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(id); return r.Status == db.StatusWaiting })
	_, body := get(t, ts.URL+fmt.Sprintf("/run/%d", id))
	box := body[strings.Index(body, "approval-box"):]
	box = box[:strings.Index(box, "deny-note")]
	if !strings.Contains(box, `<pre class="call mono" data-command>make build &amp;&amp; make test</pre>`) || strings.Contains(box, `\`+"u0026") {
		t.Fatalf("command must be shown as text: %s", box)
	}
	if strings.Contains(body, "allow_run") {
		t.Fatal("compound commands are not offered as a group")
	}
	pending, _ := svc.DB.PendingApproval(id)
	_ = svc.Decide(pending.ID, "allow", "")
}

// Task page: saved «Указания агенту», «Остановить» next to an active run, the stand card folded; the run page
// always has the «сейчас:» line for live updates.
func TestTaskPageNotesStopAndRunNow(t *testing.T) {
	ts, svc, fr := newServer(t)
	issue, _ := svc.AddIssue("#7")
	if code, _ := postJSON(t, ts.URL+fmt.Sprintf("/api/issues/%d/notes", issue.ID), map[string]any{"notes": "без миграций"}); code != 200 {
		t.Fatal(code)
	}
	fr.Block = make(chan struct{})
	defer close(fr.Block)
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	id, err := svc.StartPlan(issue.ID, "", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { r, _ := svc.DB.GetRun(id); return r.Status == db.StatusRunning })
	_, body := get(t, ts.URL+issuePath(issue.ID))
	if !strings.Contains(body, `data-saved="без миграций"`) || !strings.Contains(body, ">без миграций</textarea>") {
		t.Fatal("saved notes must be shown")
	}
	if !strings.Contains(body, fmt.Sprintf(`post('/api/runs/%d/cancel'`, id)) {
		t.Fatal("an active run can be stopped from the task page")
	}
	_, body = get(t, ts.URL+fmt.Sprintf("/run/%d", id))
	if !strings.Contains(body, `id="progress-now"`) {
		t.Fatal("the «сейчас:» line is always there")
	}
}

// Fix buttons follow the server rule: the author and an assignee see them, a reviewer does not.
func TestFixButtonsFollowRights(t *testing.T) {
	ts, svc, _ := newServer(t)
	gl := svc.GitLab.(*testutil.FakeGitLab)
	reviewing := testutil.MRPayload(44, "sha-44")
	reviewing["author"] = map[string]any{"username": "bob"}
	reviewing["assignees"] = []any{}
	reviewing["reviewers"] = []any{map[string]any{"username": "alice"}}
	gl.MRs[44] = reviewing
	assigned := testutil.MRPayload(43, "sha-43")
	assigned["author"] = map[string]any{"username": "bob"}
	gl.MRs[43] = assigned
	asReviewer, _ := svc.AddMR("!44")
	asAssignee, _ := svc.AddMR("!43")
	if _, body := get(t, ts.URL+mrPath(asReviewer.ID)); strings.Contains(body, "'ci_fix'") {
		t.Fatal("a reviewer gets no «Исправить CI»")
	}
	if _, body := get(t, ts.URL+mrPath(asAssignee.ID)); !strings.Contains(body, "'ci_fix'") {
		t.Fatal("an assignee gets «Исправить CI»")
	}
}

// CSS and JS are linked with the version and served no-cache: after an upgrade the browser takes the new files.
func TestStaticAssetsVersioned(t *testing.T) {
	ts, _, _ := newServer(t)
	_, body := get(t, ts.URL+"/")
	if !strings.Contains(body, `/static/style.css?v=test"`) || !strings.Contains(body, `/static/app.js?v=test"`) {
		t.Fatal("static links must carry the version")
	}
	resp, err := http.Get(ts.URL + "/static/style.css?v=test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

// Header switcher: «Все проекты» shows every MR with a project badge, a chosen project (cookie) only its own.
// The projects page adds a repository by path and saves its settings.
func TestProjectSwitcherAndSettings(t *testing.T) {
	ts, svc, _ := newServer(t)
	gl := svc.GitLab.(*testutil.FakeGitLab)
	otherRoot := testutil.GitRepo(t, filepath.Join(t.TempDir(), "other"))
	exec.Command("git", "-C", otherRoot, "remote", "set-url", "origin", "git@gitlab.example.com:group/other.git").Run()
	code, out := postJSON(t, ts.URL+"/api/projects", map[string]any{"repo_root": otherRoot})
	if code != 200 {
		t.Fatalf("add project: %d %v", code, out)
	}
	if code, _ := postJSON(t, ts.URL+"/api/projects", map[string]any{"repo_root": otherRoot}); code == 200 {
		t.Fatal("the same repository cannot be added twice")
	}
	projects := svc.Projects()
	if len(projects) != 2 || projects[1].ProjectPath != "group/other" || projects[1].GitLabHost != "gitlab.example.com" {
		t.Fatalf("%+v", projects)
	}
	other := projects[1]
	payload := testutil.MRPayload(50, "sha-50")
	payload["web_url"] = "https://gitlab.example.com/group/other/-/merge_requests/50"
	payload["references"] = map[string]any{"full": "group/other!50"}
	gl.MRs[50] = payload
	svc.AddMR("!42")
	svc.AddMR("group/other!50")

	_, all := get(t, ts.URL+"/mrs")
	if !strings.Contains(all, "!42") || !strings.Contains(all, "!50") || !strings.Contains(all, `badge-project">other<`) || !strings.Contains(all, `id="project-switch"`) {
		t.Fatal("all projects: both MRs with badges and the switcher")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/mrs", nil)
	req.AddCookie(&http.Cookie{Name: "project", Value: fmt.Sprint(other.ID)})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), ">!42<") || !strings.Contains(string(body), ">!50<") || strings.Contains(string(body), "badge-project") {
		t.Fatal("a chosen project shows only its MRs, without badges")
	}

	if code, _ := postJSON(t, ts.URL+fmt.Sprintf("/api/projects/%d", other.ID), map[string]any{"name": "Другой", "base_branch": "main", "issue_projects": "group/tracker/"}); code != 200 {
		t.Fatal(code)
	}
	if p, _ := svc.DB.GetProject(other.ID); p.Name != "Другой" || p.BaseBranch != "main" || p.IssueProjects != "group/tracker" {
		t.Fatalf("%+v", p)
	}
	if _, page := get(t, ts.URL+"/projects"); !strings.Contains(page, "Другой") || !strings.Contains(page, "по умолчанию") {
		t.Fatal("projects page")
	}
	req, _ = http.NewRequest("DELETE", ts.URL+fmt.Sprintf("/api/projects/%d", projects[0].ID), nil)
	if resp, _ := http.DefaultClient.Do(req); resp == nil || resp.StatusCode == 200 {
		t.Fatal("the default project cannot be deleted")
	}
}

// Telegram is off without a token: the environment page says how to turn it on; the test button answers clearly.
// /api/version reports how many runs wait for the developer (the «(N)» in the tab title).
func TestTelegramSectionAndWaitingCount(t *testing.T) {
	ts, _, _ := newServer(t)
	if _, body := get(t, ts.URL+"/doctor"); !strings.Contains(body, `id="telegram"`) || !strings.Contains(body, "TELEGRAM_BOT_TOKEN") {
		t.Fatal("telegram section")
	}
	if code, out := postJSON(t, ts.URL+"/api/telegram/test", map[string]any{}); code == 200 || !strings.Contains(fmt.Sprint(out["error"]), "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("%d %v", code, out)
	}
	_, body := get(t, ts.URL+"/api/version")
	if !strings.Contains(body, `"waiting":0`) {
		t.Fatal(body)
	}
}
