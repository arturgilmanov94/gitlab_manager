// Package skill discovers the project-local Claude agents, commands and skills without copying them.
//
// Every dashboard action maps to an expected project skill name (see Actions). The dashboard never ships
// its own workflow: it finds the project's agent/command/skill and hands the run to it, falling back to
// CLAUDE.md / AGENTS.md plus a minimal orchestration prompt when the project has no skill for the action.
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
	KindCustom  = "custom" // instructions written in the dashboard for one action (stored in data/, not in the project)
)

// Dashboard actions. The strings equal the run kinds stored in the database (package db must not be imported here).
const (
	ActionReviewFull    = "review_full"
	ActionReviewQuick   = "review_quick"
	ActionReviewVerify  = "review_verify"
	ActionFixComments   = "fix_comments"
	ActionPlan          = "plan"
	ActionImplement     = "implement"
	ActionVerifyFinding = "verify_finding"
	ActionStandTest     = "stand_test"
	ActionCIAnalyze     = "ci_analyze"
	ActionCIFix         = "ci_fix"
	ActionFixFindings   = "fix_findings"
)

// Action is one dashboard action and the project skill it looks for.
type Action struct {
	Kind      string // run kind, e.g. review_full
	SkillName string // default project skill/agent/command name
	EnvKey    string // .env variable that overrides SkillName
	Fallback  string // action whose skill is used when this one is missing ("" = none)
	Title     string // human-readable title (UI language)
	Contract  string // what the project skill is expected to do (UI language)
}

