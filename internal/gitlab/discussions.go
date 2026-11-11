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
	raw, _ := json.Marshal(payload)
	args := []string{"api"}
	if ref.Host != "" {
		args = append(args, "--hostname", ref.Host)
	}
	args = append(args, "--method", "POST", "-H", "Content-Type: application/json", "--input", "-",
		fmt.Sprintf("projects/%s/merge_requests/%d/discussions", ref.EncodedProject(), ref.IID))
	ctx, cancel := context.WithTimeout(context.Background(), g.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.Bin, args...)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("GitLab did not accept the comment: %s", firstNonEmpty(lastLine(stderr.String()), lastLine(stdout.String()), err.Error()))
	}
	var obj map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &obj); err != nil {
		return 0, errors.New("unexpected response from GitLab when posting the comment")
	}
	notes, _ := obj["notes"].([]any)
	if len(notes) > 0 {
		if note, ok := notes[0].(map[string]any); ok {
			return Int(note, "id"), nil
		}
	}
	return 0, nil
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
