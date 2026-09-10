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
	"sort"
	"strconv"
	"strings"
	"time"

	"mr-review/internal/app"
	"mr-review/internal/config"
	"mr-review/internal/db"
	"mr-review/internal/doctor"
	"mr-review/internal/gitlab"
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
	Pending     []db.Approval       // open permission prompts across all runs (header badge)
	Labels      []config.LabelStyle // highlighted GitLab labels (filter chips)
	Waiting     []int64             // runs that need the developer: permission prompts and agent questions (header chip)
	Terminal    bool                // «Открыть в терминале» works on this machine
	TerminalWhy string              // reason when it does not
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
		"md":         renderMarkdown,
		"hasPrefix":  strings.HasPrefix,
		"runPath":    runPath,
		"mrPath":     mrPath,
		"issuePath":  issuePath,
		"dict": func(pairs ...any) map[string]any {
			out := map[string]any{}
			for i := 0; i+1 < len(pairs); i += 2 {
				key, _ := pairs[i].(string)
				out[key] = pairs[i+1]
			}
			return out
		},
		"keyLabels":     func(labels string) []config.LabelStyle { return keyLabels(svc.Settings.HighlightLabels, labels) },
		"otherLabels":   func(labels string) string { return otherLabels(svc.Settings.HighlightLabels, labels) },
		"labelSlugs":    func(labels string) string { return labelSlugs(svc.Settings.HighlightLabels, labels) },
		"blobBase":      blobBase,
		"fileLink":      fileLink,
		"pipelineLabel": pipelineLabel,
		"pipelineTone":  pipelineTone,
		"fstatusLabel":  findingStatusLabel,
		"checkLabel":    checkLabel,
		"termCommand":   func(run *db.Run) string { return svc.TerminalCommand(run) },
		"list":          func(items ...any) []any { return items },
		"tokens":        formatTokens,
		"tokensTip": func(in, out, read, write int64) string {
			return fmt.Sprintf("Токены за запуск, суммарно по всем моделям (агент + субагенты)\nвход: %s · выход: %s · чтение кэша: %s · запись кэша: %s",
				formatTokens(in), formatTokens(out), formatTokens(read), formatTokens(write))
		},
	}
	partials, err := fs.Glob(assets, "templates/_*.html")
	if err != nil {
		return nil, err
	}
	for _, page := range []string{"overview", "index", "history", "issues", "mr", "issue", "run", "runs", "workspaces", "doctor"} {
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

	s.mux.HandleFunc("GET /{$}", s.overview)
	s.mux.HandleFunc("GET /mrs", s.index)
	s.mux.HandleFunc("GET /history", s.history)
	s.mux.HandleFunc("GET /issues", s.issues)
	s.mux.HandleFunc("GET /runs", s.runs)
	s.mux.HandleFunc("GET /workspaces", s.workspaces)
	s.mux.HandleFunc("GET /doctor", s.doctorPage)
	// Readable object URLs: /-/mr/1, /-/issue/2, /-/review/3, /-/task-implement/4 (+ /log).
	s.mux.HandleFunc("GET /-/mr/{id}", s.mrPage)
	s.mux.HandleFunc("GET /-/issue/{id}", s.issuePage)
	s.mux.HandleFunc("GET /-/{action}/{id}", s.runPage)
	s.mux.HandleFunc("GET /-/{action}/{id}/log", s.runLog)
	// Old paths keep working and redirect to the readable form.
	s.mux.HandleFunc("GET /mr/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mrPath(pathID(r)), http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /issue/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, issuePath(pathID(r)), http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /run/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, _ := s.svc.DB.GetRun(pathID(r))
		if run == nil {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, runPath(run.Kind, run.ID)+fragment(r), http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /run/{id}/log", s.runLog)

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
		s.result(w, map[string]any{"synced": res.Synced, "archived": res.Archived, "pruned": res.Pruned, "username": res.Username, "project": res.Project, "seconds": int(res.Duration.Seconds())}, err)
	})
	s.mux.HandleFunc("POST /api/mrs/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		mr, err := s.svc.RefreshMR(pathID(r))
		s.result(w, map[string]any{"mr": mr}, err)
	})
	s.mux.HandleFunc("DELETE /api/mrs/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.DeleteMR(pathID(r)))
	})
	s.mux.HandleFunc("POST /api/mrs/{id}/hide", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.HideMR(pathID(r)))
	})
	s.mux.HandleFunc("POST /api/mrs/{id}/unhide", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.UnhideMR(pathID(r)))
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
		s.runStarted(w, runID)
	})
	s.mux.HandleFunc("POST /api/skills/{kind}", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		err := s.svc.SaveSkillSetting(r.PathValue("kind"), body["name"], body["custom"] == "1" || body["custom"] == "true", body["text"])
		s.result(w, map[string]any{}, err)
	})
	s.mux.HandleFunc("POST /api/runs/{id}/answers", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		answers := map[int64]string{}
		for key, value := range body {
			if id, err := strconv.ParseInt(strings.TrimPrefix(key, "answer_"), 10, 64); err == nil && strings.HasPrefix(key, "answer_") {
				answers[id] = value
			}
		}
		s.result(w, map[string]any{}, s.svc.Answer(pathID(r), answers))
	})
	s.mux.HandleFunc("POST /api/runs/{id}/terminal", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		command, err := s.svc.OpenTerminal(pathID(r), body["resume"] == "1")
		s.result(w, map[string]any{"command": command}, err)
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
	s.mux.HandleFunc("POST /api/runs/{id}/plan-file", func(w http.ResponseWriter, r *http.Request) {
		path, err := s.svc.ExportPlan(pathID(r))
		s.result(w, map[string]any{"path": path}, err)
	})
	s.mux.HandleFunc("POST /api/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		s.result(w, map[string]any{}, s.svc.Decide(pathID(r), body["decision"], body["note"]))
	})
	s.mux.HandleFunc("POST /api/findings/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		s.result(w, map[string]any{}, s.svc.SetFindingStatus(pathID(r), readBody(r)["status"]))
	})
}

