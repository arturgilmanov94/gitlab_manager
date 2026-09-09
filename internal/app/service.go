// Package app orchestrates GitLab sync, agent runs (reviews, plans, implementations) and worktrees.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mr-review/internal/config"
	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/prompts"
	"mr-review/internal/runner"
	"mr-review/internal/skill"
	"mr-review/internal/worktree"
)

// UserError is a message safe to show in the UI (HTTP 400).
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &UserError{fmt.Sprintf(format, args...)} }

// Service is the application core shared by the web server and the CLI.
type Service struct {
	Settings  *config.Settings
	DB        *db.DB
	GitLab    gitlab.Client
	Runners   map[string]runner.Runner
	Worktrees *worktree.Manager

	sem       chan struct{}
	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc
	decisions map[int64]chan approvalDecision // pending approval id → channel the run goroutine waits on
	username  string
	userOnce  sync.Once
	project   *[2]string
	wg        sync.WaitGroup
}

type approvalDecision struct {
	allow    bool
	remember bool
	note     string
}

// New wires the service. Only detected runners are kept.
func New(settings *config.Settings, database *db.DB, gl gitlab.Client, runners []runner.Runner) *Service {
	s := &Service{
		Settings:  settings,
		DB:        database,
		GitLab:    gl,
		Runners:   map[string]runner.Runner{},
		sem:       make(chan struct{}, settings.RunConcurrency),
		cancels:   map[int64]context.CancelFunc{},
		decisions: map[int64]chan approvalDecision{},
	}
	for _, r := range runners {
		if _, _, ok := r.Detect(); ok {
			s.Runners[r.Name()] = r
		}
	}
	if settings.ProjectRoot != "" {
		s.Worktrees = &worktree.Manager{Root: settings.ProjectRoot, Dir: settings.WorktreeDir, BaseBranch: settings.BaseBranch}
	}
	return s
}

// ---------------------------------------------------------------------------------- environment

// RunnerInfos lists detected runners for the UI (default first).
func (s *Service) RunnerInfos() []runner.Info {
	var out []runner.Info
	order := []string{s.Settings.DefaultRunner, "claude", "codex", "cursor"}
	seen := map[string]bool{}
	for _, name := range order {
		r, ok := s.Runners[name]
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		path, version, _ := r.Detect()
		out = append(out, runner.Info{Name: name, Path: path, Version: version})
	}
	return out
}

// DefaultProject returns (host, project path) from the project's origin remote (or GITLAB_HOST).
func (s *Service) DefaultProject() (string, string) {
	if s.project == nil {
		var pair [2]string
		if s.Settings.ProjectRoot != "" {
			if host, path, ok := gitlab.ProjectFromGitRemote(s.Settings.ProjectRoot); ok {
				pair = [2]string{host, path}
			}
		}
		if s.Settings.GitLabHost != "" {
			pair[0] = s.Settings.GitLabHost
		}
		if s.Settings.GitLabProject != "" {
			pair[1] = s.Settings.GitLabProject
		}
		s.project = &pair
	}
	return s.project[0], s.project[1]
}

// CurrentUser returns the glab username (cached; "" if unavailable).
func (s *Service) CurrentUser() string {
	s.userOnce.Do(func() {
		host, _ := s.DefaultProject()
		if name, err := s.GitLab.CurrentUser(host); err == nil {
			s.username = name
		}
	})
	return s.username
}

// Skill resolves the project full-review skill (nil when none). Shown in the UI header.
func (s *Service) Skill() *skill.Skill { return s.SkillFor(skill.ActionReviewFull) }

// SkillFor resolves the project skill that backs a run kind (nil when the project has none: the run then
// follows CLAUDE.md / AGENTS.md plus the dashboard prompt). The lookup is repeated on every run so a
// `git pull` of the project changes behaviour without restarting the dashboard.
func (s *Service) SkillFor(kind string) *skill.Skill {
	if s.Settings.ProjectRoot == "" {
		return nil
	}
	return s.resolver().ForAction(kind).Skill
}

// SkillMap resolves every dashboard action to its project skill (for doctor and the UI).
func (s *Service) SkillMap() []skill.Resolution {
	if s.Settings.ProjectRoot == "" {
		return nil
	}
	return s.resolver().Map()
}

func (s *Service) resolver() *skill.Resolver {
	return skill.NewWithNames(s.Settings.ProjectRoot, s.Settings.SkillNames)
}

// Recover marks runs interrupted by a restart as failed and backfills token counters of old runs.
func (s *Service) Recover() int64 {
	n, _ := s.DB.FailStaleRuns("Interrupted: the dashboard was restarted while this run was active")
	s.backfillTokens()
	return n
}

