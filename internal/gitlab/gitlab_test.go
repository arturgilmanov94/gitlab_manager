package gitlab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMRRef(t *testing.T) {
	ref, err := ParseMRRef("https://gitlab.example.com/tn/core/tradernet/-/merge_requests/102531/diffs?x=1", "", "")
	if err != nil || ref.Host != "gitlab.example.com" || ref.ProjectPath != "tn/core/tradernet" || ref.IID != 102531 {
		t.Fatalf("%+v %v", ref, err)
	}
	if ref.EncodedProject() != "tn%2Fcore%2Ftradernet" {
		t.Fatal(ref.EncodedProject())
	}
	ref, err = ParseMRRef("group/proj!12", "gl.local", "")
	if err != nil || ref.ProjectPath != "group/proj" || ref.IID != 12 {
		t.Fatalf("%+v %v", ref, err)
	}
	ref, err = ParseMRRef("!7", "gl.local", "g/p")
	if err != nil || ref.IID != 7 || ref.ProjectPath != "g/p" {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, bad := range []string{"", "https://gitlab.example.com/tn/core/tradernet", "!7", "not a ref", "https://gl/g/p/-/issues/5"} {
		if _, err := ParseMRRef(bad, "", ""); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestParseIssueRef(t *testing.T) {
	ref, err := ParseIssueRef("https://gitlab.example.com/group/sub/project/-/issues/8217", "", "")
	if err != nil || ref.ProjectPath != "group/sub/project" || ref.IID != 8217 {
		t.Fatalf("%+v %v", ref, err)
	}
	if _, err := ParseIssueRef("g/p!3", "h", ""); err == nil {
		t.Fatal("MR short ref must not parse as issue")
	}
	if ref, err := ParseIssueRef("#3", "h", "g/p"); err != nil || ref.IID != 3 {
		t.Fatalf("%+v %v", ref, err)
	}
}

func TestParseGitRemote(t *testing.T) {
	cases := map[string][2]string{
		"ssh://gitlab@gitlab.example.com:22122/tn/core/tradernet.git": {"gitlab.example.com", "tn/core/tradernet"},
		"git@gitlab.example.com:tn/core/tradernet.git":                {"gitlab.example.com", "tn/core/tradernet"},
		"https://gitlab.example.com/tn/core/tradernet":                {"gitlab.example.com", "tn/core/tradernet"},
	}
	for remote, want := range cases {
		host, path, ok := ParseGitRemote(remote)
		if !ok || host != want[0] || path != want[1] {
			t.Fatalf("%s -> %s %s %v", remote, host, path, ok)
		}
	}
	if _, _, ok := ParseGitRemote(""); ok {
		t.Fatal("empty must fail")
	}
}

func TestPayloadHelpers(t *testing.T) {
	obj := map[string]any{
		"iid": 5.0, "sha": "list-sha", "diff_refs": map[string]any{"head_sha": "head-sha"},
		"references": map[string]any{"full": "g/p!5"}, "web_url": "https://gl/g/p/-/merge_requests/5",
		"labels": []any{"bug", "backend"},
	}
	if HeadSHA(obj) != "head-sha" || Int(obj, "iid") != 5 || Labels(obj) != "bug, backend" {
		t.Fatal("helpers")
	}
	host, project, ok := ProjectPathOf(obj, "!")
	if !ok || host != "gl" || project != "g/p" {
		t.Fatalf("%s %s %v", host, project, ok)
	}
	delete(obj, "references")
	if _, project, _ = ProjectPathOf(obj, "!"); project != "g/p" {
		t.Fatalf("fallback to web_url: %s", project)
	}
}

func TestParseCompare(t *testing.T) {
	obj := map[string]any{
		"commits": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}},
		"diffs": []any{
			map[string]any{"diff": "--- a/x.php\n+++ b/x.php\n@@ -1,2 +1,3 @@\n-old\n+new\n+more\n context\n"},
			map[string]any{"diff": "+++ b/y.php\n+only\n"},
		},
	}
	got := ParseCompare(obj)
	if got != (Changes{Commits: 2, Files: 2, Additions: 3, Deletions: 1}) {
		t.Fatalf("%+v", got)
	}
}

func TestJobsAndTrace(t *testing.T) {
	jobs := ParseJobs([]map[string]any{{"id": 5.0, "name": "phpunit", "stage": "test", "status": "failed", "web_url": "u", "failure_reason": "script_failure", "allow_failure": true}})
	if len(jobs) != 1 || jobs[0].ID != 5 || jobs[0].Name != "phpunit" || !jobs[0].AllowFailure || jobs[0].FailureReason != "script_failure" {
		t.Fatalf("%+v", jobs)
	}
	raw := "\x1b[0Ksection_start:1700000000:step_script\r\n\x1b[32mRunning\x1b[0m\nline2\nline3\nFAILED\n"
	if got := TailLines(raw, 2); got != "line3\nFAILED" {
		t.Fatalf("%q", got)
	}
	if got := TailLines(raw, 0); strings.Contains(got, "\x1b") || strings.Contains(got, "section_start") || !strings.Contains(got, "Running") {
		t.Fatalf("%q", got)
	}
	if id, u := PipelineID(map[string]any{"head_pipeline": map[string]any{"id": 9.0, "web_url": "p"}}), PipelineURL(map[string]any{"head_pipeline": map[string]any{"web_url": "p"}}); id != 9 || u != "p" {
		t.Fatal(id, u)
	}
}

func TestParseDiffsAndThreads(t *testing.T) {
	diffs := ParseDiffs([]map[string]any{{"old_path": "a.php", "new_path": "b.php", "renamed_file": true, "diff": "@@"}, {"old_path": "c.php", "new_path": "c.php", "new_file": true, "diff": "+x"}})
	if len(diffs) != 2 || !diffs[0].Renamed || diffs[0].NewPath != "b.php" || !diffs[1].New {
		t.Fatalf("%+v", diffs)
	}
	threads := ParseDiscussions([]map[string]any{
		{"id": "d1", "notes": []any{map[string]any{"id": 11.0, "resolvable": true, "resolved": false, "body": "why?", "author": map[string]any{"username": "bob", "name": "Bob"},
			"position": map[string]any{"new_path": "src/A.php", "old_path": "src/Old.php", "new_line": 12.0, "head_sha": "h1", "base_sha": "b1"}}, map[string]any{"id": 12.0, "body": "reply", "author": map[string]any{"username": "alice"}}}},
		{"id": "d2", "notes": []any{map[string]any{"resolvable": true, "resolved": true, "body": "done", "resolved_by": map[string]any{"username": "alice"}}}},
		{"id": "d3", "notes": []any{map[string]any{"resolvable": false, "body": "general note"}}},
		{"id": "d4", "notes": []any{map[string]any{"system": true, "body": "added 2 commits"}}},
	})
	if len(threads) != 3 || Unresolved(threads) != 1 {
		t.Fatalf("%+v", threads)
	}
	if d := threads[0]; d.Author() != "bob" || d.File != "src/A.php" || d.OldPath != "src/Old.php" || d.Line != 12 || d.HeadSHA != "h1" || len(d.Notes) != 2 || d.Notes[1].Author != "alice" || d.Notes[0].ID != 11 {
		t.Fatalf("%+v", d)
	}
	if d := threads[1]; !d.Resolved || d.ResolvedBy != "alice" || threads[2].Resolvable {
		t.Fatalf("%+v", threads)
	}
}

func TestNewFileLines(t *testing.T) {
	diff := "@@ -8,3 +8,4 @@ func x\n ctx8\n-gone\n+new9\n+new10\n ctx\n@@ -40 +41,2 @@\n+n41\n c\n\\ No newline at end of file\n"
	got := NewFileLines(diff)
	want := map[int64]DiffLine{8: {OldLine: 8}, 9: {Added: true}, 10: {Added: true}, 11: {OldLine: 10}, 41: {Added: true}, 42: {OldLine: 40}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Fatalf("line %d: %+v, want %+v", n, got[n], w)
		}
	}
}

