// Package prompts holds the dashboard-specific prompt additions and JSON schemas.
//
// Project rules (CLAUDE.md, the review agent/skill) stay authoritative: Claude Code loads them
// itself because every run starts with cwd inside the project. The text here only adds what the
// UI needs: concrete MR/issue, mode, read-only or edit constraints and the result shape.
package prompts

import (
	"encoding/json"
	"fmt"
	"strings"

	"mr-review/internal/skill"
)

const findingSchema = `{
  "type": "object",
  "properties": {
    "severity": {"type": "string", "enum": ["CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO"]},
    "category": {"type": "string", "description": "short slug: bug, regression, security, error-handling, logic, style, test, other"},
    "file": {"type": "string", "description": "repository-relative path, empty if not file-specific"},
    "line": {"type": ["integer", "null"], "description": "line in the new version of the file, null if unknown"},
    "title": {"type": "string", "description": "one line, the problem itself"},
    "description": {"type": "string", "description": "markdown: short paragraphs and bullets, evidence as path/file.php:line in inline code, why it matters; not one dense paragraph"},
    "suggestion": {"type": "string", "description": "concrete fix proposal as markdown (fenced code block for code); empty if none"}
  },
  "required": ["severity", "category", "file", "line", "title", "description", "suggestion"],
  "additionalProperties": false
}`

const discussionSchema = `{
  "type": "object",
  "properties": {
    "author": {"type": "string"},
    "file": {"type": "string"},
    "line": {"type": ["integer", "null"]},
    "body": {"type": "string", "description": "the reviewer's comment, shortened if long"},
    "assessment": {"type": "string", "description": "is it addressed in the current head? what remains?"},
    "addressed": {"type": "boolean"}
  },
  "required": ["author", "file", "line", "body", "assessment", "addressed"],
  "additionalProperties": false
}`

// ReviewSchema is the structured result of a full review.
var ReviewSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 60, "description": "REQUIRED prose, 3-8 sentences: first what the MR changes and why (files, subsystems, scope), then the overall assessment. Never a placeholder or the MR title alone."},
    "verdict": {"type": "string", "enum": ["approve", "approve_with_comments", "request_changes", "blocked"], "description": "must agree with the findings: blocked = CRITICAL present, request_changes = HIGH present, approve_with_comments = only MEDIUM/LOW/INFO, approve = no findings"},
    "reviewed_sha": {"type": "string", "description": "head SHA that was actually reviewed"},
    "findings": {"type": "array", "items": ` + findingSchema + `},
    "unresolved_discussions": {"type": "array", "items": ` + discussionSchema + `}
  },
  "required": ["summary", "verdict", "reviewed_sha", "findings", "unresolved_discussions"],
  "additionalProperties": false
}`)

// VerifySchema is the structured result of a verify run.
var VerifySchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 60, "description": "REQUIRED prose, 3-8 sentences: what the MR does, what the new commits changed, and the overall assessment now. Never a placeholder."},
    "verdict": {"type": "string", "enum": ["approve", "approve_with_comments", "request_changes", "blocked"], "description": "must agree with the findings still open after this check"},
    "reviewed_sha": {"type": "string"},
    "verified": {"type": "array", "items": {
      "type": "object",
      "properties": {
        "finding_id": {"type": "integer", "description": "id of the previous finding being verified"},
        "status": {"type": "string", "enum": ["open", "fixed", "obsolete"]},
        "note": {"type": "string", "description": "evidence: what changed (or did not) at the new head"}
      },
      "required": ["finding_id", "status", "note"],
      "additionalProperties": false
    }},
    "new_findings": {"type": "array", "items": ` + findingSchema + `, "description": "only issues introduced by the newer commits"},
    "unresolved_discussions": {"type": "array", "items": ` + discussionSchema + `}
  },
  "required": ["summary", "verdict", "reviewed_sha", "verified", "new_findings", "unresolved_discussions"],
  "additionalProperties": false
}`)

// VerifyFindingSchema is the structured result of re-examining one finding.
var VerifyFindingSchema = []byte(`{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["confirmed", "false_positive", "obsolete", "unclear"], "description": "confirmed = the problem is real at the current head; false_positive = the finding is wrong; obsolete = the code changed and the concern no longer applies; unclear = cannot decide from the code alone"},
    "evidence": {"type": "string", "minLength": 40, "description": "markdown, 2-6 sentences: what exactly in the code (path:line in inline code) proves the decision; for unclear — what information is missing"},
    "severity": {"type": "string", "enum": ["", "CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO"], "description": "corrected severity if the original one is wrong, else empty"},
    "suggestion": {"type": "string", "description": "markdown: a better or more concrete fix if you have one, else empty"}
  },
  "required": ["status", "evidence", "severity", "suggestion"],
  "additionalProperties": false
}`)

// questionsSchema lets an agent stop and ask the developer instead of guessing (Phase 4 «Нужен ваш ответ»).
const questionsSchema = `"ask": {"type": "array", "items": {
      "type": "object",
      "properties": {
        "question": {"type": "string", "description": "one concrete question the developer must answer before you can continue"},
        "options": {"type": "array", "items": {"type": "string"}, "description": "suggested answers to pick from; empty when free text is needed"},
        "why": {"type": "string", "description": "what depends on the answer; what you would otherwise have to guess"}
      },
      "required": ["question", "options", "why"],
      "additionalProperties": false
    }, "description": "EMPTY unless you are blocked: when a decision is genuinely the developer's (business rule, ambiguous requirement, destructive choice), stop, fill this list and leave the other fields minimal"}`

