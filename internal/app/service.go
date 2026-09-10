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
	"mr-review/internal/terminal"
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

	envSkillNames map[string]string  // SKILL_* from .env, the base the UI overrides are merged onto
	terminal      *terminal.Emulator // detected once at start; nil = «Открыть в терминале» unavailable
	terminalNote  string             // why no terminal is available

	sem       chan struct{}
	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc
	changes   map[string]gitlab.Changes       // compare results per "ref from..to" (Changes)
	jobs      map[string][]prompts.FailedJob  // failed jobs with log tails per "ref pipeline" (FailedJobsFor)
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
	em, note := terminal.Detect(settings.TerminalCmd)
	s := &Service{
		Settings:  settings,
		DB:        database,
		GitLab:    gl,
		Runners:   map[string]runner.Runner{},
		sem:       make(chan struct{}, settings.RunConcurrency),
		cancels:   map[int64]context.CancelFunc{},
		decisions: map[int64]chan approvalDecision{},
	}
	s.terminal, s.terminalNote = em, note
	if database != nil {
		s.applySkillSettings()
	}
	for _, r := range runners {
		if _, _, ok := r.Detect(); ok {
			s.Runners[r.Name()] = r
		}
	}
	if settings.ReportLanguage != "" {
		prompts.Language = settings.ReportLanguage
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
	s.mu.Lock()
	names := make(map[string]string, len(s.Settings.SkillNames))
	for k, v := range s.Settings.SkillNames {
		names[k] = v
	}
	custom := make(map[string]string, len(s.Settings.CustomSkills))
	for k, v := range s.Settings.CustomSkills {
		custom[k] = v
	}
	s.mu.Unlock()
	return skill.NewWithNames(s.Settings.ProjectRoot, names).WithCustom(custom)
}

// SkillSetting is the UI override of one action: another project skill name and/or dashboard-written instructions.
type SkillSetting struct {
	Name   string // project skill name override ("" = .env / default)
	Custom bool   // use the dashboard instructions instead of a project skill
	Text   string // the instructions
}

// SkillSettings returns the UI overrides per action kind (stored in the settings table).
func (s *Service) SkillSettings() map[string]SkillSetting {
	out := map[string]SkillSetting{}
	values, _ := s.DB.SettingsWithPrefix("skill.")
	for _, action := range skill.Actions {
		out[action.Kind] = SkillSetting{
			Name:   values["skill."+action.Kind+".name"],
			Custom: values["skill."+action.Kind+".custom"] == "1",
			Text:   values["skill."+action.Kind+".text"],
		}
	}
	return out
}

// SaveSkillSetting stores the UI override of an action and applies it immediately (no restart).
func (s *Service) SaveSkillSetting(kind, name string, useCustom bool, text string) error {
	if skill.ActionFor(kind) == nil {
		return userErr("unknown action %q", kind)
	}
	name, text = strings.TrimSpace(name), strings.TrimSpace(text)
	if useCustom && text == "" {
		return userErr("custom instructions are empty: write them or switch the checkbox off")
	}
	if strings.ContainsAny(name, " /\\") {
		return userErr("skill name must be a plain agent/command/skill name, not a path")
	}
	custom := ""
	if useCustom {
		custom = "1"
	}
	for key, value := range map[string]string{"skill." + kind + ".name": name, "skill." + kind + ".custom": custom, "skill." + kind + ".text": text} {
		if err := s.DB.SetSetting(key, value); err != nil {
			return err
		}
	}
	s.applySkillSettings()
	return nil
}

// applySkillSettings merges the UI overrides into the settings the resolver and doctor read: .env values stay
// as the base, a UI name replaces the .env name, custom instructions replace the skill.
func (s *Service) applySkillSettings() {
	values, _ := s.DB.SettingsWithPrefix("skill.")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envSkillNames == nil {
		s.envSkillNames = map[string]string{}
		for k, v := range s.Settings.SkillNames {
			s.envSkillNames[k] = v
		}
	}
	names := map[string]string{}
	for k, v := range s.envSkillNames {
		names[k] = v
	}
	custom := map[string]string{}
	for _, action := range skill.Actions {
		if name := strings.TrimSpace(values["skill."+action.Kind+".name"]); name != "" {
			names[action.Kind] = name
		}
		if values["skill."+action.Kind+".custom"] == "1" && strings.TrimSpace(values["skill."+action.Kind+".text"]) != "" {
			custom[action.Kind] = values["skill."+action.Kind+".text"]
		}
	}
	s.Settings.SkillNames, s.Settings.CustomSkills = names, custom
}

// SkillCandidates lists the agents, commands and skills found in the project (for the UI dropdown).
func (s *Service) SkillCandidates() []skill.Skill {
	if s.Settings.ProjectRoot == "" {
		return nil
	}
	return skill.NewWithNames(s.Settings.ProjectRoot, nil).Candidates()
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
		PipelineStatus:  gitlab.PipelineStatus(payload),
		Diverged:        gitlab.Int(payload, "diverged_commits_count"),
		Draft:           gitlab.Bool(payload, "draft") || gitlab.Bool(payload, "work_in_progress"),
		ChangesCount:    gitlab.Str(payload, "changes_count"),
		Labels:          gitlab.Labels(payload),
		PipelineID:      gitlab.PipelineID(payload),
		PipelineURL:     gitlab.PipelineURL(payload),
	}
}

