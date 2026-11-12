package db

import (
	"database/sql"
	"encoding/json"
)

// DiscussionNote is one comment of a live MR discussion.
type DiscussionNote struct {
	ID         int64  `json:"id"`
	Author     string `json:"author"`
	AuthorName string `json:"author_name"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
	System     bool   `json:"system,omitempty"`
}

// MRDiscussion is a discussion of an MR as GitLab had it at the last sync: a thread on a diff line, a thread on
// the MR itself, or a plain comment (Resolvable false).
type MRDiscussion struct {
	ID         int64
	MRID       int64
	GitLabID   string
	Author     string // who started it
	File       string // "" = on the MR itself
	OldPath    string
	Line       int64 // line of the new file; 0 = a removed line (OldLine) or no line
	OldLine    int64
	HeadSHA    string // the diff version the comment was written against
	BaseSHA    string
	Resolvable bool
	Resolved   bool
	ResolvedBy string
	Notes      []DiscussionNote
	CreatedAt  string // first note
	UpdatedAt  string // last note
	// FirstSeenAt is when the dashboard first saw the thread.
	FirstSeenAt string
}

// Open reports a thread that waits for an answer.
func (m MRDiscussion) Open() bool { return m.Resolvable && !m.Resolved }

// First is the note that started the discussion.
func (m MRDiscussion) First() DiscussionNote {
	for _, n := range m.Notes {
		if !n.System {
			return n
		}
	}
	return DiscussionNote{}
}

// People are the notes written by people, in order (GitLab's own «changed this line in version 3» left out).
func (m MRDiscussion) People() []DiscussionNote {
	var out []DiscussionNote
	for _, n := range m.Notes {
		if !n.System {
			out = append(out, n)
		}
	}
	return out
}

// Replies are the people's notes after the first one.
func (m MRDiscussion) Replies() []DiscussionNote {
	var out []DiscussionNote
	first := true
	for _, n := range m.Notes {
		if n.System {
			continue
		}
		if first {
			first = false
			continue
		}
		out = append(out, n)
	}
	return out
}

// LastAuthor is who wrote last in the thread.
func (m MRDiscussion) LastAuthor() string {
	for i := len(m.Notes) - 1; i >= 0; i-- {
		if !m.Notes[i].System {
			return m.Notes[i].Author
		}
	}
	return m.Author
}

// FirstNoteID is the GitLab note id of the first comment (the #note_<id> anchor).
func (m MRDiscussion) FirstNoteID() int64 {
	if len(m.Notes) == 0 {
		return 0
	}
	return m.Notes[0].ID
}

const mrDiscussionColumns = "id, mr_id, gitlab_id, author, file, old_path, line, old_line, head_sha, base_sha, resolvable, resolved, resolved_by, notes_json, created_at, updated_at, first_seen_at"

func scanMRDiscussion(row interface{ Scan(...any) error }) (*MRDiscussion, error) {
	var m MRDiscussion
	var resolvable, resolved int
	var notes string
	if err := row.Scan(&m.ID, &m.MRID, &m.GitLabID, &m.Author, &m.File, &m.OldPath, &m.Line, &m.OldLine, &m.HeadSHA, &m.BaseSHA, &resolvable, &resolved,
		&m.ResolvedBy, &notes, &m.CreatedAt, &m.UpdatedAt, &m.FirstSeenAt); err != nil {
		return nil, err
	}
	m.Resolvable, m.Resolved = resolvable == 1, resolved == 1
	_ = json.Unmarshal([]byte(notes), &m.Notes)
	return &m, nil
}

// ReplaceMRDiscussions stores the discussions of an MR as just read from GitLab: known threads are updated in
// place (their ids and first_seen_at stay), threads GitLab no longer has are removed. It returns the threads
// that were not known before, and whether the MR had been read before (false = the first sync, when everything
// is «new» and nothing is news).
func (d *DB) ReplaceMRDiscussions(mrID int64, items []MRDiscussion) (added []MRDiscussion, hadSync bool, err error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var syncedAt string
	_ = tx.QueryRow("SELECT discussions_synced_at FROM merge_requests WHERE id = ?", mrID).Scan(&syncedAt)
	known := map[string]bool{}
	rows, err := tx.Query("SELECT gitlab_id FROM mr_discussions WHERE mr_id = ?", mrID)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, err
		}
		known[id] = true
	}
	rows.Close()
	now := Now()
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	for _, it := range items {
		notes, _ := json.Marshal(it.Notes)
		if it.Notes == nil {
			notes = []byte("[]")
		}
		if _, err := tx.Exec(`INSERT INTO mr_discussions (mr_id, gitlab_id, author, file, old_path, line, old_line, head_sha, base_sha, resolvable, resolved, resolved_by, notes_json, created_at, updated_at, first_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (mr_id, gitlab_id) DO UPDATE SET author = excluded.author, file = excluded.file, old_path = excluded.old_path, line = excluded.line,
				old_line = excluded.old_line, head_sha = excluded.head_sha, base_sha = excluded.base_sha, resolvable = excluded.resolvable, resolved = excluded.resolved,
				resolved_by = excluded.resolved_by, notes_json = excluded.notes_json, created_at = excluded.created_at, updated_at = excluded.updated_at`,
			mrID, it.GitLabID, it.Author, it.File, it.OldPath, it.Line, it.OldLine, it.HeadSHA, it.BaseSHA, flag(it.Resolvable), flag(it.Resolved), it.ResolvedBy,
			string(notes), it.CreatedAt, it.UpdatedAt, now); err != nil {
			return nil, false, err
		}
		if !known[it.GitLabID] {
			added = append(added, it)
		}
		delete(known, it.GitLabID)
	}
	for id := range known {
		if _, err := tx.Exec("DELETE FROM mr_discussions WHERE mr_id = ? AND gitlab_id = ?", mrID, id); err != nil {
			return nil, false, err
		}
	}
	if _, err := tx.Exec("UPDATE merge_requests SET discussions_synced_at = ? WHERE id = ?", now, mrID); err != nil {
		return nil, false, err
	}
	return added, syncedAt != "", tx.Commit()
}

// ListMRDiscussions returns the stored discussions of an MR, oldest first.
func (d *DB) ListMRDiscussions(mrID int64) ([]MRDiscussion, error) {
	rows, err := d.sql.Query("SELECT "+mrDiscussionColumns+" FROM mr_discussions WHERE mr_id = ? ORDER BY created_at, id", mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MRDiscussion
	for rows.Next() {
		m, err := scanMRDiscussion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetMRDiscussion returns a stored live discussion or nil.
func (d *DB) GetMRDiscussion(id int64) (*MRDiscussion, error) {
	m, err := scanMRDiscussion(d.sql.QueryRow("SELECT "+mrDiscussionColumns+" FROM mr_discussions WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return m, err
}

// DiscussionsSyncedAt is when the discussions of the MR were last read from GitLab ("" = never).
func (d *DB) DiscussionsSyncedAt(mrID int64) string {
	var at string
	_ = d.sql.QueryRow("SELECT discussions_synced_at FROM merge_requests WHERE id = ?", mrID).Scan(&at)
	return at
}

// Assessment is what the latest review said about a reviewer thread.
type Assessment struct {
	RunID     int64
	Kind      string
	HeadSHA   string
	Text      string
	Addressed bool
}

// ThreadAssessments returns, per GitLab discussion id, the assessment of the latest finished review of the MR
// that named the thread.
func (d *DB) ThreadAssessments(mrID int64) (map[string]Assessment, error) {
	rows, err := d.sql.Query(`SELECT x.gitlab_id, x.run_id, r.kind, r.head_sha, x.assessment, x.addressed FROM discussions x
		JOIN runs r ON r.id = x.run_id WHERE r.mr_id = ? AND r.status = 'done' AND x.gitlab_id != '' ORDER BY x.run_id, x.id`, mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Assessment{}
	for rows.Next() {
		var id string
		var a Assessment
		var addressed int
		if err := rows.Scan(&id, &a.RunID, &a.Kind, &a.HeadSHA, &a.Text, &addressed); err != nil {
			return nil, err
		}
		a.Addressed = addressed == 1
		out[id] = a // later runs overwrite earlier ones
	}
	return out, rows.Err()
}

// SetMRUnresolved stores the number of open threads counted from a discussions sync. Like UpsertMR it clears the
// «seen» mark when the number changed.
func (d *DB) SetMRUnresolved(id, unresolved int64) error {
	_, err := d.sql.Exec("UPDATE merge_requests SET seen_at = CASE WHEN unresolved != ? THEN '' ELSE seen_at END, unresolved = ? WHERE id = ?", unresolved, unresolved, id)
	return err
}

// discussionsFingerprint changes when a thread of the MR appears, gets a reply or is resolved.
func (d *DB) discussionsFingerprint(mrID int64) string {
	var x string
	_ = d.sql.QueryRow(`SELECT COALESCE(group_concat(y, '|'), '') FROM (SELECT gitlab_id || ':' || resolved || ':' || updated_at || ':' || length(notes_json) AS y
		FROM mr_discussions WHERE mr_id = ? ORDER BY id)`, mrID).Scan(&x)
	return x
}

// MRDetails are the people and the merge readiness of an MR as GitLab reports them.
type MRDetails struct {
	ApprovedBy   []string `json:"approved_by"`
	Reviewers    []string `json:"reviewers"`
	Assignees    []string `json:"assignees"`
	HasConflicts bool     `json:"has_conflicts"`
	// MergeStatus is GitLab's detailed_merge_status: mergeable, not_approved, discussions_not_resolved, conflict,
	// ci_must_pass, ci_still_running, draft_status, need_rebase, checking… ("" on old GitLab versions).
	MergeStatus string `json:"merge_status"`
}

// SetMRDetails stores the details of an MR.
func (d *DB) SetMRDetails(id int64, details MRDetails) error {
	raw, _ := json.Marshal(details)
	_, err := d.sql.Exec("UPDATE merge_requests SET gitlab_details = ? WHERE id = ?", string(raw), id)
	return err
}

// MRDetailsOf returns the stored details of an MR (zero value when never read).
func (d *DB) MRDetailsOf(id int64) MRDetails {
	var raw string
	var out MRDetails
	_ = d.sql.QueryRow("SELECT gitlab_details FROM merge_requests WHERE id = ?", id).Scan(&raw)
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
