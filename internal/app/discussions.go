package app

import (
	"fmt"
	"strings"
	"time"

	"mr-review/internal/db"
	"mr-review/internal/gitlab"
	"mr-review/internal/prompts"
)

// Live discussions (C-19). The threads of an MR are kept in mr_discussions as GitLab has them — with every reply,
// resolved or not — instead of being a snapshot inside a review run. The «Обсуждения» tab, the counters and the
// review prompt read the same rows; the AI assessment of a thread is attached to it by the GitLab discussion id.

// discussionsStale decides whether a background sync must read the discussions of an MR again: it was never read,
// something visible changed (comment count, head, GitLab's update time), or the stored state is older than
// DISCUSSIONS_MAX_AGE_SEC. Resolving a thread changes none of the visible signs, hence the age limit.
func (s *Service) discussionsStale(previous, fresh *db.MergeRequest) bool {
	if previous == nil {
		return true
	}
	if previous.NotesCount != fresh.NotesCount || previous.HeadSHA != fresh.HeadSHA || previous.GitLabUpdatedAt != fresh.GitLabUpdatedAt {
		return true
	}
	at := s.DB.DiscussionsSyncedAt(previous.ID)
	if at == "" {
		return true
	}
	synced, err := time.Parse(time.RFC3339, at)
	return err != nil || time.Since(synced) >= time.Duration(s.Settings.DiscussionsMaxAgeSec)*time.Second
}

// storeDiscussions writes what GitLab returned into mr_discussions and tells about threads that are news.
func (s *Service) storeDiscussions(mr *db.MergeRequest, list []gitlab.Discussion) {
	if mr == nil {
		return
	}
	rows := make([]db.MRDiscussion, 0, len(list))
	for _, d := range list {
		row := db.MRDiscussion{GitLabID: d.ID, Author: d.Author(), File: d.File, OldPath: d.OldPath, Line: d.Line, OldLine: d.OldLine,
			HeadSHA: d.HeadSHA, BaseSHA: d.BaseSHA, Resolvable: d.Resolvable, Resolved: d.Resolved, ResolvedBy: d.ResolvedBy}
		for _, n := range d.Notes {
			row.Notes = append(row.Notes, db.DiscussionNote{ID: n.ID, Author: n.Author, AuthorName: n.AuthorName, Body: n.Body, CreatedAt: n.CreatedAt, System: n.System})
		}
		if len(d.Notes) > 0 {
			row.CreatedAt, row.UpdatedAt = d.Notes[0].CreatedAt, d.Notes[len(d.Notes)-1].CreatedAt
		}
		rows = append(rows, row)
	}
	added, hadSync, err := s.DB.ReplaceMRDiscussions(mr.ID, rows)
	if err != nil {
		fmt.Printf("discussions of !%d: %v\n", mr.IID, err)
		return
	}
	if hadSync {
		s.notifyThreads(mr, added)
	}
}

// SyncDiscussions reads the discussions of an MR from GitLab now (the «Обсуждения» tab, a run about to start).
// On a GitLab error the stored threads stay and the error is returned for the page to show.
func (s *Service) SyncDiscussions(mrID int64) error {
	if v := s.forMR(mrID); v != s {
		return v.SyncDiscussions(mrID)
	}
	mr, _ := s.DB.GetMR(mrID)
	if mr == nil {
		return userErr("merge request #%d is not in the dashboard", mrID)
	}
	list, err := s.GitLab.Discussions(gitlab.Ref{Host: mr.GitLabHost, ProjectPath: mr.ProjectPath, IID: mr.IID})
	if err != nil {
		return &UserError{"не удалось прочитать обсуждения из GitLab: " + err.Error()}
	}
	s.storeDiscussions(mr, list)
	_ = s.DB.SetMRUnresolved(mr.ID, gitlab.Unresolved(list))
	return nil
}

// promptThreads are the unresolved threads of an MR for a review prompt, read from GitLab right before the run
// (the stored ones when GitLab does not answer).
func (s *Service) promptThreads(mr *db.MergeRequest) []prompts.ThreadNote {
	_ = s.SyncDiscussions(mr.ID)
	list, _ := s.DB.ListMRDiscussions(mr.ID)
	var out []prompts.ThreadNote
	for _, d := range list {
		if !d.Open() {
			continue
		}
		note := prompts.ThreadNote{ID: d.GitLabID, Author: d.Author, File: d.File, Line: firstPositive(d.Line, d.OldLine), Body: d.First().Body}
		for _, r := range d.Replies() {
			note.Replies = append(note.Replies, prompts.ThreadReply{Author: r.Author, Body: r.Body})
		}
		out = append(out, note)
	}
	return out
}

// matchThread finds the GitLab discussion an assessment of a review is about: the id the agent returned when it
// names a known thread, else the only open thread of that author at that place.
func matchThread(live []db.MRDiscussion, id, author, file string, line *int64) string {
	id = strings.TrimSpace(id)
	for _, d := range live {
		if id != "" && (d.GitLabID == id || strings.HasPrefix(d.GitLabID, id) && len(id) >= 8) {
			return d.GitLabID
		}
	}
	author = strings.TrimPrefix(strings.TrimSpace(author), "@")
	var found string
	for _, d := range live {
		if !d.Resolvable || d.Author != author || d.File != file {
			continue
		}
		if line != nil && *line > 0 && d.Line != *line && d.OldLine != *line {
			continue
		}
		if found != "" {
			return "" // two candidates: better no link than a wrong one
		}
		found = d.GitLabID
	}
	return found
}

// ThreadView is a live discussion with what the latest review said about it.
type ThreadView struct {
	db.MRDiscussion
	Assessment *db.Assessment
	URL        string // the thread in GitLab
	Mine       bool   // started by the current user
	WaitsForMe bool   // open, and the last word is someone else's
}