// questionsRule is appended to prompts of runs that may stop with questions.
const questionsRule = "\nIf you cannot finish without a decision that is the developer's to make (ambiguous requirement, business rule, a choice with " +
	"different results), do NOT guess: stop and return the structured result with `ask` filled (each with `question`, `options`, `why`) and the " +
	"other fields minimal. The developer answers in the dashboard and your session continues with the answers. Otherwise leave `ask` empty. " +
	"(`ask` is for blocking decisions only; informational open questions for the author go into the result's own fields.)\n"

// Answers builds the prompt that continues a session after the developer answered the agent's questions.
func Answers(pairs []QA) string {
	var b strings.Builder
	b.WriteString("The developer answered your questions. Continue the task in this same session to completion and return the full structured result " +
		"in the requested schema (with `ask` empty unless you are blocked again).\n\n--- answers ---\n")
	for i, qa := range pairs {
		fmt.Fprintf(&b, "%d. Q: %s\n   A: %s\n", i+1, strings.TrimSpace(qa.Question), strings.TrimSpace(qa.Answer))
	}
	b.WriteString("--- end answers ---\n")
	return b.String()
}

// QA is a question with the developer's answer.
type QA struct {
	Question string
	Answer   string
}

// PlanSchema is the structured result of a read-only task analysis.
var PlanSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "markdown, 3-10 sentences in short paragraphs: what the task asks for and the proposed approach"},
    "steps": {"type": "array", "items": {"type": "string"}, "description": "ordered implementation steps, each one self-contained (markdown, inline code for identifiers)"},
    "files": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "change": {"type": "string", "description": "what changes there and why"}
    }, "required": ["path", "change"], "additionalProperties": false}},
    "risks": {"type": "array", "items": {"type": "string"}},
    "questions": {"type": "array", "items": {"type": "string"}, "description": "open questions for the author/business"},
    "estimate": {"type": "string", "description": "rough size: XS/S/M/L/XL with one sentence"},
    ` + questionsSchema + `
  },
  "required": ["summary", "steps", "files", "risks", "questions", "estimate", "ask"],
  "additionalProperties": false
}`)

// BugSchema is the structured result of a bug analysis (the «Разбор бага» mode of «Исследовать»).
var BugSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "markdown, 3-8 sentences: what is broken, where the defect lives, how to fix it"},
    "expected": {"type": "string", "description": "expected behaviour, one paragraph"},
    "actual": {"type": "string", "description": "actual behaviour as reported/observed"},
    "reproduction": {"type": "array", "items": {"type": "string"}, "description": "steps or a script to reproduce; empty if it cannot be reproduced from the code alone"},
    "path": {"type": "array", "items": {"type": "string"}, "description": "execution path: entry point → ... → the failing place, as path/file.php:line or Class::method"},
    "root_cause": {"type": "string", "description": "markdown: the defect itself, with the exact place in inline code"},
    "evidence": {"type": "array", "items": {"type": "string"}, "description": "what proves the root cause: code lines, logs, tests"},
    "fix": {"type": "string", "description": "markdown: the proposed change, files and what changes; a fenced code block where it helps"},
    "risks": {"type": "array", "items": {"type": "string"}},
    "questions": {"type": "array", "items": {"type": "string"}, "description": "open questions for the reporter"},
    "estimate": {"type": "string", "description": "rough size of the fix: XS/S/M/L with one sentence"},
    ` + questionsSchema + `
  },
  "required": ["summary", "expected", "actual", "reproduction", "path", "root_cause", "evidence", "fix", "risks", "questions", "estimate", "ask"],
  "additionalProperties": false
}`)

// PlanBug builds the read-only bug analysis prompt («Разобрать баг»).
func PlanBug(issue Issue, notes string, s *skill.Skill) string {
	return SlashPrefix(s, issue.WebURL) + "Mode: BUG ANALYSIS (read-only: find the root cause, do not change code)\n\n" + issueBlock(issue, notes) + "\n" + skillLine(s) + "\n\n" +
		"Treat the task as a bug report. Establish expected vs actual behaviour, follow the execution path through the code (you are in the project root), " +
		"find the root cause with evidence (exact files and lines, logs, existing tests), describe how to reproduce it, and propose the fix. " +
		"Do not stop at the first plausible spot: confirm the cause against the code. You may fetch more context with read-only `glab api` calls.\n" + questionsRule + "\n" + readOnlyRules()
}

