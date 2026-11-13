package runner

import (
	"encoding/json"
	"testing"
)

func TestReadOnlyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"git log --oneline -20", true},
		{"git -C /repo --no-pager show HEAD:src/A.php", true},
		{"git diff origin/develop...HEAD -- src/", true},
		{"git blame -L 10,20 src/A.php", true},
		{"git merge-base HEAD origin/develop && git rev-parse HEAD", true},
		{"git status --short; git grep -n foo", true},
		{"rg -n 'a|b' src | head -50", true},
		{`grep -rn "x && y" src | wc -l`, true},
		{"cat a.txt | sort | head", true},
		{"find . -name '*.php' -newer x", true},
		{"ls -la src 2>/dev/null", true},
		{"git log 2>&1 | tail -5", true},
		{"glab api projects/1/merge_requests/2/discussions | jq '.[] | .id'", true},
		{"glab api -X GET projects/1", true},
		{"cd src && ls", true},
		{"sed -n 1,200p .claude/skills/tn-review/SKILL.md | head -150; git log --oneline -3 HEAD; git merge-base HEAD origin/develop", true},
		{"git log --oneline -1 && git fetch -q origin develop 2>&1 | tail -2; git merge-base HEAD origin/develop && git diff --stat $(git merge-base HEAD origin/develop) HEAD | tail -35", true},
		{"git diff $(git rev-parse --short HEAD~3) HEAD -- src/", true},
		{"sed -n '/^## Stage/,/^## /p' SKILL.md", true},
		{"sed -e 's/foo/bar/g' -e '10q' a.txt", true},
		{"sed -nE '1~2p;$p' a", true},
		{"cut -d: -f1 a | sort | uniq -c | sort -rn", true},
		{"git ls-tree -r HEAD --name-only | nl", true},
		{"diff a.txt b.txt", true},

		{"git push origin HEAD", false},
		{"git commit -m x", false},
		{"git checkout develop", false},
		{"git -c core.pager=sh log", false},
		{"git diff --output=/tmp/x", false},
		{"rm -rf src", false},
		{"git log > /tmp/log.txt", false},
		{"git log >> out", false},
		{"cat a > b", false},
		{"curl -X POST https://example.com", false},
		{"curl https://example.com", false},
		{"glab api -X POST projects/1/notes", false},
		{"glab api --method PUT projects/1", false},
		{"glab api projects/1/notes -f body=hi", false},
		{"glab mr approve 1", false},
		{"find . -name x -delete", false},
		{"find . -exec rm {} ;", false},
		{"rg --pre ./evil x", false},
		{"sort -o out in", false},
		{"cat $(echo /etc/passwd)", false},
		{"cat `whoami`", false},
		{`echo "$(rm -rf x)"`, false},
		{"sleep 1 &", false},
		{"git log && rm x", false},
		{"git log | sh", false},
		{"PAGER=evil git log", false},
		{"cat < /etc/passwd", false},
		{"diff <(git show a) <(git show b)", false},
		{"echo 'unbalanced", false},
		{"", false},
		{"git log &&", false},
		{"sed -i s/a/b/ f", false},
		{"sed -ni p f", false},
		{"sed --in-place=.bak s/a/b/ f", false},
		{"sed 's/a/b/w out' f", false},
		{"sed 's/a/b/e' f", false},
		{"sed '1w out' f", false},
		{"sed '1e rm -rf x' f", false},
		{"sed 'r /etc/passwd' f", false},
		{"sed -f evil.sed f", false},
		{"sed '1{w out' f", false},
		{"sed", false},
		{"uniq in out", false},
		{"git fetch --upload-pack=evil origin", false},
		{"git diff $(cat x)", false},
		{"git diff $(git rev-parse --output=x HEAD)", false},
		{"git diff $(git merge-base HEAD $(rm x))", false},
		{"git diff $(git log -1 --format=%H; rm x)", false},
	}
	for _, c := range cases {
		if got := ReadOnlyCommand(c.cmd); got != c.want {
			t.Errorf("%q: got %v want %v", c.cmd, got, c.want)
		}
	}
}

func TestSimilarKeyAndPlainJSON(t *testing.T) {
	for cmd, want := range map[string]string{
		"git fetch origin develop":    "Bash: git fetch",
		"php bin/console cache:clear": "Bash: php bin/console",
		"rm -rf x":                    "Bash: rm",
		"git log && rm x":             "",
	} {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		if got := SimilarKey("Bash", input); got != want {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
	if SimilarKey("Write", nil) != "Write" {
		t.Fatal("non-Bash tools are keyed by name")
	}
	if got := string(MarshalPlain(map[string]string{"command": "a && b > c"})); got != `{"command":"a && b > c"}` {
		t.Fatal(got)
	}
}