// backfillTokens fills token counters for runs stored before token accounting existed (from raw_result).
func (s *Service) backfillTokens() {
	ids, err := s.DB.RunsWithoutTokens()
	if err != nil {
		return
	}
	for _, id := range ids {
		run, _ := s.DB.GetRun(id)
		if run == nil || run.RawResult == "" {
			continue
		}
		payload, err := runner.ParseClaudeJSON(run.RawResult)
		if err != nil {
			continue
		}
		u := runner.ClaudeUsage(payload)
		if u.Total() == 0 {
			continue
		}
		_ = s.DB.UpdateRun(id, map[string]any{"input_tokens": u.Input, "output_tokens": u.Output, "cache_read_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite})
	}
}

// Shutdown cancels active runs and waits briefly for workers.
func (s *Service) Shutdown() {
	s.mu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

func (s *Service) requireRoot() (string, error) {
	if s.Settings.ProjectRoot == "" {
		return "", &UserError{s.Settings.ProjectRootError}
	}
	return s.Settings.ProjectRoot, nil
}

func (s *Service) pickRunner(name string) (runner.Runner, error) {
	if name == "" {
		name = s.Settings.DefaultRunner
	}
	if r, ok := s.Runners[name]; ok {
		return r, nil
	}
	if len(s.Runners) == 0 {
		return nil, userErr("no coding agent found: install Claude Code (claude), Codex (codex) or Cursor CLI (cursor-agent)")
	}
	return nil, userErr("runner %q is not available on this machine", name)
}

// ---------------------------------------------------------------------------------- merge requests

func (s *Service) mrFromPayload(payload map[string]any, ref gitlab.Ref) db.MergeRequest {
	return db.MergeRequest{
		GitLabHost:      ref.Host,
		ProjectPath:     ref.ProjectPath,
		IID:             gitlab.Int(payload, "iid"),
		WebURL:          firstOf(gitlab.Str(payload, "web_url"), ref.MRURL()),
		Title:           gitlab.Str(payload, "title"),
		Author:          gitlab.Str(gitlab.Nested(payload, "author"), "username"),
		SourceBranch:    gitlab.Str(payload, "source_branch"),
		TargetBranch:    gitlab.Str(payload, "target_branch"),
		State:           gitlab.Str(payload, "state"),
		HeadSHA:         gitlab.HeadSHA(payload),
		GitLabUpdatedAt: gitlab.Str(payload, "updated_at"),
	}
}

func (s *Service) fetchMR(ref gitlab.Ref) (*db.MergeRequest, error) {
	payload, err := s.GitLab.GetMR(ref)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	row := s.mrFromPayload(payload, ref)
	if n, err := s.GitLab.CountUnresolved(ref); err == nil {
		row.Unresolved = n
	}
	return s.DB.UpsertMR(row)
}

// AddMR adds a merge request by URL / short reference.
func (s *Service) AddMR(reference string) (*db.MergeRequest, error) {
	host, project := s.DefaultProject()
	ref, err := gitlab.ParseMRRef(reference, host, project)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.fetchMR(ref)
}

// RefreshMR re-reads a merge request from GitLab.
func (s *Service) RefreshMR(id int64) (*db.MergeRequest, error) {
	mr, err := s.DB.GetMR(id)
	if err != nil || mr == nil {
		return nil, userErr("merge request #%d is not in the dashboard", id)
	}
	return s.fetchMR(gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID})
}

// SyncResult reports a sync.
type SyncResult struct {
	Username string
	Synced   int
	Project  string
}

// SyncMRs pulls open MRs where the current user has one of the configured roles.
func (s *Service) SyncMRs() (SyncResult, error) {
	host, project := s.DefaultProject()
	if host == "" {
		return SyncResult{}, userErr("GitLab host is unknown: set GITLAB_HOST or make sure the project has an origin remote")
	}
	username := s.CurrentUser()
	if username == "" {
		return SyncResult{}, userErr("glab is not authenticated for %s (run `glab auth login --hostname %s`)", host, host)
	}
	filter := ""
	if s.Settings.GitLabSyncOnlyProject {
		filter = project
	}
	items, err := s.GitLab.ListOpenMRs(host, username, s.Settings.GitLabSyncRoles, filter)
	if err != nil {
		return SyncResult{}, &UserError{err.Error()}
	}
	result := SyncResult{Username: username, Project: firstOf(filter, "*")}
	for _, item := range items {
		itemHost, itemProject, ok := gitlab.ProjectPathOf(item, "!")
		if !ok {
			itemHost, itemProject = host, project
		}
		ref := gitlab.Ref{Host: itemHost, ProjectPath: itemProject, IID: gitlab.Int(item, "iid")}
		row := s.mrFromPayload(item, ref)
		if n, err := s.GitLab.CountUnresolved(ref); err == nil {
			row.Unresolved = n
		}
		if _, err := s.DB.UpsertMR(row); err == nil {
			result.Synced++
		}
	}
	return result, nil
}

// DeleteMR removes an MR (cancelling an active run first).
func (s *Service) DeleteMR(id int64) error {
	if active, _ := s.DB.ActiveRunForMR(id); active != nil {
		s.Cancel(active.ID)
	}
	return s.DB.DeleteMR(id)
}

// ---------------------------------------------------------------------------------- issues

func (s *Service) fetchIssue(ref gitlab.Ref) (*db.Issue, error) {
	payload, err := s.GitLab.GetIssue(ref)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.DB.UpsertIssue(issueFromPayload(payload, ref))
}

func issueFromPayload(payload map[string]any, ref gitlab.Ref) db.Issue {
	return db.Issue{
		GitLabHost:      ref.Host,
		ProjectPath:     ref.ProjectPath,
		IID:             gitlab.Int(payload, "iid"),
		WebURL:          firstOf(gitlab.Str(payload, "web_url"), ref.IssueURL()),
		Title:           gitlab.Str(payload, "title"),
		Description:     gitlab.Str(payload, "description"),
		Author:          gitlab.Str(gitlab.Nested(payload, "author"), "username"),
		State:           gitlab.Str(payload, "state"),
		Labels:          gitlab.Labels(payload),
		GitLabUpdatedAt: gitlab.Str(payload, "updated_at"),
	}
}

// AddIssue adds an issue by URL / short reference.
func (s *Service) AddIssue(reference string) (*db.Issue, error) {
	host, project := s.DefaultProject()
	ref, err := gitlab.ParseIssueRef(reference, host, project)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.fetchIssue(ref)
}

// RefreshIssue re-reads an issue.
func (s *Service) RefreshIssue(id int64) (*db.Issue, error) {
	issue, err := s.DB.GetIssue(id)
	if err != nil || issue == nil {
		return nil, userErr("issue #%d is not in the dashboard", id)
	}
	return s.fetchIssue(gitlab.Ref{Host: issue.GitLabHost, ProjectPath: issue.ProjectPath, IID: issue.IID})
}

// SyncIssues pulls open issues assigned to the current user.
func (s *Service) SyncIssues() (SyncResult, error) {
	host, project := s.DefaultProject()
	if host == "" {
		return SyncResult{}, userErr("GitLab host is unknown: set GITLAB_HOST or make sure the project has an origin remote")
	}
	username := s.CurrentUser()
	if username == "" {
		return SyncResult{}, userErr("glab is not authenticated for %s", host)
	}
	filter := ""
	if s.Settings.GitLabSyncOnlyProject {
		filter = project
	}
	items, err := s.GitLab.ListOpenIssues(host, username, filter)
	if err != nil {
		return SyncResult{}, &UserError{err.Error()}
	}
	result := SyncResult{Username: username, Project: firstOf(filter, "*")}
	for _, item := range items {
		itemHost, itemProject, ok := gitlab.ProjectPathOf(item, "#")
		if !ok {
			itemHost, itemProject = host, project
		}
		ref := gitlab.Ref{Host: itemHost, ProjectPath: itemProject, IID: gitlab.Int(item, "iid")}
		if _, err := s.DB.UpsertIssue(issueFromPayload(item, ref)); err == nil {
			result.Synced++
		}
	}
	return result, nil
}

// DeleteIssue removes an issue (cancelling an active run first).
func (s *Service) DeleteIssue(id int64) error {
	if active, _ := s.DB.ActiveRunForIssue(id); active != nil {
		s.Cancel(active.ID)
	}
	return s.DB.DeleteIssue(id)
}

// ---------------------------------------------------------------------------------- starting runs

// StartReview queues a quick/full/verify review of an MR.
func (s *Service) StartReview(mrID int64, kind, runnerName string) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	switch kind {
	case db.KindReviewQuick, db.KindReviewFull, db.KindReviewVerify:
	default:
		return 0, userErr("unknown review kind %q", kind)
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	if active, _ := s.DB.ActiveRunForMR(mrID); active != nil {
		return 0, userErr("a run for this merge request is already queued or running (#%d)", active.ID)
	}
	mr, err := s.RefreshMR(mrID)
	if err != nil {
		return 0, err
	}
	run := db.Run{Kind: kind, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), SkillIdentifier: skillID(s.SkillFor(kind))}
	if kind == db.KindReviewVerify {
		base, _ := s.DB.LatestDoneReview(mrID)
		if base == nil {
			return 0, userErr("nothing to verify yet: run a quick or full review first")
		}
		run.BaseRunID = &base.ID
	}
	return s.enqueue(run)
}

// StartFixComments queues an edit run that addresses unresolved reviewer discussions in a worktree of the MR branch.
func (s *Service) StartFixComments(mrID int64, runnerName, notes string) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	if active, _ := s.DB.ActiveRunForMR(mrID); active != nil {
		return 0, userErr("a run for this merge request is already queued or running (#%d)", active.ID)
	}
	mr, err := s.RefreshMR(mrID)
	if err != nil {
		return 0, err
	}
	if mr.SourceBranch == "" {
		return 0, userErr("the merge request has no source branch")
	}
	run := db.Run{Kind: db.KindFixComments, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), Notes: notes,
		Branch: mr.SourceBranch, WorkDir: s.Worktrees.Path(mr.SourceBranch), SkillIdentifier: skillID(s.SkillFor(db.KindFixComments))}
	return s.enqueue(run)
}