// ImplementSchema is the structured report after editing in a worktree.
var ImplementSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "markdown, 3-10 sentences in short paragraphs: what was implemented and how"},
    "changes": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "description": {"type": "string"}
    }, "required": ["path", "description"], "additionalProperties": false}},
    "tests": {"type": "string", "description": "which tests/checks were run and their result; 'none' if nothing"},
    "todo": {"type": "array", "items": {"type": "string"}, "description": "what is left or needs a human decision"},
    "self_review": {"type": "string", "description": "markdown: your own review of the final diff as a strict reviewer — what you checked, what you found and fixed, what remains a concern; never empty"},
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention"},
    ` + questionsSchema + `
  },
  "required": ["summary", "changes", "tests", "todo", "self_review", "commit_message", "ask"],
  "additionalProperties": false
}`)

// Language is the language of every human-readable field the agent returns (summary, titles, descriptions,
// assessments, steps, risks, todo). Code, identifiers, file paths and commit messages stay as they are.
// Set from REPORT_LANGUAGE at startup; "ru" by default because the dashboard UI is Russian.
var Language = "ru"

var languageNames = map[string]string{"ru": "Russian", "en": "English", "de": "German", "uk": "Ukrainian", "kk": "Kazakh"}

// languageRule tells the agent which language to write the report in.
func languageRule() string {
	name := languageNames[strings.ToLower(Language)]
	if name == "" {
		name = Language
	}
	return "- LANGUAGE: write every human-readable field of the result (summary, titles, descriptions, suggestions prose, " +
		"assessments, notes, steps, risks, questions, actions, tests, todo) in " + name + ", even though these instructions are in English. " +
		"Keep code, identifiers, file paths, branch names and the commit message as they are.\n"
}

const readOnlyRulesText = `Dashboard run constraints (orchestration only, project rules stay authoritative):
- This is a READ-ONLY run triggered from a local dashboard. Do NOT modify, create or delete any file in
  the repository. Do NOT checkout, switch, reset, stash, commit, push or otherwise change the working tree
  or branches. Skip any "apply fixes locally" step of the review workflow — put the proposed fixes into
  the JSON ` + "`suggestion`" + ` fields instead.
- Obtain MR/issue metadata, diffs and discussions through ` + "`glab api`" + ` (read-only GET calls). Read-only git
  commands (git log, git diff, git show, git fetch) are allowed for context.
- Do not post comments, approvals or any other write operation to GitLab.
- The final answer must be ONLY the JSON object matching the provided schema, no prose around it.
`

const editRulesText = `Dashboard run constraints (orchestration only, project rules stay authoritative):
- You are working in a dedicated git worktree created for this task; it is safe to edit files here.
- Do NOT commit, push, checkout another branch, reset, stash or touch other worktrees. The developer
  reviews the diff in the dashboard and commits/pushes explicitly.
