package runner

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Shell commands a read-only run may execute without asking the developer: they only read the repository or
// GitLab. ReadOnlyCommand decides whether a whole command line consists of them.

// readOnlyGit are the git subcommands that never change the repository, the index or the working tree.
var readOnlyGit = map[string]bool{
	"log": true, "show": true, "diff": true, "blame": true, "merge-base": true, "rev-parse": true,
	"status": true, "grep": true, "ls-files": true, "cat-file": true, "rev-list": true, "shortlog": true,
	"ls-tree": true, "describe": true, "name-rev": true, "show-ref": true, "for-each-ref": true,
	// fetch only updates remote-tracking refs and objects; a review needs it for `git merge-base HEAD origin/develop`.
	"fetch": true,
}

// readOnlyTools are plain read-only programs (dangerous options are refused in safeArgs).
var readOnlyTools = map[string]bool{
	"rg": true, "grep": true, "cat": true, "head": true, "tail": true, "ls": true, "find": true, "wc": true,
	"jq": true, "sort": true, "cd": true, "pwd": true, "echo": true, "sed": true, "cut": true, "tr": true,
	"nl": true, "uniq": true, "diff": true, "stat": true, "basename": true, "dirname": true, "realpath": true,
	"true": true,
}

// ReadOnlyCommand reports whether every part of a shell command line (split on |, &&, ||, ;) is a read-only
// command from the list above. Command substitution, background jobs, environment prefixes and output
// redirections (except to /dev/null or between stdout/stderr) make the whole line not read-only.
func ReadOnlyCommand(line string) bool {
	parts, ok := splitShell(line)
	if !ok || len(parts) == 0 {
		return false
	}
	for _, words := range parts {
		if !readOnlyPart(words) {
			return false
		}
	}
	return true
}

func readOnlyPart(words []string) bool {
	if len(words) == 0 {
		return false
	}
	name := words[0]
	if strings.Contains(name, "=") { // VAR=value cmd: the variable may change what the program runs (GIT_*, PAGER)
		return false
	}
	name = filepath.Base(name)
	args := words[1:]
	switch {
	case name == "git":
		return readOnlyGitArgs(args)
	case name == "glab":
		return readOnlyGlab(args)
	case readOnlyTools[name]:
		return safeArgs(name, args)
	}
	return false
}

// readOnlyGitArgs accepts `git [--no-pager] [-C dir] <read-only subcommand> ...` without options that write files
// or run external programs.
func readOnlyGitArgs(args []string) bool {
	for len(args) > 0 {
		switch {
		case args[0] == "--no-pager":
			args = args[1:]
		case args[0] == "-C" && len(args) > 1:
			args = args[2:]
		default:
			goto sub
		}
	}
sub:
	if len(args) == 0 || !readOnlyGit[args[0]] {
		return false
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--output") || a == "--ext-diff" || strings.HasPrefix(a, "--open-files-in-pager") || a == "-O" {
			return false
		}
		// fetch: options that run another program or touch submodules / other repositories.
		if args[0] == "fetch" && (strings.HasPrefix(a, "--upload-pack") || a == "-u" || strings.HasPrefix(a, "--recurse-submodules") ||
			a == "--multiple" || a == "--all" || strings.HasPrefix(a, "--write-") || a == "--update-head-ok" || strings.HasPrefix(a, "--refmap")) {
			return false
		}
	}
	return true
}

// readOnlyGlab accepts `glab api <path>` GET requests only: an explicit other method or any request body
// (fields, --input) makes it a write.
func readOnlyGlab(args []string) bool {
	if len(args) == 0 || args[0] != "api" {
		return false
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 >= len(args) || !strings.EqualFold(args[i+1], "GET") {
				return false
			}
			i++
		case strings.HasPrefix(a, "-X") && len(a) > 2:
			if !strings.EqualFold(strings.TrimPrefix(a, "-X"), "GET") {
				return false
			}
		case strings.HasPrefix(a, "--method="):
			if !strings.EqualFold(strings.TrimPrefix(a, "--method="), "GET") {
				return false
			}
		case a == "-f" || a == "-F" || strings.HasPrefix(a, "--field") || strings.HasPrefix(a, "--raw-field") || strings.HasPrefix(a, "--input"):
			return false
		}
	}
	return true
}

// safeArgs refuses the options of read-only tools that write files or run other programs.
func safeArgs(name string, args []string) bool {
	for _, a := range args {
		switch name {
		case "find":
			if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || a == "-delete" || strings.HasPrefix(a, "-fprint") || a == "-fls" {
				return false
			}
		case "rg":
			if strings.HasPrefix(a, "--pre") {
				return false
			}
		case "sort":
			if a == "-o" || strings.HasPrefix(a, "--output") || (strings.HasPrefix(a, "-o") && !strings.HasPrefix(a, "--")) {
				return false
			}
		}
	}
	switch name {
	case "sed":
		return safeSed(args)
	case "uniq": // `uniq in out` writes out
		return len(positional(args)) <= 1
	}
	return true
}

