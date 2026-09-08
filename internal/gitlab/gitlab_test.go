package gitlab

import "testing"

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
	ref, err := ParseIssueRef("https://gitlab.ffintech.com/tn/project/eu/eu/-/issues/8217", "", "")
	if err != nil || ref.ProjectPath != "tn/project/eu/eu" || ref.IID != 8217 {
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