- Do not post anything to GitLab.
- Follow the project's coding rules, run the relevant tests/style checks if the project defines them.
- The final answer must be ONLY the JSON object matching the provided schema, no prose around it.
`

// MR is the subset of merge request data the prompts need.
type MR struct {
	WebURL, ProjectPath, Host string
	IID                       int64
	Title, SourceBranch       string
	TargetBranch, HeadSHA     string
	Context                   *MRContext // prefetched by the dashboard (nil = the agent fetches everything itself)
}

// MRContext is what the dashboard prefetched for a review so the agent skips the first round of tool calls.
type MRContext struct {
	Worktree      string       // read-only worktree at exactly HeadSHA ("" = the run works in the project root, code may differ from the MR)
	Files         []FileChange // changed files
	Diff          string       // unified diff of the MR ("" when omitted)
	DiffTruncated bool         // the diff was too large: only the file list is given
	Threads       []ThreadNote // unresolved reviewer discussions
}

// FileChange is one changed file with its change type.
type FileChange struct {
	Path   string
	Change string // A added | D deleted | R renamed (from old path) | M modified
}

// ThreadNote is an unresolved reviewer discussion for the prompt.
type ThreadNote struct {
	Author string
	File   string
	Line   int64
	Body   string
}

// contextBlock renders the prefetched context (Phase «ускорение ревью»: worktree at head + diff + discussions).
func contextBlock(c *MRContext, head string) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n--- prefetched by the dashboard (GitLab, at the head SHA above) ---\n")
	if c.Worktree != "" {
		b.WriteString("You are in a READ-ONLY git worktree checked out at exactly the head SHA (" + c.Worktree + "): read the MR's files directly with Read/Grep instead of fetching them from GitLab. Do not change anything here.\n")
	}
	fmt.Fprintf(&b, "Changed files (%d):\n", len(c.Files))
	for _, f := range c.Files {
		fmt.Fprintf(&b, "  %s %s\n", f.Change, f.Path)
	}
	if len(c.Threads) > 0 {
		fmt.Fprintf(&b, "Unresolved reviewer discussions (%d):\n", len(c.Threads))
		for _, t := range c.Threads {
			where := t.File
			if t.Line > 0 {
				where = fmt.Sprintf("%s:%d", t.File, t.Line)
			}
			fmt.Fprintf(&b, "  - @%s at %s: %s\n", t.Author, firstNonEmpty(where, "(general)"), strings.ReplaceAll(strings.TrimSpace(t.Body), "\n", " "))
		}
	} else {
		b.WriteString("Unresolved reviewer discussions: none.\n")
	}
	switch {
	case c.Diff != "":
		b.WriteString("MR diff:\n```diff\n" + c.Diff + "\n```\n")
	case c.DiffTruncated:
		b.WriteString("The diff is too large to inline; fetch it per file with `glab api` only where needed.\n")
	}
	b.WriteString("Use this instead of re-fetching the MR metadata, file list, diff or discussions; fetch more only when something is missing.\n--- end prefetched ---\n")
	return b.String()
}

// Issue is the subset of issue data the prompts need.
type Issue struct {
	WebURL, ProjectPath, Host string
	IID                       int64
	Title, Description        string
}

// PrevFinding is a previous finding passed to verify.
type PrevFinding struct {
	ID          int64  `json:"finding_id"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        *int64 `json:"line"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// skillLine tells the agent which project skill governs this run. The project skill is authoritative;
// without one the run follows CLAUDE.md / AGENTS.md plus the dashboard constraints below.
func skillLine(s *skill.Skill) string {
	switch {
	case s == nil:
		return "No dedicated project skill was detected for this action; follow the rules from the project's CLAUDE.md / AGENTS.md instructions."
	case s.Kind == skill.KindAgent:
		return fmt.Sprintf("You are running as the project's `%s` agent; follow its workflow.", s.Name)
	case s.Kind == skill.KindCustom:
		return "The developer wrote the following instructions for this action in the dashboard. Follow them together with the project's CLAUDE.md / AGENTS.md; " +
			"they never lift the read-only / workspace constraints stated below.\n--- developer instructions ---\n" + strings.TrimSpace(s.Body) + "\n--- end instructions ---"
	default:
		return fmt.Sprintf("Apply the project's `/%s` skill workflow.", s.Name)
	}
}

// SlashPrefix returns the slash-command invocation for non-agent skills (agents are selected with --agent).
func SlashPrefix(s *skill.Skill, url string) string {
	if s == nil || s.Kind == skill.KindAgent || s.Kind == skill.KindCustom {
		return ""
	}
	return "/" + s.Name + " " + url + "\n\n"
}

func mrBlock(mr MR) string {
	head := mr.HeadSHA
	if head == "" {
		head = "(unknown, use current head)"
	}
	return fmt.Sprintf("Merge request: %s\nProject path: %s (GitLab host: %s)\nMR IID: %d\nTitle: %s\nSource branch: %s -> target branch: %s\nHead SHA to review: %s\n",
		mr.WebURL, mr.ProjectPath, mr.Host, mr.IID, mr.Title, mr.SourceBranch, mr.TargetBranch, head) + contextBlock(mr.Context, head)
}

// FullReview builds the full review prompt.
func FullReview(mr MR, s *skill.Skill) string {
	return SlashPrefix(s, mr.WebURL) + "Mode: FULL REVIEW\n\n" + mrBlock(mr) + "\n" + skillLine(s) + "\n\n" +
		"Review the MR at the head SHA above. Report every real problem as a finding with a severity from CRITICAL/HIGH/MEDIUM/LOW/INFO. " +
		"Include unresolved reviewer discussions with your assessment of whether the current head addresses them. " +
		"Set `reviewed_sha` to the SHA you actually reviewed.\n" + summaryRule + readOnlyRules()
}

// Verify builds the verify prompt.
func Verify(mr MR, s *skill.Skill, previousSHA string, previous []PrevFinding) string {
	if previousSHA == "" {
		previousSHA = "(unknown)"
	}
	if previous == nil {
		previous = []PrevFinding{}
	}
	list, _ := json.MarshalIndent(previous, "", "  ")
	return SlashPrefix(s, mr.WebURL) + "Mode: VERIFY (re-check previous findings on a newer head)\n\n" + mrBlock(mr) +
		"Previously reviewed SHA: " + previousSHA + "\n\n" + skillLine(s) + "\n\nPrevious open findings (JSON):\n" + string(list) + "\n\n" +
		"For EACH previous finding decide whether at the current head it is `fixed`, still `open`, or `obsolete` (the code no longer exists / the concern no longer applies), with a short evidence note. " +
		"Compare the diff between the previously reviewed SHA and the current head where helpful. Report only genuinely new problems introduced by the newer commits in `new_findings`. " +
		"Re-assess unresolved reviewer discussions. Set `reviewed_sha` to the SHA you actually reviewed.\n" + summaryRule + readOnlyRules()
}

// summaryRule makes the summary a real description: the dashboard shows it as the headline of the review.
const summaryRule = "The `summary` MUST start with 2-5 sentences describing what the MR changes and why (subsystems, key files, scope), " +
	"then give the overall assessment. The `verdict` MUST agree with the findings (no findings = approve).\n" +
	"FORMAT: `summary`, finding `description` and `suggestion` are markdown rendered in a dashboard. Write them as short " +
	"paragraphs separated by blank lines (what the MR does / key changes as a bullet list / assessment), one idea per " +
	"paragraph, bullets for enumerations, inline code for identifiers and paths, fenced code blocks for code. Never one dense wall of text.\n\n"

func issueBlock(issue Issue, notes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue: %s\nProject path: %s (GitLab host: %s)\nIssue IID: %d\nTitle: %s\n\n--- issue description ---\n%s\n--- end description ---\n",
		issue.WebURL, issue.ProjectPath, issue.Host, issue.IID, issue.Title, strings.TrimSpace(issue.Description))
	if strings.TrimSpace(notes) != "" {
		fmt.Fprintf(&b, "\n--- additional instructions from the developer ---\n%s\n--- end instructions ---\n", strings.TrimSpace(notes))
	}
	return b.String()
}

// Plan builds the read-only task analysis prompt.
func Plan(issue Issue, notes string, s *skill.Skill) string {
	return SlashPrefix(s, issue.WebURL) + "Mode: PLAN (read-only analysis of a task)\n\n" + issueBlock(issue, notes) + "\n" + skillLine(s) + "\n\n" +
		"Study the task and the relevant code in this repository (you are in the project root). Produce a concrete implementation plan: " +
		"ordered steps, files to change with what changes, risks/regressions to watch, open questions, and a rough size estimate. " +
		"You may fetch more context from GitLab with read-only `glab api` calls (linked issues, related MRs).\n" + questionsRule + "\n" + readOnlyRules()
}

// Implement builds the editing prompt for a worktree run.
func Implement(issue Issue, notes, branch, baseBranch string, s *skill.Skill) string {
	return SlashPrefix(s, issue.WebURL) + "Mode: IMPLEMENT (edit files in a dedicated worktree)\n\n" + issueBlock(issue, notes) +
		fmt.Sprintf("\nYou are in a git worktree on branch `%s` created from `origin/%s`. Implement the task here.\n", branch, baseBranch) +
		skillLine(s) + "\n\n" +
		"Work in phases, in this order: (1) PLAN — understand the task and the code, decide the change (briefly); (2) IMPLEMENT — follow the project's rules " +
		"and conventions, keep the change focused on the task; (3) TESTS — add or update tests where the project covers the area and run the project's " +
		"style/test checks that apply to the touched files; (4) SELF-REVIEW — read the whole diff as a strict reviewer (regressions, error handling, edge " +
		"cases, naming, leftovers), fix what you find, and describe it in `self_review`; (5) REPORT — what you changed, what you ran, what is left.\n" + questionsRule + "\n" + editRules()
}

// readOnlyRules / editRules are the dashboard constraints plus the report-language rule.
func readOnlyRules() string { return readOnlyRulesText + languageRule() }
func editRules() string     { return editRulesText + languageRule() }

// CheckedFinding is one finding handed to a verify_finding run.
type CheckedFinding struct {
	ID          int64  `json:"finding_id"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	File        string `json:"file"`
	Line        *int64 `json:"line"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
}

// VerifyFinding builds the prompt that re-examines a single finding without trusting it.
func VerifyFinding(mr MR, s *skill.Skill, f CheckedFinding) string {
	body, _ := json.MarshalIndent(f, "", "  ")
	return SlashPrefix(s, mr.WebURL) + "Mode: VERIFY ONE FINDING (second opinion on a single review finding)\n\n" + mrBlock(mr) + "\n" + skillLine(s) + "\n\n" +
		"An earlier review reported the finding below. Do NOT assume it is correct. Read the code at the head SHA above (and the MR diff " +
		"where helpful) and decide: `confirmed` (the problem is real), `false_positive` (the finding is wrong), `obsolete` (the code changed, " +
		"the concern no longer applies) or `unclear` (cannot be decided from the code alone). Give concrete evidence with file paths and lines. " +
		"Stay focused on this one finding: do not review the rest of the MR.\n\nFinding (JSON):\n" + string(body) + "\n\n" + readOnlyRules()
}

// Continued prefixes the prompt of a run that the developer chose to start inside an earlier agent session
// (Phase C: "Продолжить сессию #N" instead of a new chat).
func Continued(previousKind string) string {
	return "CONTEXT: you are continuing your own earlier session on this object (your previous task there: " + previousKind + "). " +
		"Reuse what you already learned; re-read only what may have changed since then. The task below is a NEW task in that same conversation — " +
		"answer it in full with the requested structured result.\n\n"
}

// FollowUp wraps a developer question for a resumed session.
func FollowUp(question string) string {
	name := languageNames[strings.ToLower(Language)]
	if name == "" {
		name = Language
	}
	return "Follow-up question from the developer about your previous result (answer in plain text, concise, reference files/lines where useful; " +
		"this is still a READ-ONLY conversation — do not modify files). Answer in the same language as the question, " +
		"defaulting to " + name + ".\n\n" + strings.TrimSpace(question)
}

// QuickReview builds a lighter, diff-focused review prompt.
func QuickReview(mr MR, s *skill.Skill) string {
	return SlashPrefix(s, mr.WebURL) + "Mode: QUICK REVIEW (light pass)\n\n" + mrBlock(mr) + "\n" + skillLine(s) + "\n\n" +
		"Do a fast first-pass review: read the MR diff and the unresolved discussions, look only at the files the MR touches " +
		"(open surrounding code only when a change cannot be judged without it), and report CRITICAL/HIGH/MEDIUM problems only. " +
		"Skip style nits unless they violate an explicit project rule. Keep the summary to 3-5 sentences. " +
		"Set `reviewed_sha` to the SHA you actually reviewed.\n" + summaryRule + readOnlyRules()
}

// FixSchema is the structured report after addressing reviewer comments in a worktree.
var FixSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "what was changed to address the reviewers, 3-8 sentences"},
    "addressed": {"type": "array", "items": {"type": "object", "properties": {
      "author": {"type": "string"},
      "file": {"type": "string"},
      "line": {"type": ["integer", "null"]},
      "comment": {"type": "string", "description": "the reviewer's request, shortened"},
      "action": {"type": "string", "description": "what was done, or why it was intentionally not done"},
      "done": {"type": "boolean"}
    }, "required": ["author", "file", "line", "comment", "action", "done"], "additionalProperties": false}},
    "changes": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "description": {"type": "string"}
    }, "required": ["path", "description"], "additionalProperties": false}},
    "tests": {"type": "string", "description": "which tests/checks were run and their result; 'none' if nothing"},
    "todo": {"type": "array", "items": {"type": "string"}, "description": "comments that need a human decision"},
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention"},
    ` + questionsSchema + `
  },
  "required": ["summary", "addressed", "changes", "tests", "todo", "commit_message", "ask"],
  "additionalProperties": false
}`)