// fetchMR reads an MR from GitLab with its discussions, approvals and my roles, and stores it.
// manual marks MRs added by hand (they stay in the list regardless of roles).
func (s *Service) fetchMR(ref gitlab.Ref, manual bool) (*db.MergeRequest, error) {
	payload, err := s.GitLab.GetMR(ref)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	row := s.mrFromPayload(payload, ref)
	row.Manual = manual
	if n, err := s.GitLab.CountUnresolved(ref); err == nil {
		row.Unresolved = n
	}
	me := s.CurrentUser()
	if approvals, err := s.GitLab.GetApprovals(ref); err == nil {
		row.ApprovalsGiven, row.ApprovalsRequired = approvals.Given, approvals.Required
		row.ApprovedByMe = me != "" && contains(approvals.ApprovedBy, me)
	}
	if me != "" {
		var roles []string
		if row.Author == me {
			roles = append(roles, "author")
		}
		if contains(gitlab.Usernames(payload, "assignees"), me) {
			roles = append(roles, "assignee")
		}
		if contains(gitlab.Usernames(payload, "reviewers"), me) {
			roles = append(roles, "reviewer")
		}
		row.MyRoles = strings.Join(roles, ",")
	}
	return s.DB.UpsertMR(row)
}

// AddMR adds a merge request by URL / short reference (kept in the list until removed by hand).
func (s *Service) AddMR(reference string) (*db.MergeRequest, error) {
	host, project := s.DefaultProject()
	ref, err := gitlab.ParseMRRef(reference, host, project)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.fetchMR(ref, true)
}

// RefreshMR re-reads a merge request from GitLab.
func (s *Service) RefreshMR(id int64) (*db.MergeRequest, error) {
	mr, err := s.DB.GetMR(id)
	if err != nil || mr == nil {
		return nil, userErr("merge request #%d is not in the dashboard", id)
	}
	return s.fetchMR(gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}, false)
}

// SyncResult reports a sync.
type SyncResult struct {
	Username string
	Synced   int
	Pruned   int // MRs that no longer concern me and had no runs: removed
	Archived int // MRs that no longer concern me but have runs: kept in the history
	Project  string
	Duration time.Duration
}

// syncConcurrency is how many glab processes a sync runs at once (each MR costs three API calls of ~0.7 s).
const syncConcurrency = 6

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
	started := time.Now()
	result := SyncResult{Username: username, Project: firstOf(filter, "*")}
	// Every MR GitLab lists for me plus every open MR already in the dashboard is refreshed individually (the
	// list payload lacks pipeline/divergence/approval details), then classified: still mine → main list; no
	// longer mine (approved by me, role removed, merged/closed) → history when it has runs, otherwise removed.
	// Merged/closed MRs already in the history are not refreshed again: they do not change.
	refs := map[string]gitlab.Ref{}
	for _, item := range items {
		itemHost, itemProject, ok := gitlab.ProjectPathOf(item, "!")
		if !ok {
			itemHost, itemProject = host, project
		}
		ref := gitlab.Ref{Host: itemHost, ProjectPath: itemProject, IID: gitlab.Int(item, "iid")}
		refs[fmt.Sprintf("%s|%s|%d", ref.Host, ref.ProjectPath, ref.IID)] = ref
	}
	known, _ := s.DB.ListMRs()
	for _, mr := range known {
		key := fmt.Sprintf("%s|%s|%d", mr.GitLabHost, mr.ProjectPath, mr.IID)
		if _, ok := refs[key]; !ok && !mr.Closed() {
			refs[key] = gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
		}
	}
	s.CurrentUser() // resolve once before the workers start
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, syncConcurrency)
	for _, ref := range refs {
		wg.Add(1)
		go func(ref gitlab.Ref) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fresh, err := s.fetchMR(ref, false)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case fresh.Relevant():
				result.Synced++
			case fresh.Hidden || s.DB.CountRunsForMR(fresh.ID) > 0:
				result.Archived++
			default:
				if active, _ := s.DB.ActiveRunForMR(fresh.ID); active != nil {
					result.Archived++
					return
				}
				if err := s.DB.DeleteMR(fresh.ID); err == nil {
					result.Pruned++
				}
			}
		}(ref)
	}
	wg.Wait()
	result.Duration = time.Since(started)
	return result, nil
}

// DeleteMR removes an MR for good, with its runs (cancelling an active run first).
func (s *Service) DeleteMR(id int64) error {
	if active, _ := s.DB.ActiveRunForMR(id); active != nil {
		s.Cancel(active.ID)
	}
	return s.DB.DeleteMR(id)
}

// HideMR moves an MR into the history (its runs are kept); an active run is stopped.
func (s *Service) HideMR(id int64) error {
	if mr, _ := s.DB.GetMR(id); mr == nil {
		return userErr("merge request #%d is not in the dashboard", id)
	}
	if active, _ := s.DB.ActiveRunForMR(id); active != nil {
		s.Cancel(active.ID)
	}
	return s.DB.SetMRHidden(id, true)
}