// Actions is the ordered map "action → expected project skill". Documented in docs/SKILLS.md.
var Actions = []Action{
	{
		Kind: ActionReviewFull, SkillName: "mr-review", EnvKey: "SKILL_REVIEW_FULL",
		Title: "Полное ревью MR",
		Contract: "Получает ссылку на MR и head SHA. Читает MR, diff и обсуждения через glab, изучает затронутый код, " +
			"находит реальные проблемы всех уровней (CRITICAL…INFO) с файлом, строкой и предложением фикса, оценивает нерешённые обсуждения. Ничего не меняет.",
	},
	{
		Kind: ActionReviewQuick, SkillName: "mr-review-quick", EnvKey: "SKILL_REVIEW_QUICK", Fallback: ActionReviewFull,
		Title: "Быстрое ревью MR",
		Contract: "То же, что полное ревью, но только по diff и обсуждениям, без обхода кодовой базы; в отчёт идут CRITICAL/HIGH/MEDIUM. " +
			"Без своего skill используется skill полного ревью с пометкой «быстрый проход».",
	},
	{
		Kind: ActionReviewVerify, SkillName: "mr-review-verify", EnvKey: "SKILL_REVIEW_VERIFY", Fallback: ActionReviewFull,
		Title: "Проверка изменений после ревью",
		Contract: "Получает прежний SHA, текущий head и список открытых замечаний. По каждому решает: исправлено / открыто / неактуально " +
			"с доказательством, и ищет только новые проблемы из новых коммитов. Без своего skill используется skill полного ревью.",
	},
	{
		Kind: ActionVerifyFinding, SkillName: "mr-verify-finding", EnvKey: "SKILL_VERIFY_FINDING", Fallback: ActionReviewFull,
		Title: "Проверка одного замечания",
		Contract: "Получает ссылку на MR, head SHA и одно замечание ревью (файл, строка, описание, предложение). Не считая его верным априори, " +
			"перепроверяет по коду: подтверждено / ложное срабатывание / неактуально / недостаточно данных, с доказательством. Ничего не меняет. " +
			"Без своего skill используется skill полного ревью.",
	},
	{
		Kind: ActionFixComments, SkillName: "mr-fix-comments", EnvKey: "SKILL_FIX_COMMENTS",
		Title: "Исправление замечаний ревьюеров",
		Contract: "Работает в worktree на ветке MR. Читает нерешённые обсуждения через glab, правит код по правилам проекта, " +
			"прогоняет проверки; вопросы и бизнес-решения оставляет человеку. Не коммитит, не пушит, не пишет в GitLab.",
	},
	{
		Kind: ActionStandTest, SkillName: "mr-stand-test", EnvKey: "SKILL_STAND_TEST",
		Title: "Проверка MR на стенде",
		Contract: "Работает в worktree на ветке MR. Через skill доступа к стенду проекта (STAND_SKILL) заливает изменённые файлы MR на стенд, " +
			"прогоняет там относящиеся к правке тесты, пишет и запускает скрипт, эмулирующий функциональность MR с моками внешних систем, " +
			"и отчитывается: что залито, результаты тестов, путь и вывод скрипта, найденные проблемы. Не коммитит, не пушит, на стенде не трогает git и БД. " +
			"Без своего skill dashboard ведёт сценарий сам, опираясь на skill доступа к стенду.",
	},
	{
		Kind: ActionCIAnalyze, SkillName: "mr-ci-analyze", EnvKey: "SKILL_CI_ANALYZE",
		Title: "Разбор упавшего pipeline",
		Contract: "Получает MR, head SHA, список упавших jobs и хвосты их логов. Только чтение: по логам, diff MR и коду проекта находит причину каждого падения " +
			"(код MR, тест, флак, окружение CI), предлагает исправление и говорит, можно ли исправить в MR. Без своего skill dashboard ведёт разбор сам.",
	},
	{
		Kind: ActionCIFix, SkillName: "mr-ci-fix", EnvKey: "SKILL_CI_FIX",
		Title: "Исправление CI",
		Contract: "Работает в worktree на ветке MR. Получает упавшие jobs и хвосты логов (и разбор, если был). Исправляет причину в коде MR, прогоняет " +
			"относящиеся тесты, делает self-review; если исправить в MR нельзя (инфраструктура, флаки) — объясняет причину и ничего не меняет. Не коммитит, не пушит.",
	},
	{
		Kind: ActionFixFindings, SkillName: "mr-fix-findings", EnvKey: "SKILL_FIX_FINDINGS", Fallback: ActionFixComments,
		Title: "Исправление выбранных замечаний",
		Contract: "Работает в worktree на ветке MR. Получает выбранные разработчиком замечания AI-ревью и нерешённые обсуждения ревьюеров. Каждое замечание " +
			"сначала перепроверяет, не считая верным априори; правит только подтверждённые, прогоняет тесты, делает self-review и отчитывается по каждому: " +
			"исправлено / не подтверждено / пропущено. Не коммитит, не пушит, в GitLab не пишет. Без своего skill используется skill исправления замечаний ревьюеров.",
	},
	{
		Kind: ActionPlan, SkillName: "task-plan", EnvKey: "SKILL_PLAN",
		Title: "Исследование задачи",
		Contract: "Получает задачу GitLab (ссылка, описание) и указания разработчика. Без изменений кода составляет план: шаги, файлы и что в них меняется, " +
			"риски, открытые вопросы, оценка размера.",
	},
	{
		Kind: ActionImplement, SkillName: "task-implement", EnvKey: "SKILL_IMPLEMENT",
		Title: "Решение задачи",
		Contract: "Работает в worktree на новой ветке от базовой. Реализует задачу по правилам проекта, добавляет/обновляет тесты, " +
			"прогоняет проверки и отчитывается: изменения, что прогнал, что осталось, предложенное сообщение коммита. Не коммитит и не пушит.",
	},
}

var standWords = regexp.MustCompile(`(?i)стенд|\bstand\b|staging|dev-server`)

// StandSkill finds the project's skill for reaching the developer's stand: by name (STAND_SKILL) or the first
// candidate whose name/description talks about a stand. nil when the project has none.
func (r *Resolver) StandSkill(name string) *Skill {
	candidates := r.Candidates()
	if name = strings.TrimSpace(name); name != "" {
		for i := range candidates {
			if candidates[i].Name == name {
				return &candidates[i]
			}
		}
		return nil
	}
	for i := range candidates {
		if standWords.MatchString(candidates[i].Name + " " + candidates[i].Description) {
			return &candidates[i]
		}
	}
	return nil
}

// ActionFor returns the action for a run kind (nil for unknown kinds).
func ActionFor(kind string) *Action {
	for i := range Actions {
		if Actions[i].Kind == kind {
			return &Actions[i]
		}
	}
	return nil
}

// Resolution is the outcome of looking up the skill for one action.
type Resolution struct {
	Action Action
	Wanted string // name that was looked for (override or default)
	Skill  *Skill // nil: no project skill, the run uses CLAUDE.md / AGENTS.md plus the dashboard prompt
	Via    string // kind of the fallback action whose skill is used ("" when found directly or not found)
}

// Found reports whether a project skill backs the action (directly or through the fallback).
func (r Resolution) Found() bool { return r.Skill != nil }

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
	Body        string // KindCustom: the instructions themselves (go into the prompt)
}