// FailedJob is a CI job handed to the CI prompts.
type FailedJob struct {
	Name          string
	Stage         string
	FailureReason string
	AllowFailure  bool
	WebURL        string
	Trace         string // tail of the log
}

func jobsBlock(jobs []FailedJob) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Failed jobs of the head pipeline (%d):\n", len(jobs))
	for _, j := range jobs {
		allow := ""
		if j.AllowFailure {
			allow = ", allowed to fail"
		}
		reason := j.FailureReason
		if reason == "" {
			reason = "unknown"
		}
		fmt.Fprintf(&b, "\n=== job %q (stage %s, reason %s%s) %s ===\n%s\n=== end of log tail ===\n", j.Name, j.Stage, reason, allow, j.WebURL, strings.TrimSpace(j.Trace))
	}
	return b.String()
}

// CIAnalyzeSchema is the structured result of «Разобрать ошибки CI».
var CIAnalyzeSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 40, "description": "markdown, 3-6 sentences: why the pipeline failed and what to do"},
    "jobs": {"type": "array", "items": {"type": "object", "properties": {
      "name": {"type": "string"},
      "kind": {"type": "string", "enum": ["code", "test", "flaky", "infrastructure", "config", "unknown"], "description": "code = the MR's change is wrong; test = a test needs updating; flaky = unrelated/intermittent; infrastructure/config = CI environment"},
      "cause": {"type": "string", "description": "markdown: the concrete cause with evidence from the log and the code (path:line)"},
      "fix": {"type": "string", "description": "markdown: what to change to make the job pass; empty if nothing in the MR can fix it"},
      "fixable_in_mr": {"type": "boolean"}
    }, "required": ["name", "kind", "cause", "fix", "fixable_in_mr"], "additionalProperties": false}},
    "fixable_in_mr": {"type": "boolean", "description": "true when at least one failure can be fixed by changing the MR branch"}
  },
  "required": ["summary", "jobs", "fixable_in_mr"],
  "additionalProperties": false
}`)

// CIAnalyze builds the read-only prompt that explains a failed pipeline.
func CIAnalyze(mr MR, jobs []FailedJob, s *skill.Skill) string {
	return SlashPrefix(s, mr.WebURL) + "Mode: CI ANALYSIS (read-only: explain why the head pipeline failed)\n\n" + mrBlock(mr) + "\n" + skillLine(s) + "\n\n" + jobsBlock(jobs) + "\n" +
		"For every failed job find the concrete cause: relate the log to the MR diff (`glab api` for the diff, read the code in this repository at the head SHA). " +
		"Classify it (code / test / flaky / infrastructure / config), give evidence, and say whether a change in the MR branch would fix it and what that change is. " +
		"Do not guess when the log is inconclusive: say so.\n" + summaryRuleShort + readOnlyRules()
}

const summaryRuleShort = "FORMAT: markdown fields are rendered in a dashboard; short paragraphs, bullets, inline code for identifiers and paths.\n\n"

// CIFixSchema is the report of «Исправить CI» (an edit run).
var CIFixSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "markdown, 3-8 sentences: what made the pipeline fail and what was changed"},
    "changes": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "description": {"type": "string"}
    }, "required": ["path", "description"], "additionalProperties": false}},
    "tests": {"type": "string", "description": "which tests/checks were run locally and their result; 'none' with the reason if nothing"},
    "todo": {"type": "array", "items": {"type": "string"}, "description": "what is left or needs a human decision"},
    "self_review": {"type": "string", "description": "markdown: review of the final diff as a strict reviewer"},
    "unfixable": {"type": "array", "items": {"type": "object", "properties": {
      "job": {"type": "string"}, "reason": {"type": "string", "description": "why this failure cannot be fixed in the MR (flaky, infrastructure, needs another team)"}
    }, "required": ["job", "reason"], "additionalProperties": false}},
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention; empty when nothing changed"},
    ` + questionsSchema + `
  },
  "required": ["summary", "changes", "tests", "todo", "self_review", "unfixable", "commit_message", "ask"],
  "additionalProperties": false
}`)