// ---------------------------------------------------------------------------------- pages

func (s *Server) base(nav, title string) pageBase {
	pending, _ := s.svc.DB.PendingApprovals()
	var waiting []int64
	seen := map[int64]bool{}
	for _, a := range pending {
		if !seen[a.RunID] {
			seen[a.RunID] = true
			waiting = append(waiting, a.RunID)
		}
	}
	if ids, _ := s.svc.DB.RunsWaitingForAnswers(); len(ids) > 0 {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				waiting = append(waiting, id)
			}
		}
	}
	termOK, termWhy := s.svc.TerminalAvailable()
	if termOK {
		termWhy = ""
	}
	return pageBase{
		Version:     s.version,
		ProjectRoot: s.svc.Settings.ProjectRoot,
		Skill:       s.svc.Skill(),
		Runners:     s.svc.RunnerInfos(),
		Username:    s.svc.CurrentUser(),
		Counts:      s.svc.DB.Counts(),
		Pending:     pending,
		Waiting:     waiting,
		Labels:      s.svc.Settings.HighlightLabels,
		Terminal:    termOK,
		TerminalWhy: termWhy,
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

// index lists the MRs that still concern the developer (open, not approved by me, I have a role or added by hand).
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	mrs, err := s.svc.DB.ListMRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	relevant, archived := splitMRs(mrs)
	s.render(w, "index", map[string]any{"Base": s.base("mrs", "Merge requests"), "MRs": relevant, "Archived": len(archived)})
}

// history lists MRs that no longer concern the developer (approved, merged, closed, role removed) but were
// worked on: every review action is still available there.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	mrs, err := s.svc.DB.ListMRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, archived := splitMRs(mrs)
	s.render(w, "history", map[string]any{"Base": s.base("history", "История"), "MRs": archived})
}

