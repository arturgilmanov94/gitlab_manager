package app

import (
	"strconv"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
)

// Task stage and links (T20). The stage is stored on the task so that the list, the page and the inbox agree; it is
// computed from the task's runs and its linked MRs unless the developer set it by hand.

// refreshIssueLinks stores the MRs GitLab links to the task (closes / related), the dashboard's MRs on the task's
// branch, and the task's comments. A GitLab error keeps what was stored.
func (s *Service) refreshIssueLinks(issue *db.Issue, ref gitlab.Ref) {
	linked, notes, err := s.GitLab.IssueLinks(ref)
	oldMRs, oldNotes := s.DB.IssueLinks(issue.ID)
	if err != nil && linked == nil {
		linked = nil
		for _, m := range oldMRs {
			if m.Via != "branch" {
				linked = append(linked, gitlab.LinkedMR{ProjectPath: m.ProjectPath, IID: m.IID, Title: m.Title, State: m.State, WebURL: m.WebURL, SourceBranch: m.SourceBranch, Closes: m.Via == "closes"})
			}
		}
	}
	var stored []db.IssueNote
	if err != nil && notes == nil {
		stored = oldNotes
	}
	for _, n := range notes {
		stored = append(stored, db.IssueNote{Author: n.Author, Body: n.Body, CreatedAt: n.CreatedAt})
	}
	var mrs []db.LinkedMR
	seen := map[string]bool{}
	for _, m := range linked {
		via := "related"
		if m.Closes {
			via = "closes"
		}
		row := db.LinkedMR{Host: ref.Host, ProjectPath: m.ProjectPath, IID: m.IID, Title: m.Title, State: m.State, WebURL: m.WebURL, SourceBranch: m.SourceBranch, Via: via}
		if known, _ := s.DB.FindMR(ref.Host, m.ProjectPath, m.IID); known != nil {
			row.MRID = known.ID
		}
		seen[m.ProjectPath+"!"+itoa64(m.IID)] = true
		mrs = append(mrs, row)
	}
	// MRs of the dashboard opened from the task's branch, even when nobody linked them in GitLab.
	branch := s.IssueBranch(issue)
	if all, err := s.DB.ListMRs(); err == nil {
		for _, m := range all {
			if m.SourceBranch != branch || seen[m.ProjectPath+"!"+itoa64(m.IID)] {
				continue
			}
			mrs = append(mrs, db.LinkedMR{Host: m.GitLabHost, ProjectPath: m.ProjectPath, IID: m.IID, Title: m.Title, State: m.State, WebURL: m.WebURL, SourceBranch: m.SourceBranch, Via: "branch", MRID: m.ID})
		}
	}
	_ = s.DB.SetIssueLinks(issue.ID, mrs, stored)
}

// UpdateIssueStage recomputes and stores the stage of a task (a stage set by hand stays).
func (s *Service) UpdateIssueStage(issueID int64) {
	issue, _ := s.DB.GetIssue(issueID)
	if issue == nil {
		return
	}
	runs, _ := s.DB.ListRunsForIssue(issueID)
	mrs, _ := s.DB.IssueLinks(issueID)
	_ = s.DB.SetIssueStage(issueID, ComputeStage(issue, runs, mrs))
}

// ComputeStage derives the stage of a task: the task closed → done; a linked MR merged → merged, open → review;
// otherwise from its runs: one working → research / implementing; a finished implementation → implemented;
// a finished plan → planned; else new.
func ComputeStage(issue *db.Issue, runs []db.RunSummary, mrs []db.LinkedMR) string {
	if issue.Closed() {
		return "done"
	}
	merged, open := false, false
	for _, m := range mrs {
		switch m.State {
		case "merged":
			merged = true
		case "opened":
			open = true
		}
	}
	if open {
		return "review"
	}
	if merged {
		return "merged"
	}
	var plan, impl bool
	for _, r := range runs {
		if r.Active() {
			if r.Kind == db.KindPlan {
				return "research"
			}
			if r.Kind == db.KindImplement {
				return "implementing"
			}
		}
		if r.Status == db.StatusDone {
			plan = plan || r.Kind == db.KindPlan
			impl = impl || r.Kind == db.KindImplement
		}
	}
	switch {
	case impl:
		return "implemented"
	case plan:
		return "planned"
	}
	return "new"
}

// SetIssueStage sets the stage of a task by hand; "auto" gives it back to the computation.
func (s *Service) SetIssueStage(issueID int64, stage string) error {
	if issue, _ := s.DB.GetIssue(issueID); issue == nil {
		return userErr("задача #%d не найдена", issueID)
	}
	if stage == "auto" {
		if err := s.DB.SetIssueStageManual(issueID, "", false); err != nil {
			return err
		}
		s.UpdateIssueStage(issueID)
		return nil
	}
	if !db.ValidStage(stage) {
		return userErr("неизвестная стадия %q", stage)
	}
	return s.DB.SetIssueStageManual(issueID, stage, true)
}

// stageAfterRun refreshes the stage of the run's task when a run stops.
func (s *Service) stageAfterRun(runID int64) {
	if run, _ := s.DB.GetRun(runID); run != nil && run.IssueID != nil {
		s.UpdateIssueStage(*run.IssueID)
	}
}

// IssuesOfMR returns the tasks an MR belongs to (linked in GitLab or opened from the task's branch).
func (s *Service) IssuesOfMR(mr *db.MergeRequest) []db.Issue {
	list, _ := s.DB.IssuesOfMR(mr.GitLabHost, mr.ProjectPath, mr.IID)
	return list
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