// UnhideMR brings a hidden MR back to the main list (as long as it still concerns me).
func (s *Service) UnhideMR(id int64) error {
	if mr, _ := s.DB.GetMR(id); mr == nil {
		return userErr("merge request #%d is not in the dashboard", id)
	}
	return s.DB.SetMRHidden(id, false)
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
	// Tasks live wherever the team tracks them (often not in the code repository): by default every project where
	// I am the assignee; GITLAB_ISSUE_PROJECTS narrows it. A single project goes into the API call, several are
	// filtered client-side.
	wanted := map[string]bool{}
	for _, p := range s.Settings.GitLabIssueProjects {
		wanted[strings.ToLower(strings.Trim(p, "/"))] = true
	}
	filter := ""
	if len(wanted) == 1 {
		filter = s.Settings.GitLabIssueProjects[0]
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
		if len(wanted) > 0 && !wanted[strings.ToLower(itemProject)] {
			continue
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
// StartReview queues a review run. continueRunID > 0 starts it inside the agent session of that earlier run
// of the same MR (the developer's choice of context); 0 = new chat.
func (s *Service) StartReview(mrID int64, kind, runnerName string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	switch kind {
	case db.KindReviewQuick, db.KindReviewFull, db.KindReviewVerify:
	default:
		return 0, userErr("unknown review kind %q", kind)
	}
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Settings.ProjectRoot)
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
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
	run := db.Run{Kind: kind, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), SkillIdentifier: skillID(s.SkillFor(kind)), ContinueRunID: runIDPtr(prev)}
	if kind == db.KindReviewVerify {
		base, _ := s.DB.LatestDoneReview(mrID)
		if base == nil {
			return 0, userErr("nothing to verify yet: run a quick or full review first")
		}
		run.BaseRunID = &base.ID
	}
	return s.enqueue(run)
}

// StartVerifyFinding queues a read-only mini run that re-examines one finding of the MR's review («Проверить
// замечание»). Several finding checks may run in parallel; a review of the same MR must not be active.
func (s *Service) StartVerifyFinding(mrID, findingID int64, runnerName string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	finding, _ := s.DB.GetFinding(findingID)
	if finding == nil {
		return 0, userErr("finding #%d not found", findingID)
	}
	origin, _ := s.DB.GetRun(finding.RunID)
	if origin == nil || origin.MRID == nil || *origin.MRID != mrID {
		return 0, userErr("finding #%d does not belong to this merge request", findingID)
	}
	if finding.Status != "open" {
		return 0, userErr("finding #%d is not open (%s); reopen it first", findingID, finding.Status)
	}
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Settings.ProjectRoot)
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	if active, _ := s.DB.ActiveRunForMR(mrID); active != nil && active.Kind != db.KindVerifyFinding {
		return 0, userErr("a run for this merge request is already queued or running (#%d)", active.ID)
	}
	if active, _ := s.DB.ActiveFindingCheck(findingID); active != nil {
		return 0, userErr("this finding is already being checked (#%d)", active.ID)
	}
	mr, err := s.RefreshMR(mrID)
	if err != nil {
		return 0, err
	}
	fid := finding.ID
	return s.enqueue(db.Run{Kind: db.KindVerifyFinding, MRID: &mr.ID, FindingID: &fid, HeadSHA: mr.HeadSHA, Runner: r.Name(),
		SkillIdentifier: skillID(s.SkillFor(db.KindVerifyFinding)), ContinueRunID: runIDPtr(prev)})
}

// TerminalAvailable reports whether «Открыть в терминале» can work here, with the reason when it cannot.
func (s *Service) TerminalAvailable() (bool, string) {
	if s.terminal == nil {
		return false, s.terminalNote
	}
	if !terminal.GraphicalSession() {
		return false, "the dashboard runs without a graphical session (DISPLAY / WAYLAND_DISPLAY unset), so it cannot open windows"
	}
	return true, s.terminal.Name
}

// TerminalDir is the directory a run's terminal opens in: its workspace when it still exists, else the project root.
func (s *Service) TerminalDir(run *db.Run) string {
	if run.WorkDir != "" && dirExists(run.WorkDir) {
		return run.WorkDir
	}
	return s.Settings.ProjectRoot
}

// ResumeCommand is the interactive command that continues the run's agent session ("" when there is none).
func (s *Service) ResumeCommand(run *db.Run) string {
	if run == nil || run.SessionID == "" {
		return ""
	}
	bin := run.Runner
	switch run.Runner {
	case "claude":
		bin = s.Settings.ClaudeBin
	case "codex":
		bin = s.Settings.CodexBin
	case "cursor":
		bin = "cursor-agent"
	}
	return terminal.ResumeCommand(run.Runner, bin, run.SessionID)
}

// TerminalCommand is the full shell line a developer can paste: cd into the run's directory and resume the session.
func (s *Service) TerminalCommand(run *db.Run) string {
	dir := s.TerminalDir(run)
	if resume := s.ResumeCommand(run); resume != "" {
		return "cd " + terminal.Quote(dir) + " && " + resume
	}
	return "cd " + terminal.Quote(dir)
}

// OpenTerminal opens a terminal window for a run: in its directory, continuing the agent session when resume is
// set and the run has finished with a session id. Returns the command that was started inside the terminal.
func (s *Service) OpenTerminal(runID int64, resume bool) (string, error) {
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return "", userErr("run #%d not found", runID)
	}
	if ok, why := s.TerminalAvailable(); !ok {
		return "", userErr("cannot open a terminal: %s", why)
	}
	command := ""
	if resume {
		if run.Active() {
			return "", userErr("the session is still running; wait for it to finish, then continue it in the terminal")
		}
		command = s.ResumeCommand(run)
		if command == "" {
			return "", userErr("this run has no resumable session (the %s agent did not report one)", run.Runner)
		}
	}
	dir := s.TerminalDir(run)
	if dir == "" {
		return "", userErr("project root is unknown")
	}
	if err := s.terminal.Open(dir, terminal.ShellCommand(command)); err != nil {
		return "", &UserError{err.Error()}
	}
	if command == "" {
		return "cd " + terminal.Quote(dir), nil
	}
	return "cd " + terminal.Quote(dir) + " && " + command, nil
}