func splitMRs(mrs []db.MRListItem) (relevant, archived []db.MRListItem) {
	for _, mr := range mrs {
		if mr.Relevant() {
			relevant = append(relevant, mr)
		} else {
			archived = append(archived, mr)
		}
	}
	return relevant, archived
}

// Readable URLs.
func runSlug(kind string) string {
	switch kind {
	case db.KindReviewFull:
		return "review"
	case db.KindReviewQuick:
		return "quick-review"
	case db.KindReviewVerify:
		return "verify"
	case db.KindFixComments:
		return "fix-comments"
	case db.KindPlan:
		return "task-plan"
	case db.KindImplement:
		return "task-implement"
	case db.KindVerifyFinding:
		return "verify-finding"
	case db.KindStandTest:
		return "stand-test"
	}
	return "run"
}

var runSlugs = map[string]bool{"review": true, "quick-review": true, "verify": true, "verify-finding": true, "stand-test": true, "fix-comments": true, "task-plan": true, "task-implement": true, "run": true}

func runPath(kind string, id any) string { return fmt.Sprintf("/-/%s/%v", runSlug(kind), id) }
func mrPath(id any) string               { return fmt.Sprintf("/-/mr/%v", id) }
func issuePath(id any) string            { return fmt.Sprintf("/-/issue/%v", id) }

func fragment(r *http.Request) string {
	if r.URL.Fragment != "" {
		return "#" + r.URL.Fragment
	}
	return ""
}

// inboxItem is one line of the overview's "Нужно от меня" block: a state, a text and exactly one action.
type inboxItem struct {
	Tone    string // warning | danger | neutral | info | success
	Glyph   string
	Title   string
	Detail  string
	Href    string // primary action as a link, or
	Action  string // primary action label
	Onclick string // primary action as JS (startRun / post)
}