// Identifier is kind:name.
func (s Skill) Identifier() string { return s.Kind + ":" + s.Name }

// Invocation describes how the skill is invoked.
func (s Skill) Invocation() string {
	if s.Kind == KindAgent {
		return "claude --agent " + s.Name
	}
	if s.Kind == KindCustom {
		return "instructions from the dashboard inside the prompt"
	}
	return "/" + s.Name + " <url> (slash command in the prompt)"
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
	Root   string
	Names  map[string]string // action kind → skill name override (from .env or the UI); missing = Action.SkillName
	Custom map[string]string // action kind → instructions written in the dashboard; they replace the project skill
}

// WithCustom attaches dashboard-written instructions per action (see Custom).
func (r *Resolver) WithCustom(custom map[string]string) *Resolver {
	r.Custom = custom
	return r
}

// customSkill wraps dashboard instructions for an action as a Skill.
func customSkill(action Action, text string) *Skill {
	desc := strings.TrimSpace(text)
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = desc[:i]
	}
	if len(desc) > 120 {
		desc = desc[:120] + "…"
	}
	return &Skill{Kind: KindCustom, Name: action.Kind, RelPath: "data/ (настройки dashboard)", Description: desc, Body: text}
}

// New creates a resolver; preferred overrides the full-review skill name ("" = default).
func New(root, preferred string) *Resolver {
	names := map[string]string{}
	if preferred != "" {
		names[ActionReviewFull] = preferred
	}
	return &Resolver{Root: root, Names: names}
}

// NewWithNames creates a resolver with per-action skill name overrides.
func NewWithNames(root string, names map[string]string) *Resolver {
	if names == nil {
		names = map[string]string{}
	}
	return &Resolver{Root: root, Names: names}
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

// Wanted returns the skill name looked for by an action: the .env override or the default.
func (r *Resolver) Wanted(action Action) string {
	if name := strings.TrimSpace(r.Names[action.Kind]); name != "" {
		return name
	}
	return action.SkillName
}

// ForAction resolves the project skill for a run kind: exact name first, then the fallback action's skill,
// then (for the full review only) any candidate whose name/description mentions reviewing merge requests.
func (r *Resolver) ForAction(kind string) Resolution {
	action := ActionFor(kind)
	if action == nil {
		return Resolution{Action: Action{Kind: kind}}
	}
	return r.resolve(*action, r.Candidates())
}

func (r *Resolver) resolve(action Action, candidates []Skill) Resolution {
	if text := strings.TrimSpace(r.Custom[action.Kind]); text != "" {
		return Resolution{Action: action, Wanted: "custom", Skill: customSkill(action, text)}
	}
	res := Resolution{Action: action, Wanted: r.Wanted(action)}
	for i := range candidates {
		if candidates[i].Name == res.Wanted {
			res.Skill = &candidates[i]
			return res
		}
	}
	if action.Fallback != "" {
		if fallback := ActionFor(action.Fallback); fallback != nil {
			if via := r.resolve(*fallback, candidates); via.Skill != nil {
				res.Skill, res.Via = via.Skill, firstOf(via.Via, fallback.Kind)
				return res
			}
		}
	}
	if action.Kind == ActionReviewFull {
		res.Skill = heuristicReview(candidates)
	}
	return res
}

func heuristicReview(candidates []Skill) *Skill {
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

// Map resolves every dashboard action in the documented order.
func (r *Resolver) Map() []Resolution {
	candidates := r.Candidates()
	out := make([]Resolution, 0, len(Actions))
	for _, action := range Actions {
		out = append(out, r.resolve(action, candidates))
	}
	return out
}

// Resolve returns the full-review skill (nil when none). Kept for callers that only care about reviews.
func (r *Resolver) Resolve() *Skill { return r.ForAction(ActionReviewFull).Skill }

// Validate checks that the skill file exists and has usable frontmatter.
func (r *Resolver) Validate(s Skill) Validation {
	if s.Kind == KindCustom {
		if strings.TrimSpace(s.Body) == "" {
			return Validation{false, []string{"custom instructions are empty"}}
		}
		return Validation{true, nil}
	}
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
		out = append(out, Skill{Kind: kind, Name: name, RelPath: rel, Description: meta["description"], Model: meta["model"]})
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
		out = append(out, Skill{Kind: KindSkill, Name: name, RelPath: filepath.ToSlash(rel), Description: meta["description"]})
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

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
