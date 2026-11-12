package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Version is one diff version of an MR (every push makes a new one). Its SHAs anchor diff comments.
type Version struct {
	ID                                int64
	HeadSHA, BaseSHA, StartSHA, State string
}

// Position places a diff comment on a line of a file in an MR version. NewLine is the line in the new file;
// OldLine is set as well for an unchanged (context) line and alone for a removed one.
type Position struct {
	BaseSHA, StartSHA, HeadSHA string
	OldPath, NewPath           string
	NewLine, OldLine           int64
}

// MRVersions lists the diff versions of an MR, newest first.
func (g *Glab) MRVersions(ref Ref) ([]Version, error) {
	list, err := g.apiList(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d/versions?per_page=100", ref.EncodedProject(), ref.IID))
	if err != nil {
		return nil, err
	}
	return ParseVersions(list), nil
}

// ParseVersions converts a versions payload.
func ParseVersions(list []map[string]any) []Version {
	var out []Version
	for _, item := range list {
		out = append(out, Version{ID: Int(item, "id"), HeadSHA: Str(item, "head_commit_sha"), BaseSHA: Str(item, "base_commit_sha"),
			StartSHA: Str(item, "start_commit_sha"), State: Str(item, "state")})
	}
	return out
}

// MRVersionDiffs returns the per-file diffs of one MR version.
func (g *Glab) MRVersionDiffs(ref Ref, versionID int64) ([]FileDiff, error) {
	obj, err := g.apiObject(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d/versions/%d", ref.EncodedProject(), ref.IID, versionID))
	if err != nil {
		return nil, err
	}
	raw, _ := obj["diffs"].([]any)
	var list []map[string]any
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			list = append(list, m)
		}
	}
	return ParseDiffs(list), nil
}

// RawFile returns a file of the repository at a commit.
func (g *Glab) RawFile(ref Ref, path, sha string) (string, error) {
	return g.APIRaw(ref.Host, fmt.Sprintf("projects/%s/repository/files/%s/raw?ref=%s", ref.EncodedProject(), url.PathEscape(path), url.QueryEscape(sha)))
}

// CreateDiscussion starts a discussion in an MR: on a diff line when pos is given, otherwise on the MR itself.
// It returns the id of the created note (for the #note_<id> anchor).
func (g *Glab) CreateDiscussion(ref Ref, body string, pos *Position) (int64, error) {
	payload := map[string]any{"body": body}
	if pos != nil {
		p := map[string]any{"position_type": "text", "base_sha": pos.BaseSHA, "start_sha": pos.StartSHA, "head_sha": pos.HeadSHA,
			"old_path": firstNonEmpty(pos.OldPath, pos.NewPath), "new_path": pos.NewPath}
		if pos.NewLine > 0 {
			p["new_line"] = pos.NewLine
		}
		if pos.OldLine > 0 {
			p["old_line"] = pos.OldLine
		}
		payload["position"] = p
	}
	obj, err := g.apiWrite(ref.Host, "POST", fmt.Sprintf("projects/%s/merge_requests/%d/discussions", ref.EncodedProject(), ref.IID), payload)
	if err != nil {
		return 0, fmt.Errorf("GitLab did not accept the comment: %w", err)
	}
	notes, _ := obj["notes"].([]any)
	if len(notes) > 0 {
		if note, ok := notes[0].(map[string]any); ok {
			return Int(note, "id"), nil
		}
	}
	return 0, nil
}

// apiWrite sends a JSON payload with POST/PUT/DELETE and returns the response object. These are the only calls
// of the dashboard that change something in GitLab; each one is started by the developer's button.
func (g *Glab) apiWrite(host, method, path string, payload map[string]any) (map[string]any, error) {
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "--method", method)
	var stdin []byte
	if payload != nil {
		stdin, _ = json.Marshal(payload)
		args = append(args, "-H", "Content-Type: application/json", "--input", "-")
	}
	args = append(args, path)
	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New(firstNonEmpty(apiMessage(stdout.Bytes()), lastLine(stderr.String()), lastLine(stdout.String()), err.Error()))
	}
	obj := map[string]any{}
	if text := bytes.TrimSpace(stdout.Bytes()); len(text) > 0 && text[0] == '{' {
		if err := json.Unmarshal(text, &obj); err != nil {
			return nil, errors.New("unexpected response from GitLab")
		}
	}
	return obj, nil
}

// apiMessage is the "message" of a GitLab error body ("" when there is none).
func apiMessage(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(bytes.TrimSpace(body), &obj) != nil {
		return ""
	}
	switch m := obj["message"].(type) {
	case string:
		return m
	case nil:
		return Str(obj, "error")
	default:
		raw, _ := json.Marshal(m)
		return string(raw)
	}
}

// ReplyDiscussion adds a note to a discussion of an MR and returns the id of the new note.
func (g *Glab) ReplyDiscussion(ref Ref, discussionID, body string) (int64, error) {
	obj, err := g.apiWrite(ref.Host, "POST", fmt.Sprintf("projects/%s/merge_requests/%d/discussions/%s/notes", ref.EncodedProject(), ref.IID, url.PathEscape(discussionID)),
		map[string]any{"body": body})
	if err != nil {
		return 0, fmt.Errorf("GitLab did not accept the reply: %w", err)
	}
	return Int(obj, "id"), nil
}

// ResolveDiscussion resolves a thread of an MR or opens it again.
func (g *Glab) ResolveDiscussion(ref Ref, discussionID string, resolved bool) error {
	_, err := g.apiWrite(ref.Host, "PUT", fmt.Sprintf("projects/%s/merge_requests/%d/discussions/%s?resolved=%t", ref.EncodedProject(), ref.IID, url.PathEscape(discussionID), resolved), nil)
	if err != nil {
		return fmt.Errorf("GitLab did not change the thread: %w", err)
	}
	return nil
}