// overview is the home page: what needs the developer, which agents are working, how my MRs are doing.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	mrs, _ := s.svc.DB.ListMRs()
	issues, _ := s.svc.DB.ListIssues()
	runs, _ := s.svc.DB.ListRuns(300)
	username := s.svc.CurrentUser()
	var inbox []inboxItem
	var active []db.RunListItem
	for _, run := range runs {
		if !run.Active() {
			continue
		}
		active = append(active, run)
		if run.Status == db.StatusWaiting {
			inbox = append(inbox, inboxItem{Tone: "warning", Glyph: "⚠", Title: runObjectLabel(run) + " · агенту нужен ваш ответ",
				Detail: strings.TrimPrefix(run.Progress, "Нужен ваш ответ: "), Href: runPath(run.Kind, run.ID) + "#approval", Action: "Ответить"})
		}
	}
	mine := struct{ Total, PipelineFailed, Unresolved, Approved, Stale, NeverReviewed int }{}
	for _, mr := range mrs {
		if !mr.Relevant() {
			continue
		}
		isMine := username != "" && mr.Author == username
		st := aiState(mr)
		if isMine {
			mine.Total++
			if mr.PipelineStatus == "failed" {
				mine.PipelineFailed++
				inbox = append(inbox, inboxItem{Tone: "danger", Glyph: "✕", Title: fmt.Sprintf("!%d · Pipeline failed", mr.IID), Detail: mr.Title,
					Href: mr.WebURL, Action: "Открыть pipeline ↗"})
			}
			if mr.Unresolved > 0 {
				mine.Unresolved++
			}
			if mr.ApprovalsRequired > 0 && mr.ApprovalsGiven >= mr.ApprovalsRequired {
				mine.Approved++
			}
		}
		switch st {
		case "stale":
			mine.Stale++
			inbox = append(inbox, inboxItem{Tone: "warning", Glyph: "⚠", Title: fmt.Sprintf("!%d · есть новые коммиты после ревью", mr.IID), Detail: mr.Title,
				Action: "Проверить изменения", Onclick: fmt.Sprintf("startRun('/api/mrs/%d/runs', 'verify', this)", mr.ID)})
		case "failed":
			inbox = append(inbox, inboxItem{Tone: "danger", Glyph: "✕", Title: fmt.Sprintf("!%d · ревью не удалось", mr.IID), Detail: mr.Title,
				Action: "Повторить", Onclick: fmt.Sprintf("post('/api/runs/%d/retry', {}, this).then(r => r && reload())", mr.Last.ID)})
		case "never":
			if !isMine {
				mine.NeverReviewed++
				inbox = append(inbox, inboxItem{Tone: "neutral", Glyph: "○", Title: fmt.Sprintf("!%d · ещё не проверен", mr.IID), Detail: mr.Title + " · " + mr.Author,
					Action: "Запустить ревью", Onclick: fmt.Sprintf("startRun('/api/mrs/%d/runs', 'full', this)", mr.ID)})
			}
		}
	}
	for _, issue := range issues {
		if issue.Last != nil && issue.Last.Status == db.StatusDone && issue.Last.Kind == db.KindImplement {
			inbox = append(inbox, inboxItem{Tone: "success", Glyph: "✓", Title: fmt.Sprintf("#%d · готово к MR", issue.IID), Detail: issue.Title,
				Href: runPath(issue.Last.Kind, issue.Last.ID), Action: "Подготовить MR"})
		}
		if issue.Last != nil && issue.Last.Status == db.StatusFailed {
			inbox = append(inbox, inboxItem{Tone: "danger", Glyph: "✕", Title: fmt.Sprintf("#%d · %s не удалось", issue.IID, strings.ToLower(kindLabel(issue.Last.Kind))), Detail: issue.Title,
				Action: "Повторить", Onclick: fmt.Sprintf("post('/api/runs/%d/retry', {}, this).then(r => r && reload())", issue.Last.ID)})
		}
	}
	sort.SliceStable(inbox, func(i, j int) bool { return inboxRank(inbox[i]) < inboxRank(inbox[j]) })
	relevant, _ := splitMRs(mrs)
	s.render(w, "overview", map[string]any{
		"Base": s.base("overview", "Обзор"), "Inbox": inbox, "Active": active, "Mine": mine,
		"MRCount": len(relevant), "IssueCount": len(issues),
	})
}

func inboxRank(item inboxItem) int {
	switch {
	case strings.Contains(item.Title, "нужен ваш ответ"):
		return 0
	case strings.Contains(item.Title, "Pipeline failed"):
		return 1
	case strings.Contains(item.Title, "новые коммиты"):
		return 2
	case item.Tone == "danger":
		return 3
	case item.Tone == "success":
		return 4
	}
	return 5
}

// runObjectLabel names the MR or issue a run belongs to.
func runObjectLabel(run db.RunListItem) string {
	switch {
	case run.MRIID != 0:
		return fmt.Sprintf("!%d %s", run.MRIID, run.MRTitle)
	case run.IssueIID != 0:
		return fmt.Sprintf("#%d %s", run.IssueIID, run.IssueTitle)
	}
	return fmt.Sprintf("сессия #%d", run.ID)
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.svc.DB.ListRuns(300)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Runs sharing an agent session form one conversation chain (Phase C): shown in the row, oldest first.
	chains := map[string][]db.RunListItem{}
	for _, run := range runs {
		if run.SessionID != "" {
			chains[run.SessionID] = append([]db.RunListItem{run}, chains[run.SessionID]...)
		}
	}
	var active, finished []runRow
	for _, run := range runs {
		row := runRow{RunListItem: run}
		if chain := chains[run.SessionID]; len(chain) > 1 {
			row.Chain = chain
		}
		if run.Active() {
			active = append(active, row)
		} else {
			finished = append(finished, row)
		}
	}
	s.render(w, "runs", map[string]any{"Base": s.base("runs", "Сессии"), "Active": active, "Finished": finished})
}

