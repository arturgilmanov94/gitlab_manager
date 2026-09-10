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

func TestActionMapAndFallbacks(t *testing.T) {
	root := project(t)
	r := NewWithNames(root, nil)
	byKind := map[string]Resolution{}
	for _, res := range r.Map() {
		byKind[res.Action.Kind] = res
	}
	if len(byKind) != len(Actions) {
		t.Fatalf("map must cover every action: %d != %d", len(byKind), len(Actions))
	}
	// Full review: the project agent, found directly.
	if res := byKind[ActionReviewFull]; res.Skill == nil || res.Skill.Name != "mr-review" || res.Via != "" || res.Wanted != "mr-review" {
		t.Fatalf("%+v", res)
	}
	// Quick review and verify: no own skill → the full-review agent via fallback.
	for _, kind := range []string{ActionReviewQuick, ActionReviewVerify} {
		if res := byKind[kind]; res.Skill == nil || res.Skill.Name != "mr-review" || res.Via != ActionReviewFull {
			t.Fatalf("%s: %+v", kind, res)
		}
	}
	// Task actions: nothing in the project → CLAUDE.md only.
	for _, kind := range []string{ActionPlan, ActionImplement, ActionFixComments} {
		if res := byKind[kind]; res.Skill != nil || res.Found() {
			t.Fatalf("%s must be unresolved: %+v", kind, res)
		}
	}
	if byKind[ActionPlan].Wanted != "task-plan" || byKind[ActionImplement].Wanted != "task-implement" || byKind[ActionFixComments].Wanted != "mr-fix-comments" {
		t.Fatalf("default names: %+v", byKind)
	}

	// The project adds its own skills: they win over the fallback.
	write(t, filepath.Join(root, ".claude", "skills", "task-plan", "SKILL.md"), "---\nname: task-plan\ndescription: Plan a task\n---\n")
	write(t, filepath.Join(root, ".claude", "agents", "mr-review-quick.md"), "---\nname: mr-review-quick\ndescription: Quick diff review\n---\n")
	if res := r.ForAction(ActionPlan); res.Skill == nil || res.Skill.Kind != KindSkill || res.Skill.Name != "task-plan" || res.Via != "" {
		t.Fatalf("%+v", res)
	}
	if res := r.ForAction(ActionReviewQuick); res.Skill == nil || res.Skill.Kind != KindAgent || res.Skill.Name != "mr-review-quick" || res.Via != "" {
		t.Fatalf("%+v", res)
	}

	// Names can be overridden per action (.env SKILL_* variables).
	r = NewWithNames(root, map[string]string{ActionImplement: "task-plan", ActionReviewFull: "missing-one"})
	if res := r.ForAction(ActionImplement); res.Skill == nil || res.Skill.Name != "task-plan" || res.Wanted != "task-plan" {
		t.Fatalf("%+v", res)
	}
	// An override that does not exist falls back to the review heuristic for the full review.
	if res := r.ForAction(ActionReviewFull); res.Skill == nil || res.Skill.Name != "mr-review" || res.Wanted != "missing-one" {
		t.Fatalf("%+v", res)
	}
	if r.ForAction("unknown_kind").Skill != nil || ActionFor("unknown_kind") != nil {
		t.Fatal("unknown kinds resolve to nothing")
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

func TestCustomInstructionsReplaceProjectSkill(t *testing.T) {
	root := t.TempDir()
	r := NewWithNames(root, nil).WithCustom(map[string]string{ActionPlan: "Always start from the ticket's acceptance criteria.\nList affected modules."})
	res := r.ForAction(ActionPlan)
	if res.Skill == nil || res.Skill.Kind != KindCustom || res.Wanted != "custom" || res.Skill.Body == "" || res.Skill.Description != "Always start from the ticket's acceptance criteria." {
		t.Fatalf("%+v", res)
	}
	if v := r.Validate(*res.Skill); !v.OK {
		t.Fatalf("%+v", v)
	}
	if r.ForAction(ActionReviewFull).Skill != nil {
		t.Fatal("other actions are untouched")
	}
	if NewWithNames(root, nil).WithCustom(map[string]string{ActionPlan: "  "}).ForAction(ActionPlan).Skill != nil {
		t.Fatal("blank instructions do not count")
	}
}
