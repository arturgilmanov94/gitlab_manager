// Package db is the SQLite storage layer (pure Go driver, no cgo).
package db

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Run kinds and statuses.
const (
	KindReviewQuick   = "review_quick"
	KindReviewFull    = "review_full"
	KindReviewVerify  = "review_verify"
	KindFixComments   = "fix_comments"
	KindPlan          = "plan"
	KindImplement     = "implement"
	KindVerifyFinding = "verify_finding" // re-examine one finding of a review (read-only mini run)
	KindStandTest     = "stand_test"     // deploy the MR to the developer's stand, run tests and an emulation script there
	KindCIAnalyze     = "ci_analyze"     // read-only: why the head pipeline failed (job traces + diff)
	KindCIFix         = "ci_fix"         // edit in the MR worktree: fix what makes the pipeline fail
	KindFixFindings   = "fix_findings"   // edit in the MR worktree: address the findings/discussions the developer selected
	KindThreadCheck   = "thread_check"   // read-only: look into one GitLab thread of an MR with the developer's note, suggest a reply
	KindAsk           = "ask"            // a follow-up question in the agent session of a finished run (BaseRunID), read-only

	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusWaiting   = "waiting" // the agent asked for a permission and waits for the developer's answer
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Now returns the current UTC time in RFC3339.
func Now() string { return time.Now().UTC().Truncate(time.Second).Format(time.RFC3339) }

// DB wraps the SQL handle.
type DB struct {
	sql   *sql.DB
	runMu sync.Mutex // CreateRun: the duplicate check and the insert happen as one step
}

// Open opens (creating directories as needed) the SQLite database.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	handle, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(1) // serialise writers; SQLite is single-writer anyway
	return &DB{sql: handle}, nil
}

// Close closes the handle.
func (d *DB) Close() error { return d.sql.Close() }

