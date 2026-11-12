package app

import (
	"mr-review/internal/db"
	"mr-review/internal/gitlab"
)

// Actions on the MR itself (G6): approve / unapprove and a retry of the failed pipeline. Merge is not here on
// purpose. Each is the developer's button; nothing of this is ever started by an agent or a sync.

// ApproveMR approves the MR as the current user, or takes the approval back. sha is the head the page showed
// when the button was pressed: an approval of commits that arrived meanwhile is refused here and by GitLab.
func (s *Service) ApproveMR(mrID int64, approve bool, sha string) (*db.MergeRequest, error) {
	if v := s.forMR(mrID); v != s {
		return v.ApproveMR(mrID, approve, sha)
	}
	mr, _ := s.DB.GetMR(mrID)
	if mr == nil {
		return nil, userErr("merge request #%d is not in the dashboard", mrID)
	}
	if mr.Closed() {
		return nil, userErr("MR уже закрыт или влит")
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	if approve {
		if sha == "" {
			sha = mr.HeadSHA
		}
		if mr.HeadSHA != "" && sha != mr.HeadSHA {
			return nil, userErr("в MR появились новые коммиты (%s → %s): обновите страницу и посмотрите их перед одобрением", shortID(sha), shortID(mr.HeadSHA))
		}
		if err := s.GitLab.Approve(ref, sha); err != nil {
			return nil, &UserError{err.Error()}
		}
	} else if err := s.GitLab.Unapprove(ref); err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.fetchMR(ref, false)
}

// RetryPipeline restarts the failed jobs of the MR's head pipeline.
func (s *Service) RetryPipeline(mrID int64) (*db.MergeRequest, error) {
	if v := s.forMR(mrID); v != s {
		return v.RetryPipeline(mrID)
	}
	mr, _ := s.DB.GetMR(mrID)
	if mr == nil {
		return nil, userErr("merge request #%d is not in the dashboard", mrID)
	}
	if mr.PipelineID == 0 {
		return nil, userErr("у MR нет pipeline: обновите MR из GitLab")
	}
	if mr.PipelineStatus != "failed" && mr.PipelineStatus != "canceled" {
		return nil, userErr("pipeline не упал (%s): перезапускать нечего", firstOf(mr.PipelineStatus, "нет статуса"))
	}
	ref := gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID}
	if err := s.GitLab.RetryPipeline(ref, mr.PipelineID); err != nil {
		return nil, &UserError{err.Error()}
	}
	return s.fetchMR(ref, false)
}

// MergeReadiness is GitLab's detailed_merge_status in words, with a tone for the page.
func MergeReadiness(d db.MRDetails) (text, tone string) {
	if d.HasConflicts {
		return "конфликты с целевой веткой", "danger"
	}
	switch d.MergeStatus {
	case "mergeable":
		return "готов к merge", "success"
	case "not_approved", "approvals_syncing":
		return "ждёт approvals", "warning"
	case "discussions_not_resolved":
		return "есть нерешённые обсуждения", "warning"
	case "conflict":
		return "конфликты с целевой веткой", "danger"
	case "ci_must_pass":
		return "pipeline должен пройти", "warning"
	case "ci_still_running":
		return "pipeline ещё идёт", "neutral"
	case "draft_status":
		return "Draft: merge закрыт", "neutral"
	case "need_rebase":
		return "нужен rebase на целевую ветку", "warning"
	case "blocked_status":
		return "заблокирован другим MR", "warning"
	case "not_open":
		return "MR не открыт", "neutral"
	case "jira_association_missing", "external_status_checks", "requested_changes", "security_policy_violations", "locked_paths", "locked_lfs_files", "title_regex":
		return "merge закрыт правилом проекта (" + d.MergeStatus + ")", "warning"
	case "", "checking", "unchecked", "preparing", "commits_status", "merge_request_blocked":
		return "", ""
	}
	return d.MergeStatus, "neutral"
}