// CIFix builds the editing prompt that fixes a failed pipeline in the MR worktree. analysis is the summary of an
// earlier CI analysis run ("" when none).
func CIFix(mr MR, jobs []FailedJob, analysis string, s *skill.Skill) string {
	prior := ""
	if strings.TrimSpace(analysis) != "" {
		prior = "\n--- earlier analysis (verify it against the code before acting on it) ---\n" + strings.TrimSpace(analysis) + "\n--- end analysis ---\n"
	}
	return SlashPrefix(s, mr.WebURL) + "Mode: CI FIX (edit files in a dedicated worktree of the MR branch)\n\n" + mrBlock(mr) + prior + "\n" + skillLine(s) + "\n\n" + jobsBlock(jobs) + "\n" +
		fmt.Sprintf("You are in a git worktree checked out on the MR source branch `%s`. ", mr.SourceBranch) +
		"For each failed job: find the cause in the code (relate the log to the diff), fix it following the project's rules, and run the tests/checks that the job runs " +
		"as far as they can run locally. Fix only what the failures require: no unrelated refactoring. If a failure cannot be fixed in this MR (flaky test, " +
		"infrastructure, another team's code), change nothing for it and explain why in `unfixable`. Finish with a strict self-review of the diff.\n" + questionsRule + "\n" + editRules()
}