// StartPlan queues a read-only task analysis.
func (s *Service) StartPlan(issueID int64, runnerName, notes string) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	if active, _ := s.DB.ActiveRunForIssue(issueID); active != nil {
		return 0, userErr("a run for this issue is already queued or running (#%d)", active.ID)
	}
	issue, err := s.RefreshIssue(issueID)
	if err != nil {
		return 0, err
	}
	return s.enqueue(db.Run{Kind: db.KindPlan, IssueID: &issue.ID, Runner: r.Name(), Notes: notes, SkillIdentifier: skillID(s.SkillFor(db.KindPlan))})
}

// StartImplement queues an edit run in a worktree on `branch` (default: the issue reference).
func (s *Service) StartImplement(issueID int64, runnerName, notes, branch string) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	if active, _ := s.DB.ActiveRunForIssue(issueID); active != nil {
		return 0, userErr("a run for this issue is already queued or running (#%d)", active.ID)
	}
	issue, err := s.RefreshIssue(issueID)
	if err != nil {
		return 0, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = issue.Ref()
	}
	if strings.ContainsAny(branch, " \t~^:?*[\\") || strings.HasPrefix(branch, "-") {
		return 0, userErr("invalid branch name %q", branch)
	}
	return s.enqueue(db.Run{Kind: db.KindImplement, IssueID: &issue.ID, Runner: r.Name(), Notes: notes,
		Branch: branch, WorkDir: s.Worktrees.Path(branch), SkillIdentifier: skillID(s.SkillFor(db.KindImplement))})
}