// fakeGlab is a glab binary that answers every `glab api` list with pages of 100, 100 and 50 unresolved threads.
const fakeGlab = `#!/bin/sh
for a; do last=$a; done
echo "$last" >> "$0.calls"
page=$(echo "$last" | sed -n 's/.*[?&]page=\([0-9]*\).*/\1/p')
count=0
case "$page" in 1|2) count=100;; 3) count=50;; esac
printf '['
i=0
while [ $i -lt $count ]; do
  [ $i -gt 0 ] && printf ','
  printf '{"id":"%s-%s","notes":[{"resolvable":true,"resolved":false}]}' "$page" "$i"
  i=$((i+1))
done
printf ']'
`

// 250 discussions over three pages are all counted; the page loop stops at the short page.
func TestPaginatedLists(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "glab")
	if err := os.WriteFile(bin, []byte(fakeGlab), 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGlab(bin)
	ref := Ref{Host: "gl", ProjectPath: "g/p", IID: 1}
	list, err := g.Discussions(ref)
	if err != nil || Unresolved(list) != 250 {
		t.Fatalf("want 250 unresolved, got %d %v", Unresolved(list), err)
	}
	calls, _ := os.ReadFile(bin + ".calls")
	if got := strings.Count(string(calls), "\n"); got != 3 {
		t.Fatalf("three pages expected, got %d:\n%s", got, calls)
	}
}