// Migrate applies the embedded migrations that have not been recorded yet (in file-name order).
func (d *DB) Migrate() ([]string, error) {
	if _, err := d.sql.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return nil, err
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var applied []string
	for _, file := range names {
		name := strings.TrimSuffix(file, ".sql")
		var exists int
		if err := d.sql.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE name = ?", name).Scan(&exists); err != nil {
			return nil, err
		}
		if exists > 0 {
			continue
		}
		script, err := migrationFS.ReadFile("migrations/" + file)
		if err != nil {
			return nil, err
		}
		if _, err := d.sql.Exec(string(script)); err != nil {
			return nil, fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := d.sql.Exec("INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)", name, Now()); err != nil {
			return nil, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

// SchemaVersion lists applied migrations (empty if the DB is not initialised).
func (d *DB) SchemaVersion() []string {
	rows, err := d.sql.Query("SELECT name FROM schema_migrations ORDER BY name")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------- merge requests

// MergeRequest row.
type MergeRequest struct {
	ID              int64
	GitLabHost      string
	ProjectPath     string
	IID             int64
	WebURL          string
	Title           string
	Author          string
	SourceBranch    string
	TargetBranch    string
	State           string
	HeadSHA         string
	Unresolved      int64 // unresolved reviewer discussions
	GitLabUpdatedAt string
	SyncedAt        string
	AddedAt         string
	// GitLab state (Phase 2): head pipeline status, approvals, commits the target is ahead by, draft flag.
	PipelineStatus    string
	ApprovalsGiven    int64
	ApprovalsRequired int64
	Diverged          int64
	Draft             bool
	ChangesCount      string
	// Scope: my roles in the MR ("author,reviewer"), whether I approved it, whether it was added by hand.
	MyRoles      string
	ApprovedByMe bool
	Manual       bool
	Hidden       bool // hidden by the developer ("Убрать из dashboard"): lives in the history until brought back
	// Labels is the comma separated GitLab label list (same form as Issue.Labels).
	Labels string
	// Head pipeline identity (Phase 6): the id for the jobs API and the web link.
	PipelineID  int64
	PipelineURL string
	// NotesCount is GitLab's user_notes_count: comments of people (system notes excluded). Together with head_sha
	// and unresolved it decides whether a sync clears the "seen" mark.
	NotesCount int64
	// SeenAt is when the developer marked the MR as seen ("" = not seen). Cleared by UpsertMR on new activity.
	SeenAt string
	// ReviewFocus is what the developer asked reviews of this MR to pay attention to ("" = nothing special).
	ReviewFocus string
	// ProjectID is the project (repository) of the MR; 0 = a GitLab project without a repository here.
	ProjectID int64
}

// Seen reports whether the developer marked the MR as seen (and nothing new arrived since).
func (m MergeRequest) Seen() bool { return m.SeenAt != "" }

// Ref is "project!iid".
func (m MergeRequest) Ref() string { return fmt.Sprintf("%s!%d", m.ProjectPath, m.IID) }

// Closed reports merged or closed MRs.
func (m MergeRequest) Closed() bool { return m.State == "merged" || m.State == "closed" }

// Relevant reports whether the MR still belongs in the main list: open, not hidden, not approved by me, and
// I am still author / assignee / reviewer (or it was added by hand).
func (m MergeRequest) Relevant() bool {
	return !m.Closed() && !m.Hidden && !m.ApprovedByMe && (m.MyRoles != "" || m.Manual)
}

// CanFix reports whether user may let an agent change the MR branch (fix comments, findings or CI): the author or
// an assignee. A reviewer gives feedback, the branch is not theirs to change.
func (m MergeRequest) CanFix(user string) bool {
	if user == "" {
		return false
	}
	if m.Author == user {
		return true
	}
	for _, role := range strings.Split(m.MyRoles, ",") {
		if role == "assignee" {
			return true
		}
	}
	return false
}

// relevantWhere is the SQL form of Relevant().
const relevantWhere = "state NOT IN ('merged', 'closed') AND hidden = 0 AND approved_by_me = 0 AND (my_roles != '' OR manual = 1)"

const mrColumns = "id, gitlab_host, project_path, iid, web_url, title, author, source_branch, target_branch, state, head_sha, unresolved, gitlab_updated_at, synced_at, added_at, pipeline_status, approvals_given, approvals_required, diverged, draft, changes_count, my_roles, approved_by_me, manual, hidden, labels, pipeline_id, pipeline_url, notes_count, seen_at, review_focus, project_id"

func scanMRInto(m *MergeRequest, s scanner) error {
	var draft, approved, manual, hidden int
	if err := s.Scan(&m.ID, &m.GitLabHost, &m.ProjectPath, &m.IID, &m.WebURL, &m.Title, &m.Author, &m.SourceBranch, &m.TargetBranch, &m.State, &m.HeadSHA, &m.Unresolved, &m.GitLabUpdatedAt, &m.SyncedAt, &m.AddedAt,
		&m.PipelineStatus, &m.ApprovalsGiven, &m.ApprovalsRequired, &m.Diverged, &draft, &m.ChangesCount, &m.MyRoles, &approved, &manual, &hidden, &m.Labels, &m.PipelineID, &m.PipelineURL, &m.NotesCount, &m.SeenAt, &m.ReviewFocus, (*intOrNull)(&m.ProjectID)); err != nil {
		return err
	}
	m.Draft, m.ApprovedByMe, m.Manual, m.Hidden = draft == 1, approved == 1, manual == 1, hidden == 1
	return nil
}

// SetMRHidden hides an MR into the history or brings it back to the main list.
func (d *DB) SetMRHidden(id int64, hidden bool) error {
	v := 0
	if hidden {
		v = 1
	}
	_, err := d.sql.Exec("UPDATE merge_requests SET hidden = ? WHERE id = ?", v, id)
	return err
}

// SetMRSeen marks an MR as seen (dimmed, at the end of the list) or clears the mark.
func (d *DB) SetMRSeen(id int64, seen bool) error {
	if !seen {
		_, err := d.sql.Exec("UPDATE merge_requests SET seen_at = '' WHERE id = ?", id)
		return err
	}
	// seen_sha remembers the head that was looked at; it outlives the mark (see SeenSHA).
	_, err := d.sql.Exec("UPDATE merge_requests SET seen_at = ?, seen_sha = head_sha WHERE id = ?", Now(), id)
	return err
}

// SeenSHA is the head of the MR when the developer last marked it as seen ("" = never).
func (d *DB) SeenSHA(id int64) string {
	var sha string
	_ = d.sql.QueryRow("SELECT seen_sha FROM merge_requests WHERE id = ?", id).Scan(&sha)
	return sha
}

// SetMRReviewFocus stores what the next reviews of an MR should pay attention to.
func (d *DB) SetMRReviewFocus(id int64, focus string) error {
	_, err := d.sql.Exec("UPDATE merge_requests SET review_focus = ? WHERE id = ?", focus, id)
	return err
}

func scanMR(s scanner) (*MergeRequest, error) {
	var m MergeRequest
	if err := scanMRInto(&m, s); err != nil {
		return nil, err
	}
	return &m, nil
}

type scanner interface{ Scan(dest ...any) error }

// UpsertMR inserts or refreshes a merge request.
func (d *DB) UpsertMR(m MergeRequest) (*MergeRequest, error) {
	now := Now()
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	// `manual` is sticky: once added by hand the MR stays until removed by hand. `seen_at` survives a refresh
	// only while nothing new arrived: a new head, a new or resolved discussion, a new comment clear it.
	_, err := d.sql.Exec(`
		INSERT INTO merge_requests (gitlab_host, project_path, iid, web_url, title, author, source_branch, target_branch, state, head_sha, unresolved, gitlab_updated_at, synced_at, added_at,
			pipeline_status, approvals_given, approvals_required, diverged, draft, changes_count, my_roles, approved_by_me, manual, labels, pipeline_id, pipeline_url, notes_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (gitlab_host, project_path, iid) DO UPDATE SET
			web_url = excluded.web_url, title = excluded.title, author = excluded.author,
			source_branch = excluded.source_branch, target_branch = excluded.target_branch,
			state = excluded.state, head_sha = excluded.head_sha, unresolved = excluded.unresolved,
			gitlab_updated_at = excluded.gitlab_updated_at, synced_at = excluded.synced_at,
			pipeline_status = excluded.pipeline_status, approvals_given = excluded.approvals_given, approvals_required = excluded.approvals_required,
			diverged = excluded.diverged, draft = excluded.draft, changes_count = excluded.changes_count,
			my_roles = excluded.my_roles, approved_by_me = excluded.approved_by_me, manual = MAX(merge_requests.manual, excluded.manual),
			labels = excluded.labels, pipeline_id = excluded.pipeline_id, pipeline_url = excluded.pipeline_url,
			notes_count = excluded.notes_count,
			seen_at = CASE WHEN merge_requests.head_sha != excluded.head_sha OR merge_requests.unresolved != excluded.unresolved
			               OR merge_requests.notes_count != excluded.notes_count THEN '' ELSE merge_requests.seen_at END`,
		m.GitLabHost, m.ProjectPath, m.IID, m.WebURL, m.Title, m.Author, m.SourceBranch, m.TargetBranch, m.State, m.HeadSHA, m.Unresolved, m.GitLabUpdatedAt, now, now,
		m.PipelineStatus, m.ApprovalsGiven, m.ApprovalsRequired, m.Diverged, flag(m.Draft), m.ChangesCount, m.MyRoles, flag(m.ApprovedByMe), flag(m.Manual), m.Labels, m.PipelineID, m.PipelineURL, m.NotesCount)
	if err != nil {
		return nil, err
	}
	return scanMR(d.sql.QueryRow("SELECT "+mrColumns+" FROM merge_requests WHERE gitlab_host = ? AND project_path = ? AND iid = ?", m.GitLabHost, m.ProjectPath, m.IID))
}

// FindMR returns the stored merge request of a GitLab reference or nil.
func (d *DB) FindMR(host, projectPath string, iid int64) (*MergeRequest, error) {
	m, err := scanMR(d.sql.QueryRow("SELECT "+mrColumns+" FROM merge_requests WHERE gitlab_host = ? AND project_path = ? AND iid = ?", host, projectPath, iid))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// GetMR returns a merge request or nil.
func (d *DB) GetMR(id int64) (*MergeRequest, error) {
	m, err := scanMR(d.sql.QueryRow("SELECT "+mrColumns+" FROM merge_requests WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// DeleteMR removes a merge request and (by cascade) its runs.
func (d *DB) DeleteMR(id int64) error {
	_, err := d.sql.Exec("DELETE FROM merge_requests WHERE id = ?", id)
	return err
}

// CountRunsForMR returns how many runs an MR has (history worth keeping).
func (d *DB) CountRunsForMR(mrID int64) int64 {
	var n int64
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM runs WHERE mr_id = ?", mrID).Scan(&n)
	return n
}

// LastRun is the summary of the newest run attached to an MR/issue.
type LastRun struct {
	ID           int64
	Kind         string
	Status       string
	Verdict      string
	HeadSHA      string
	Runner       string
	Model        string
	FinishedAt   string
	CreatedAt    string
	OpenFindings int64
	Tokens       int64
	SessionID    string // agent session the run left behind ("" = cannot be continued)
}

// Active reports whether this run is queued, running or waiting for the developer.
func (r *LastRun) Active() bool {
	return r != nil && (r.Status == StatusQueued || r.Status == StatusRunning || r.Status == StatusWaiting)
}

// DoneReview summarises the latest completed review of an MR.
type DoneReview struct {
	ID         int64
	Kind       string
	Verdict    string
	HeadSHA    string
	FinishedAt string
	OpenMajor  int64 // CRITICAL + HIGH
	OpenMinor  int64 // MEDIUM
	OpenInfo   int64 // LOW + INFO
}

// Open is the total of open findings.
func (r DoneReview) Open() int64 { return r.OpenMajor + r.OpenMinor + r.OpenInfo }

// MRListItem is an MR with its latest run and latest completed review.
type MRListItem struct {
	MergeRequest
	Last         *LastRun
	Done         *DoneReview
	Stale        bool
	ContextRunID int64 // newest finished run with an agent session that can be continued (0 = none)
}

// contextRunSQL picks the newest finished run of an object that left a resumable agent session.
const contextRunSQL = "(SELECT id FROM runs c WHERE c.%s = %s.id AND c.session_id != '' AND c.status NOT IN ('queued', 'running', 'waiting') ORDER BY c.id DESC LIMIT 1)"

// ListMRs returns all MRs, newest GitLab activity first, with their latest run.
func (d *DB) ListMRs() ([]MRListItem, error) {
	rows, err := d.sql.Query(`
		SELECT ` + prefixed(mrColumns, "mr.") + `,
		       r.id, r.kind, r.status, r.verdict, r.head_sha, r.runner, r.model, r.finished_at, r.created_at, r.session_id,
		       (SELECT COUNT(*) FROM findings f WHERE f.run_id = r.id AND f.status = 'open'),
		       r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens,
		       d.id, d.kind, d.verdict, d.head_sha, d.finished_at,
		       (SELECT COUNT(*) FROM findings f WHERE f.run_id = d.id AND f.status = 'open' AND f.severity IN ('CRITICAL', 'HIGH')),
		       (SELECT COUNT(*) FROM findings f WHERE f.run_id = d.id AND f.status = 'open' AND f.severity = 'MEDIUM'),
		       (SELECT COUNT(*) FROM findings f WHERE f.run_id = d.id AND f.status = 'open' AND f.severity IN ('LOW', 'INFO')),
		       COALESCE(` + fmt.Sprintf(contextRunSQL, "mr_id", "mr") + `, 0)
		FROM merge_requests mr
		LEFT JOIN runs r ON r.id = (SELECT id FROM runs WHERE mr_id = mr.id AND kind NOT IN ('verify_finding', 'thread_check', 'ask') ORDER BY id DESC LIMIT 1)
		LEFT JOIN runs d ON d.id = (SELECT id FROM runs WHERE mr_id = mr.id AND status = 'done' AND kind IN ('review_full', 'review_verify', 'review_quick') ORDER BY id DESC LIMIT 1)
		ORDER BY CASE WHEN mr.gitlab_updated_at = '' THEN mr.added_at ELSE mr.gitlab_updated_at END DESC, mr.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MRListItem
	for rows.Next() {
		var item MRListItem
		var last LastRun
		var id, open, tokens, dID, dMajor, dMinor, dInfo sql.NullInt64
		var kind, status, verdict, sha, runner, model, finished, created, session, dKind, dVerdict, dSHA, dFinished sql.NullString
		var draft, approved, manual, hidden int
		if err := rows.Scan(&item.ID, &item.GitLabHost, &item.ProjectPath, &item.IID, &item.WebURL, &item.Title, &item.Author, &item.SourceBranch, &item.TargetBranch, &item.State, &item.HeadSHA, &item.Unresolved, &item.GitLabUpdatedAt, &item.SyncedAt, &item.AddedAt,
			&item.PipelineStatus, &item.ApprovalsGiven, &item.ApprovalsRequired, &item.Diverged, &draft, &item.ChangesCount, &item.MyRoles, &approved, &manual, &hidden, &item.Labels, &item.PipelineID, &item.PipelineURL, &item.NotesCount, &item.SeenAt, &item.ReviewFocus, (*intOrNull)(&item.ProjectID),
			&id, &kind, &status, &verdict, &sha, &runner, &model, &finished, &created, &session, &open, &tokens,
			&dID, &dKind, &dVerdict, &dSHA, &dFinished, &dMajor, &dMinor, &dInfo, &item.ContextRunID); err != nil {
			return nil, err
		}
		item.Draft, item.ApprovedByMe, item.Manual, item.Hidden = draft == 1, approved == 1, manual == 1, hidden == 1
		if id.Valid {
			last = LastRun{ID: id.Int64, Kind: kind.String, Status: status.String, Verdict: verdict.String, HeadSHA: sha.String, Runner: runner.String, Model: model.String, FinishedAt: finished.String, CreatedAt: created.String, OpenFindings: open.Int64, Tokens: tokens.Int64, SessionID: session.String}
			item.Last = &last
		}
		if dID.Valid {
			item.Done = &DoneReview{dID.Int64, dKind.String, dVerdict.String, dSHA.String, dFinished.String, dMajor.Int64, dMinor.Int64, dInfo.Int64}
			item.Stale = item.Done.HeadSHA != "" && item.Done.HeadSHA != item.HeadSHA
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- issues

// Issue row.
type Issue struct {
	ID              int64
	GitLabHost      string
	ProjectPath     string
	IID             int64
	WebURL          string
	Title           string
	Description     string
	Author          string
	State           string
	Labels          string
	GitLabUpdatedAt string
	SyncedAt        string
	AddedAt         string
	AgentNotes      string // «Указания агенту», saved on the task as the developer types
	ProjectID       int64  // project (repository) the task is worked in
	Stage           string // stored stage (see IssueStages); "" = not computed yet
	StageManual     bool   // the developer set the stage by hand
}

// LocalHost marks a task created in the dashboard, without a GitLab issue (G2).
const LocalHost = "local"

// Local reports a task created in the dashboard, without a GitLab issue.
func (i Issue) Local() bool { return i.GitLabHost == LocalHost }

// Ref is "project#iid" — also the project's branch naming convention; a local task is "task-<n>".
func (i Issue) Ref() string {
	if i.Local() {
		return fmt.Sprintf("task-%d", i.IID)
	}
	return fmt.Sprintf("%s#%d", i.ProjectPath, i.IID)
}

// Closed reports whether the issue is done in GitLab and so out of the task list. An empty state is an issue
// added by hand that has not been fetched yet — it stays in the list.
// A local task is closed by setting its stage to «Закрыта».
func (i Issue) Closed() bool {
	if i.Local() {
		return i.Stage == "done"
	}
	return i.State != "" && i.State != "opened"
}

const issueColumns = "id, gitlab_host, project_path, iid, web_url, title, description, author, state, labels, gitlab_updated_at, synced_at, added_at, agent_notes, project_id, stage, stage_manual"

func scanIssue(s scanner) (*Issue, error) {
	var i Issue
	err := s.Scan(&i.ID, &i.GitLabHost, &i.ProjectPath, &i.IID, &i.WebURL, &i.Title, &i.Description, &i.Author, &i.State, &i.Labels, &i.GitLabUpdatedAt, &i.SyncedAt, &i.AddedAt, &i.AgentNotes, (*intOrNull)(&i.ProjectID), &i.Stage, &i.StageManual)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// UpsertIssue inserts or refreshes an issue.
func (d *DB) UpsertIssue(i Issue) (*Issue, error) {
	now := Now()
	_, err := d.sql.Exec(`
		INSERT INTO issues (gitlab_host, project_path, iid, web_url, title, description, author, state, labels, gitlab_updated_at, synced_at, added_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (gitlab_host, project_path, iid) DO UPDATE SET
			web_url = excluded.web_url, title = excluded.title, description = excluded.description, author = excluded.author,
			state = excluded.state, labels = excluded.labels, gitlab_updated_at = excluded.gitlab_updated_at, synced_at = excluded.synced_at`,
		i.GitLabHost, i.ProjectPath, i.IID, i.WebURL, i.Title, i.Description, i.Author, i.State, i.Labels, i.GitLabUpdatedAt, now, now)
	if err != nil {
		return nil, err
	}
	return scanIssue(d.sql.QueryRow("SELECT "+issueColumns+" FROM issues WHERE gitlab_host = ? AND project_path = ? AND iid = ?", i.GitLabHost, i.ProjectPath, i.IID))
}

// GetIssue returns an issue or nil.
func (d *DB) GetIssue(id int64) (*Issue, error) {
	i, err := scanIssue(d.sql.QueryRow("SELECT "+issueColumns+" FROM issues WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return i, err
}

// CountRunsForIssue returns how many runs an issue has (history worth keeping).
func (d *DB) CountRunsForIssue(issueID int64) int64 {
	var n int64
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM runs WHERE issue_id = ?", issueID).Scan(&n)
	return n
}

// CreateLocalIssue stores a task without a GitLab issue, numbered after the other local tasks.
func (d *DB) CreateLocalIssue(title, description string, projectID *int64) (*Issue, error) {
	now := Now()
	res, err := d.sql.Exec(`INSERT INTO issues (gitlab_host, project_path, iid, web_url, title, description, author, state, labels, gitlab_updated_at, synced_at, added_at, project_id, stage)
		VALUES (?, 'local', (SELECT COALESCE(MAX(iid), 0) + 1 FROM issues WHERE gitlab_host = ?), '', ?, ?, '', 'opened', '', ?, ?, ?, ?, 'new')`,
		LocalHost, LocalHost, title, description, now, now, now, nullInt(projectID))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return d.GetIssue(id)
}

// UpdateLocalIssue changes the title and description of a local task.
func (d *DB) UpdateLocalIssue(id int64, title, description string) error {
	_, err := d.sql.Exec("UPDATE issues SET title = ?, description = ?, gitlab_updated_at = ? WHERE id = ? AND gitlab_host = ?", title, description, Now(), id, LocalHost)
	return err
}

// DeleteIssue removes an issue and its runs.
func (d *DB) DeleteIssue(id int64) error {
	_, err := d.sql.Exec("DELETE FROM issues WHERE id = ?", id)
	return err
}

// IssueListItem is an issue with its latest run.
type IssueListItem struct {
	Issue
	Last         *LastRun
	ContextRunID int64 // newest finished run with an agent session that can be continued (0 = none)
}

// ListIssues returns all issues with their latest run.
func (d *DB) ListIssues() ([]IssueListItem, error) {
	rows, err := d.sql.Query(`
		SELECT ` + prefixed(issueColumns, "i.") + `,
		       r.id, r.kind, r.status, r.verdict, r.head_sha, r.runner, r.model, r.finished_at, r.created_at, r.session_id,
		       COALESCE(` + fmt.Sprintf(contextRunSQL, "issue_id", "i") + `, 0)
		FROM issues i
		LEFT JOIN runs r ON r.id = (SELECT id FROM runs WHERE issue_id = i.id AND kind != 'ask' ORDER BY id DESC LIMIT 1)
		ORDER BY CASE WHEN i.gitlab_updated_at = '' THEN i.added_at ELSE i.gitlab_updated_at END DESC, i.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueListItem
	for rows.Next() {
		var item IssueListItem
		var id sql.NullInt64
		var kind, status, verdict, sha, runner, model, finished, created, session sql.NullString
		if err := rows.Scan(&item.ID, &item.GitLabHost, &item.ProjectPath, &item.IID, &item.WebURL, &item.Title, &item.Description, &item.Author, &item.State, &item.Labels, &item.GitLabUpdatedAt, &item.SyncedAt, &item.AddedAt, &item.AgentNotes, (*intOrNull)(&item.ProjectID), &item.Stage, &item.StageManual,
			&id, &kind, &status, &verdict, &sha, &runner, &model, &finished, &created, &session, &item.ContextRunID); err != nil {
			return nil, err
		}
		if id.Valid {
			item.Last = &LastRun{ID: id.Int64, Kind: kind.String, Status: status.String, Verdict: verdict.String, HeadSHA: sha.String, Runner: runner.String, Model: model.String, FinishedAt: finished.String, CreatedAt: created.String, SessionID: session.String}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- runs

// Run row.
type Run struct {
	ID               int64
	Kind             string
	MRID             *int64
	IssueID          *int64
	BaseRunID        *int64
	ContinueRunID    *int64 // run whose agent session this one continues (developer's choice); nil = new chat
	FindingID        *int64 // verify_finding runs: the finding being re-examined
	Mode             string // plan runs: "" = task plan, "bug" = bug analysis (expected/actual, root cause, fix)
	SelectionJSON    string // fix_findings runs: {"findings":[ids],"discussions":[ids]} chosen by the developer
	HeadSHA          string
	Status           string
	Runner           string
	Model            string // models the agent actually used, filled after the run
	RequestedModel   string // model picked by the developer at start ("" = the agent's default)
	ProjectID        int64  // project (repository) the run worked in
	SkillIdentifier  string
	Notes            string
	Prompt           string
	Summary          string
	Verdict          string
	ResultJSON       string
	RawResult        string
	Error            string
	LogPath          string
	SessionID        string
	WorkDir          string
	Branch           string
	CostUSD          float64
	DurationMs       int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CreatedAt        string
	StartedAt        string
	FinishedAt       string
	Progress         string // last tool call of the agent (live)
	DenialsJSON      string // tool calls the agent was refused, as reported by the runner
	PlanPath         string // markdown file the plan was exported to
	// Context window fill of the agent session: tokens in the context at the last turn of the main agent and the
	// window size (0 = unknown). Only runs started from 0.21.0 report them.
	ContextTokens int64
	ContextWindow int64
}

// TotalTokens is the sum of all token kinds consumed by the run.
func (r Run) TotalTokens() int64 {
	return r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
}

// ContextPct is how full the agent's context window is, in whole percents (0 when unknown).
func (r Run) ContextPct() int64 {
	if r.ContextWindow <= 0 || r.ContextTokens <= 0 {
		return 0
	}
	return r.ContextTokens * 100 / r.ContextWindow
}

// Active reports whether the run is queued, running or waiting for the developer.
func (r Run) Active() bool {
	return r.Status == StatusQueued || r.Status == StatusRunning || r.Status == StatusWaiting
}

// activeStatuses is the SQL list of statuses Active() covers.
const activeStatuses = "('queued', 'running', 'waiting')"

// IsReview reports whether the run is a review kind (produces findings).
// IsSideCheck reports whether the run is a small read-only check of one item of an MR (a finding, a thread):
// several of them may run together, next to each other but not next to other runs of the MR.
func (r Run) IsSideCheck() bool { return r.Kind == KindVerifyFinding || r.Kind == KindThreadCheck }

func (r Run) IsReview() bool {
	return r.Kind == KindReviewFull || r.Kind == KindReviewVerify || r.Kind == KindReviewQuick
}

// AgentSelector is the runner selector that reproduces this run's agent and model: "claude", "claude:opus".
func (r Run) AgentSelector() string {
	if r.RequestedModel == "" || r.RequestedModel == "default" {
		return r.Runner
	}
	return r.Runner + ":" + r.RequestedModel
}

// IsEdit reports whether the run edits files in a worktree.
func (r Run) IsEdit() bool {
	return r.Kind == KindImplement || r.Kind == KindFixComments || r.Kind == KindStandTest || r.Kind == KindCIFix || r.Kind == KindFixFindings
}

const runColumns = "id, kind, mr_id, issue_id, base_run_id, head_sha, status, runner, model, skill_identifier, notes, prompt, summary, verdict, result_json, raw_result, error, log_path, session_id, work_dir, branch, cost_usd, duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at, started_at, finished_at, progress, denials_json, plan_path, continue_run_id, finding_id, mode, selection_json, context_tokens, context_window, requested_model, project_id"

func scanRun(s scanner) (*Run, error) {
	var r Run
	var mrID, issueID, baseID, contID, findingID sql.NullInt64
	err := s.Scan(&r.ID, &r.Kind, &mrID, &issueID, &baseID, &r.HeadSHA, &r.Status, &r.Runner, &r.Model, &r.SkillIdentifier, &r.Notes, &r.Prompt, &r.Summary, &r.Verdict, &r.ResultJSON, &r.RawResult, &r.Error, &r.LogPath, &r.SessionID, &r.WorkDir, &r.Branch, &r.CostUSD, &r.DurationMs, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.Progress, &r.DenialsJSON, &r.PlanPath, &contID, &findingID, &r.Mode, &r.SelectionJSON, &r.ContextTokens, &r.ContextWindow, &r.RequestedModel, (*intOrNull)(&r.ProjectID))
	if err != nil {
		return nil, err
	}
	if contID.Valid {
		r.ContinueRunID = &contID.Int64
	}
	if findingID.Valid {
		r.FindingID = &findingID.Int64
	}
	if mrID.Valid {
		r.MRID = &mrID.Int64
	}
	if issueID.Valid {
		r.IssueID = &issueID.Int64
	}
	if baseID.Valid {
		r.BaseRunID = &baseID.Int64
	}
	return &r, nil
}

// ActiveRunError refuses a run because another one is queued or running on the same object or workspace.
type ActiveRunError struct{ ID int64 }

func (e *ActiveRunError) Error() string {
	return fmt.Sprintf("a run for this object is already queued or running (#%d)", e.ID)
}

// CreateRun inserts a queued run and returns its id. It refuses (ActiveRunError) when the same MR or task already
// has an active run (several «Проверить замечание» of one MR may run together), or when an edit run would share
// the workspace of an active one: the check and the insert are one step, so two clicks cannot both pass.
func (d *DB) CreateRun(r Run) (int64, error) {
	d.runMu.Lock()
	defer d.runMu.Unlock()
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var active int64
	switch {
	case r.MRID != nil:
		err = tx.QueryRow("SELECT id FROM runs WHERE mr_id = ? AND status IN "+activeStatuses+" AND NOT (kind IN ('verify_finding', 'thread_check') AND ? IN ('verify_finding', 'thread_check')) ORDER BY id LIMIT 1",
			*r.MRID, r.Kind).Scan(&active)
	case r.IssueID != nil:
		err = tx.QueryRow("SELECT id FROM runs WHERE issue_id = ? AND status IN "+activeStatuses+" ORDER BY id LIMIT 1", *r.IssueID).Scan(&active)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err == nil && active == 0 && r.WorkDir != "" && EditKinds[r.Kind] {
		err = tx.QueryRow("SELECT id FROM runs WHERE work_dir = ? AND status IN "+activeStatuses+" AND kind IN "+editKindsSQL+" ORDER BY id LIMIT 1", r.WorkDir).Scan(&active)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if active != 0 {
		return 0, &ActiveRunError{active}
	}
	res, err := tx.Exec(`
		INSERT INTO runs (kind, mr_id, issue_id, base_run_id, continue_run_id, finding_id, head_sha, status, runner, model, requested_model, skill_identifier, notes, work_dir, branch, mode, selection_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, nullInt(r.MRID), nullInt(r.IssueID), nullInt(r.BaseRunID), nullInt(r.ContinueRunID), nullInt(r.FindingID), r.HeadSHA, r.Runner, r.Model, r.RequestedModel, r.SkillIdentifier, r.Notes, r.WorkDir, r.Branch, r.Mode, r.SelectionJSON, Now())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// EditKinds are the run kinds that change files in a worktree: two of them must never share one.
var EditKinds = map[string]bool{KindImplement: true, KindFixComments: true, KindCIFix: true, KindFixFindings: true, KindStandTest: true}

// editKindsSQL is EditKinds for SQL (keep in sync with migration 022).
const editKindsSQL = "('implement', 'fix_comments', 'ci_fix', 'fix_findings', 'stand_test')"

// UpdateRun sets the given columns.
func (d *DB) UpdateRun(id int64, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	var sets []string
	var args []any
	for key, value := range fields {
		sets = append(sets, key+" = ?")
		args = append(args, value)
	}
	args = append(args, id)
	_, err := d.sql.Exec("UPDATE runs SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

// KnownContextWindow returns the context window size the given model reported in an earlier run (0 = none):
// the live percent of a running session uses it until the session's own result arrives.
func (d *DB) KnownContextWindow(model string) int64 {
	if model == "" {
		return 0
	}
	var w int64
	_ = d.sql.QueryRow("SELECT context_window FROM runs WHERE context_window > 0 AND (model = ? OR model LIKE ? OR model LIKE ?) ORDER BY id DESC LIMIT 1",
		model, model+", %", "%, "+model).Scan(&w)
	return w
}

// GetRun returns a run or nil.
func (d *DB) GetRun(id int64) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// RunSummary is a run with finding counters.
type RunSummary struct {
	Run
	OpenFindings  int64
	TotalFindings int64
}

func (d *DB) listRuns(where string, arg any) ([]RunSummary, error) {
	rows, err := d.sql.Query(`SELECT `+prefixed(runColumns, "r.")+`,
		(SELECT COUNT(*) FROM findings f WHERE f.run_id = r.id AND f.status = 'open'),
		(SELECT COUNT(*) FROM findings f WHERE f.run_id = r.id)
		FROM runs r WHERE `+where+` ORDER BY r.id DESC`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var s RunSummary
		var mrID, issueID, baseID, contID, findingID sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Kind, &mrID, &issueID, &baseID, &s.HeadSHA, &s.Status, &s.Runner, &s.Model, &s.SkillIdentifier, &s.Notes, &s.Prompt, &s.Summary, &s.Verdict, &s.ResultJSON, &s.RawResult, &s.Error, &s.LogPath, &s.SessionID, &s.WorkDir, &s.Branch, &s.CostUSD, &s.DurationMs, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens, &s.CreatedAt, &s.StartedAt, &s.FinishedAt, &s.Progress, &s.DenialsJSON, &s.PlanPath, &contID, &findingID, &s.Mode, &s.SelectionJSON, &s.ContextTokens, &s.ContextWindow, &s.RequestedModel, (*intOrNull)(&s.ProjectID), &s.OpenFindings, &s.TotalFindings); err != nil {
			return nil, err
		}
		if contID.Valid {
			s.ContinueRunID = &contID.Int64
		}
		if findingID.Valid {
			s.FindingID = &findingID.Int64
		}
		if mrID.Valid {
			s.MRID = &mrID.Int64
		}
		if issueID.Valid {
			s.IssueID = &issueID.Int64
		}
		if baseID.Valid {
			s.BaseRunID = &baseID.Int64
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListRunsForMR returns the MR's runs, newest first.
func (d *DB) ListRunsForMR(mrID int64) ([]RunSummary, error) { return d.listRuns("r.mr_id = ?", mrID) }

// ListRunsForIssue returns the issue's runs, newest first.
func (d *DB) ListRunsForIssue(issueID int64) ([]RunSummary, error) {
	return d.listRuns("r.issue_id = ?", issueID)
}

// ListRunsBySession returns every run that shares an agent session (the conversation chain), oldest first.
func (d *DB) ListRunsBySession(sessionID string) ([]RunSummary, error) {
	if sessionID == "" {
		return nil, nil
	}
	runs, err := d.listRuns("r.session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs, nil
}

// LatestRunForWorkDir returns the newest run that used a worktree directory, or nil.
func (d *DB) LatestRunForWorkDir(dir string) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE work_dir = ? ORDER BY id DESC LIMIT 1", dir))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ActiveRunForWorkDir returns the queued/running/waiting run working in dir, or nil.
func (d *DB) ActiveRunForWorkDir(dir string) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE work_dir = ? AND status IN "+activeStatuses+" ORDER BY id DESC LIMIT 1", dir))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// LastActivityForWorkDir is when a run last touched dir: the newest finish, start or creation time, and the
// newest follow-up message of those runs ("" when no run used the directory).
func (d *DB) LastActivityForWorkDir(dir string) string {
	var at string
	_ = d.sql.QueryRow(`SELECT COALESCE(MAX(t), '') FROM (
		SELECT MAX(MAX(finished_at, started_at, created_at)) AS t FROM runs WHERE work_dir = ?
		UNION ALL
		SELECT MAX(m.created_at) FROM messages m JOIN runs r ON r.id = m.run_id WHERE r.work_dir = ?)`, dir, dir).Scan(&at)
	return at
}

// RunListItem is a run with the object it belongs to (for the sessions page and the overview).
type RunListItem struct {
	RunSummary
	MRIID         int64
	MRTitle       string
	MRWebURL      string
	IssueIID      int64
	IssueTitle    string
	IssueWebURL   string
	ObjectProject string
}

// ListRuns returns the newest runs of every kind with their MR/issue, active ones first.
func (d *DB) ListRuns(limit int) ([]RunListItem, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.sql.Query(`SELECT `+prefixed(runColumns, "r.")+`,
		(SELECT COUNT(*) FROM findings f WHERE f.run_id = r.id AND f.status = 'open'),
		(SELECT COUNT(*) FROM findings f WHERE f.run_id = r.id),
		COALESCE(mr.iid, 0), COALESCE(mr.title, ''), COALESCE(mr.web_url, ''), COALESCE(mr.project_path, ''),
		COALESCE(i.iid, 0), COALESCE(i.title, ''), COALESCE(i.web_url, ''), COALESCE(i.project_path, '')
		FROM runs r
		LEFT JOIN merge_requests mr ON mr.id = r.mr_id
		LEFT JOIN issues i ON i.id = r.issue_id
		ORDER BY CASE WHEN r.status IN `+activeStatuses+` THEN 0 ELSE 1 END, r.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunListItem
	for rows.Next() {
		var s RunListItem
		var mrID, issueID, baseID, contID, findingID sql.NullInt64
		var mrProject, issueProject string
		if err := rows.Scan(&s.ID, &s.Kind, &mrID, &issueID, &baseID, &s.HeadSHA, &s.Status, &s.Runner, &s.Model, &s.SkillIdentifier, &s.Notes, &s.Prompt, &s.Summary, &s.Verdict, &s.ResultJSON, &s.RawResult, &s.Error, &s.LogPath, &s.SessionID, &s.WorkDir, &s.Branch, &s.CostUSD, &s.DurationMs, &s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.CacheWriteTokens, &s.CreatedAt, &s.StartedAt, &s.FinishedAt, &s.Progress, &s.DenialsJSON, &s.PlanPath, &contID, &findingID, &s.Mode, &s.SelectionJSON, &s.ContextTokens, &s.ContextWindow, &s.RequestedModel, (*intOrNull)(&s.ProjectID), &s.OpenFindings, &s.TotalFindings,
			&s.MRIID, &s.MRTitle, &s.MRWebURL, &mrProject, &s.IssueIID, &s.IssueTitle, &s.IssueWebURL, &issueProject); err != nil {
			return nil, err
		}
		if mrID.Valid {
			s.MRID = &mrID.Int64
			s.ObjectProject = mrProject
		}
		if issueID.Valid {
			s.IssueID = &issueID.Int64
			s.ObjectProject = issueProject
		}
		if baseID.Valid {
			s.BaseRunID = &baseID.Int64
		}
		if contID.Valid {
			s.ContinueRunID = &contID.Int64
		}
		if findingID.Valid {
			s.FindingID = &findingID.Int64
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestDoneReview returns the newest completed review run for an MR.
func (d *DB) LatestDoneReview(mrID int64) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE mr_id = ? AND status = 'done' AND kind IN ('review_full', 'review_verify', 'review_quick') ORDER BY id DESC LIMIT 1", mrID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ActiveRunForMR returns the queued/running/waiting run of an MR, if any.
func (d *DB) ActiveRunForMR(mrID int64) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE mr_id = ? AND status IN "+activeStatuses+" ORDER BY id DESC LIMIT 1", mrID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ActiveRunForIssue returns the queued/running/waiting run of an issue, if any.
func (d *DB) ActiveRunForIssue(issueID int64) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE issue_id = ? AND status IN "+activeStatuses+" ORDER BY id DESC LIMIT 1", issueID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// RunsWithoutTokens lists finished runs that have a raw result but no token counters yet.
func (d *DB) RunsWithoutTokens() ([]int64, error) {
	rows, err := d.sql.Query("SELECT id FROM runs WHERE raw_result != '' AND input_tokens + output_tokens + cache_read_tokens + cache_write_tokens = 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// FailStaleRuns marks active runs as failed and their open permission prompts as expired (after a restart).
func (d *DB) FailStaleRuns(message string) (int64, error) {
	_, _ = d.sql.Exec("UPDATE approvals SET status = 'expired', decided_at = ? WHERE status = 'pending'", Now())
	// Runs waiting for the developer's answers to the agent's questions need no process: they survive a restart.
	// Queued runs never started: they stay in the queue and are launched again (QueuedRunIDs).
	res, err := d.sql.Exec("UPDATE runs SET status = 'failed', error = ?, finished_at = ? WHERE status IN ('running', 'waiting')"+
		" AND NOT (status = 'waiting' AND EXISTS (SELECT 1 FROM run_questions q WHERE q.run_id = runs.id AND q.answer = ''))", message, Now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// QueuedRunIDs lists runs still waiting in the queue, oldest first (re-launched after a restart).
func (d *DB) QueuedRunIDs() ([]int64, error) {
	rows, err := d.sql.Query("SELECT id FROM runs WHERE status = 'queued' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// QueueCounts reports how many runs are queued and how many hold an agent slot.
func (d *DB) QueueCounts() (queued, running int64) {
	// A run waiting for answers to its questions has no process and holds no agent slot; one waiting for a
	// permission prompt does.
	_ = d.sql.QueryRow("SELECT COALESCE(SUM(status = 'queued'), 0), COALESCE(SUM(status = 'running' OR (status = 'waiting' AND NOT EXISTS "+
		"(SELECT 1 FROM run_questions q WHERE q.run_id = runs.id AND q.answer = ''))), 0) FROM runs WHERE status IN "+activeStatuses).Scan(&queued, &running)
	return queued, running
}

// QueuePosition is how many queued runs were created before this one (0 = next in line).
func (d *DB) QueuePosition(runID int64) int64 {
	var n int64
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM runs WHERE status = 'queued' AND id < ?", runID).Scan(&n)
	return n
}

// SetFindingGitLabNote stores the link to the GitLab comment a finding was posted as.
func (d *DB) SetFindingGitLabNote(id int64, url string) error {
	_, err := d.sql.Exec("UPDATE findings SET gitlab_note_url = ? WHERE id = ?", url, id)
	return err
}

// DataFingerprint changes whenever what a page shows changes: MRs and issues as synced from GitLab and the state of
// the runs. Open pages compare it to the one they were rendered with and refresh themselves. Sync timestamps are left
// out on purpose. mrID > 0 narrows it to one MR and its runs; 0 covers every MR, issue and run.
func (d *DB) DataFingerprint(mrID int64) string {
	const mrRow = `id || ':' || head_sha || ':' || state || ':' || unresolved || ':' || gitlab_updated_at || ':' || pipeline_status || ':' ||
		approvals_given || ':' || my_roles || ':' || approved_by_me || ':' || hidden || ':' || notes_count || ':' || labels || ':' || title || ':' ||
		draft || ':' || diverged || ':' || seen_at || ':' || gitlab_details`
	const runRow = `id || ':' || status || ':' || (SELECT COUNT(*) FROM findings f WHERE f.run_id = runs.id AND f.status = 'open')`
	var mrs, issues, runs string
	if mrID > 0 {
		_ = d.sql.QueryRow("SELECT COALESCE("+mrRow+", '') FROM merge_requests WHERE id = ?", mrID).Scan(&mrs)
		_ = d.sql.QueryRow("SELECT COALESCE(group_concat(x, '|'), '') FROM (SELECT "+runRow+" AS x FROM runs WHERE mr_id = ? ORDER BY id)", mrID).Scan(&runs)
		issues = d.discussionsFingerprint(mrID) // the slot is free for one MR: its live threads
	} else {
		_ = d.sql.QueryRow("SELECT COALESCE(group_concat(x, '|'), '') FROM (SELECT " + mrRow + " AS x FROM merge_requests ORDER BY id)").Scan(&mrs)
		_ = d.sql.QueryRow(`SELECT COALESCE(group_concat(x, '|'), '') FROM (SELECT id || ':' || state || ':' || gitlab_updated_at || ':' || labels || ':' || title AS x
			FROM issues ORDER BY id)`).Scan(&issues)
		// Only the latest runs matter for the lists; the full history would make this grow without bound.
		_ = d.sql.QueryRow("SELECT COALESCE(group_concat(x, '|'), '') FROM (SELECT " + runRow + " AS x FROM runs WHERE id > (SELECT COALESCE(MAX(id), 0) - 200 FROM runs) OR status IN " + activeStatuses + " ORDER BY id)").Scan(&runs)
	}
	h := fnv.New64a()
	for _, part := range []string{mrs, issues, runs} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// ---------------------------------------------------------------------------------- approvals

// Approval statuses.
const (
	ApprovalPending = "pending"
	ApprovalAllowed = "allowed"
	ApprovalDenied  = "denied"
	ApprovalExpired = "expired"
)

// Approval is one permission prompt of an agent run.
type Approval struct {
	ID              int64
	RunID           int64
	ToolName        string
	Description     string
	InputJSON       string
	SuggestionsJSON string
	Reason          string
	Status          string
	Remember        bool
	Note            string
	CreatedAt       string
	DecidedAt       string
}

// Pending reports whether the developer still has to answer.
func (a Approval) Pending() bool { return a.Status == ApprovalPending }

const approvalColumns = "id, run_id, tool_name, description, input_json, suggestions_json, reason, status, remember, note, created_at, decided_at"

func scanApproval(s scanner) (*Approval, error) {
	var a Approval
	var remember int
	if err := s.Scan(&a.ID, &a.RunID, &a.ToolName, &a.Description, &a.InputJSON, &a.SuggestionsJSON, &a.Reason, &a.Status, &remember, &a.Note, &a.CreatedAt, &a.DecidedAt); err != nil {
		return nil, err
	}
	a.Remember = remember == 1
	return &a, nil
}

// CreateApproval records a pending permission prompt and returns its id.
func (d *DB) CreateApproval(a Approval) (int64, error) {
	res, err := d.sql.Exec(`INSERT INTO approvals (run_id, tool_name, description, input_json, suggestions_json, reason, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`, a.RunID, a.ToolName, a.Description, a.InputJSON, a.SuggestionsJSON, a.Reason, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DecideApproval stores the developer's answer.
func (d *DB) DecideApproval(id int64, status string, remember bool, note string) error {
	rem := 0
	if remember {
		rem = 1
	}
	_, err := d.sql.Exec("UPDATE approvals SET status = ?, remember = ?, note = ?, decided_at = ? WHERE id = ?", status, rem, note, Now(), id)
	return err
}

// GetApproval returns an approval or nil.
func (d *DB) GetApproval(id int64) (*Approval, error) {
	a, err := scanApproval(d.sql.QueryRow("SELECT "+approvalColumns+" FROM approvals WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// PendingApproval returns the open prompt of a run, if any.
func (d *DB) PendingApproval(runID int64) (*Approval, error) {
	a, err := scanApproval(d.sql.QueryRow("SELECT "+approvalColumns+" FROM approvals WHERE run_id = ? AND status = 'pending' ORDER BY id LIMIT 1", runID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// ListApprovals returns every prompt of a run in order.
func (d *DB) ListApprovals(runID int64) ([]Approval, error) {
	rows, err := d.sql.Query("SELECT "+approvalColumns+" FROM approvals WHERE run_id = ? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// PendingApprovals lists all open prompts across runs (for the header badge), oldest first.
func (d *DB) PendingApprovals() ([]Approval, error) {
	rows, err := d.sql.Query("SELECT " + approvalColumns + " FROM approvals WHERE status = 'pending' ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- findings

// Finding row.
type Finding struct {
	ID              int64
	RunID           int64
	Ordinal         int64
	OriginFindingID *int64
	Status          string
	Severity        string
	Category        string
	File            string
	Line            *int64
	Title           string
	Description     string
	Suggestion      string
	Comment         string // the ready GitLab text in the developer's style ("" = built from description and suggestion)
	VerifyNote      string
	CheckStatus     string // outcome of «Проверить замечание»: confirmed | false_positive | obsolete | unclear ("" = not checked)
	CheckRunID      *int64 // the verify_finding run that produced CheckStatus
	GitLabNoteURL   string // the GitLab comment this finding was posted as ("" = not posted)
}

// ReplaceFindings replaces a run's findings.
func (d *DB) ReplaceFindings(runID int64, findings []Finding) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM findings WHERE run_id = ?", runID); err != nil {
		return err
	}
	for i, f := range findings {
		status := f.Status
		if status == "" {
			status = "open"
		}
		severity := strings.ToUpper(f.Severity)
		if severity == "" {
			severity = "MEDIUM"
		}
		title := f.Title
		if title == "" {
			title = "(untitled)"
		}
		// check_status, check_run_id and gitlab_note_url travel with a finding carried over by verify: a finding
		// posted to GitLab or checked by an agent stays so on the newer head.
		if _, err := tx.Exec(`INSERT INTO findings (run_id, ordinal, origin_finding_id, status, severity, category, file, line, title, description, suggestion, gitlab_comment, verify_note,
			check_status, check_run_id, gitlab_note_url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, i+1, nullInt(f.OriginFindingID), status, severity, f.Category, f.File, nullInt(f.Line), title, f.Description, f.Suggestion, f.Comment, f.VerifyNote,
			f.CheckStatus, nullInt(f.CheckRunID), f.GitLabNoteURL); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Manual finding statuses set by the developer (besides open / fixed / obsolete set by runs).
var ManualFindingStatuses = map[string]bool{"open": true, "false_positive": true, "ignored": true, "resolved": true}

// SetFindingStatus stores the developer's decision about a finding.
func (d *DB) SetFindingStatus(id int64, status string) error {
	if !ManualFindingStatuses[status] {
		return fmt.Errorf("unknown finding status %q", status)
	}
	res, err := d.sql.Exec("UPDATE findings SET status = ? WHERE id = ?", status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finding #%d not found", id)
	}
	return nil
}

// ActiveFindingCheck returns the queued/running verify_finding run of a finding, or nil.
func (d *DB) ActiveFindingCheck(findingID int64) (*Run, error) {
	run, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE finding_id = ? AND kind = 'verify_finding' AND status IN "+activeStatuses+" ORDER BY id DESC LIMIT 1", findingID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

// CheckStatuses are the outcomes of «Проверить замечание».
var CheckStatuses = map[string]bool{"confirmed": true, "false_positive": true, "obsolete": true, "unclear": true, "fixed": true, "not_confirmed": true}

// SetFindingCheck records the outcome of a verify_finding run: the check status, the evidence note, the run,
// and the finding status it implies (false_positive / obsolete close the finding; confirmed / unclear keep it open).
func (d *DB) SetFindingCheck(id int64, checkStatus, note string, runID int64) error {
	if !CheckStatuses[checkStatus] {
		return fmt.Errorf("unknown check status %q", checkStatus)
	}
	// "fixed" (by a fix_findings run) keeps the finding open: the fix is a claim until a verify confirms it.
	status := "open"
	if checkStatus == "false_positive" || checkStatus == "obsolete" {
		status = checkStatus
	}
	res, err := d.sql.Exec("UPDATE findings SET status = ?, check_status = ?, verify_note = ?, check_run_id = ? WHERE id = ?", status, checkStatus, note, runID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finding #%d not found", id)
	}
	return nil
}

// GetFinding returns a finding or nil.
func (d *DB) GetFinding(id int64) (*Finding, error) {
	rows, err := d.sql.Query("SELECT id, run_id, ordinal, origin_finding_id, status, severity, category, file, line, title, description, suggestion, gitlab_comment, verify_note, check_status, check_run_id, gitlab_note_url FROM findings WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var f Finding
	var origin, line, checkRun sql.NullInt64
	if err := rows.Scan(&f.ID, &f.RunID, &f.Ordinal, &origin, &f.Status, &f.Severity, &f.Category, &f.File, &line, &f.Title, &f.Description, &f.Suggestion, &f.Comment, &f.VerifyNote, &f.CheckStatus, &checkRun, &f.GitLabNoteURL); err != nil {
		return nil, err
	}
	if origin.Valid {
		f.OriginFindingID = &origin.Int64
	}
	if line.Valid {
		f.Line = &line.Int64
	}
	if checkRun.Valid {
		f.CheckRunID = &checkRun.Int64
	}
	return &f, nil
}

// ListFindings returns a run's findings in order.
func (d *DB) ListFindings(runID int64) ([]Finding, error) {
	rows, err := d.sql.Query("SELECT id, run_id, ordinal, origin_finding_id, status, severity, category, file, line, title, description, suggestion, gitlab_comment, verify_note, check_status, check_run_id, gitlab_note_url FROM findings WHERE run_id = ? ORDER BY ordinal", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		var origin, line, checkRun sql.NullInt64
		if err := rows.Scan(&f.ID, &f.RunID, &f.Ordinal, &origin, &f.Status, &f.Severity, &f.Category, &f.File, &line, &f.Title, &f.Description, &f.Suggestion, &f.Comment, &f.VerifyNote, &f.CheckStatus, &checkRun, &f.GitLabNoteURL); err != nil {
			return nil, err
		}
		if origin.Valid {
			f.OriginFindingID = &origin.Int64
		}
		if line.Valid {
			f.Line = &line.Int64
		}
		if checkRun.Valid {
			f.CheckRunID = &checkRun.Int64
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Discussion row.
type Discussion struct {
	ID         int64
	RunID      int64
	Author     string
	File       string
	Line       *int64
	Body       string
	Assessment string
	Addressed  bool
	// GitLabID is the GitLab discussion the assessment is about ("" = unknown: a run before 0.32).
	GitLabID string
}

// ReplaceDiscussions replaces a run's unresolved discussions.
func (d *DB) ReplaceDiscussions(runID int64, items []Discussion) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM discussions WHERE run_id = ?", runID); err != nil {
		return err
	}
	for _, it := range items {
		addressed := 0
		if it.Addressed {
			addressed = 1
		}
		if _, err := tx.Exec("INSERT INTO discussions (run_id, author, file, line, body, assessment, addressed, gitlab_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			runID, it.Author, it.File, nullInt(it.Line), it.Body, it.Assessment, addressed, it.GitLabID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDiscussions returns a run's discussions.
func (d *DB) ListDiscussions(runID int64) ([]Discussion, error) {
	rows, err := d.sql.Query("SELECT id, run_id, author, file, line, body, assessment, addressed, gitlab_id FROM discussions WHERE run_id = ? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Discussion
	for rows.Next() {
		var it Discussion
		var line sql.NullInt64
		var addressed int
		if err := rows.Scan(&it.ID, &it.RunID, &it.Author, &it.File, &line, &it.Body, &it.Assessment, &addressed, &it.GitLabID); err != nil {
			return nil, err
		}
		if line.Valid {
			it.Line = &line.Int64
		}
		it.Addressed = addressed == 1
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- messages

// Message is one follow-up chat entry.
type Message struct {
	ID        int64
	RunID     int64
	Role      string
	Content   string
	CostUSD   float64
	Tokens    int64
	CreatedAt string
}

// AddMessage appends a chat message.
func (d *DB) AddMessage(runID int64, role, content string, cost float64, tokens int64) (int64, error) {
	res, err := d.sql.Exec("INSERT INTO messages (run_id, role, content, cost_usd, tokens, created_at) VALUES (?, ?, ?, ?, ?, ?)", runID, role, content, cost, tokens, Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListMessages returns a run's chat.
func (d *DB) ListMessages(runID int64) ([]Message, error) {
	rows, err := d.sql.Query("SELECT id, run_id, role, content, cost_usd, tokens, created_at FROM messages WHERE run_id = ? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.RunID, &m.Role, &m.Content, &m.CostUSD, &m.Tokens, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Counts returns simple totals for the header.
func (d *DB) Counts() map[string]int64 {
	out := map[string]int64{}
	for _, table := range []string{"merge_requests", "issues", "runs", "findings"} {
		var n int64
		_ = d.sql.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		out[table] = n
	}
	// The header counts what the main MR list shows; the rest is the history.
	var relevant int64
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM merge_requests WHERE " + relevantWhere).Scan(&relevant)
	out["history"] = out["merge_requests"] - relevant
	out["merge_requests"] = relevant
	var ci int64
	_ = d.sql.QueryRow("SELECT COUNT(*) FROM merge_requests WHERE " + relevantWhere + " AND pipeline_status = 'failed'").Scan(&ci)
	out["ci"] = ci
	var cost float64
	_ = d.sql.QueryRow("SELECT COALESCE(SUM(cost_usd), 0) FROM runs").Scan(&cost)
	out["cost_cents"] = int64(cost * 100)
	var tokens int64
	_ = d.sql.QueryRow("SELECT COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0) + (SELECT COALESCE(SUM(tokens), 0) FROM messages) FROM runs").Scan(&tokens)
	out["tokens"] = tokens
	return out
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func prefixed(columns, prefix string) string {
	parts := strings.Split(columns, ", ")
	for i := range parts {
		parts[i] = prefix + parts[i]
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------------- settings (UI overrides)

// SetSetting stores a UI setting; an empty value deletes the key.
func (d *DB) SetSetting(key, value string) error {
	if value == "" {
		_, err := d.sql.Exec("DELETE FROM settings WHERE key = ?", key)
		return err
	}
	_, err := d.sql.Exec("INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at", key, value, Now())
	return err
}

// SettingsWithPrefix returns every setting whose key starts with prefix.
func (d *DB) SettingsWithPrefix(prefix string) (map[string]string, error) {
	rows, err := d.sql.Query("SELECT key, value FROM settings WHERE key LIKE ? ESCAPE '\\'", strings.ReplaceAll(strings.ReplaceAll(prefix, "_", "\\_"), "%", "\\%")+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- agent questions

// Question is one question the agent asked the developer during a run.
type Question struct {
	ID         int64
	RunID      int64
	Round      int64
	Ordinal    int64
	Question   string
	Options    []string
	Why        string
	Answer     string
	CreatedAt  string
	AnsweredAt string
}

// Answered reports whether the developer replied.
func (q Question) Answered() bool { return q.AnsweredAt != "" }

const questionColumns = "id, run_id, round, ordinal, question, options_json, why, answer, created_at, answered_at"

func scanQuestion(s scanner) (*Question, error) {
	var q Question
	var options string
	if err := s.Scan(&q.ID, &q.RunID, &q.Round, &q.Ordinal, &q.Question, &options, &q.Why, &q.Answer, &q.CreatedAt, &q.AnsweredAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(options), &q.Options)
	return &q, nil
}

// AddQuestions stores a new round of questions for a run and returns the round number.
func (d *DB) AddQuestions(runID int64, questions []Question) (int64, error) {
	var round int64
	_ = d.sql.QueryRow("SELECT COALESCE(MAX(round), 0) FROM run_questions WHERE run_id = ?", runID).Scan(&round)
	round++
	now := Now()
	for i, q := range questions {
		options, _ := json.Marshal(q.Options)
		if q.Options == nil {
			options = []byte("[]")
		}
		if _, err := d.sql.Exec("INSERT INTO run_questions (run_id, round, ordinal, question, options_json, why, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			runID, round, i+1, q.Question, string(options), q.Why, now); err != nil {
			return 0, err
		}
	}
	return round, nil
}

// ListQuestions returns every question of a run, oldest first.
func (d *DB) ListQuestions(runID int64) ([]Question, error) {
	rows, err := d.sql.Query("SELECT "+questionColumns+" FROM run_questions WHERE run_id = ? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

// PendingQuestions returns the unanswered questions of a run.
func (d *DB) PendingQuestions(runID int64) ([]Question, error) {
	all, err := d.ListQuestions(runID)
	if err != nil {
		return nil, err
	}
	var out []Question
	for _, q := range all {
		if !q.Answered() {
			out = append(out, q)
		}
	}
	return out, nil
}

// AnswerQuestion stores the developer's answer.
func (d *DB) AnswerQuestion(id int64, answer string) error {
	res, err := d.sql.Exec("UPDATE run_questions SET answer = ?, answered_at = ? WHERE id = ? AND answered_at = ''", answer, Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("question #%d not found or already answered", id)
	}
	return nil
}

// RunsWaitingForAnswers lists ids of runs that wait for the developer's answers to agent questions.
func (d *DB) RunsWaitingForAnswers() ([]int64, error) {
	rows, err := d.sql.Query("SELECT DISTINCT r.id FROM runs r JOIN run_questions q ON q.run_id = r.id AND q.answer = '' WHERE r.status = 'waiting' ORDER BY r.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------- run events (timeline)

// Phases of a run in display order.
var Phases = []string{"workspace", "analysis", "plan", "implement", "tests", "commands", "subagent", "question"}

// PhaseLabel is the human name of a phase.
func PhaseLabel(phase string) string {
	switch phase {
	case "workspace":
		return "Подготовка workspace"
	case "analysis":
		return "Анализ"
	case "plan":
		return "План"
	case "implement":
		return "Реализация"
	case "tests":
		return "Тесты и проверки"
	case "commands":
		return "Команды"
	case "subagent":
		return "Субагенты"
	case "question":
		return "Вопросы"
	}
	return phase
}

var testCommand = regexp.MustCompile(`(?i)\b(phpunit|pytest|go test|npm test|yarn test|pnpm test|composer test|vendor/bin/|phpstan|psalm|php-cs-fixer|phpcs|eslint|prettier|golangci-lint|go vet|gofmt|rubocop|jest|vitest|make test|make lint|lint)\b`)
var readCommand = regexp.MustCompile(`(?i)^(glab api|glab mr|glab issue|git (log|diff|show|status|blame|branch|rev-parse|ls-files)|cat |head |tail |ls |find |grep |rg |wc |tree )`)

// PhaseFor classifies a tool call into a phase.
func PhaseFor(tool, detail string) string {
	switch tool {
	case "Read", "Grep", "Glob", "LS", "WebFetch", "WebSearch", "NotebookRead":
		return "analysis"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "implement"
	case "TodoWrite", "ExitPlanMode", "EnterPlanMode":
		return "plan"
	case "Agent", "Task":
		return "subagent"
	case "AskUserQuestion":
		return "question"
	case "Bash":
		switch {
		case testCommand.MatchString(detail):
			return "tests"
		case readCommand.MatchString(detail):
			return "analysis"
		default:
			return "commands"
		}
	}
	return "commands"
}

// RunEvent is one tool call of a run.
type RunEvent struct {
	ID     int64
	RunID  int64
	At     string
	Phase  string
	Tool   string
	Detail string
}

// AddRunEvent appends a tool call to the run's timeline.
func (d *DB) AddRunEvent(runID int64, phase, tool, detail string) error {
	_, err := d.sql.Exec("INSERT INTO run_events (run_id, at, phase, tool, detail) VALUES (?, ?, ?, ?, ?)", runID, Now(), phase, tool, detail)
	return err
}

// ListRunEvents returns the tool calls of a run in order.
func (d *DB) ListRunEvents(runID int64) ([]RunEvent, error) {
	rows, err := d.sql.Query("SELECT id, run_id, at, phase, tool, detail FROM run_events WHERE run_id = ? ORDER BY id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunEvent
	for rows.Next() {
		var e RunEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.At, &e.Phase, &e.Tool, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PhaseSummary is one step of the timeline: how many tool calls, when, what was touched last.
type PhaseSummary struct {
	Phase      string
	Label      string
	Count      int
	FirstAt    string
	LastAt     string
	LastDetail string
	Current    bool // the phase of the latest tool call
}

// Timeline groups a run's events into phases in order of first appearance; the last event marks the current phase.
func Timeline(events []RunEvent) []PhaseSummary {
	var out []PhaseSummary
	index := map[string]int{}
	for _, e := range events {
		i, ok := index[e.Phase]
		if !ok {
			index[e.Phase] = len(out)
			out = append(out, PhaseSummary{Phase: e.Phase, Label: PhaseLabel(e.Phase), FirstAt: e.At})
			i = len(out) - 1
		}
		out[i].Count++
		out[i].LastAt = e.At
		out[i].LastDetail = e.Detail
	}
	for i := range out {
		out[i].Current = false
	}
	if len(events) > 0 {
		out[index[events[len(events)-1].Phase]].Current = true
	}
	return out
}

// TimelineFor loads and groups a run's events.
func (d *DB) TimelineFor(runID int64) []PhaseSummary {
	events, _ := d.ListRunEvents(runID)
	return Timeline(events)
}

// RunWarnings returns the warnings recorded for a run (tool "warning"), oldest first.
func (d *DB) RunWarnings(runID int64) []string {
	rows, err := d.sql.Query("SELECT detail FROM run_events WHERE run_id = ? AND tool = 'warning' ORDER BY id", runID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var detail string
		if rows.Scan(&detail) == nil {
			out = append(out, detail)
		}
	}
	return out
}

// CurrentPhase is the label of the latest phase of a run ("" without events).
func (d *DB) CurrentPhase(runID int64) string {
	var phase, detail string
	if err := d.sql.QueryRow("SELECT phase, detail FROM run_events WHERE run_id = ? ORDER BY id DESC LIMIT 1", runID).Scan(&phase, &detail); err != nil {
		return ""
	}
	return PhaseLabel(phase)
}

var bugWords = regexp.MustCompile(`(?i)(^|[^a-zа-яё])(bug|баг|fix|hotfix|defect|ошибк|исправ|не работает|падает|broken|crash)`)

// LooksLikeBug guesses from labels and title whether an issue is a bug report (the «Разбор бага» mode is preselected).
func (i Issue) LooksLikeBug() bool { return bugWords.MatchString(i.Labels + " " + i.Title) }

// GetDiscussion returns a stored reviewer discussion or nil.
func (d *DB) GetDiscussion(id int64) (*Discussion, error) {
	var x Discussion
	var line sql.NullInt64
	var addressed int
	err := d.sql.QueryRow("SELECT id, run_id, author, file, line, body, assessment, addressed, gitlab_id FROM discussions WHERE id = ?", id).Scan(&x.ID, &x.RunID, &x.Author, &x.File, &line, &x.Body, &x.Assessment, &addressed, &x.GitLabID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if line.Valid {
		x.Line = &line.Int64
	}
	x.Addressed = addressed == 1
	return &x, nil
}

// Selection is what a fix_findings run was asked to address.
type Selection struct {
	Findings    []int64 `json:"findings"`
	Discussions []int64 `json:"discussions"`       // per-run discussion rows (runs before 0.32)
	Threads     []int64 `json:"threads,omitempty"` // live threads (mr_discussions)
}

// SelectionOf decodes a run's selection ("" → empty).
func (r Run) SelectionOf() Selection {
	var sel Selection
	_ = json.Unmarshal([]byte(r.SelectionJSON), &sel)
	return sel
}

// RejectedFindings returns the findings of an MR the developer rejected (false_positive, ignored), newest first,
// one per file+title: verify runs carry a rejected finding forward, so the same one appears in several runs.
func (d *DB) RejectedFindings(mrID int64) ([]Finding, error) {
	rows, err := d.sql.Query(`SELECT f.id, f.run_id, f.status, f.severity, f.file, f.line, f.title FROM findings f JOIN runs r ON r.id = f.run_id
		WHERE r.mr_id = ? AND f.status IN ('false_positive', 'ignored') ORDER BY f.id DESC`, mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []Finding
	for rows.Next() {
		var f Finding
		var line sql.NullInt64
		if err := rows.Scan(&f.ID, &f.RunID, &f.Status, &f.Severity, &f.File, &line, &f.Title); err != nil {
			return nil, err
		}
		if line.Valid {
			f.Line = &line.Int64
		}
		key := f.File + "\x00" + f.Title
		if !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
	}
	return out, rows.Err()
}

// SetIssueAgentNotes stores the «Указания агенту» of a task.
func (d *DB) SetIssueAgentNotes(id int64, notes string) error {
	res, err := d.sql.Exec("UPDATE issues SET agent_notes = ? WHERE id = ?", notes, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("issue #%d not found", id)
	}
	return nil
}

// ActiveAsk returns the queued/running follow-up question about a run, or nil.
func (d *DB) ActiveAsk(baseRunID int64) (*Run, error) {
	r, err := scanRun(d.sql.QueryRow("SELECT "+runColumns+" FROM runs WHERE kind = 'ask' AND base_run_id = ? AND status IN "+activeStatuses+" ORDER BY id DESC LIMIT 1", baseRunID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ThreadChecks returns the latest «Исследовать тред» run of every thread of an MR, by mr_discussions id.
func (d *DB) ThreadChecks(mrID int64) (map[int64]*Run, error) {
	rows, err := d.sql.Query("SELECT "+runColumns+" FROM runs WHERE mr_id = ? AND kind = 'thread_check' ORDER BY id", mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		for _, id := range r.SelectionOf().Threads {
			out[id] = r
		}
	}
	return out, rows.Err()
}
