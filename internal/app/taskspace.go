package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mr-review/internal/db"
	"mr-review/internal/worktree"
)

// The task's worktree (G1): one git worktree per task for every stage — investigation, plan, implementation.
// The developer's own checkout (PROJECT_ROOT) is never the place an agent works in.

// IssueBranch is the branch a task is worked on: the branch of its latest run that had one, else the task
// reference (group/project#12).
func (s *Service) IssueBranch(issue *db.Issue) string {
	runs, _ := s.DB.ListRunsForIssue(issue.ID)
	for _, r := range runs { // newest first
		if r.Branch != "" {
			return r.Branch
		}
	}
	return issue.Ref()
}

// branchFree refuses to start a run when its branch is checked out somewhere else — typically in the developer's
// own checkout — before anything is queued: git allows a branch in one worktree only, and the bare git error
// («already checked out at…») would otherwise surface in the middle of a run.
func (s *Service) branchFree(branch string) error {
	if s.Worktrees == nil || branch == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	at := s.Worktrees.CheckedOutAt(ctx, branch)
	if at == "" || samePath(at, s.Worktrees.Path(branch)) {
		return nil
	}
	if samePath(at, s.Settings.ProjectRoot) {
		return userErr("ветка %s сейчас открыта в вашей рабочей копии %s. Агент работает в отдельном workspace и не может взять ту же ветку: переключите рабочую копию на другую ветку (git switch %s) или укажите для задачи другую ветку", branch, at, s.Settings.BaseBranch)
	}
	return userErr("ветка %s уже открыта в другом worktree: %s. Удалите его (git worktree remove) или укажите для задачи другую ветку", branch, at)
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// UpdateWorkspace rebases the worktree of a run on a freshly fetched origin/<base> («Обновить»). Refused while a
// run is working in it or with uncommitted changes; a conflict aborts the rebase and leaves the branch as it was.
func (s *Service) UpdateWorkspace(runID int64) (string, error) {
	if v := s.forRun(runID); v != s {
		return v.UpdateWorkspace(runID)
	}
	run, _ := s.DB.GetRun(runID)
	if run == nil || run.WorkDir == "" || run.Branch == "" {
		return "", userErr("у запуска #%d нет workspace", runID)
	}
	if s.Worktrees == nil || !s.Worktrees.Exists(run.WorkDir) || s.Worktrees.IsReview(run.WorkDir) {
		return "", userErr("workspace уже удалён")
	}
	if busy, _ := s.DB.ActiveRunForWorkDir(run.WorkDir); busy != nil {
		return "", userErr("в этом workspace сейчас работает запуск #%d: дождитесь его или остановите", busy.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	before := s.Worktrees.Head(ctx, run.WorkDir)
	if err := s.Worktrees.Rebase(ctx, run.WorkDir, func(string) {}); err != nil {
		if errors.Is(err, worktree.ErrUnreachable) {
			return "", userErr("не удалось получить origin/%s: GitLab недоступен (VPN?)", s.Settings.BaseBranch)
		}
		return "", &UserError{err.Error()}
	}
	if s.Worktrees.Head(ctx, run.WorkDir) == before {
		return fmt.Sprintf("ветка %s уже на свежем origin/%s", run.Branch, s.Settings.BaseBranch), nil
	}
	return fmt.Sprintf("ветка %s перенесена на свежий origin/%s", run.Branch, s.Settings.BaseBranch), nil
}

// applyAgentCommits turns the `commits` of an implementation report into local commits in the task's worktree
// (G4: the agent commits locally step by step; the push is the developer's). A step whose paths have no changes is
// skipped; whatever the steps do not cover stays uncommitted and is visible on the run page.
func (s *Service) applyAgentCommits(ctx context.Context, run *db.Run, structured []byte, logln func(string)) {
	var report struct {
		Commits []struct {
			Message string   `json:"message"`
			Paths   []string `json:"paths"`
		} `json:"commits"`
	}
	if json.Unmarshal(structured, &report) != nil || len(report.Commits) == 0 || run.WorkDir == "" || !s.Worktrees.Exists(run.WorkDir) {
		return
	}
	made := 0
	for _, c := range report.Commits {
		ok, err := s.Worktrees.CommitPaths(ctx, run.WorkDir, strings.TrimSpace(c.Message), c.Paths)
		if err != nil {
			logln(worktree.WarnPrefix + "коммит «" + firstLine(c.Message) + "» не создан: " + err.Error())
			_ = s.DB.AddRunEvent(run.ID, "workspace", "commit", "не создан: "+firstLine(c.Message))
			continue
		}
		if ok {
			made++
			_ = s.DB.AddRunEvent(run.ID, "workspace", "commit", firstLine(c.Message))
		}
	}
	if st := s.Worktrees.State(ctx, run.WorkDir); st.Dirty > 0 {
		logln(worktree.WarnPrefix + fmt.Sprintf("после коммитов агента остались незакоммиченные файлы: %d", st.Dirty))
	}
	logln(fmt.Sprintf("local commits made from the agent's steps: %d", made))
}

// PushPreview is what «Push и draft MR» will do, shown before the developer confirms.
type PushPreview struct {
	Branch, Base  string
	Commits       []worktree.Commit
	Dirty         int
	Unpushed      int  // commits not on origin/<branch> yet (when the branch is there)
	HasUpstream   bool // the branch is on origin already
	Title, Body   string
	ExistingMR    *db.MergeRequest // an MR from this branch already exists: the push updates it
	ExistingMRURL string
	Ready         bool   // something to push and nothing uncommitted
	Why           string // why not Ready
}

// PushPreviewFor builds the preview of the run's workspace: commits to push, MR title and description.
func (s *Service) PushPreviewFor(runID int64) (*PushPreview, error) {
	if v := s.forRun(runID); v != s {
		return v.PushPreviewFor(runID)
	}
	run, err := s.editRun(runID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	p := &PushPreview{Branch: run.Branch, Base: s.Settings.BaseBranch, Commits: s.Worktrees.CommitLog(ctx, run.WorkDir)}
	p.Dirty = s.Worktrees.State(ctx, run.WorkDir).Dirty
	p.Unpushed, p.HasUpstream = s.Worktrees.Unpushed(ctx, run.WorkDir)
	if mr := s.mrOfBranch(run.Branch); mr != nil {
		p.ExistingMR, p.ExistingMRURL = mr, mr.WebURL
	}
	var issue *db.Issue
	if run.IssueID != nil {
		issue, _ = s.DB.GetIssue(*run.IssueID)
	}
	draft := s.MRDraftFor(run, issue)
	p.Title, p.Body = draft.Title, draft.Description
	if issue != nil && !issue.Local() {
		closes := closesRef(issue, s)
		p.Body = strings.Replace(p.Body, "Closes "+issue.WebURL, "Closes "+closes, 1)
	}
	if !strings.HasPrefix(strings.ToLower(p.Title), "draft:") {
		p.Title = "Draft: " + p.Title
	}
	switch {
	case p.Dirty > 0:
		p.Why = fmt.Sprintf("в workspace есть незакоммиченные изменения (%d файлов): закоммитьте их или попросите агента", p.Dirty)
	case len(p.Commits) == 0:
		p.Why = "в ветке нет коммитов относительно origin/" + p.Base + ": пушить нечего"
	case p.HasUpstream && p.Unpushed == 0:
		p.Why = "все коммиты уже в origin/" + p.Branch + ": пушить нечего"
	default:
		p.Ready = true
	}
	return p, nil
}

// closesRef is how the MR description closes the task: «#12» in the same GitLab project, «group/project#12» otherwise.
func closesRef(issue *db.Issue, s *Service) string {
	if _, project := s.DefaultProject(); project == issue.ProjectPath {
		return fmt.Sprintf("#%d", issue.IID)
	}
	return fmt.Sprintf("%s#%d", issue.ProjectPath, issue.IID)
}

// mrOfBranch is an open MR of the dashboard from branch (nil = none).
func (s *Service) mrOfBranch(branch string) *db.MergeRequest {
	all, err := s.DB.ListMRs()
	if err != nil {
		return nil
	}
	for _, m := range all {
		if m.SourceBranch == branch && !m.Closed() {
			mr := m.MergeRequest
			return &mr
		}
	}
	return nil
}

// PushAndDraftMR pushes the run's branch and creates a draft MR with the confirmed title and description
// («Push и draft MR», G4). When an MR from the branch already exists only the push is done. Returns the MR link.
func (s *Service) PushAndDraftMR(runID int64, title, description string) (string, error) {
	if v := s.forRun(runID); v != s {
		return v.PushAndDraftMR(runID, title, description)
	}
	p, err := s.PushPreviewFor(runID)
	if err != nil {
		return "", err
	}
	if !p.Ready {
		return "", userErr("%s", p.Why)
	}
	if _, err := s.Push(runID); err != nil {
		return "", err
	}
	if p.ExistingMR != nil {
		_, _ = s.RefreshMR(p.ExistingMR.ID)
		return p.ExistingMRURL, nil
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = p.Title
	}
	if !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	if strings.TrimSpace(description) == "" {
		description = p.Body
	}
	url, err := s.CreateMR(runID, title, description)
	if err != nil {
		return "", &UserError{"ветка запушена, но MR не создан: " + err.Error()}
	}
	if added, err := s.AddMR(url); err == nil && added != nil {
		if run, _ := s.DB.GetRun(runID); run != nil && run.IssueID != nil {
			_, _ = s.RefreshIssue(*run.IssueID) // the task finds its MR, the stage moves to review
		}
	}
	return url, nil
}