// StandSchema is the structured report after deploying and exercising the MR on the developer's stand.
var StandSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 60, "description": "markdown, 3-8 sentences: what was deployed, what was run on the stand, overall outcome"},
    "deployed": {"type": "array", "items": {"type": "string"}, "description": "repository-relative paths synced to the stand"},
    "tests": {"type": "string", "description": "markdown: which tests/checks ran on the stand and their result; 'none' with the reason if nothing could run"},
    "script_path": {"type": "string", "description": "repository-relative path of the emulation script written in the worktree; empty if none"},
    "script_output": {"type": "string", "description": "trimmed output of the emulation script on the stand (last ~60 lines); empty if not run"},
    "problems": {"type": "array", "items": {"type": "object", "properties": {
      "severity": {"type": "string", "enum": ["CRITICAL", "HIGH", "MEDIUM", "LOW", "INFO"]},
      "title": {"type": "string"},
      "description": {"type": "string", "description": "markdown with evidence from the stand (output, log lines, file:line)"}
    }, "required": ["severity", "title", "description"], "additionalProperties": false}, "description": "problems observed while running the MR on the stand"},
    "changes": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "description": {"type": "string"}
    }, "required": ["path", "description"], "additionalProperties": false}, "description": "files created or changed in the worktree (the script, fixtures)"},
    "todo": {"type": "array", "items": {"type": "string"}, "description": "what needs a human: stand access problems, migrations not run, data that had to be faked"},
    "commit_message": {"type": "string", "description": "suggested commit message if the script is worth keeping; empty otherwise"},
    ` + questionsSchema + `
  },
  "required": ["summary", "deployed", "tests", "script_path", "script_output", "problems", "changes", "todo", "commit_message", "ask"],
  "additionalProperties": false
}`)

// StandTest builds the prompt that deploys the MR to the developer's stand and exercises it there. standSkill is the
// project skill that explains how to reach the stand; s is the optional project skill for the whole scenario.
func StandTest(mr MR, notes, targetBranch string, standSkill *skill.Skill, s *skill.Skill) string {
	var extra string
	if strings.TrimSpace(notes) != "" {
		extra = "\n--- additional instructions from the developer ---\n" + strings.TrimSpace(notes) + "\n--- end instructions ---\n"
	}
	access := "The project has no dedicated stand-access skill: use the stand access described in CLAUDE.md / AGENTS.md; if there is none, stop and report it in `todo`."
	if standSkill != nil {
		access = fmt.Sprintf("Stand access: the project skill `%s` (%s) describes the stand and the only allowed way to talk to it (wrapper script, hosts, paths). "+
			"Read it first and use exactly its commands for every push / run / pull on the stand.", standSkill.Name, standSkill.RelPath)
	}
	return SlashPrefix(s, mr.WebURL) + "Mode: STAND TEST (deploy the MR to the developer's stand and exercise it there; edits only in this worktree)\n\n" + mrBlock(mr) + extra +
		"\n" + skillLine(s) + "\n" + access + "\n\n" +
		fmt.Sprintf("You are in a git worktree checked out on the MR source branch `%s` (target `%s`). Steps:\n", mr.SourceBranch, targetBranch) +
		fmt.Sprintf("1. List the files the MR changes: `git diff --name-status origin/%s...HEAD`.\n", targetBranch) +
		"2. Sync exactly those files to the stand with the stand skill's push mechanism (paths are identical on the stand). Never run `git push`, `git pull`, checkout or reset on the stand; never run DB migrations or touch Redis/DB data unless the developer's instructions explicitly ask — list what was skipped in `todo`.\n" +
		"3. On the stand, run the project's tests/checks that cover the touched code (unit tests of the touched classes, linters the project uses). Record the outcome in `tests`.\n" +
		"4. Write an emulation script in this worktree, in the place the project uses for one-off scripts (look at the existing conventions), named after the MR. It must exercise the functionality the MR adds or changes end-to-end while mocking or stubbing external systems (HTTP APIs, queues, mail, payment providers) so it runs on the stand without side effects. Push it to the stand, run it there, capture the output in `script_output` and the path in `script_path`.\n" +
		"5. Report problems you observed (errors, wrong behaviour, missing config) in `problems` with evidence, and everything that needs a human in `todo`.\n" +
		"Do not modify the project code itself in this run — only add the script and its fixtures. Leave the stand consistent: if you had to change stand-only config, revert it.\n" + questionsRule + "\n" + editRules()
}

// SelectedDiscussion is a reviewer discussion chosen for fixing.
type SelectedDiscussion struct {
	ID     int64  `json:"discussion_id"`
	Author string `json:"author"`
	File   string `json:"file"`
	Line   *int64 `json:"line"`
	Body   string `json:"body"`
}

// FixFindingsSchema is the report of «Исправить выбранные».
var FixFindingsSchema = []byte(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "markdown, 3-8 sentences: what was confirmed and fixed, what was not"},
    "results": {"type": "array", "items": {"type": "object", "properties": {
      "finding_id": {"type": "integer"},
      "status": {"type": "string", "enum": ["fixed", "not_confirmed", "skipped"], "description": "fixed = confirmed and changed; not_confirmed = the finding is wrong or already handled, nothing changed; skipped = needs a human decision"},
      "note": {"type": "string", "description": "markdown: what was changed or why not, with path:line"}
    }, "required": ["finding_id", "status", "note"], "additionalProperties": false}},
    "addressed": {"type": "array", "items": {"type": "object", "properties": {
      "author": {"type": "string"},
      "file": {"type": "string"},
      "line": {"type": ["integer", "null"]},
      "comment": {"type": "string", "description": "the reviewer's request, shortened"},
      "action": {"type": "string", "description": "what was done, or why it was intentionally not done"},
      "done": {"type": "boolean"}
    }, "required": ["author", "file", "line", "comment", "action", "done"], "additionalProperties": false}, "description": "one entry per selected reviewer discussion"},
    "changes": {"type": "array", "items": {"type": "object", "properties": {
      "path": {"type": "string"}, "description": {"type": "string"}
    }, "required": ["path", "description"], "additionalProperties": false}},
    "tests": {"type": "string", "description": "which tests/checks were run and their result; 'none' with the reason if nothing"},
    "todo": {"type": "array", "items": {"type": "string"}, "description": "what needs a human decision"},
    "self_review": {"type": "string", "description": "markdown: review of the final diff as a strict reviewer"},
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention; empty when nothing changed"},
    ` + questionsSchema + `
  },
  "required": ["summary", "results", "addressed", "changes", "tests", "todo", "self_review", "commit_message", "ask"],
  "additionalProperties": false
}`)