// Changes summarises what happened on the MR since a reviewed SHA (compare API), cached per SHA pair.
func (s *Service) Changes(mr *db.MergeRequest, fromSHA string) *gitlab.Changes {
	if mr == nil || fromSHA == "" || mr.HeadSHA == "" || fromSHA == mr.HeadSHA {
		return nil
	}
	key := fmt.Sprintf("%s/%s!%d %s..%s", mr.GitLabHost, mr.ProjectPath, mr.IID, fromSHA, mr.HeadSHA)
	s.mu.Lock()
	cached, ok := s.changes[key]
	s.mu.Unlock()
	if ok {
		return &cached
	}
	changes, err := s.GitLab.Compare(gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}, fromSHA, mr.HeadSHA)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	if s.changes == nil {
		s.changes = map[string]gitlab.Changes{}
	}
	s.changes[key] = changes
	s.mu.Unlock()
	return &changes
}

// FailedJobsFor returns the failed jobs of the MR's head pipeline with the tail of each log (cached per pipeline).
// nil when the pipeline is not failed or unknown.
func (s *Service) FailedJobsFor(mr *db.MergeRequest) []prompts.FailedJob {
	if mr == nil || mr.PipelineStatus != "failed" || mr.PipelineID == 0 {
		return nil
	}
	key := fmt.Sprintf("%s/%s!%d pipeline %d", mr.GitLabHost, mr.ProjectPath, mr.IID, mr.PipelineID)
	s.mu.Lock()
	cached, ok := s.jobs[key]
	s.mu.Unlock()
	if ok {
		return cached
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	list, err := s.GitLab.FailedJobs(ref, mr.PipelineID)
	if err != nil {
		return nil
	}
	var out []prompts.FailedJob
	for _, j := range list {
		trace, _ := s.GitLab.JobTrace(ref, j.ID, 200)
		out = append(out, prompts.FailedJob{Name: j.Name, Stage: j.Stage, FailureReason: j.FailureReason, AllowFailure: j.AllowFailure, WebURL: j.WebURL, Trace: trace})
	}
	s.mu.Lock()
	if s.jobs == nil {
		s.jobs = map[string][]prompts.FailedJob{}
	}
	s.jobs[key] = out
	s.mu.Unlock()
	return out
}

// StartCIAnalyze queues a read-only run that explains why the MR's head pipeline failed («Разобрать ошибки CI»).
func (s *Service) StartCIAnalyze(mrID int64, runnerName string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Settings.ProjectRoot)
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
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
	if mr.PipelineStatus != "failed" || mr.PipelineID == 0 {
		return 0, userErr("the head pipeline of !%d is not failed (%s)", mr.IID, firstOf(mr.PipelineStatus, "unknown"))
	}
	return s.enqueue(db.Run{Kind: db.KindCIAnalyze, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), SkillIdentifier: skillID(s.SkillFor(db.KindCIAnalyze)), ContinueRunID: runIDPtr(prev)})
}

// StartCIFix queues an edit run in the MR worktree that fixes what makes the head pipeline fail («Исправить CI»).
// analysisRunID (0 = none) hands the summary of an earlier CI analysis to the agent.
func (s *Service) StartCIFix(mrID int64, runnerName string, analysisRunID, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
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
	if mr.PipelineStatus != "failed" || mr.PipelineID == 0 {
		return 0, userErr("the head pipeline of !%d is not failed (%s)", mr.IID, firstOf(mr.PipelineStatus, "unknown"))
	}
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Worktrees.Path(mr.SourceBranch))
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	var base *int64
	if analysisRunID > 0 {
		if analysis, _ := s.DB.GetRun(analysisRunID); analysis != nil && analysis.Kind == db.KindCIAnalyze && analysis.MRID != nil && *analysis.MRID == mrID {
			base = &analysis.ID
		}
	}
	run := db.Run{Kind: db.KindCIFix, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), BaseRunID: base, ContinueRunID: runIDPtr(prev),
		Branch: mr.SourceBranch, WorkDir: s.Worktrees.Path(mr.SourceBranch), SkillIdentifier: skillID(s.SkillFor(db.KindCIFix))}
	return s.enqueue(run)
}

// StandSkill is the project skill that explains how to reach the developer's stand (nil when the project has none).
func (s *Service) StandSkill() *skill.Skill {
	if s.Settings.ProjectRoot == "" {
		return nil
	}
	return skill.NewWithNames(s.Settings.ProjectRoot, nil).StandSkill(s.Settings.StandSkill)
}

// StartStandTest queues an edit run in a worktree of the MR branch that deploys the MR to the developer's stand,
// runs tests there and writes/runs an emulation script («Проверить на стенде»). Needs a stand-access skill in the
// project (STAND_SKILL or detected) or a dedicated stand_test skill.
func (s *Service) StartStandTest(mrID int64, runnerName, notes string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	if s.StandSkill() == nil && s.SkillFor(db.KindStandTest) == nil {
		return 0, userErr("the project has no stand skill: add .claude/skills/<name>/SKILL.md describing how to reach the stand (or set STAND_SKILL / SKILL_STAND_TEST)")
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
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Worktrees.Path(mr.SourceBranch))
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	run := db.Run{Kind: db.KindStandTest, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), Notes: notes, ContinueRunID: runIDPtr(prev),
		Branch: mr.SourceBranch, WorkDir: s.Worktrees.Path(mr.SourceBranch), SkillIdentifier: skillID(s.SkillFor(db.KindStandTest))}
	return s.enqueue(run)
}