func positional(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			out = append(out, a)
		}
	}
	return out
}

// safeSed accepts sed without in-place editing whose script only prints, deletes, quits or substitutes
// (`sed -n 1,200p f`, `sed -n '/re/,/re/p'`, `sed 's/a/b/g'`): commands that write files (w, W, s///w),
// read them (r, R) or run programs (e, s///e) make it not read-only.
func safeSed(args []string) bool {
	var scripts []string
	haveScript := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--quiet" || a == "--silent" || a == "-E" || a == "-r" || a == "--regexp-extended" ||
			a == "-s" || a == "--separate" || a == "-u" || a == "--unbuffered" || a == "-z" || a == "--null-data" ||
			a == "--posix" || a == "--debug" || a == "--sandbox":
		case a == "-e" || a == "--expression":
			if i+1 >= len(args) {
				return false
			}
			scripts = append(scripts, args[i+1])
			haveScript = true
			i++
		case strings.HasPrefix(a, "--expression="):
			scripts = append(scripts, strings.TrimPrefix(a, "--expression="))
			haveScript = true
		case strings.HasPrefix(a, "-") && a != "-":
			// -i/--in-place, -f script files, -l and combined flags like -ni: not accepted.
			if strings.Trim(a[1:], "nEsur") != "" || strings.HasPrefix(a, "--") {
				return false
			}
		case !haveScript:
			scripts = append(scripts, a)
			haveScript = true
		}
	}
	if !haveScript {
		return false
	}
	for _, sc := range scripts {
		if !safeSedScript(sc) {
			return false
		}
	}
	return true
}

// safeSedScript checks every command of a sed script (separated by ; or newlines): an optional address or range
// (line numbers, $, /regex/), an optional !, then one of p P d D q Q = n N or s/re/repl/flags with flags from
// g p i I m M and digits.
func safeSedScript(sc string) bool {
	rs := []rune(sc)
	i := 0
	skipSpace := func() {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
			i++
		}
	}
	// delimited reads up to the closing delimiter d (backslash escapes it); false if there is none.
	delimited := func(d rune) bool {
		for i < len(rs) && rs[i] != d {
			if rs[i] == '\\' {
				i++
				if i >= len(rs) {
					return false
				}
			}
			if rs[i] == '\n' {
				return false
			}
			i++
		}
		if i >= len(rs) {
			return false
		}
		i++ // the delimiter
		return true
	}
	address := func() bool {
		switch {
		case i < len(rs) && rs[i] == '$':
			i++
		case i < len(rs) && rs[i] >= '0' && rs[i] <= '9':
			for i < len(rs) && (rs[i] >= '0' && rs[i] <= '9' || rs[i] == '~') {
				i++
			}
		case i < len(rs) && rs[i] == '/':
			i++
			if !delimited('/') {
				return false
			}
			for i < len(rs) && (rs[i] == 'I' || rs[i] == 'M') {
				i++
			}
		}
		return true
	}
	for {
		skipSpace()
		for i < len(rs) && (rs[i] == ';' || rs[i] == '\n') {
			i++
			skipSpace()
		}
		if i >= len(rs) {
			return true
		}
		if !address() {
			return false
		}
		if i < len(rs) && rs[i] == ',' {
			i++
			if i < len(rs) && rs[i] == '+' {
				i++
			}
			if !address() {
				return false
			}
		}
		skipSpace()
		if i < len(rs) && rs[i] == '!' {
			i++
			skipSpace()
		}
		if i >= len(rs) {
			return false
		}
		cmd := rs[i]
		i++
		switch {
		case strings.ContainsRune("pPdDqQ=nN", cmd):
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' && (cmd == 'q' || cmd == 'Q') {
				i++
			}
		case cmd == 's':
			if i >= len(rs) || rs[i] == '\\' || rs[i] == '\n' {
				return false
			}
			d := rs[i]
			i++
			if !delimited(d) || !delimited(d) {
				return false
			}
			for i < len(rs) && strings.ContainsRune("gpiImM0123456789", rs[i]) {
				i++
			}
		default:
			return false
		}
		skipSpace()
		if i < len(rs) && rs[i] != ';' && rs[i] != '\n' && rs[i] != '}' {
			return false
		}
		if i < len(rs) && rs[i] == '}' {
			return false
		}
	}
}