// Threads is what the «Обсуждения» tab shows.
type Threads struct {
	Open, Resolved, Comments []ThreadView
	SyncedAt                 string
}

// Waiting counts the open threads where the last word is not the current user's.
func (t Threads) Waiting() int {
	n := 0
	for _, v := range t.Open {
		if v.WaitsForMe {
			n++
		}
	}
	return n
}

// Total counts everything on the tab.
func (t Threads) Total() int { return len(t.Open) + len(t.Resolved) + len(t.Comments) }

// ThreadsOf returns the stored discussions of an MR grouped for the page: open threads first (the ones waiting
// for the current user on top), then resolved ones, then plain comments.
func (s *Service) ThreadsOf(mr *db.MergeRequest) Threads {
	list, _ := s.DB.ListMRDiscussions(mr.ID)
	assessments, _ := s.DB.ThreadAssessments(mr.ID)
	// Reviews made before threads carried their GitLab id: the latest one is matched by author and place.
	if latest, _ := s.DB.LatestDoneReview(mr.ID); latest != nil {
		old, _ := s.DB.ListDiscussions(latest.ID)
		for _, o := range old {
			if o.GitLabID != "" {
				continue
			}
			if id := matchThread(list, "", o.Author, o.File, o.Line); id != "" {
				if _, ok := assessments[id]; !ok {
					assessments[id] = db.Assessment{RunID: latest.ID, Kind: latest.Kind, HeadSHA: latest.HeadSHA, Text: o.Assessment, Addressed: o.Addressed}
				}
			}
		}
	}
	me := s.CurrentUser()
	out := Threads{SyncedAt: s.DB.DiscussionsSyncedAt(mr.ID)}
	var waiting, rest []ThreadView
	for _, d := range list {
		v := ThreadView{MRDiscussion: d, Mine: me != "" && d.Author == me}
		if a, ok := assessments[d.GitLabID]; ok {
			v.Assessment = &a
		}
		if id := d.FirstNoteID(); id > 0 && mr.WebURL != "" {
			v.URL = fmt.Sprintf("%s#note_%d", mr.WebURL, id)
		}
		switch {
		case d.Open():
			v.WaitsForMe = me != "" && d.LastAuthor() != me
			if v.WaitsForMe {
				waiting = append(waiting, v)
			} else {
				rest = append(rest, v)
			}
		case d.Resolvable:
			out.Resolved = append(out.Resolved, v)
		default:
			out.Comments = append(out.Comments, v)
		}
	}
	out.Open = append(waiting, rest...)
	return out
}

// RefreshThreads reads the discussions of an MR from GitLab and reports whether the page showing them is out of date.
func (s *Service) RefreshThreads(mrID int64) (changed bool, err error) {
	before := s.DataVersion(mrID)
	if err := s.SyncDiscussions(mrID); err != nil {
		return false, err
	}
	return s.DataVersion(mrID) != before, nil
}

// ThreadCode returns the code a thread on a diff line is about, at the commit the comment was written against
// (a comment on a removed line: the old file at the base of that diff version).
func (s *Service) ThreadCode(threadID int64) (*FindingCode, error) {
	d, _ := s.DB.GetMRDiscussion(threadID)
	if d == nil {
		return nil, userErr("обсуждение #%d не найдено", threadID)
	}
	if v := s.forMR(d.MRID); v != s {
		return v.ThreadCode(threadID)
	}
	mr, _ := s.DB.GetMR(d.MRID)
	if mr == nil {
		return nil, userErr("merge request of discussion #%d not found", threadID)
	}
	if d.File == "" {
		return nil, userErr("обсуждение не привязано к файлу")
	}
	if d.Line == 0 && d.OldLine > 0 && d.BaseSHA != "" {
		code, err := s.codeExcerpt(mr, d.BaseSHA, firstOf(d.OldPath, d.File), d.OldLine)
		if code != nil {
			code.InDiff, code.Note = false, "строка удалена в MR: показан файл до изменений"
		}
		return code, err
	}
	return s.codeExcerpt(mr, firstOf(d.HeadSHA, mr.HeadSHA), d.File, d.Line)
}

// notifyThreads tells in Telegram about new threads other people opened in my MRs.
func (s *Service) notifyThreads(mr *db.MergeRequest, added []db.MRDiscussion) {
	if !s.telegram.Enabled() || len(added) == 0 {
		return
	}
	me := s.CurrentUser()
	if me == "" || mr.Author != me {
		return
	}
	for _, d := range added {
		if !d.Open() || d.Author == me || d.Author == "" {
			continue
		}
		label := fmt.Sprintf("!%d", mr.IID)
		if !s.Settings.TelegramNoTitles && mr.Title != "" {
			label += " «" + mr.Title + "»"
		}
		lines := []string{"💬 Новый тред в моём MR " + label, "@" + d.Author}
		if d.File != "" {
			where := d.File
			if n := firstPositive(d.Line, d.OldLine); n > 0 {
				where = fmt.Sprintf("%s:%d", d.File, n)
			}
			lines[1] += " · " + where
		}
		if !s.Settings.TelegramNoTitles {
			lines = append(lines, clip(firstLine(d.First().Body), 300))
		}
		lines = append(lines, fmt.Sprintf("%s/mr/%d#discussions", s.DashboardURL(), mr.ID))
		key, text := fmt.Sprintf("thread %d %s", mr.ID, d.GitLabID), strings.Join(lines, "\n")
		go func() {
			if _, err := s.telegram.Notify(key, text); err != nil {
				fmt.Printf("telegram: %v\n", err)
			}
		}()
	}
}

func clip(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n]) + "…"
}
