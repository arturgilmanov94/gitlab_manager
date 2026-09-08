// Package web serves the dashboard: server-rendered pages plus a small JSON API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"mr-review/internal/app"
	"mr-review/internal/db"
	"mr-review/internal/doctor"
	"mr-review/internal/runner"
	"mr-review/internal/skill"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server holds the parsed templates and the service.
type Server struct {
	svc      *app.Service
	version  string
	runners  []runner.Runner
	pages    map[string]*template.Template
	mux      *http.ServeMux
	started  time.Time
	basePage func() pageBase
}

type pageBase struct {
	Version     string
	ProjectRoot string
	Skill       *skill.Skill
	Runners     []runner.Info
	Username    string
	Counts      map[string]int64
	Nav         string
	Title       string
	Started     time.Time
}

// New builds the HTTP server.
func New(svc *app.Service, version string, runners []runner.Runner) (*Server, error) {
	s := &Server{svc: svc, version: version, runners: runners, pages: map[string]*template.Template{}, mux: http.NewServeMux(), started: time.Now()}
	funcs := template.FuncMap{
		"shortSHA": func(sha string) string {
			if len(sha) > 10 {
				return sha[:10]
			}
			return sha
		},
		"when":      formatWhen,
		"kindLabel": kindLabel,
		"kindTip":   kindTip,
		"verdict":   verdictLabel,
		"seconds": func(ms int64) string {
			if ms <= 0 {
				return "—"
			}
			return fmt.Sprintf("%ds", ms/1000)
		},
		"money": func(v float64) string {
			if v <= 0 {
				return "—"
			}
			return fmt.Sprintf("$%.2f", v)
		},
		"deref": func(p *int64) string {
			if p == nil {
				return ""
			}
			return strconv.FormatInt(*p, 10)
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"prettyJSON": func(s string) string {
			var v any
			if json.Unmarshal([]byte(s), &v) != nil {
				return s
			}
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
		"lower":    strings.ToLower,
		"divCents": func(cents int64) float64 { return float64(cents) / 100 },
		"tokens":   formatTokens,
		"tokensTip": func(in, out, read, write int64) string {
			return fmt.Sprintf("Токены за запуск, суммарно по всем моделям (агент + субагенты)\nвход: %s · выход: %s · чтение кэша: %s · запись кэша: %s",
				formatTokens(in), formatTokens(out), formatTokens(read), formatTokens(write))
		},
	}
	partials, err := fs.Glob(assets, "templates/_*.html")
	if err != nil {
		return nil, err
	}
	for _, page := range []string{"index", "issues", "mr", "issue", "run", "doctor"} {
		files := append([]string{"templates/layout.html", "templates/" + page + ".html"}, partials...)
		t, err := template.New("layout").Funcs(funcs).ParseFS(assets, files...)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	s.routes()
	return s, nil
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	static, _ := fs.Sub(assets, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	s.mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusFound)
	})

	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /issues", s.issues)
	s.mux.HandleFunc("GET /mr/{id}", s.mrPage)
	s.mux.HandleFunc("GET /issue/{id}", s.issuePage)
	s.mux.HandleFunc("GET /run/{id}", s.runPage)
	s.mux.HandleFunc("GET /run/{id}/log", s.runLog)
	s.mux.HandleFunc("GET /doctor", s.doctorPage)

	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.version, "project_root": s.svc.Settings.ProjectRoot})
	})
	s.mux.HandleFunc("GET /api/doctor", func(w http.ResponseWriter, r *http.Request) {
		rep := doctor.Run(s.svc.Settings, s.version, s.svc.GitLab, s.runners)
		writeJSON(w, 200, map[string]any{"ready": rep.Ready(), "checks": rep.Checks})
	})

	s.mux.HandleFunc("POST /api/mrs", s.apiAddMR)
	s.mux.HandleFunc("POST /api/mrs/sync", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.svc.SyncMRs()
		s.result(w, map[string]any{"synced": res.Synced, "username": res.Username, "project": res.Project}, err)
	})
	s.mux.HandleFunc("POST /api/mrs/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		mr, err := s.svc.RefreshMR(pathID(r))
		s.result(w, map[string]any{"mr": mr}, err)
	})
	s.mux.HandleFunc("DELETE /api/mrs/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.DeleteMR(pathID(r)))
	})
	s.mux.HandleFunc("POST /api/mrs/{id}/runs", s.apiStartMRRun)

	s.mux.HandleFunc("POST /api/issues", s.apiAddIssue)
	s.mux.HandleFunc("POST /api/issues/sync", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.svc.SyncIssues()
		s.result(w, map[string]any{"synced": res.Synced, "username": res.Username, "project": res.Project}, err)
	})
	s.mux.HandleFunc("POST /api/issues/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		issue, err := s.svc.RefreshIssue(pathID(r))
		s.result(w, map[string]any{"issue": issue}, err)
	})
	s.mux.HandleFunc("DELETE /api/issues/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.DeleteIssue(pathID(r)))
	})
	s.mux.HandleFunc("POST /api/issues/{id}/runs", s.apiStartIssueRun)

	s.mux.HandleFunc("GET /api/runs/{id}", s.apiRun)
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": s.svc.Cancel(pathID(r))})
	})
	s.mux.HandleFunc("POST /api/runs/{id}/ask", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		answer, err := s.svc.Ask(pathID(r), body["question"])
		s.result(w, map[string]any{"answer": answer}, err)
	})
	s.mux.HandleFunc("POST /api/runs/{id}/commit", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.svc.Commit(pathID(r), readBody(r)["message"])
		s.result(w, map[string]any{"output": out}, err)
	})
	s.mux.HandleFunc("POST /api/runs/{id}/push", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.svc.Push(pathID(r))
		s.result(w, map[string]any{"output": out}, err)
	})
	s.mux.HandleFunc("POST /api/runs/{id}/mr", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		url, err := s.svc.CreateMR(pathID(r), body["title"], body["description"])
		s.result(w, map[string]any{"url": url}, err)
	})
	s.mux.HandleFunc("DELETE /api/runs/{id}/worktree", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.RemoveWorktree(pathID(r)))
	})
}