// changesIf returns what changed since the reviewed SHA when the review is stale (nil otherwise).
func changesIf(stale bool, svc *app.Service, mr *db.MergeRequest, latest *db.Run) *gitlab.Changes {
	if !stale || latest == nil {
		return nil
	}
	return svc.Changes(mr, latest.HeadSHA)
}

// checkLabel is the human label of a finding check outcome.
func checkLabel(status string) string {
	switch status {
	case "confirmed":
		return "подтверждено агентом"
	case "false_positive":
		return "ложное срабатывание"
	case "obsolete":
		return "неактуально"
	case "unclear":
		return "недостаточно данных"
	}
	return status
}

// runRow is a run of the sessions list with the other runs of the same agent session.
type runRow struct {
	db.RunListItem
	Chain []db.RunListItem
}

func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Workspaces()
	data := map[string]any{"Base": s.base("workspaces", "Workspaces"), "Workspaces": items}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, "workspaces", data)
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
	sha := mr.HeadSHA
	if latest != nil && latest.HeadSHA != "" {
		sha = latest.HeadSHA
	}
	s.render(w, "mr", map[string]any{
		"Base": s.base("mrs", fmt.Sprintf("!%d %s", mr.IID, mr.Title)), "MR": mr, "Runs": runs, "Latest": latest,
		"Sessions":   s.svc.Resumable(runs, s.svc.Settings.ProjectRoot),
		"Changes":    changesIf(stale, s.svc, mr, latest),
		"StandSkill": s.svc.StandSkill(), "StandTestSkill": s.svc.SkillFor(db.KindStandTest),
		"Findings": findings, "Discussions": discussions, "Active": active, "LastRun": lastRun,
		"Stale": stale, "State": state, "OpenMajor": major, "OpenMinor": minor, "OpenInfo": info,
		"IsMine": username != "" && mr.Author == username,
		"Closed": mr.State == "merged" || mr.State == "closed",
		"Blob":   blobBase(mr.WebURL), "SHA": sha,
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
		"PlanSessions": s.svc.Resumable(runs, s.svc.Settings.ProjectRoot), "ImplSessions": s.svc.Resumable(runs, s.svc.Worktrees.Path(issue.Ref())),
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
	if action := r.PathValue("action"); action != "" && !runSlugs[action] {
		http.NotFound(w, r)
		return
	}
	run, _ := s.svc.DB.GetRun(pathID(r))
	if run == nil {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"Base": s.base("runs", fmt.Sprintf("Run #%d", run.ID)), "Run": run, "Blob": "", "SHA": run.HeadSHA}
	if run.MRID != nil {
		mr, _ := s.svc.DB.GetMR(*run.MRID)
		data["MR"] = mr
		if mr != nil {
			data["Blob"] = blobBase(mr.WebURL)
		}
	}
	if run.IssueID != nil {
		issue, _ := s.svc.DB.GetIssue(*run.IssueID)
		data["Issue"] = issue
		if issue != nil {
			data["Blob"] = blobBase(issue.WebURL)
		}
	}
	if chain, _ := s.svc.DB.ListRunsBySession(run.SessionID); len(chain) > 1 {
		data["Chain"] = chain
		if chain[0].ID != run.ID {
			data["ContinuedFrom"] = chain[0]
		}
	}
	if run.ContinueRunID != nil {
		if prev, _ := s.svc.DB.GetRun(*run.ContinueRunID); prev != nil {
			data["ContinueFrom"] = prev
		}
	}
	if run.IsReview() {
		data["Findings"], _ = s.svc.DB.ListFindings(run.ID)
		data["Discussions"], _ = s.svc.DB.ListDiscussions(run.ID)
	}
	if run.Kind == db.KindVerifyFinding && run.FindingID != nil {
		if finding, _ := s.svc.DB.GetFinding(*run.FindingID); finding != nil {
			data["Finding"] = finding
			if origin, _ := s.svc.DB.GetRun(finding.RunID); origin != nil {
				data["FindingRun"] = origin
			}
		}
		if run.ResultJSON != "" {
			var check map[string]any
			_ = json.Unmarshal([]byte(run.ResultJSON), &check)
			data["Check"] = check
		}
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
	data["Timeline"] = s.svc.DB.TimelineFor(run.ID)
	data["Messages"], _ = s.svc.DB.ListMessages(run.ID)
	data["LogTail"] = tail(run.LogPath, 12000)
	data["Approvals"], _ = s.svc.DB.ListApprovals(run.ID)
	if run.Status == db.StatusWaiting {
		data["Pending"], _ = s.svc.DB.PendingApproval(run.ID)
		data["Questions"], _ = s.svc.DB.PendingQuestions(run.ID)
	}
	if all, _ := s.svc.DB.ListQuestions(run.ID); len(all) > 0 {
		var answered []db.Question
		for _, q := range all {
			if q.Answered() {
				answered = append(answered, q)
			}
		}
		data["AnsweredQuestions"] = answered
	}
	if run.DenialsJSON != "" {
		var denials []map[string]any
		_ = json.Unmarshal([]byte(run.DenialsJSON), &denials)
		data["Denials"] = denials
	}
	data["PlansDir"] = s.svc.PlansDir()
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
	s.render(w, "doctor", map[string]any{"Base": s.base("doctor", "Doctor"), "Report": rep, "Skills": s.svc.SkillMap(),
		"SkillSettings": s.svc.SkillSettings(), "Candidates": s.svc.SkillCandidates()})
}

// ---------------------------------------------------------------------------------- api

func (s *Server) apiAddMR(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	mr, err := s.svc.AddMR(firstNonEmpty(body["url"], body["reference"]))
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mr": mr, "redirect": mrPath(mr.ID)})
}

func (s *Server) apiAddIssue(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	issue, err := s.svc.AddIssue(firstNonEmpty(body["url"], body["reference"]))
	if err != nil {
		s.result(w, nil, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "issue": issue, "redirect": issuePath(issue.ID)})
}

// runStarted is the JSON answer for a newly started run.
func (s *Server) runStarted(w http.ResponseWriter, runID int64) {
	kind := ""
	if run, _ := s.svc.DB.GetRun(runID); run != nil {
		kind = run.Kind
	}
	writeJSON(w, 200, map[string]any{"ok": true, "run_id": runID, "redirect": runPath(kind, runID)})
}

func (s *Server) apiStartMRRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	id := pathID(r)
	var runID int64
	var err error
	cont, _ := strconv.ParseInt(body["continue_run"], 10, 64) // agent session to continue; 0 = new chat
	switch body["kind"] {
	case "quick":
		runID, err = s.svc.StartReview(id, db.KindReviewQuick, body["runner"], cont)
	case "full", "":
		runID, err = s.svc.StartReview(id, db.KindReviewFull, body["runner"], cont)
	case "verify":
		runID, err = s.svc.StartReview(id, db.KindReviewVerify, body["runner"], cont)
	case "fix":
		runID, err = s.svc.StartFixComments(id, body["runner"], body["notes"], cont)
	case "verify_finding":
		findingID, _ := strconv.ParseInt(body["finding"], 10, 64)
		runID, err = s.svc.StartVerifyFinding(id, findingID, body["runner"], cont)
	case "stand":
		runID, err = s.svc.StartStandTest(id, body["runner"], body["notes"], cont)
	default:
		writeJSON(w, 400, map[string]any{"error": "kind must be quick, full, verify, verify_finding, stand or fix"})
		return
	}
	if err != nil {
		s.result(w, nil, err)
		return
	}
	s.runStarted(w, runID)
}

func (s *Server) apiStartIssueRun(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	id := pathID(r)
	var runID int64
	var err error
	cont, _ := strconv.ParseInt(body["continue_run"], 10, 64) // agent session to continue; 0 = new chat
	switch body["kind"] {
	case "plan", "":
		runID, err = s.svc.StartPlan(id, body["runner"], body["notes"], cont)
	case "implement":
		notes := body["notes"]
		if planID, perr := strconv.ParseInt(body["plan_run"], 10, 64); perr == nil && planID > 0 {
			if plan, _ := s.svc.DB.GetRun(planID); plan != nil && plan.ResultJSON != "" {
				notes = strings.TrimSpace(notes + "\n\nPlan from the investigation run (follow it unless it contradicts the code you find):\n" + planAsText(plan.ResultJSON))
			}
		}
		runID, err = s.svc.StartImplement(id, body["runner"], notes, body["branch"], cont)
	default:
		writeJSON(w, 400, map[string]any{"error": "kind must be plan or implement"})
		return
	}
	if err != nil {
		s.result(w, nil, err)
		return
	}
	s.runStarted(w, runID)
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	run, _ := s.svc.DB.GetRun(pathID(r))
	if run == nil {
		writeJSON(w, 404, map[string]any{"error": "run not found"})
		return
	}
	findings, _ := s.svc.DB.ListFindings(run.ID)
	payload := map[string]any{
		"id": run.ID, "kind": run.Kind, "status": run.Status, "verdict": run.Verdict, "summary": run.Summary,
		"error": run.Error, "runner": run.Runner, "cost_usd": run.CostUSD, "tokens": run.TotalTokens(), "duration_ms": run.DurationMs,
		"started_at": run.StartedAt, "finished_at": run.FinishedAt, "findings": len(findings), "session_id": run.SessionID,
		"progress": run.Progress, "phase": s.svc.DB.CurrentPhase(run.ID), "timeline": s.svc.DB.TimelineFor(run.ID),
	}
	if pending, _ := s.svc.DB.PendingApproval(run.ID); pending != nil {
		payload["pending_approval"] = map[string]any{"id": pending.ID, "tool": pending.ToolName, "description": pending.Description}
	}
	writeJSON(w, 200, payload)
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
	case db.KindVerifyFinding:
		return "Проверка замечания"
	case db.KindStandTest:
		return "Проверка на стенде"
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
	case "verify_finding":
		return "AI перепроверит только это замечание, не считая его верным априори: подтверждено / ложное срабатывание / неактуально / недостаточно данных, с доказательством из кода. Только чтение, остальной MR не трогается."
	case "stand":
		return "Агент в отдельном workspace ветки MR через skill доступа к стенду заливает изменённые файлы MR на стенд, прогоняет там тесты по затронутому коду, пишет скрипт-эмуляцию функциональности MR с моками внешних систем, запускает его на стенде и отчитывается. Код MR не меняет; git и БД на стенде не трогает."
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
	case "allow":
		return "Разрешить только этот вызов. Агент продолжит работу; следующий такой же вызов снова спросит."
	case "allow_always":
		return "Разрешить и применить правила, которые предложил агент, до конца этой сессии: похожие вызовы больше не спросят."
	case "deny":
		return "Отклонить вызов. Агент получит ваш комментарий и продолжит без этого действия."
	case "plan-file":
		return "Сохранить план как markdown-файл в каталог планов проекта (.claude/plans), чтобы работать с ним из терминала или IDE."
	case "stop":
		return "Остановить агента. Частичный результат не сохраняется; можно запустить снова."
	case "remove":
		return "Убрать MR из основного списка в «Историю» с пометкой «скрыт вами». Запуски сохраняются, вернуть можно оттуда. В GitLab ничего не меняется."
	case "restore":
		return "Вернуть MR в основной список, если он всё ещё вас касается (открыт, вы автор, assignee или reviewer, вы его не одобряли)."
	case "delete":
		return "Удалить MR из dashboard окончательно вместе с историей запусков. В GitLab ничего не меняется."
	case "refresh":
		return "Перечитать данные из GitLab. Ничего не запускается."
	case "sync-mrs":
		return "Подтянуть открытые MR, где вы reviewer, assignee или автор, из текущего проекта. Ничего не запускается."
	case "sync-issues":
		return "Подтянуть открытые задачи, где вы assignee. Ничего не запускается."
	case "add":
		return "Загрузить из GitLab по ссылке и добавить в dashboard. Ничего не запускается."
	case "answer":
		return "Ответы уйдут в ту же сессию агента, и он продолжит задачу с учётом ваших решений. Каждый вопрос нужно закрыть: вариантом или своим текстом."
	case "terminal":
		return "Откроет окно терминала на этой машине в каталоге сессии и продолжит её интерактивно (resume той же сессии агента). В терминале действуют ваши обычные права агента, а не ограничения dashboard."
	case "terminal-dir":
		return "Откроет окно терминала на этой машине в каталоге сессии (workspace или корень проекта) без запуска агента."
	case "terminal-copy":
		return "Скопировать команду: перейти в каталог сессии и продолжить её. Для случая, когда dashboard открыт с другой машины."
	}
	return ""
}