// DiffLine is one line of the new file as the MR diff sees it.
type DiffLine struct {
	Added   bool  // the line is new in the MR
	OldLine int64 // for a context line: its number in the old file
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// NewFileLines maps the new-file line numbers a unified diff shows (added and context lines) to what they are.
// Lines outside the hunks are not in the map: GitLab does not accept a diff comment there.
func NewFileLines(diff string) map[int64]DiffLine {
	out := map[int64]DiffLine{}
	var oldN, newN int64
	in := false
	for _, line := range strings.Split(diff, "\n") {
		if m := hunkRe.FindStringSubmatch(line); m != nil {
			oldN, _ = strconv.ParseInt(m[1], 10, 64)
			newN, _ = strconv.ParseInt(m[2], 10, 64)
			in = true
			continue
		}
		if !in || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			out[newN] = DiffLine{Added: true}
			newN++
		case '-':
			oldN++
		case ' ':
			out[newN] = DiffLine{OldLine: oldN}
			oldN++
			newN++
		}
	}
	return out
}

// Note is one comment of a discussion.
type Note struct {
	ID         int64  `json:"id"`
	Author     string `json:"author"`      // username
	AuthorName string `json:"author_name"` // display name
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
	System     bool   `json:"system,omitempty"` // written by GitLab ("changed this line in version 3")
}

// Discussion is one discussion of an MR as people see it: a thread on a diff line, a thread on the MR itself or a
// plain comment (Resolvable false). Discussions made only of system notes are not people talking and are left out.
type Discussion struct {
	ID         string
	Resolvable bool
	Resolved   bool
	ResolvedBy string
	// Where the thread sits ("" = on the MR itself). Line is the line of the new file; a comment on a removed line
	// has OldLine alone. The SHAs are the diff version the comment was written against.
	File, OldPath    string
	Line, OldLine    int64
	HeadSHA, BaseSHA string
	Notes            []Note
}

// Author is who started the discussion.
func (d Discussion) Author() string {
	if len(d.Notes) == 0 {
		return ""
	}
	return d.Notes[0].Author
}

// Discussions returns every discussion of an MR with all its notes.
func (g *Glab) Discussions(ref Ref) ([]Discussion, error) {
	items, err := g.apiList(ref.Host, fmt.Sprintf("projects/%s/merge_requests/%d/discussions?per_page=100", ref.EncodedProject(), ref.IID))
	if err != nil {
		return nil, err
	}
	return ParseDiscussions(items), nil
}

// ParseDiscussions converts a discussions payload.
func ParseDiscussions(items []map[string]any) []Discussion {
	var out []Discussion
	for _, disc := range items {
		raw, _ := disc["notes"].([]any)
		d := Discussion{ID: Str(disc, "id")}
		people := false
		for i, item := range raw {
			note, ok := item.(map[string]any)
			if !ok {
				continue
			}
			author := Nested(note, "author")
			n := Note{ID: Int(note, "id"), Author: Str(author, "username"), AuthorName: Str(author, "name"), Body: Str(note, "body"),
				CreatedAt: Str(note, "created_at"), System: Bool(note, "system")}
			people = people || !n.System
			if i == 0 {
				d.Resolvable, d.Resolved = Bool(note, "resolvable"), Bool(note, "resolved")
				d.ResolvedBy = Str(Nested(note, "resolved_by"), "username")
				pos := Nested(note, "position")
				d.File, d.OldPath = firstNonEmpty(Str(pos, "new_path"), Str(pos, "old_path")), Str(pos, "old_path")
				d.Line, d.OldLine = Int(pos, "new_line"), Int(pos, "old_line")
				d.HeadSHA, d.BaseSHA = Str(pos, "head_sha"), Str(pos, "base_sha")
			}
			d.Notes = append(d.Notes, n)
		}
		if d.ID == "" || !people {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Unresolved counts the threads that still wait for an answer: resolvable and not resolved.
func Unresolved(list []Discussion) int64 {
	var n int64
	for _, d := range list {
		if d.Resolvable && !d.Resolved {
			n++
		}
	}
	return n
}

// Approve approves an MR as the current user. sha is the head the developer looked at: GitLab refuses when the
// MR has moved on since, so an approval never lands on commits nobody saw.
func (g *Glab) Approve(ref Ref, sha string) error {
	payload := map[string]any{}
	if sha != "" {
		payload["sha"] = sha
	}
	if _, err := g.apiWrite(ref.Host, "POST", fmt.Sprintf("projects/%s/merge_requests/%d/approve", ref.EncodedProject(), ref.IID), payload); err != nil {
		return fmt.Errorf("GitLab did not accept the approval: %w", err)
	}
	return nil
}

// Unapprove takes the current user's approval back.
func (g *Glab) Unapprove(ref Ref) error {
	if _, err := g.apiWrite(ref.Host, "POST", fmt.Sprintf("projects/%s/merge_requests/%d/unapprove", ref.EncodedProject(), ref.IID), nil); err != nil {
		return fmt.Errorf("GitLab did not take the approval back: %w", err)
	}
	return nil
}

// RetryPipeline restarts the failed and cancelled jobs of a pipeline.
func (g *Glab) RetryPipeline(ref Ref, pipelineID int64) error {
	if _, err := g.apiWrite(ref.Host, "POST", fmt.Sprintf("projects/%s/pipelines/%d/retry", ref.EncodedProject(), pipelineID), nil); err != nil {
		return fmt.Errorf("GitLab did not restart the pipeline: %w", err)
	}
	return nil
}