func (s *Service) enqueue(run db.Run) (int64, error) {
	id, err := s.DB.CreateRun(run)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[id] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.cancels, id)
			s.mu.Unlock()
			cancel()
		}()
		s.sem <- struct{}{}
		defer func() { <-s.sem }()
		s.execute(ctx, id)
	}()
	return id, nil
}

// Retry starts a new run with the same parameters as a finished/failed one.
func (s *Service) Retry(runID int64) (int64, error) {
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return 0, userErr("run #%d not found", runID)
	}
	if run.Active() {
		return 0, userErr("the run is still active")
	}
	switch run.Kind {
	case db.KindReviewQuick, db.KindReviewFull, db.KindReviewVerify:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartReview(*run.MRID, run.Kind, run.Runner)
	case db.KindFixComments:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartFixComments(*run.MRID, run.Runner, run.Notes)
	case db.KindPlan:
		if run.IssueID == nil {
			return 0, userErr("run has no issue")
		}
		return s.StartPlan(*run.IssueID, run.Runner, run.Notes)
	case db.KindImplement:
		if run.IssueID == nil {
			return 0, userErr("run has no issue")
		}
		return s.StartImplement(*run.IssueID, run.Runner, run.Notes, run.Branch)
	}
	return 0, userErr("cannot retry a %s run", run.Kind)
}

// Cancel stops a queued/running run.
func (s *Service) Cancel(runID int64) bool {
	run, _ := s.DB.GetRun(runID)
	if run == nil || !run.Active() {
		return false
	}
	s.mu.Lock()
	cancel := s.cancels[runID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if run.Status == db.StatusQueued {
		_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusCancelled, "error": "Cancelled before start", "finished_at": db.Now()})
	}
	return true
}

// ---------------------------------------------------------------------------------- execution

func (s *Service) logFile(runID int64) (*os.File, string) {
	dir := s.Settings.RunLogDir()
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fmt.Sprintf("run-%d.log", runID))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, path
	}
	return file, path
}

func (s *Service) fail(runID int64, message string) {
	_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusFailed, "error": message, "finished_at": db.Now()})
}