// splitLabels turns the stored "a, b, c" label list into trimmed names.
func splitLabels(labels string) []string {
	var out []string
	for _, item := range strings.Split(labels, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// keyLabels returns the highlighted labels present on an object, in the configured order and with their colours.
func keyLabels(styles []config.LabelStyle, labels string) []config.LabelStyle {
	present := map[string]bool{}
	for _, name := range splitLabels(labels) {
		present[strings.ToLower(name)] = true
	}
	var out []config.LabelStyle
	for _, style := range styles {
		if present[strings.ToLower(style.Name)] {
			out = append(out, style)
		}
	}
	return out
}

// otherLabels returns the labels that are not highlighted, comma separated (shown as plain muted text).
func otherLabels(styles []config.LabelStyle, labels string) string {
	key := map[string]bool{}
	for _, style := range styles {
		key[strings.ToLower(style.Name)] = true
	}
	var out []string
	for _, name := range splitLabels(labels) {
		if !key[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	return strings.Join(out, ", ")
}

// labelSlugs is the space separated lower-case list of highlighted labels on an object (row filter attribute).
func labelSlugs(styles []config.LabelStyle, labels string) string {
	var out []string
	for _, style := range keyLabels(styles, labels) {
		out = append(out, strings.ToLower(style.Name))
	}
	return strings.Join(out, " ")
}

// pipelineLabel / pipelineTone render the GitLab head pipeline status.
func pipelineLabel(status string) string {
	switch status {
	case "success":
		return "Pipeline passed"
	case "failed":
		return "Pipeline failed"
	case "running", "pending", "created", "preparing", "waiting_for_resource":
		return "Pipeline идёт"
	case "canceled", "skipped":
		return "Pipeline " + status
	case "":
		return "Pipeline нет"
	}
	return "Pipeline " + status
}

func pipelineTone(status string) string {
	switch status {
	case "success":
		return "success"
	case "failed":
		return "danger"
	case "running", "pending", "created", "preparing", "waiting_for_resource":
		return "info"
	}
	return "neutral"
}

// findingStatusLabel is the human label of a finding status.
func findingStatusLabel(status string) string {
	switch status {
	case "open":
		return "открыто"
	case "fixed":
		return "исправлено"
	case "obsolete":
		return "неактуально"
	case "false_positive":
		return "ложное срабатывание"
	case "ignored":
		return "игнорируется"
	case "resolved":
		return "решено вручную"
	}
	return status
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

// aiState derives the AI-review state of a list item: never | queued | running | waiting | current | stale | failed.
func aiState(item db.MRListItem) string {
	if item.Last != nil && (item.Last.Status == db.StatusQueued || item.Last.Status == db.StatusRunning || item.Last.Status == db.StatusWaiting) {
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
	case "waiting":
		return "Нужен ваш ответ"
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
	case "stale", "waiting":
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
	case db.StatusWaiting:
		return "Нужен ваш ответ"
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
	case db.StatusWaiting:
		return "warning"
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
	case db.KindVerifyFinding:
		return "Не удалось проверить замечание"
	case db.KindStandTest:
		return "Не удалось проверить MR на стенде"
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