// StartFixComments queues an edit run that addresses unresolved reviewer discussions in a worktree of the MR branch.
// continueRunID > 0 continues the agent session of an earlier run in the same worktree (0 = new chat).
func (s *Service) StartFixComments(mrID int64, runnerName, notes string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
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
	prev, err := s.continuation(continueRunID, &mrID, nil, s.Worktrees.Path(mr.SourceBranch))
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	run := db.Run{Kind: db.KindFixComments, MRID: &mr.ID, HeadSHA: mr.HeadSHA, Runner: r.Name(), Notes: notes, ContinueRunID: runIDPtr(prev),
		Branch: mr.SourceBranch, WorkDir: s.Worktrees.Path(mr.SourceBranch), SkillIdentifier: skillID(s.SkillFor(db.KindFixComments))}
	return s.enqueue(run)
}

// continuation resolves the developer's choice to start a run inside an earlier agent session. The earlier run must
// belong to the same MR/issue, be finished, have reported a session id and have worked in the same directory:
// Claude Code keeps sessions per working directory, so a review session (project root) cannot continue in a worktree.
// The new run inherits the runner of that session. continueRunID <= 0 means "new chat" and returns nil.
func (s *Service) continuation(continueRunID int64, mrID, issueID *int64, dir string) (*db.Run, error) {
	if continueRunID <= 0 {
		return nil, nil
	}
	prev, _ := s.DB.GetRun(continueRunID)
	if prev == nil {
		return nil, userErr("session #%d not found", continueRunID)
	}
	if (mrID != nil && (prev.MRID == nil || *prev.MRID != *mrID)) || (issueID != nil && (prev.IssueID == nil || *prev.IssueID != *issueID)) {
		return nil, userErr("session #%d belongs to another merge request or task", continueRunID)
	}
	if prev.Active() {
		return nil, userErr("session #%d is still active: wait for it to finish or start a new chat", continueRunID)
	}
	if prev.SessionID == "" {
		return nil, userErr("session #%d cannot be continued: the %s agent did not report a session id", continueRunID, prev.Runner)
	}
	if prev.WorkDir != "" && prev.WorkDir != dir {
		return nil, userErr("session #%d worked in another directory (%s); an agent session can only be continued from the same workspace", continueRunID, prev.WorkDir)
	}
	return prev, nil
}

// derefID is the value of a nullable run id (0 when nil).
func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// runIDPtr is the id of a run as a nullable reference.
func runIDPtr(run *db.Run) *int64 {
	if run == nil {
		return nil
	}
	id := run.ID
	return &id
}

// Resumable filters the runs of an object down to those whose agent session can be continued from dir: finished,
// with a session id, same working directory (runs older than work_dir tracking count as project-root runs).
func (s *Service) Resumable(runs []db.RunSummary, dir string) []db.RunSummary {
	var out []db.RunSummary
	for _, run := range runs {
		workDir := run.WorkDir
		if workDir == "" {
			workDir = s.Settings.ProjectRoot
		}
		if !run.Active() && run.SessionID != "" && workDir == dir {
			out = append(out, run)
		}
	}
	return out
}

// StartPlan queues a read-only task analysis. continueRunID > 0 continues the agent session of an earlier
// project-root run of the same issue (0 = new chat).
func (s *Service) StartPlan(issueID int64, runnerName, notes string, continueRunID int64, mode string) (int64, error) {
	if mode != "" && mode != "bug" {
		return 0, userErr("unknown plan mode %q (task or bug)", mode)
	}
	if _, err := s.requireRoot(); err != nil {
		return 0, err
	}
	prev, err := s.continuation(continueRunID, nil, &issueID, s.Settings.ProjectRoot)
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
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
	return s.enqueue(db.Run{Kind: db.KindPlan, IssueID: &issue.ID, Runner: r.Name(), Notes: notes, Mode: mode, SkillIdentifier: skillID(s.SkillFor(db.KindPlan)), ContinueRunID: runIDPtr(prev)})
}

