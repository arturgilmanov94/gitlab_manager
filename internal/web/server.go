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
		"lower":           strings.ToLower,
		"divCents":        func(cents int64) float64 { return float64(cents) / 100 },
		"aiState":         aiState,
		"aiLabel":         aiLabel,
		"aiTone":          aiTone,
		"statusLabel":     statusLabel,
		"statusTone":      statusTone,
		"glyph":           glyph,
		"findingsSummary": findingsSummary,
		"firstLine": func(s string) string {
			s = strings.TrimSpace(s)
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				return s[:i]
			}
			if len(s) > 160 {
				return s[:160] + "…"
			}
			return s
		},
		"errorTitle": errorTitle,
		"tokens":     formatTokens,
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
	s.mux.HandleFunc("POST /api/runs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		runID, err := s.svc.Retry(pathID(r))
		if err != nil {
			s.result(w, nil, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID, "redirect": fmt.Sprintf("/run/%d", runID)})
	})
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
	stale := latest != nil && latest.HeadSHA != "" && latest.HeadSHA != mr.HeadSHA
	var lastRun *db.RunSummary
	if len(runs) > 0 {
		lastRun = &runs[0]
	}
	state := "never"
	switch {
	case active != nil:
		state = active.Status
	case lastRun != nil && lastRun.Status == db.StatusFailed && lastRun.IsReview():
		state = "failed"
	case latest != nil && stale:
		state = "stale"
	case latest != nil:
		state = "current"
	}
	var major, minor, info int64
	for _, f := range findings {
		if f.Status != "open" {
			continue
		}
		switch f.Severity {
		case "CRITICAL", "HIGH":
			major++
		case "MEDIUM":
			minor++
		default:
			info++
		}
	}
	s.render(w, "mr", map[string]any{
		"Base": s.base("mrs", fmt.Sprintf("!%d %s", mr.IID, mr.Title)), "MR": mr, "Runs": runs, "Latest": latest,
		"Findings": findings, "Discussions": discussions, "Active": active, "LastRun": lastRun,
		"Stale": stale, "State": state, "OpenMajor": major, "OpenMinor": minor, "OpenInfo": info,
		"IsMine": username != "" && mr.Author == username,
		"Closed": mr.State == "merged" || mr.State == "closed",
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
	var plan, impl, failed *db.RunSummary
	for i := range runs {
		r := &runs[i]
		switch {
		case r.Status == db.StatusDone && r.Kind == db.KindPlan && plan == nil:
			plan = r
		case r.Status == db.StatusDone && r.Kind == db.KindImplement && impl == nil:
			impl = r
		case r.Status == db.StatusFailed && failed == nil && i == 0:
			failed = r
		}
	}
	state := "new"
	switch {
	case active != nil && active.Kind == db.KindPlan:
		state = "researching"
	case active != nil:
		state = "implementing"
	case failed != nil:
		state = "failed"
	case impl != nil:
		state = "ready"
	case plan != nil:
		state = "planned"
	}
	s.render(w, "issue", map[string]any{
		"Base": s.base("issues", fmt.Sprintf("#%d %s", issue.IID, issue.Title)), "Issue": issue, "Runs": runs, "Active": active,
		"Plan": plan, "Impl": impl, "Failed": failed, "State": state,
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
	s.render(w, "doctor", map[string]any{"Base": s.base("doctor", "Doctor"), "Report": rep, "Skills": s.svc.SkillMap()})
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
		notes := body["notes"]
		if planID, perr := strconv.ParseInt(body["plan_run"], 10, 64); perr == nil && planID > 0 {
			if plan, _ := s.svc.DB.GetRun(planID); plan != nil && plan.ResultJSON != "" {
				notes = strings.TrimSpace(notes + "\n\nPlan from the investigation run (follow it unless it contradicts the code you find):\n" + planAsText(plan.ResultJSON))
			}
		}
		runID, err = s.svc.StartImplement(id, body["runner"], notes, body["branch"])
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
		return "Проверка изменений"
	case db.KindFixComments:
		return "Исправление замечаний"
	case db.KindPlan:
		return "Исследование"
	case db.KindImplement:
		return "Решение задачи"
	}
	return kind
}

func kindTip(kind string) string {
	switch kind {
	case "quick":
		return "AI пройдёт только по diff MR и обсуждениям, без обхода кодовой базы. Быстрее и дешевле по токенам; в отчёт попадают CRITICAL/HIGH/MEDIUM."
	case "full":
		return "AI заново проверит весь MR на текущем HEAD по правилам проекта: контекст кода, регрессии, безопасность, все уровни замечаний."
	case "verify":
		return "AI проверит только изменения после последнего ревью и обновит статусы прежних замечаний: исправлено / открыто / неактуально."
	case "fix":
		return "Агент создаст отдельный workspace на ветке MR и исправит код по нерешённым обсуждениям ревьюеров. Commit и push — только по вашей кнопке."
	case "plan":
		return "Агент изучит задачу и код без изменений: план, затронутые файлы, риски, открытые вопросы, оценка."
	case "implement":
		return "Агент создаст отдельный workspace на новой ветке и реализует задачу; ваша рабочая копия не меняется. Commit, push и MR — по вашим кнопкам."
	case "implement_plan":
		return "Агент реализует план из исследования в отдельном workspace на новой ветке."
	case "retry":
		return "Запустить то же самое ещё раз с теми же параметрами."
	case "stop":
		return "Остановить агента. Частичный результат не сохраняется; можно запустить снова."
	case "remove":
		return "Убрать из dashboard вместе с историей запусков. В GitLab ничего не меняется."
	case "refresh":
		return "Перечитать данные из GitLab. Ничего не запускается."
	case "sync-mrs":
		return "Подтянуть открытые MR, где вы reviewer, assignee или автор, из текущего проекта. Ничего не запускается."
	case "sync-issues":
		return "Подтянуть открытые задачи, где вы assignee. Ничего не запускается."
	case "add":
		return "Загрузить из GitLab по ссылке и добавить в dashboard. Ничего не запускается."
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

// aiState derives the AI-review state of a list item: never | queued | running | current | stale | failed.
func aiState(item db.MRListItem) string {
	if item.Last != nil && (item.Last.Status == db.StatusQueued || item.Last.Status == db.StatusRunning) {
		return item.Last.Status
	}
	if item.Last != nil && item.Last.Status == db.StatusFailed && (item.Done == nil || item.Last.ID > item.Done.ID) {
		return "failed"
	}
	if item.Done == nil {
		return "never"
	}
	if item.Stale {
		return "stale"
	}
	return "current"
}

func aiLabel(state string) string {
	switch state {
	case "never":
		return "Ещё не проверен"
	case "queued":
		return "В очереди"
	case "running":
		return "Проверяется"
	case "current":
		return "Проверен"
	case "stale":
		return "Есть новые изменения"
	case "failed":
		return "Ошибка ревью"
	}
	return state
}

func aiTone(state string) string {
	switch state {
	case "current":
		return "success"
	case "stale":
		return "warning"
	case "failed":
		return "danger"
	case "queued", "running":
		return "info"
	}
	return "neutral"
}

// statusLabel is the human status of a run.
func statusLabel(status string) string {
	switch status {
	case db.StatusQueued:
		return "В очереди"
	case db.StatusRunning:
		return "Выполняется"
	case db.StatusDone:
		return "Завершён"
	case db.StatusFailed:
		return "Ошибка"
	case db.StatusCancelled:
		return "Остановлен"
	}
	return status
}

func statusTone(status string) string {
	switch status {
	case db.StatusDone:
		return "success"
	case db.StatusFailed:
		return "danger"
	case db.StatusQueued, db.StatusRunning:
		return "info"
	}
	return "neutral"
}

// glyph is the text glyph of a tone (never the only carrier of meaning).
func glyph(tone string) string {
	switch tone {
	case "success":
		return "✓"
	case "warning":
		return "⚠"
	case "danger":
		return "✕"
	case "info":
		return "●"
	}
	return "○"
}

func findingsSummary(major, minor, info int64) string {
	var parts []string
	if major > 0 {
		parts = append(parts, fmt.Sprintf("%d major", major))
	}
	if minor > 0 {
		parts = append(parts, fmt.Sprintf("%d minor", minor))
	}
	if info > 0 {
		parts = append(parts, fmt.Sprintf("%d info", info))
	}
	if len(parts) == 0 {
		return "0 открытых замечаний"
	}
	return strings.Join(parts, " · ")
}

// errorTitle turns a run kind into a human failure headline.
func errorTitle(kind string) string {
	switch kind {
	case db.KindReviewQuick, db.KindReviewFull, db.KindReviewVerify:
		return "Не удалось выполнить ревью"
	case db.KindPlan:
		return "Не удалось исследовать задачу"
	case db.KindImplement:
		return "Не удалось решить задачу"
	case db.KindFixComments:
		return "Не удалось исправить замечания"
	}
	return "Запуск завершился с ошибкой"
}

// planAsText renders a plan result JSON as readable text for the implementation notes.
func planAsText(raw string) string {
	var plan struct {
		Summary string   `json:"summary"`
		Steps   []string `json:"steps"`
		Files   []struct {
			Path   string `json:"path"`
			Change string `json:"change"`
		} `json:"files"`
		Risks []string `json:"risks"`
	}
	if json.Unmarshal([]byte(raw), &plan) != nil {
		return raw
	}
	var b strings.Builder
	b.WriteString(plan.Summary + "\n\nSteps:\n")
	for i, step := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	if len(plan.Files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range plan.Files {
			fmt.Fprintf(&b, "- %s: %s\n", f.Path, f.Change)
		}
	}
	if len(plan.Risks) > 0 {
		b.WriteString("\nRisks:\n")
		for _, r := range plan.Risks {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	return b.String()
}