// ---------------------------------------------------------------------------------- pages

func (s *Server) base(nav, title string) pageBase {
	return pageBase{
		Version:     s.version,
		ProjectRoot: s.svc.Settings.ProjectRoot,
		Skill:       s.svc.Skill(),
		Runners:     s.svc.RunnerInfos(),
		Username:    s.svc.CurrentUser(),
		Counts:      s.svc.DB.Counts(),
		Nav:         nav,
		Title:       title,
		Started:     s.started,
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	mrs, err := s.svc.DB.ListMRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, "index", map[string]any{"Base": s.base("mrs", "Merge requests"), "MRs": mrs})
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request) {
	issues, err := s.svc.DB.ListIssues()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, "issues", map[string]any{"Base": s.base("issues", "Задачи"), "Issues": issues})
}

func (s *Server) mrPage(w http.ResponseWriter, r *http.Request) {
	mr, _ := s.svc.DB.GetMR(pathID(r))
	if mr == nil {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.svc.DB.ListRunsForMR(mr.ID)
	latest, _ := s.svc.DB.LatestDoneReview(mr.ID)
	var findings []db.Finding
	var discussions []db.Discussion
	if latest != nil {
		findings, _ = s.svc.DB.ListFindings(latest.ID)
		discussions, _ = s.svc.DB.ListDiscussions(latest.ID)
	}
	active, _ := s.svc.DB.ActiveRunForMR(mr.ID)
	username := s.svc.CurrentUser()
	s.render(w, "mr", map[string]any{
		"Base": s.base("mrs", fmt.Sprintf("!%d %s", mr.IID, mr.Title)), "MR": mr, "Runs": runs, "Latest": latest,
		"Findings": findings, "Discussions": discussions, "Active": active,
		"Stale":  latest != nil && latest.HeadSHA != "" && latest.HeadSHA != mr.HeadSHA,
		"IsMine": username != "" && mr.Author == username,
	})
}

func (s *Server) issuePage(w http.ResponseWriter, r *http.Request) {
	issue, _ := s.svc.DB.GetIssue(pathID(r))
	if issue == nil {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.svc.DB.ListRunsForIssue(issue.ID)
	active, _ := s.svc.DB.ActiveRunForIssue(issue.ID)
	s.render(w, "issue", map[string]any{
		"Base": s.base("issues", fmt.Sprintf("#%d %s", issue.IID, issue.Title)), "Issue": issue, "Runs": runs, "Active": active,
		"DefaultBranch": issue.Ref(), "BaseBranch": s.svc.Settings.BaseBranch,
	})
}

type planResult struct {
	Summary   string `json:"summary"`
	Steps     []string
	Files     []struct{ Path, Change string }
	Risks     []string
	Questions []string
	Estimate  string
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	run, _ := s.svc.DB.GetRun(pathID(r))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"Base": s.base("", fmt.Sprintf("Run #%d", run.ID)), "Run": run}
	if run.MRID != nil {
		data["MR"], _ = s.svc.DB.GetMR(*run.MRID)
	}
	if run.IssueID != nil {
		data["Issue"], _ = s.svc.DB.GetIssue(*run.IssueID)
	}
	if run.IsReview() {
		data["Findings"], _ = s.svc.DB.ListFindings(run.ID)
		data["Discussions"], _ = s.svc.DB.ListDiscussions(run.ID)
	}
	if run.Kind == db.KindPlan && run.ResultJSON != "" {
		var plan map[string]any
		_ = json.Unmarshal([]byte(run.ResultJSON), &plan)
		data["Plan"] = plan
	}
	if run.IsEdit() {
		if run.ResultJSON != "" {
			var report map[string]any
			_ = json.Unmarshal([]byte(run.ResultJSON), &report)
			data["Report"] = report
		}
		if !run.Active() {
			data["Worktree"] = s.svc.Worktree(run)
		}
	}
	data["Messages"], _ = s.svc.DB.ListMessages(run.ID)
	data["LogTail"] = tail(run.LogPath, 12000)
	s.render(w, "run", data)
}

func (s *Server) runLog(w http.ResponseWriter, r *http.Request) {
	run, _ := s.svc.DB.GetRun(pathID(r))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if run.LogPath == "" {
		fmt.Fprint(w, "(no log yet)")
		return
	}
	data, err := os.ReadFile(run.LogPath)
	if err != nil {
		fmt.Fprint(w, "(no log yet)")
		return
	}
	_, _ = w.Write(data)
}

func (s *Server) doctorPage(w http.ResponseWriter, r *http.Request) {
	rep := doctor.Run(s.svc.Settings, s.version, s.svc.GitLab, s.runners)
	s.render(w, "doctor", map[string]any{"Base": s.base("doctor", "Doctor"), "Report": rep})
}

// ---------------------------------------------------------------------------------- api

func (s *Server) apiAddMR(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	mr, err := s.svc.AddMR(firstNonEmpty(body["url"], body["reference"]))
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mr": mr, "redirect": fmt.Sprintf("/mr/%d", mr.ID)})
}

func (s *Server) apiAddIssue(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	issue, err := s.svc.AddIssue(firstNonEmpty(body["url"], body["reference"]))
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "issue": issue, "redirect": fmt.Sprintf("/issue/%d", issue.ID)})
}