// StartImplement queues an edit run in a worktree on `branch` (default: the issue reference). continueRunID > 0
// continues the agent session of an earlier run in that same worktree (0 = new chat).
func (s *Service) StartImplement(issueID int64, runnerName, notes, branch string, continueRunID int64) (int64, error) {
	if _, err := s.requireRoot(); err != nil {
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
	prev, err := s.continuation(continueRunID, nil, &issueID, s.Worktrees.Path(branch))
	if err != nil {
		return 0, err
	}
	if prev != nil {
		runnerName = prev.Runner
	}
	r, err := s.pickRunner(runnerName)
	if err != nil {
		return 0, err
	}
	return s.enqueue(db.Run{Kind: db.KindImplement, IssueID: &issue.ID, Runner: r.Name(), Notes: notes, ContinueRunID: runIDPtr(prev),
		Branch: branch, WorkDir: s.Worktrees.Path(branch), SkillIdentifier: skillID(s.SkillFor(db.KindImplement))})
}

func (s *Service) enqueue(run db.Run) (int64, error) {
	id, err := s.DB.CreateRun(run)
	if err != nil {
		return 0, err
	}
	s.launch(id)
	return id, nil
}

// launch runs a queued run in the background, respecting RUN_CONCURRENCY.
func (s *Service) launch(id int64) {
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
}

// asksQuestions reports whether a run kind may stop with questions for the developer.
func asksQuestions(kind string) bool {
	return kind == db.KindPlan || kind == db.KindImplement || kind == db.KindFixComments || kind == db.KindStandTest || kind == db.KindCIFix
}

// Answer stores the developer's answers to the agent's pending questions and continues the run in the same
// agent session. answers maps question id → answer text; every pending question needs one.
func (s *Service) Answer(runID int64, answers map[int64]string) error {
	run, _ := s.DB.GetRun(runID)
	if run == nil {
		return userErr("run #%d not found", runID)
	}
	pending, _ := s.DB.PendingQuestions(runID)
	if run.Status != db.StatusWaiting || len(pending) == 0 {
		return userErr("run #%d is not waiting for answers", runID)
	}
	for _, q := range pending {
		if strings.TrimSpace(answers[q.ID]) == "" {
			return userErr("question %d has no answer: «%s»", q.Ordinal, q.Question)
		}
	}
	for _, q := range pending {
		if err := s.DB.AnswerQuestion(q.ID, strings.TrimSpace(answers[q.ID])); err != nil {
			return err
		}
	}
	if err := s.DB.UpdateRun(runID, map[string]any{"status": db.StatusQueued, "progress": "", "error": ""}); err != nil {
		return err
	}
	s.launch(runID)
	return nil
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
		return s.StartReview(*run.MRID, run.Kind, run.Runner, derefID(run.ContinueRunID))
	case db.KindFixComments:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartFixComments(*run.MRID, run.Runner, run.Notes, derefID(run.ContinueRunID))
	case db.KindVerifyFinding:
		if run.MRID == nil || run.FindingID == nil {
			return 0, userErr("run has no finding")
		}
		return s.StartVerifyFinding(*run.MRID, *run.FindingID, run.Runner, derefID(run.ContinueRunID))
	case db.KindStandTest:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartStandTest(*run.MRID, run.Runner, run.Notes, derefID(run.ContinueRunID))
	case db.KindCIAnalyze:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartCIAnalyze(*run.MRID, run.Runner, derefID(run.ContinueRunID))
	case db.KindCIFix:
		if run.MRID == nil {
			return 0, userErr("run has no merge request")
		}
		return s.StartCIFix(*run.MRID, run.Runner, derefID(run.BaseRunID), derefID(run.ContinueRunID))
	case db.KindPlan:
		if run.IssueID == nil {
			return 0, userErr("run has no issue")
		}
		return s.StartPlan(*run.IssueID, run.Runner, run.Notes, derefID(run.ContinueRunID), run.Mode)
	case db.KindImplement:
		if run.IssueID == nil {
			return 0, userErr("run has no issue")
		}
		return s.StartImplement(*run.IssueID, run.Runner, run.Notes, run.Branch, derefID(run.ContinueRunID))
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
	} else if run.Status == db.StatusWaiting && cancel == nil {
		_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusCancelled, "error": "Cancelled while waiting for your answers", "finished_at": db.Now(), "progress": ""})
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
		Event: func(ev runner.ToolEvent) {
			_ = s.DB.AddRunEvent(runID, db.PhaseFor(ev.Tool, ev.Detail), ev.Tool, ev.Detail)
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
				// Continue the review's own agent session: it already holds the MR context (Phase C).
				if base.SessionID != "" && base.Runner == run.Runner {
					req.ResumeSessionID = base.SessionID
					req.Agent = ""
					logln(fmt.Sprintf("continuing agent session of run #%d", base.ID))
				}
			}
			req.Prompt, req.Schema = prompts.Verify(pm, sk, baseSHA, toPrev(previous)), prompts.VerifySchema
		}
	case db.KindVerifyFinding:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		finding, _ := s.DB.GetFinding(derefID(run.FindingID))
		if finding == nil {
			s.fail(runID, "finding disappeared")
			return
		}
		req.Prompt, req.Schema = prompts.VerifyFinding(promptMR(mr), sk, prompts.CheckedFinding{ID: finding.ID, Severity: finding.Severity, Category: finding.Category,
			File: finding.File, Line: finding.Line, Title: finding.Title, Description: finding.Description, Suggestion: finding.Suggestion}), prompts.VerifyFindingSchema
		req.SessionName = fmt.Sprintf("verify-finding !%d #%d", mr.IID, runID)
	case db.KindCIAnalyze:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		jobs := s.FailedJobsFor(mr)
		if len(jobs) == 0 {
			s.fail(runID, "no failed jobs found for the head pipeline (GitLab API unavailable or the pipeline is no longer failed)")
			return
		}
		req.Prompt, req.Schema = prompts.CIAnalyze(promptMR(mr), jobs, sk), prompts.CIAnalyzeSchema
		req.SessionName = fmt.Sprintf("ci-analyze !%d #%d", mr.IID, runID)
	case db.KindCIFix:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		jobs := s.FailedJobsFor(mr)
		if len(jobs) == 0 {
			s.fail(runID, "no failed jobs found for the head pipeline (GitLab API unavailable or the pipeline is no longer failed)")
			return
		}
		analysis := ""
		if run.BaseRunID != nil {
			if base, _ := s.DB.GetRun(*run.BaseRunID); base != nil {
				analysis = base.Summary
			}
		}
		_ = s.DB.AddRunEvent(runID, "workspace", "worktree", run.Branch)
		path, err := s.Worktrees.Prepare(ctx, run.Branch, logln)
		if err != nil {
			s.fail(runID, "worktree: "+err.Error())
			return
		}
		req.Dir, req.Mode, req.ProtectDirs = path, runner.ModeEdit, nil
		req.Prompt, req.Schema = prompts.CIFix(promptMR(mr), jobs, analysis, sk), prompts.CIFixSchema
		req.SessionName = fmt.Sprintf("ci-fix !%d #%d", mr.IID, runID)
	case db.KindStandTest:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		_ = s.DB.AddRunEvent(runID, "workspace", "worktree", run.Branch)
		path, err := s.Worktrees.Prepare(ctx, run.Branch, logln)
		if err != nil {
			s.fail(runID, "worktree: "+err.Error())
			return
		}
		stand := s.StandSkill()
		if stand != nil {
			logln("stand access skill: " + stand.Identifier() + " (" + stand.RelPath + ")")
		}
		req.Dir, req.Mode, req.ProtectDirs = path, runner.ModeEdit, nil
		req.Prompt, req.Schema = prompts.StandTest(promptMR(mr), run.Notes, firstOf(mr.TargetBranch, s.Settings.BaseBranch), stand, sk), prompts.StandSchema
		req.SessionName = fmt.Sprintf("stand-test !%d #%d", mr.IID, runID)
	case db.KindFixComments:
		mr, _ := s.DB.GetMR(*run.MRID)
		if mr == nil {
			s.fail(runID, "merge request disappeared")
			return
		}
		_ = s.DB.AddRunEvent(runID, "workspace", "worktree", run.Branch)
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
			if run.Mode == "bug" {
				req.Prompt, req.Schema = prompts.PlanBug(pi, run.Notes, sk), prompts.BugSchema
			} else {
				req.Prompt, req.Schema = prompts.Plan(pi, run.Notes, sk), prompts.PlanSchema
			}
			req.SessionName = fmt.Sprintf("plan #%d run %d", issue.IID, runID)
		} else {
			_ = s.DB.AddRunEvent(runID, "workspace", "worktree", run.Branch)
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

	// Continuation after the developer answered the agent's questions: the same session goes on with the answers.
	questions, _ := s.DB.ListQuestions(runID)
	if len(questions) > 0 && run.SessionID != "" && questions[len(questions)-1].Answered() {
		round := questions[len(questions)-1].Round
		var pairs []prompts.QA
		for _, q := range questions {
			if q.Round == round {
				pairs = append(pairs, prompts.QA{Question: q.Question, Answer: q.Answer})
			}
		}
		req.ResumeSessionID, req.Agent = run.SessionID, ""
		req.Prompt = prompts.Answers(pairs)
		logln(fmt.Sprintf("continuing after %d answer(s) of the developer (round %d)", len(pairs), round))
	} else if run.ContinueRunID != nil {
		if prev, _ := s.DB.GetRun(*run.ContinueRunID); prev != nil && prev.SessionID != "" {
			req.ResumeSessionID, req.Agent = prev.SessionID, ""
			req.Prompt = prompts.Continued(prev.Kind) + req.Prompt
			logln(fmt.Sprintf("continuing agent session of run #%d (chosen by the developer)", prev.ID))
		}
	}

	_ = s.DB.UpdateRun(runID, map[string]any{"status": db.StatusRunning, "started_at": db.Now(), "prompt": req.Prompt, "log_path": logPath, "work_dir": req.Dir})
	result, runErr := r.Run(ctx, req)

	// Counters accumulate over the stages of one run (a run that stopped with questions and continued).
	fields := map[string]any{"finished_at": db.Now(), "progress": ""}
	if result != nil {
		fields["cost_usd"] = run.CostUSD + result.CostUSD
		fields["duration_ms"] = run.DurationMs + result.DurationMs
		fields["input_tokens"] = run.InputTokens + result.Usage.Input
		fields["output_tokens"] = run.OutputTokens + result.Usage.Output
		fields["cache_read_tokens"] = run.CacheReadTokens + result.Usage.CacheRead
		fields["cache_write_tokens"] = run.CacheWriteTokens + result.Usage.CacheWrite
		fields["session_id"] = firstOf(result.SessionID, run.SessionID)
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
	// The agent stopped to ask the developer: keep the run waiting with its session; the answers continue it.
	if asksQuestions(run.Kind) {
		var asked struct {
			Ask []struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
				Why      string   `json:"why"`
			} `json:"ask"`
		}
		_ = json.Unmarshal(result.Structured, &asked)
		if len(asked.Ask) > 0 {
			if fields["session_id"] == "" {
				fields["status"], fields["error"] = db.StatusFailed, fmt.Sprintf("the agent asked %d question(s) but the %s runner reported no session id, so the run cannot be continued", len(asked.Ask), run.Runner)
				_ = s.DB.UpdateRun(runID, fields)
				return
			}
			var qs []db.Question
			for _, q := range asked.Ask {
				qs = append(qs, db.Question{Question: q.Question, Options: q.Options, Why: q.Why})
			}
			if _, err := s.DB.AddQuestions(runID, qs); err != nil {
				fields["status"], fields["error"] = db.StatusFailed, "cannot store questions: "+err.Error()
				_ = s.DB.UpdateRun(runID, fields)
				return
			}
			fields["status"], fields["finished_at"] = db.StatusWaiting, ""
			fields["progress"] = fmt.Sprintf("Нужен ваш ответ: %d вопрос(ов) агента", len(asked.Ask))
			fields["result_json"] = string(result.Structured)
			logln(fmt.Sprintf("agent asked %d question(s); waiting for the developer", len(asked.Ask)))
			_ = s.DB.UpdateRun(runID, fields)
			return
		}
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
	if run.Kind == db.KindVerifyFinding {
		var check struct {
			Status   string `json:"status"`
			Evidence string `json:"evidence"`
		}
		if err := json.Unmarshal(structured, &check); err != nil {
			return err
		}
		fields["summary"], fields["verdict"] = check.Evidence, check.Status
		if run.FindingID == nil {
			return errors.New("run has no finding")
		}
		return s.DB.SetFindingCheck(*run.FindingID, check.Status, check.Evidence, run.ID)
	}
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
	// The verdict shown in the UI always agrees with the findings: an "approve with comments" without a single
	// open finding (or an "approve" next to a HIGH) confuses more than it informs.
	fields["verdict"] = DeriveVerdict(res.Verdict, findings)
	if err := s.DB.ReplaceFindings(run.ID, findings); err != nil {
		return err
	}
	var discussions []db.Discussion
	for _, d := range res.Discussions {
		discussions = append(discussions, db.Discussion{Author: d.Author, File: d.File, Line: d.Line, Body: d.Body, Assessment: d.Assessment, Addressed: d.Addressed})
	}
	return s.DB.ReplaceDiscussions(run.ID, discussions)
}

// DeriveVerdict returns the verdict that matches the open findings: blocked for CRITICAL, request_changes for
// HIGH, approve_with_comments for anything else open, approve when nothing is open. The agent's own verdict is
// kept only when it says the same.
func DeriveVerdict(agentVerdict string, findings []db.Finding) string {
	derived := "approve"
	for _, f := range findings {
		if f.Status != "open" && f.Status != "" {
			continue
		}
		switch strings.ToUpper(f.Severity) {
		case "CRITICAL":
			return "blocked"
		case "HIGH":
			derived = "request_changes"
		default:
			if derived == "approve" {
				derived = "approve_with_comments"
			}
		}
	}
	return derived
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

// SetFindingStatus records the developer's decision about a finding (open / false_positive / ignored / resolved).
func (s *Service) SetFindingStatus(findingID int64, status string) error {
	if f, _ := s.DB.GetFinding(findingID); f == nil {
		return userErr("finding #%d not found", findingID)
	}
	if err := s.DB.SetFindingStatus(findingID, status); err != nil {
		return &UserError{err.Error()}
	}
	return nil
}

// Workspace summarises one worktree for the workspaces page.
type Workspace struct {
	Path        string
	Branch      string
	Head        string
	Run         *db.Run // newest run that used it (nil when unknown)
	Active      bool
	Dirty       bool
	HasUpstream bool
	Unpushed    int
	State       string // active | dirty | clean | pushed
}

// Workspaces lists the dashboard's worktrees with their git state and the run that created them.
func (s *Service) Workspaces() ([]Workspace, error) {
	if s.Worktrees == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), worktree.Timeout)
	defer cancel()
	entries, err := s.Worktrees.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Workspace
	for _, e := range entries {
		w := Workspace{Path: e.Path, Branch: e.Branch, Head: e.Head}
		w.Run, _ = s.DB.LatestRunForWorkDir(e.Path)
		if w.Run != nil && w.Run.Active() {
			w.Active = true
		}
		status, _ := s.Worktrees.Status(ctx, e.Path)
		w.Dirty = strings.TrimSpace(status) != ""
		w.Unpushed, w.HasUpstream = s.Worktrees.Unpushed(ctx, e.Path)
		switch {
		case w.Active:
			w.State = "active"
		case w.Dirty:
			w.State = "dirty"
		case w.HasUpstream && w.Unpushed == 0:
			w.State = "pushed"
		default:
			w.State = "clean"
		}
		out = append(out, w)
	}
	return out, nil
}

