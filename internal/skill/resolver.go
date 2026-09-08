// Package skill discovers the project-local Claude review skill without copying it.
package skill

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	DefaultName = "mr-review"
	KindAgent   = "agent"
	KindCommand = "command"
	KindSkill   = "skill"
)

var kindPriority = map[string]int{KindAgent: 0, KindCommand: 1, KindSkill: 2}

var (
	reviewWords = regexp.MustCompile(`(?i)\breview`)
	mrWords     = regexp.MustCompile(`(?i)merge[\s_-]?request|\bMR\b|pull[\s_-]?request`)
)

// Skill is a logical reference to a project-local agent/command/skill.
type Skill struct {
	Kind        string
	Name        string
	RelPath     string
	Description string
	Model       string
}

// Identifier is kind:name.
func (s Skill) Identifier() string { return s.Kind + ":" + s.Name }

// Invocation describes how the skill is invoked.
func (s Skill) Invocation() string {
	if s.Kind == KindAgent {
		return "claude --agent " + s.Name
	}
	return "/" + s.Name + " <mr-url> (slash command in the prompt)"
}

// AbsPath joins the project root and the relative path.
func (s Skill) AbsPath(root string) string { return filepath.Join(root, filepath.FromSlash(s.RelPath)) }

// InstructionFile is a project file Claude Code loads automatically.
type InstructionFile struct {
	RelPath string
	Kind    string // claude-md | agents-md | settings | rule | agent | command | skill
}

// Validation is the result of a lightweight check.
type Validation struct {
	OK       bool
	Problems []string
}

// Resolver scans PROJECT_ROOT.
type Resolver struct {
	Root      string
	Preferred string
}

// New creates a resolver; preferred "" means DefaultName.
func New(root, preferred string) *Resolver {
	if preferred == "" {
		preferred = DefaultName
	}
	return &Resolver{Root: root, Preferred: preferred}
}

// ParseFrontmatter parses a simple `key: value` YAML frontmatter block.
func ParseFrontmatter(text string) map[string]string {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return out
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || !strings.Contains(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(key)] = value
	}
	return out
}

// Candidates lists every agent/command/skill found under .claude/.
func (r *Resolver) Candidates() []Skill {
	var found []Skill
	found = append(found, r.scanMarkdown(filepath.Join(r.Root, ".claude", "agents"), KindAgent)...)
	found = append(found, r.scanMarkdown(filepath.Join(r.Root, ".claude", "commands"), KindCommand)...)
	found = append(found, r.scanSkills(filepath.Join(r.Root, ".claude", "skills"))...)
	sort.Slice(found, func(i, j int) bool {
		if kindPriority[found[i].Kind] != kindPriority[found[j].Kind] {
			return kindPriority[found[i].Kind] < kindPriority[found[j].Kind]
		}
		return found[i].Name < found[j].Name
	})
	return found
}

// Resolve picks the preferred skill, then a review+MR match, then any review match.
func (r *Resolver) Resolve() *Skill {
	candidates := r.Candidates()
	for i := range candidates {
		if candidates[i].Name == r.Preferred {
			return &candidates[i]
		}
	}
	for i := range candidates {
		text := candidates[i].Name + " " + candidates[i].Description
		if reviewWords.MatchString(text) && mrWords.MatchString(text) {
			return &candidates[i]
		}
	}
	for i := range candidates {
		if reviewWords.MatchString(candidates[i].Name + " " + candidates[i].Description) {
			return &candidates[i]
		}
	}
	return nil
}

// Validate checks that the skill file exists and has usable frontmatter.
func (r *Resolver) Validate(s Skill) Validation {
	data, err := os.ReadFile(s.AbsPath(r.Root))
	if err != nil {
		return Validation{false, []string{"cannot read " + s.RelPath + ": " + err.Error()}}
	}
	var problems []string
	if strings.TrimSpace(string(data)) == "" {
		problems = append(problems, s.RelPath+" is empty")
	}
	meta := ParseFrontmatter(string(data))
	if s.Kind == KindAgent && meta["name"] == "" {
		problems = append(problems, "agent frontmatter has no 'name' — `claude --agent` needs it")
	}
	if meta["description"] == "" {
		problems = append(problems, "frontmatter has no 'description' (not fatal)")
	}
	ok := true
	for _, p := range problems {
		if !strings.Contains(p, "not fatal") {
			ok = false
		}
	}
	return Validation{ok, problems}
}

// InstructionFiles lists project files Claude Code picks up automatically.
func (r *Resolver) InstructionFiles() []InstructionFile {
	var files []InstructionFile
	add := func(rel, kind string) {
		if info, err := os.Stat(filepath.Join(r.Root, rel)); err == nil && !info.IsDir() {
			files = append(files, InstructionFile{rel, kind})
		}
	}
	add("CLAUDE.md", "claude-md")
	add(".claude/CLAUDE.md", "claude-md")
	add("CLAUDE.local.md", "claude-md")
	add("AGENTS.md", "agents-md")
	add(".claude/settings.json", "settings")
	add(".claude/settings.local.json", "settings")
	for _, rel := range r.walk(filepath.Join(r.Root, ".claude", "rules"), ".md") {
		files = append(files, InstructionFile{rel, "rule"})
	}
	for _, c := range r.Candidates() {
		files = append(files, InstructionFile{c.RelPath, c.Kind})
	}
	return files
}

func (r *Resolver) walk(dir, suffix string) []string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, suffix) {
			return nil
		}
		rel, err := filepath.Rel(r.Root, path)
		if err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (r *Resolver) scanMarkdown(dir, kind string) []Skill {
	var out []Skill
	for _, rel := range r.walk(dir, ".md") {
		meta := readFrontmatter(filepath.Join(r.Root, rel))
		name := meta["name"]
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(rel), ".md")
		}
		out = append(out, Skill{kind, name, rel, meta["description"], meta["model"]})
	}
	return out
}

func (r *Resolver) scanSkills(dir string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		file := filepath.Join(dir, entry.Name(), "SKILL.md")
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			continue
		}
		meta := readFrontmatter(file)
		name := meta["name"]
		if name == "" {
			name = entry.Name()
		}
		rel, _ := filepath.Rel(r.Root, file)
		out = append(out, Skill{KindSkill, name, filepath.ToSlash(rel), meta["description"], ""})
	}
	return out
}

func readFrontmatter(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseFrontmatter(string(data))
}