func (s *Server) apiStartMRRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	id := pathID(r)
	var runID int64
	var err error
	switch body["kind"] {
	case "quick":
		runID, err = s.svc.StartReview(id, db.KindReviewQuick, body["runner"])
	case "full", "":
		runID, err = s.svc.StartReview(id, db.KindReviewFull, body["runner"])
	case "verify":
		runID, err = s.svc.StartReview(id, db.KindReviewVerify, body["runner"])
	case "fix":
		runID, err = s.svc.StartFixComments(id, body["runner"], body["notes"])
	default:
		writeJSON(w, 400, map[string]any{"error": "kind must be quick, full, verify or fix"})
		return
	}
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID, "redirect": fmt.Sprintf("/run/%d", runID)})
}

func (s *Server) apiStartIssueRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	id := pathID(r)
	var runID int64
	var err error
	switch body["kind"] {
	case "plan", "":
		runID, err = s.svc.StartPlan(id, body["runner"], body["notes"])
	case "implement":
		runID, err = s.svc.StartImplement(id, body["runner"], body["notes"], body["branch"])
	default:
		writeJSON(w, 400, map[string]any{"error": "kind must be plan or implement"})
		return
	}
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID, "redirect": fmt.Sprintf("/run/%d", runID)})
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	run, _ := s.svc.DB.GetRun(pathID(r))
	if run == nil {
		writeJSON(w, 404, map[string]any{"error": "run not found"})
		return
	}
	findings, _ := s.svc.DB.ListFindings(run.ID)
	writeJSON(w, 200, map[string]any{
		"id": run.ID, "kind": run.Kind, "status": run.Status, "verdict": run.Verdict, "summary": run.Summary,
		"error": run.Error, "runner": run.Runner, "cost_usd": run.CostUSD, "tokens": run.TotalTokens(), "duration_ms": run.DurationMs,
		"started_at": run.StartedAt, "finished_at": run.FinishedAt, "findings": len(findings), "session_id": run.SessionID,
	})
}