// ---------------------------------------------------------------------------------- worktree actions

// WorktreeState summarises a worktree for the run page.
type WorktreeState struct {
	Exists      bool
	Path        string
	Branch      string
	Status      string
	Diff        string
	Log         string
	Unpushed    int  // commits not yet on origin
	HasUpstream bool // the branch exists on origin
}

// Clean reports whether everything in the worktree is committed.
func (w WorktreeState) Clean() bool { return w.Exists && strings.TrimSpace(w.Status) == "" }

// Pushed reports whether the branch is on origin with no local commits ahead.
func (w WorktreeState) Pushed() bool { return w.Exists && w.HasUpstream && w.Unpushed == 0 }

// MRDraft is the prefilled «Довести до MR» form: title and a description built from the agent's report.
type MRDraft struct {
	Title       string
	Description string
}

// MRDraftFor builds the MR title and description of an implementation run from the issue and the agent's report.
func (s *Service) MRDraftFor(run *db.Run, issue *db.Issue) MRDraft {
	draft := MRDraft{}
	if issue != nil {
		draft.Title = fmt.Sprintf("%s %s", issue.Ref(), issue.Title)
	}
	var report struct {
		Summary    string   `json:"summary"`
		Tests      string   `json:"tests"`
		Todo       []string `json:"todo"`
		SelfReview string   `json:"self_review"`
	}
	_ = json.Unmarshal([]byte(run.ResultJSON), &report)
	var b strings.Builder
	if issue != nil {
		fmt.Fprintf(&b, "Closes %s\n\n", issue.WebURL)
	}
	if report.Summary != "" {
		b.WriteString("## Что сделано\n\n" + strings.TrimSpace(report.Summary) + "\n\n")
	}
	if report.Tests != "" {
		b.WriteString("## Проверки\n\n" + strings.TrimSpace(report.Tests) + "\n\n")
	}
	if len(report.Todo) > 0 {
		b.WriteString("## Известные ограничения\n\n")
		for _, t := range report.Todo {
			b.WriteString("- " + strings.TrimSpace(t) + "\n")
		}
		b.WriteString("\n")
	}
	if report.SelfReview != "" {
		b.WriteString("## Self-review\n\n" + strings.TrimSpace(report.SelfReview) + "\n")
	}
	draft.Description = strings.TrimSpace(b.String())
	return draft
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
	state.Unpushed, state.HasUpstream = s.Worktrees.Unpushed(ctx, run.WorkDir)
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

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
