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
	if code, body := get(t, ts.URL+"/"); code != 200 || !strings.Contains(body, "Пока нет merge requests") || !strings.Contains(body, "agent:mr-review") {
		t.Fatalf("%d %s", code, body[:200])
	}
	if code, body := get(t, ts.URL+"/api/health"); code != 200 || !strings.Contains(body, svc.Settings.ProjectRoot) {
		t.Fatalf("%d %s", code, body)
	}
	code, out := postJSON(t, ts.URL+"/api/mrs", map[string]any{"url": "https://gitlab.example.com/group/sub/project/-/merge_requests/42"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	redirect := out["redirect"].(string)
	if code, body := get(t, ts.URL+redirect); code != 200 || !strings.Contains(body, "Исправить замечания ревьюеров (2)") {
		t.Fatalf("mr page: %d (author alice == current user -> fix button expected)", code)
	}
	if code, body := get(t, ts.URL+"/"); code != 200 || !strings.Contains(body, "MR 42") || !strings.Contains(body, "data-tip=") {
		t.Fatalf("%d", code)
	}

	fr.Outputs = []map[string]any{testutil.FullReviewOutput("sha-1")}
	code, out = postJSON(t, ts.URL+"/api/mrs/1/runs", map[string]any{"kind": "full", "runner": "claude"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	runURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+strings.TrimPrefix(runURL, "/run/"))
		return strings.Contains(body, `"status":"done"`)
	})
	if code, body := get(t, ts.URL+runURL); code != 200 || !strings.Contains(body, "Null deref") || !strings.Contains(body, "Нужны правки") || !strings.Contains(body, "Вопрос агенту") {
		t.Fatalf("run page %d", code)
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
	if code, body := get(t, ts.URL+"/doctor"); code != 200 || !strings.Contains(body, "Review skill") {
		t.Fatalf("doctor %d", code)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/mrs/1", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatal("delete")
	}
	if code, _ := get(t, ts.URL+"/mr/1"); code != 404 {
		t.Fatal("expected 404 after delete")
	}
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
	if code, body := get(t, ts.URL+"/issue/1"); code != 200 || !strings.Contains(body, "group/sub/project#7") || !strings.Contains(body, "Реализовать в worktree") {
		t.Fatalf("%d", code)
	}
	fr.Outputs = []map[string]any{testutil.PlanOutput()}
	code, out = postJSON(t, ts.URL+"/api/issues/1/runs", map[string]any{"kind": "plan", "notes": "n"})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	runURL := out["redirect"].(string)
	testutil.WaitFor(t, func() bool {
		_, body := get(t, ts.URL+"/api/runs/"+strings.TrimPrefix(runURL, "/run/"))
		return strings.Contains(body, `"status":"done"`)
	})
	if code, body := get(t, ts.URL+runURL); code != 200 || !strings.Contains(body, "План решения") || !strings.Contains(body, "Plan summary") {
		t.Fatalf("plan page %d", code)
	}
	if code, out := postJSON(t, ts.URL+"/api/issues/sync", map[string]any{}); code != 200 || out["synced"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}