func (s *Service) execute(ctx context.Context, runID int64) {
	defer func() {
		if r := recover(); r != nil {
			s.fail(runID, fmt.Sprintf("internal error: %v", r))
		}
	}()
	run, _ := s.DB.GetRun(runID)
	if run == nil || run.Status != db.StatusQueued {
		return
	}
	if ctx.Err() != nil {
		_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusCancelled, "error": "Cancelled before start", "finished_at": db.Now()})
		return
	}
	r, err := s.pickRunner(run.Runner)
	if err != nil {
		s.fail(runID, err.Error())
		return
	}
	log, logPath := s.logFile(runID)
	if log != nil {
		defer log.Close()
	}
	logln := func(msg string) {
		if log != nil {
			fmt.Fprintf(log, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), msg)
		}
	}
	logln(fmt.Sprintf("run #%d kind=%s runner=%s", runID, run.Kind, run.Runner))

	// Build the prompt and decide where to run. The project skill for this action is authoritative:
	// an agent is selected with --agent, a command/skill is invoked as a slash command inside the prompt.
	sk := s.SkillFor(run.Kind)
	if sk != nil {
		logln("project skill: " + sk.Identifier() + " (" + sk.RelPath + ")")
	} else {
		logln("project skill: none for " + run.Kind + "; using CLAUDE.md / AGENTS.md and the dashboard prompt")
	}
	req := runner.Request{
		Dir:          s.Settings.ProjectRoot,
		Mode:         runner.ModeReadOnly,
		Policy:       s.policy(),
		ProtectDirs:  []string{s.Settings.ProjectRoot},
		ExtraTools:   s.Settings.ClaudeExtraTools,
		MaxBudgetUSD: s.Settings.ClaudeMaxBudgetUSD,
		Timeout:      time.Duration(s.Settings.RunTimeoutSec) * time.Second,
		Log:          log,
		Permission:   s.permissionFunc(runID, logln),
		Progress: func(note string) {
			_ = s.DB.UpdateRun(runID, map[string]any{"progress": note})
		},
	}
	if sk != nil && sk.Kind == skill.KindAgent {
		req.Agent = sk.Name
	}
	var previous []db.Finding

	switch run.Kind {
	case db.KindReviewQuick, db.KindReviewFull, db.KindReviewVerify:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		pm := promptMR(mr)
		req.SessionName = fmt.Sprintf("mr-review !%d #%d", mr.IID, runID)
		switch run.Kind {
		case db.KindReviewQuick:
			req.Prompt, req.Schema = prompts.QuickReview(pm, sk), prompts.ReviewSchema
		case db.KindReviewFull:
			req.Prompt, req.Schema = prompts.FullReview(pm, sk), prompts.ReviewSchema
		default:
			var base *db.Run
			if run.BaseRunID != nil {
				base, _ = s.DB.GetRun(*run.BaseRunID)
			}
			baseSHA := ""
			if base != nil {
				baseSHA = base.HeadSHA
				all, _ := s.DB.ListFindings(base.ID)
				for _, f := range all {
					if f.Status == "open" {
						previous = append(previous, f)
					}
				}
			}
			req.Prompt, req.Schema = prompts.Verify(pm, sk, baseSHA, toPrev(previous)), prompts.VerifySchema
		}
	case db.KindFixComments:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		path, err := s.Worktrees.Prepare(ctx, run.Branch, logln)
		if err != nil {
			s.fail(runID, "worktree: "+err.Error())
			return
		}
		req.Dir, req.Mode, req.ProtectDirs = path, runner.ModeEdit, nil
		req.Prompt, req.Schema = prompts.FixComments(promptMR(mr), run.Notes, sk), prompts.FixSchema
		req.SessionName = fmt.Sprintf("fix-comments !%d #%d", mr.IID, runID)
	case db.KindPlan, db.KindImplement:
		issue, _ := s.DB.GetIssue(*run.IssueID)
		if issue == nil {
			s.fail(runID, "issue disappeared")
			return
		}
		pi := prompts.Issue{WebURL: issue.WebURL, ProjectPath: issue.ProjectPath, Host: issue.GitLabHost, IID: issue.IID, Title: issue.Title, Description: issue.Description}
		if run.Kind == db.KindPlan {
			req.Prompt, req.Schema = prompts.Plan(pi, run.Notes, sk), prompts.PlanSchema
			req.SessionName = fmt.Sprintf("plan #%d run %d", issue.IID, runID)
		} else {
			path, err := s.Worktrees.Prepare(ctx, run.Branch, logln)
			if err != nil {
				s.fail(runID, "worktree: "+err.Error())
				return
			}
			req.Dir, req.Mode, req.ProtectDirs = path, runner.ModeEdit, nil
			req.Prompt, req.Schema = prompts.Implement(pi, run.Notes, run.Branch, s.Settings.BaseBranch, sk), prompts.ImplementSchema
			req.SessionName = fmt.Sprintf("implement #%d run %d", issue.IID, runID)
		}
	default:
		s.fail(runID, "unknown run kind "+run.Kind)
		return
	}

	_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusRunning, "started_at": db.Now(), "prompt": req.Prompt, "log_path": logPath, "work_dir": req.Dir})
	result, runErr := r.Run(ctx, req)

	fields := map[string]any{"finished_at": db.Now(), "progress": ""}
	if result != nil {
		fields["cost_usd"] = result.CostUSD
		fields["duration_ms"] = result.DurationMs
		fields["input_tokens"] = result.Usage.Input
		fields["output_tokens"] = result.Usage.Output
		fields["cache_read_tokens"] = result.Usage.CacheRead
		fields["cache_write_tokens"] = result.Usage.CacheWrite
		fields["session_id"] = result.SessionID
		fields["denials_json"] = string(result.Denials)
		if len(result.Raw) > 0 && len(result.Raw) < 2_000_000 {
			fields["raw_result"] = string(result.Raw)
		}
	}
	if errors.Is(runErr, runner.ErrCancelled) || ctx.Err() == context.Canceled {
		fields["status"], fields["error"] = db.StatusCancelled, "Cancelled"
		_ = s.DB.UpdateRun(runID, fields)
		return
	}
	if runErr != nil {
		fields["status"], fields["error"] = db.StatusFailed, runErr.Error()
		_ = s.DB.UpdateRun(runID, fields)
		return
	}
	if err := s.store(run, result.Structured, previous, fields); err != nil {
		fields["status"], fields["error"] = db.StatusFailed, "cannot store result: "+err.Error()
		_ = s.DB.UpdateRun(runID, fields)
		return
	}
	fields["status"] = db.StatusDone
	_ = s.DB.UpdateRun(runID, fields)
}

// policy is the permission policy for Claude runs from the settings.
func (s *Service) policy() runner.Policy {
	switch s.Settings.ClaudePermissions {
	case "strict":
		return runner.PolicyStrict
	case "manual":
		return runner.PolicyManual
	}
	return runner.PolicyAuto
}

// ---------------------------------------------------------------------------------- permission prompts

