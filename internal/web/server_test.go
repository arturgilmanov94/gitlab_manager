package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		!strings.Contains(body, `data-mine="1" data-draft="0" data-ai="never"`) || strings.Contains(body, ">backend<") ||
		!strings.Contains(body, `data-facet="mine"`) || !strings.Contains(body, `data-facet="ai"`) || !strings.Contains(body, `class="fchip label-orange" data-value="bug"`) {
		t.Fatal("list must show highlighted labels as badges, row facets and filter chips")
	}
	postJSON(t, ts.URL+"/api/issues", map[string]any{"url": "#7"})
	if _, body := get(t, ts.URL+"/issues"); !strings.Contains(body, `data-labels=""`) || !strings.Contains(body, `data-facet="labels"`) || !strings.Contains(body, ">backend<") {
		t.Fatal("issues list must carry label facets and chips and keep the other labels as text")
	}
	// The overview lists the own MR with a failed pipeline; the never-reviewed MR is not offered for review (own MR).
	if _, body := get(t, ts.URL+"/"); !strings.Contains(body, "Pipeline failed") || !strings.Contains(body, "Открыть pipeline") {
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
	if code, out := postJSON(t, ts.URL+"/api/runs/1/ask", map[string]any{"question": "why?"}); code != 200 || out["answer"] != "Because." {
		t.Fatalf("%d %v", code, out)
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
	if _, body := get(t, ts.URL+"/issue/1"); !strings.Contains(body, "Готово к MR") || !strings.Contains(body, "Подготовить MR") {
		t.Fatal("issue page must show ready state")
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
