package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func project(t *testing.T) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "CLAUDE.md"), "# rules\n")
	write(t, filepath.Join(root, ".claude", "CLAUDE.md"), "## Review workflow defaults\n")
	write(t, filepath.Join(root, "AGENTS.md"), "# agents\n")
	write(t, filepath.Join(root, ".claude", "agents", "mr-review.md"), "---\nname: mr-review\ndescription: Use when the user pastes a GitLab merge request link.\nmodel: opus\n---\nbody\n")
	write(t, filepath.Join(root, ".claude", "skills", "php-style", "SKILL.md"), "---\nname: php-style\ndescription: PHP style\n---\n")
	return root
}

func TestParseFrontmatter(t *testing.T) {
	m := ParseFrontmatter("---\nname: mr-review\ndescription: \"Quoted\"\nmodel: opus\n---\nbody")
	if m["name"] != "mr-review" || m["description"] != "Quoted" || m["model"] != "opus" {
		t.Fatalf("%v", m)
	}
	if len(ParseFrontmatter("no frontmatter")) != 0 {
		t.Fatal("expected empty")
	}
}

func TestResolvePrefersAgent(t *testing.T) {
	root := project(t)
	r := New(root, "")
	s := r.Resolve()
	if s == nil || s.Kind != KindAgent || s.Name != "mr-review" || s.RelPath != ".claude/agents/mr-review.md" {
		t.Fatalf("%+v", s)
	}
	if s.Invocation() != "claude --agent mr-review" || s.Identifier() != "agent:mr-review" {
		t.Fatalf("%s %s", s.Invocation(), s.Identifier())
	}
	if v := r.Validate(*s); !v.OK {
		t.Fatalf("%v", v.Problems)
	}
	kinds := map[string]bool{}
	for _, f := range r.InstructionFiles() {
		kinds[f.Kind+":"+f.RelPath] = true
	}
	for _, want := range []string{"claude-md:CLAUDE.md", "claude-md:.claude/CLAUDE.md", "agents-md:AGENTS.md", "agent:.claude/agents/mr-review.md", "skill:.claude/skills/php-style/SKILL.md"} {
		if !kinds[want] {
			t.Fatalf("missing %s in %v", want, kinds)
		}
	}
}

func TestPreferredCommandAndFallbacks(t *testing.T) {
	root := project(t)
	write(t, filepath.Join(root, ".claude", "commands", "review-mr.md"), "---\ndescription: Review merge request\n---\n")
	s := New(root, "review-mr").Resolve()
	if s == nil || s.Kind != KindCommand || s.Name != "review-mr" {
		t.Fatalf("%+v", s)
	}
	_ = os.Remove(filepath.Join(root, ".claude", "agents", "mr-review.md"))
	_ = os.Remove(filepath.Join(root, ".claude", "commands", "review-mr.md"))
	write(t, filepath.Join(root, ".claude", "skills", "code-check", "SKILL.md"), "---\nname: code-check\ndescription: Review a merge request diff\n---\n")
	s = New(root, "").Resolve()
	if s == nil || s.Kind != KindSkill || s.Name != "code-check" {
		t.Fatalf("%+v", s)
	}
	_ = os.RemoveAll(filepath.Join(root, ".claude", "skills", "code-check"))
	if New(root, "").Resolve() != nil {
		t.Fatal("expected nil")
	}
}

func TestValidateMissingName(t *testing.T) {
	root := project(t)
	write(t, filepath.Join(root, ".claude", "agents", "mr-review.md"), "---\ndescription: x\n---\nbody")
	r := New(root, "")
	s := r.Resolve()
	if v := r.Validate(*s); v.OK {
		t.Fatal("expected validation failure")
	}
}