// permissionFunc records the agent's permission prompt, switches the run to "waiting" and blocks until the
// developer answers in the dashboard, the run is cancelled or the approval timeout passes (then: denied).
func (s *Service) permissionFunc(runID int64, logln func(string)) runner.PermissionFunc {
	return func(ctx context.Context, req runner.PermissionRequest) runner.PermissionDecision {
		approvalID, err := s.DB.CreateApproval(db.Approval{RunID: runID, ToolName: req.ToolName, Description: req.Description,
			InputJSON: string(req.Input), SuggestionsJSON: string(req.Suggestions), Reason: req.Reason})
		if err != nil {
			return runner.PermissionDecision{Message: "mr-review: cannot record the permission prompt: " + err.Error()}
		}
		ch := make(chan approvalDecision, 1)
		s.mu.Lock()
		s.decisions[approvalID] = ch
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.decisions, approvalID)
			s.mu.Unlock()
		}()
		_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusWaiting, "progress": "Нужен ваш ответ: " + req.ToolName + " " + req.Description})
		logln(fmt.Sprintf("waiting for the developer: approval #%d (%s %s)", approvalID, req.ToolName, req.Description))
		timeout := time.Duration(s.Settings.ApprovalTimeoutSec) * time.Second
		var decision runner.PermissionDecision
		select {
		case d := <-ch:
			decision = runner.PermissionDecision{Allow: d.allow, ApplySuggestions: d.remember, Message: d.note}
		case <-ctx.Done():
			_ = s.DB.DecideApproval(approvalID, db.ApprovalExpired, false, "run cancelled")
			return runner.PermissionDecision{Message: "run cancelled by the developer"}
		case <-time.After(timeout):
			_ = s.DB.DecideApproval(approvalID, db.ApprovalExpired, false, "no answer within "+timeout.String())
			decision = runner.PermissionDecision{Message: "mr-review: the developer did not answer within " + timeout.String() + "; treat this call as denied"}
			logln(fmt.Sprintf("approval #%d expired after %s", approvalID, timeout))
		}
		if ctx.Err() == nil {
			_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusRunning, "progress": ""})
		}
		return decision
	}
}

// Decide answers a pending permission prompt: decision is allow | allow_always (allow and apply the agent's
// suggested rules for the rest of the run) | deny (note is passed to the agent).
func (s *Service) Decide(approvalID int64, decision, note string) error {
	approval, _ := s.DB.GetApproval(approvalID)
	if approval == nil {
		return userErr("approval #%d not found", approvalID)
	}
	if !approval.Pending() {
		return userErr("this prompt was already answered (%s)", approval.Status)
	}
	var d approvalDecision
	status := db.ApprovalDenied
	switch decision {
	case "allow":
		d.allow, status = true, db.ApprovalAllowed
	case "allow_always":
		d.allow, d.remember, status = true, approval.SuggestionsJSON != "", db.ApprovalAllowed
	case "deny":
		d.note = strings.TrimSpace(note)
	default:
		return userErr("decision must be allow, allow_always or deny")
	}
	s.mu.Lock()
	ch := s.decisions[approvalID]
	s.mu.Unlock()
	if ch == nil {
		_ = s.DB.DecideApproval(approvalID, db.ApprovalExpired, false, "the run is no longer waiting")
		return userErr("the run is no longer waiting for this answer (dashboard restarted or run stopped)")
	}
	if err := s.DB.DecideApproval(approvalID, status, d.remember, d.note); err != nil {
		return err
	}
	select {
	case ch <- d:
	default:
	}
	return nil
}

// ---------------------------------------------------------------------------------- plan export

// PlansDir is where exported plans are written: PLANS_DIR or <project>/.claude/plans.
func (s *Service) PlansDir() string {
	if s.Settings.PlansDir != "" {
		return s.Settings.PlansDir
	}
	return filepath.Join(s.Settings.ProjectRoot, ".claude", "plans")
}

// ExportPlan writes a finished plan run as a markdown file into PlansDir and remembers the path.
func (s *Service) ExportPlan(runID int64) (string, error) {
	run, _ := s.DB.GetRun(runID)
	if run == nil || run.Kind != db.KindPlan {
		return "", userErr("run #%d is not a plan", runID)
	}
	if run.Status != db.StatusDone || run.ResultJSON == "" {
		return "", userErr("the plan is not finished yet")
	}
	if _, err := s.requireRoot(); err != nil {
		return "", err
	}
	var issue *db.Issue
	if run.IssueID != nil {
		issue, _ = s.DB.GetIssue(*run.IssueID)
	}
	if issue == nil {
		return "", userErr("the issue of this plan is gone")
	}
	dir := s.PlansDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", &UserError{"cannot create " + dir + ": " + err.Error()}
	}
	name := fmt.Sprintf("%s-%s-run%d.md", time.Now().Format("2006-01-02"), worktree.Slug(issue.Ref()), run.ID)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(planMarkdown(run, issue)), 0o644); err != nil {
		return "", &UserError{"cannot write " + path + ": " + err.Error()}
	}
	_ = s.DB.UpdateRun(runID, map[string]any{"plan_path": path})
	return path, nil
}

// planMarkdown renders a plan result as a markdown document in the project's plan-file style.
func planMarkdown(run *db.Run, issue *db.Issue) string {
	var plan struct {
		Summary string   `json:"summary"`
		Steps   []string `json:"steps"`
		Files   []struct {
			Path   string `json:"path"`
			Change string `json:"change"`
		} `json:"files"`
		Risks     []string `json:"risks"`
		Questions []string `json:"questions"`
		Estimate  string   `json:"estimate"`
	}
	_ = json.Unmarshal([]byte(run.ResultJSON), &plan)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", issue.Ref(), issue.Title)
	fmt.Fprintf(&b, "- Задача: %s\n- Исследование: mr-review, сессия #%d, агент %s", issue.WebURL, run.ID, run.Runner)
	if run.SkillIdentifier != "" {
		fmt.Fprintf(&b, ", skill %s", run.SkillIdentifier)
	}
	fmt.Fprintf(&b, "\n- Дата: %s\n", time.Now().Format("2006-01-02"))
	if plan.Estimate != "" {
		fmt.Fprintf(&b, "- Оценка: %s\n", plan.Estimate)
	}
	if strings.TrimSpace(run.Notes) != "" {
		fmt.Fprintf(&b, "\n## Указания разработчика\n\n%s\n", strings.TrimSpace(run.Notes))
	}
	fmt.Fprintf(&b, "\n## Суть\n\n%s\n\n## Шаги\n\n", strings.TrimSpace(plan.Summary))
	for i, step := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	if len(plan.Files) > 0 {
		b.WriteString("\n## Файлы\n\n| Файл | Что меняется |\n|---|---|\n")
		for _, f := range plan.Files {
			fmt.Fprintf(&b, "| `%s` | %s |\n", f.Path, strings.ReplaceAll(f.Change, "|", "\\|"))
		}
	}
	if len(plan.Risks) > 0 {
		b.WriteString("\n## Риски\n\n")
		for _, r := range plan.Risks {
			fmt.Fprintf(&b, "- %s\n", r)
		}
	}
	if len(plan.Questions) > 0 {
		b.WriteString("\n## Открытые вопросы\n\n")
		for _, q := range plan.Questions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
	}
	return b.String()
}

