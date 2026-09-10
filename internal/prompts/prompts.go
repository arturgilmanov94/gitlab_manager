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
    "estimate": {"type": "string", "description": "rough size: XS/S/M/L/XL with one sentence"}
  },
  "required": ["summary", "steps", "files", "risks", "questions", "estimate"],
  "additionalProperties": false
}`)

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
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention"}
  },
  "required": ["summary", "changes", "tests", "todo", "commit_message"],
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
		mr.WebURL, mr.ProjectPath, mr.Host, mr.IID, mr.Title, mr.SourceBranch, mr.TargetBranch, head)
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
		"You may fetch more context from GitLab with read-only `glab api` calls (linked issues, related MRs).\n\n" + readOnlyRules()
}

// Implement builds the editing prompt for a worktree run.
func Implement(issue Issue, notes, branch, baseBranch string, s *skill.Skill) string {
	return SlashPrefix(s, issue.WebURL) + "Mode: IMPLEMENT (edit files in a dedicated worktree)\n\n" + issueBlock(issue, notes) +
		fmt.Sprintf("\nYou are in a git worktree on branch `%s` created from `origin/%s`. Implement the task here.\n", branch, baseBranch) +
		skillLine(s) + "\n\n" +
		"Follow the project's rules and conventions, keep the change focused on the task, add or update tests where the project covers the area, " +
		"and run the project's style/test checks that apply to the touched files. Report what you changed, what you ran, and what is left.\n\n" + editRules()
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
    "commit_message": {"type": "string", "description": "suggested commit message following the project convention"}
  },
  "required": ["summary", "addressed", "changes", "tests", "todo", "commit_message"],
  "additionalProperties": false
}`)

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
		"Run the project's style/test checks that apply to the touched files. Do NOT reply to or resolve the discussions in GitLab — the developer does that after reviewing the diff.\n\n" + editRules()
}
