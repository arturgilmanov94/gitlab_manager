package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