// reviewResult mirrors prompts.ReviewSchema / VerifySchema.
type reviewResult struct {
	Summary     string `json:"summary"`
	Verdict     string `json:"verdict"`
	ReviewedSHA string `json:"reviewed_sha"`
	Findings    []struct {
		Severity    string `json:"severity"`
		Category    string `json:"category"`
		File        string `json:"file"`
		Line        *int64 `json:"line"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Suggestion  string `json:"suggestion"`
	} `json:"findings"`
	NewFindings []struct {
		Severity    string `json:"severity"`
		Category    string `json:"category"`
		File        string `json:"file"`
		Line        *int64 `json:"line"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Suggestion  string `json:"suggestion"`
	} `json:"new_findings"`
	Verified []struct {
		FindingID int64  `json:"finding_id"`
		Status    string `json:"status"`
		Note      string `json:"note"`
	} `json:"verified"`
	Discussions []struct {
		Author     string `json:"author"`
		File       string `json:"file"`
		Line       *int64 `json:"line"`
		Body       string `json:"body"`
		Assessment string `json:"assessment"`
		Addressed  bool   `json:"addressed"`
	} `json:"unresolved_discussions"`
}

func (s *Service) store(run *db.Run, structured json.RawMessage, previous []db.Finding, fields map[string]any) error {
	fields["result_json"] = string(structured)
	if !run.IsReview() {
		var generic struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(structured, &generic)
		fields["summary"] = generic.Summary
		return nil
	}
	var res reviewResult
	if err := json.Unmarshal(structured, &res); err != nil {
		return err
	}
	fields["summary"], fields["verdict"] = res.Summary, res.Verdict
	if res.ReviewedSHA != "" {
		fields["head_sha"] = res.ReviewedSHA
	}
	var findings []db.Finding
	if run.Kind == db.KindReviewVerify {
		verified := map[int64]struct{ Status, Note string }{}
		for _, v := range res.Verified {
			verified[v.FindingID] = struct{ Status, Note string }{v.Status, v.Note}
		}
		for _, prev := range previous {
			status, note := "open", "Not addressed by the verify run"
			if v, ok := verified[prev.ID]; ok {
				status, note = firstOf(v.Status, "open"), v.Note
			}
			origin := prev.ID
			findings = append(findings, db.Finding{OriginFindingID: &origin, Status: status, Severity: prev.Severity, Category: prev.Category, File: prev.File, Line: prev.Line, Title: prev.Title, Description: prev.Description, Suggestion: prev.Suggestion, VerifyNote: note})
		}
		for _, f := range res.NewFindings {
			findings = append(findings, db.Finding{Status: "open", Severity: f.Severity, Category: f.Category, File: f.File, Line: f.Line, Title: f.Title, Description: f.Description, Suggestion: f.Suggestion, VerifyNote: "New in this head"})
		}
	} else {
		for _, f := range res.Findings {
			findings = append(findings, db.Finding{Status: "open", Severity: f.Severity, Category: f.Category, File: f.File, Line: f.Line, Title: f.Title, Description: f.Description, Suggestion: f.Suggestion})
		}
	}
	if err := s.DB.ReplaceFindings(run.ID, findings); err != nil {
		return err
	}
	var discussions []db.Discussion
	for _, d := range res.Discussions {
		discussions = append(discussions, db.Discussion{Author: d.Author, File: d.File, Line: d.Line, Body: d.Body, Assessment: d.Assessment, Addressed: d.Addressed})
	}
	return s.DB.ReplaceDiscussions(run.ID, discussions)
}

// ---------------------------------------------------------------------------------- follow-up chat