// ---------------------------------------------------------------------------------- helpers

func (s *Server) result(w http.ResponseWriter, payload map[string]any, err error) {
	if err != nil {
		var ue *app.UserError
		status := 500
		if errors.As(err, &ue) {
			status = 400
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["ok"] = true
	writeJSON(w, 200, payload)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readBody(r *http.Request) map[string]string {
	out := map[string]string{}
	var raw map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&raw); err != nil {
		return out
	}
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func tail(path string, limit int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > limit {
		return string(data[len(data)-limit:])
	}
	return string(data)
}

// formatTokens renders 1234 -> "1.2k", 6_400_000 -> "6.4M", 0 -> "—".
func formatTokens(n int64) string {
	switch {
	case n <= 0:
		return "—"
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}

func formatWhen(value string) string {
	if value == "" {
		return "—"
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Local().Format("02.01 15:04")
		}
	}
	return value
}

func kindLabel(kind string) string {
	switch kind {
	case db.KindReviewQuick:
		return "Быстрое ревью"
	case db.KindReviewFull:
		return "Полное ревью"
	case db.KindReviewVerify:
		return "Проверка исправлений"
	case db.KindFixComments:
		return "Исправление замечаний"
	case db.KindPlan:
		return "План"
	case db.KindImplement:
		return "Реализация"
	}
	return kind
}

func kindTip(kind string) string {
	switch kind {
	case "quick":
		return "Лёгкий проход. Агент читает diff MR и нерешённые обсуждения, соседний код открывает только когда без него правку не оценить. В отчёт попадают CRITICAL, HIGH и MEDIUM, резюме короткое.\n\nЗапуск read-only из корня проекта: подхватываются правила проекта и агент ревью. Ничего не меняется ни локально, ни в GitLab. Обычно 1–3 минуты."
	case "full":
		return "Полный проход по правилам проекта (CLAUDE.md, агент mr-review): diff, контекст затронутого кода, нерешённые обсуждения, регрессии, обработка ошибок, безопасность, соответствие паттернам проекта. Все уровни severity, конкретные предложения фиксов.\n\nRead-only, из корня проекта. Обычно 3–10 минут."
	case "verify":
		return "Берёт открытые findings последнего завершённого ревью и проверяет каждый на текущем head MR: fixed / open / obsolete с обоснованием. Дополнительно ищет только новые проблемы, внесённые новыми коммитами.\n\nИмеет смысл после того, как в MR появились коммиты. Read-only."
	case "fix":
		return "Только для ваших MR с нерешёнными обсуждениями. Создаётся отдельный git worktree на ветке MR внутри runtime/ — ваша рабочая копия и текущая ветка не трогаются. Агент читает комментарии ревьюеров через glab, правит код по правилам проекта, гоняет проверки.\n\nПотом вы смотрите diff здесь и сами нажимаете Commit и Push; push обновит MR. Отвечать на комментарии и резолвить их в GitLab агент не будет."
	case "plan":
		return "Read-only анализ задачи из корня проекта: агент изучает описание задачи и код и выдаёт план: шаги, файлы и что в них меняется, риски, открытые вопросы, оценка размера. Можно добавить свои указания в поле ниже. Ничего не меняется."
	case "implement":
		return "Агент реализует задачу в отдельном git worktree на новой ветке от origin/develop (имя ветки по умолчанию — ссылка на задачу, как принято в проекте). Ваша рабочая копия не трогается. Правила проекта подхватываются (.claude/ линкуется в worktree).\n\nПосле завершения вы смотрите diff, коммитите, пушите и создаёте MR отсюда — каждое из этих действий отдельной кнопкой."
	}
	return ""
}

func verdictLabel(v string) string {
	switch v {
	case "approve":
		return "Можно мерджить"
	case "approve_with_comments":
		return "Ок с замечаниями"
	case "request_changes":
		return "Нужны правки"
	case "blocked":
		return "Блокер"
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