// FixFindings builds the editing prompt that addresses the selected findings and discussions in the MR worktree.
func FixFindings(mr MR, notes string, findings []CheckedFinding, discussions []SelectedDiscussion, s *skill.Skill) string {
	var extra string
	if strings.TrimSpace(notes) != "" {
		extra = "\n--- additional instructions from the developer ---\n" + strings.TrimSpace(notes) + "\n--- end instructions ---\n"
	}
	fj, _ := json.MarshalIndent(findings, "", "  ")
	dj, _ := json.MarshalIndent(discussions, "", "  ")
	if findings == nil {
		fj = []byte("[]")
	}
	if discussions == nil {
		dj = []byte("[]")
	}
	return SlashPrefix(s, mr.WebURL) + "Mode: FIX SELECTED FINDINGS (edit files in a dedicated worktree of the MR branch)\n\n" + mrBlock(mr) + extra + "\n" + skillLine(s) + "\n" +
		fmt.Sprintf("\nYou are in a git worktree checked out on the MR source branch `%s`. The developer selected what to address.\n\n", mr.SourceBranch) +
		"Selected AI-review findings (JSON):\n" + string(fj) + "\n\nSelected unresolved reviewer discussions (JSON):\n" + string(dj) + "\n\n" +
		"For EACH selected finding: first verify it against the current code — do not assume the review was right. Fix only the confirmed ones following the project's rules; " +
		"report `not_confirmed` (with why) when the finding is wrong or already handled, `skipped` when it needs a human decision. Address each selected reviewer discussion the same way " +
		"(one `addressed` entry per discussion). Do not touch anything that was not selected. Run the project's style/test checks for the touched files, then review the whole diff " +
		"as a strict reviewer. Do NOT reply to or resolve discussions in GitLab.\n" + questionsRule + "\n" + editRules()
}

// FixComments builds the prompt for addressing unresolved reviewer discussions in a worktree of the MR branch.
func FixComments(mr MR, notes string, s *skill.Skill) string {
	var extra string
	if strings.TrimSpace(notes) != "" {
		extra = "\n--- additional instructions from the developer ---\n" + strings.TrimSpace(notes) + "\n--- end instructions ---\n"
	}
	return SlashPrefix(s, mr.WebURL) + "Mode: FIX REVIEW COMMENTS (edit files in a dedicated worktree of the MR branch)\n\n" + mrBlock(mr) + extra +
		"\n" + skillLine(s) + "\n" +
		fmt.Sprintf("\nYou are in a git worktree checked out on the MR source branch `%s`. ", mr.SourceBranch) +
		"Fetch the unresolved discussions of this MR with `glab api` (discussions where notes[0].resolvable == true and resolved == false), " +
		"read each reviewer's request in the context of the current code, and change the code to address it, following the project's rules. " +
		"If a comment is a question or needs a business decision, do not guess: leave it in `todo` with your recommendation. " +
		"Run the project's style/test checks that apply to the touched files. Do NOT reply to or resolve the discussions in GitLab — the developer does that after reviewing the diff.\n" + questionsRule + "\n" + editRules()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