// Ask resumes the run's agent session with a question and stores both sides of the exchange.
func (s *Service) Ask(runID int64, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", userErr("question is empty")
	}
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return "", userErr("run #%d not found", runID)
	}
	if run.Status != db.StatusDone && run.Status != db.StatusFailed {
		return "", userErr("wait until the run has finished")
	}
	if run.SessionID == "" {
		return "", userErr("this run has no resumable session (the %s runner did not report one)", run.Runner)
	}
	r, err := s.pickRunner(run.Runner)
	if err != nil {
		return "", err
	}
	_, _ = s.DB.AddMessage(runID, "user", question, 0, 0)
	dir := run.WorkDir
	if dir == "" || !dirExists(dir) {
		dir = s.Settings.ProjectRoot
	}
	log, _ := s.logFile(runID)
	if log != nil {
		defer log.Close()
		fmt.Fprintf(log, "\n=== follow-up question ===\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	result, err := r.Run(ctx, runner.Request{
		Prompt:          prompts.FollowUp(question),
		Dir:             dir,
		Mode:            runner.ModeReadOnly,
		Policy:          s.policy(),
		ProtectDirs:     []string{s.Settings.ProjectRoot},
		ResumeSessionID: run.SessionID,
		ExtraTools:      s.Settings.ClaudeExtraTools,
		Timeout:         15 * time.Minute,
		Log:             log,
	})
	if err != nil {
		_, _ = s.DB.AddMessage(runID, "error", err.Error(), 0, 0)
		return "", &UserError{err.Error()}
	}
	answer := strings.TrimSpace(result.Text)
	_, _ = s.DB.AddMessage(runID, "assistant", answer, result.CostUSD, result.Usage.Total())
	if result.SessionID != "" && result.SessionID != run.SessionID {
		_ = s.DB.UpdateRun(runID, map[string]any{"session_id": result.SessionID})
	}
	return answer, nil
}

// ---------------------------------------------------------------------------------- worktree actions

// WorktreeState summarises a worktree for the run page.
type WorktreeState struct {
	Exists bool
	Path   string
	Branch string
	Status string
	Diff   string
	Log    string
}

// Worktree returns the current state of a run's worktree.
func (s *Service) Worktree(run *db.Run) WorktreeState {
	state := WorktreeState{Path: run.WorkDir, Branch: run.Branch}
	if s.Worktrees == nil || run.WorkDir == "" || !s.Worktrees.Exists(run.WorkDir) {
		return state
	}
	state.Exists = true
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	state.Status, _ = s.Worktrees.Status(ctx, run.WorkDir)
	state.Diff, _ = s.Worktrees.Diff(ctx, run.WorkDir)
	state.Log, _ = s.Worktrees.Log(ctx, run.WorkDir)
	return state
}

func (s *Service) editRun(runID int64) (*db.Run, error) {
	run, _ := s.DB.GetRun(runID)
	if run == nil || !run.IsEdit() {
		return nil, userErr("run #%d is not a worktree run", runID)
	}
	if run.Active() {
		return nil, userErr("the run is still active")
	}
	if s.Worktrees == nil || !s.Worktrees.Exists(run.WorkDir) {
		return nil, userErr("the worktree no longer exists")
	}
	return run, nil
}

// Commit commits everything in the run's worktree.
func (s *Service) Commit(runID int64, message string) (string, error) {
	run, err := s.editRun(runID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	out, err := s.Worktrees.Commit(ctx, run.WorkDir, message)
	if err != nil {
		return "", &UserError{err.Error()}
	}
	return out, nil
}

// Push pushes the run's branch to origin.
func (s *Service) Push(runID int64) (string, error) {
	run, err := s.editRun(runID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	out, err := s.Worktrees.Push(ctx, run.WorkDir, run.Branch)
	if err != nil {
		return "", &UserError{err.Error()}
	}
	return out, nil
}

// CreateMR opens a merge request for an implement run's branch.
func (s *Service) CreateMR(runID int64, title, description string) (string, error) {
	run, err := s.editRun(runID)
	if err != nil {
		return "", err
	}
	if run.IssueID == nil {
		return "", userErr("only implementation runs create merge requests; comment fixes update the existing MR")
	}
	issue, _ := s.DB.GetIssue(*run.IssueID)
	if issue == nil {
		return "", userErr("issue not found")
	}
	host, project := issue.GitLabHost, issue.ProjectPath
	if _, defProject := s.DefaultProject(); defProject != "" {
		project = defProject // the branch lives in the code repository, not necessarily the issue's project
	}
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("%s %s", issue.Ref(), issue.Title)
	}
	if strings.TrimSpace(description) == "" {
		description = "Closes " + issue.WebURL
	}
	url, err := s.GitLab.CreateMR(host, project, run.Branch, s.Settings.BaseBranch, title, description)
	if err != nil {
		return "", &UserError{err.Error()}
	}
	return url, nil
}

// RemoveWorktree deletes the worktree directory of a run (branch is kept).
func (s *Service) RemoveWorktree(runID int64) error {
	run, err := s.editRun(runID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	if err := s.Worktrees.Remove(ctx, run.WorkDir); err != nil {
		return &UserError{err.Error()}
	}
	return nil
}

// ---------------------------------------------------------------------------------- helpers

func promptMR(mr *db.MergeRequest) prompts.MR {
	return prompts.MR{WebURL: mr.WebURL, ProjectPath: mr.ProjectPath, Host: mr.GitLabHost, IID: mr.IID, Title: mr.Title, SourceBranch: mr.SourceBranch, TargetBranch: mr.TargetBranch, HeadSHA: mr.HeadSHA}
}

func toPrev(findings []db.Finding) []prompts.PrevFinding {
	out := make([]prompts.PrevFinding, 0, len(findings))
	for _, f := range findings {
		out = append(out, prompts.PrevFinding{ID: f.ID, Severity: f.Severity, File: f.File, Line: f.Line, Title: f.Title, Description: f.Description})
	}
	return out
}

func skillID(s *skill.Skill) string {
	if s == nil {
		return ""
	}
	return s.Identifier()
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