// splitShell splits a command line into simple commands (words with quotes removed). ok is false for anything
// it does not understand or that can run or write more than it shows: $(...), backticks, <( ), a lone &,
// redirections other than to /dev/null or between stdout and stderr, here-docs, unbalanced quotes.
func splitShell(line string) (parts [][]string, ok bool) {
	var words []string
	var word strings.Builder
	inWord := false
	dangling := false // the last operator (|, &&, ||) still needs a command after it
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	flushPart := func() bool {
		flushWord()
		if len(words) == 0 {
			return false
		}
		parts = append(parts, words)
		words = nil
		return true
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			word.WriteString(string(rs[i+1 : j]))
			inWord, i = true, j
		case c == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				} else if rs[j] == '`' || (rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(') {
					return nil, false
				}
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			word.WriteString(strings.ReplaceAll(string(rs[i+1:j]), `\"`, `"`))
			inWord, i = true, j
		case c == '\\' && i+1 < len(rs):
			word.WriteRune(rs[i+1])
			inWord, i = true, i+1
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			// $(git merge-base ...) / $(git rev-parse ...) print only an object name; anything else is refused.
			j := i + 2
			for j < len(rs) && rs[j] != ')' {
				if strings.ContainsRune("()`$'\"\\<>&|;\n", rs[j]) {
					return nil, false
				}
				j++
			}
			if j >= len(rs) || !shaSubstitution(string(rs[i+2:j])) {
				return nil, false
			}
			word.WriteString("0000000000000000000000000000000000000000")
			inWord, i = true, j
		case c == '`':
			return nil, false
		case c == '<':
			return nil, false // input redirection, here-docs and <( ) process substitution
		case c == '>':
			// Allowed: >/dev/null, 2>/dev/null, 2>&1, >&2 (the digit before > is part of the current word).
			target, n := redirectTarget(rs[i+1:])
			if target != "/dev/null" && target != "&1" && target != "&2" {
				return nil, false
			}
			if inWord && isFD(word.String()) {
				word.Reset()
				inWord = false
			}
			i += n
		case c == '|' || c == ';' || c == '\n':
			if c == '|' && i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			if !flushPart() {
				return nil, false
			}
			dangling = c == '|' // a trailing ";" is fine, a trailing "|" or "||" is not
		case c == '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				if !flushPart() {
					return nil, false
				}
				dangling = true
			} else {
				return nil, false // background job
			}
		case c == ' ' || c == '\t':
			flushWord()
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	flushWord()
	if len(words) > 0 {
		parts = append(parts, words)
	}
	if dangling && len(words) == 0 {
		return nil, false // "a &&" or "a |" with nothing after
	}
	return parts, true
}

// shaSubstitution reports whether a command substitution only prints object names: `git merge-base` or
// `git rev-parse` with revisions and the few options that do not change what kind of text is printed.
func shaSubstitution(inner string) bool {
	words := strings.Fields(inner)
	if len(words) < 3 || words[0] != "git" || (words[1] != "merge-base" && words[1] != "rev-parse") {
		return false
	}
	for _, w := range words[2:] {
		switch w {
		case "--verify", "-q", "--quiet", "--short", "--fork-point":
			continue
		}
		if strings.HasPrefix(w, "-") || !isSubcommand(strings.NewReplacer("~", "", "^", "", "@", "", "{", "", "}", "").Replace(w)) {
			return false
		}
	}
	return true
}

// redirectTarget reads the target after '>' (">>" counts as a redirection too); n is how many runes it took.
func redirectTarget(rs []rune) (string, int) {
	n := 0
	if n < len(rs) && rs[n] == '>' {
		n++
	}
	for n < len(rs) && rs[n] == ' ' {
		n++
	}
	start := n
	if n < len(rs) && rs[n] == '&' {
		n++
	}
	for n < len(rs) && !strings.ContainsRune(" \t|;&<>\n", rs[n]) {
		n++
	}
	return string(rs[start:n]), n
}

func isFD(s string) bool { return s == "1" || s == "2" }

// SimilarKey names the kind of a tool call for «allow such calls until the end of the run»: the program and its
// subcommand for Bash (`git fetch`, `php bin/console`), the tool name for everything else.
func SimilarKey(tool string, input json.RawMessage) string {
	if tool != "Bash" {
		return tool
	}
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	parts, ok := splitShell(in.Command)
	if !ok || len(parts) != 1 {
		return "" // compound or unparsable commands are approved one by one
	}
	words := parts[0]
	key := words[0]
	if len(words) > 1 && isSubcommand(words[1]) {
		key += " " + words[1]
	}
	return "Bash: " + key
}

func isSubcommand(w string) bool {
	if w == "" || strings.HasPrefix(w, "-") {
		return false
	}
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':' || r == '/' || r == '.') {
			return false
		}
	}
	return true
}

// MarshalPlain encodes v as JSON without HTML escaping: commands keep their && and > for display.
func MarshalPlain(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
