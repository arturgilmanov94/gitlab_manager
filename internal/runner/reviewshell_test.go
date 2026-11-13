package runner

import "testing"

func TestReviewShellCommand(t *testing.T) {
	allowed := []string{
		// Real prompts from review runs.
		`git fetch -q origin develop 2>&1 | tail -2; git merge-base origin/develop 0283e26b3628e68a4fde40f1031ca15e8c5b9477; git diff origin/develop...0283e26b3628e68a4fde40f1031ca15e8c5b9477 > $TMPDIR/118196.diff; wc -l $TMPDIR/118196.diff; git diff --stat origin/develop...0283e26b3628e68a4fde40f1031ca15e8c5b9477`,
		`git fetch -q origin develop 2>&1 | tail -2; git merge-base HEAD origin/develop | xargs git log -1 --format='%h %ci'; git log -1 --format='%h %ci %s' origin/develop; git show 11b884a6fc6 --stat | head -40; git show origin/develop:application/models/ModelExchangeRate.php 2>/dev/null | grep -n "function toValute" ; grep -rn "class ModelExchangeRate\b" --include=*.php application lib 2>/dev/null | head`,
		`git show origin/develop:src/Modules/FBank/Services/BankAccountsService.php | grep -n "function processInvestAmounts" ; git show origin/develop:src/Modules/FBank/Services/BankAccountsService.php | awk '/function processInvestAmounts/{f=1} f{n++; if(n>=50 && n<=110) print NR": "$0}'; git merge-tree --write-tree HEAD origin/develop 2>&1 | grep -i conflict | head`,
		`git merge-tree --write-tree --name-only HEAD origin/develop 2>&1 | head -20; git show origin/develop:src/Modules/FreedomProcessing/Services/InvestCardBalanceService.php | sed -n 118,150p`,
		`git merge-tree $(git merge-base HEAD origin/develop) HEAD origin/develop > $TMPDIR/mt.txt; grep -n "^changed in both\|^  our\|<<<<<<<" $TMPDIR/mt.txt | head -20`,
		`awk '/^changed in both/{p=0} /^changed in both/{getline a; getline b; getline c; getline d; if (d ~ /BankAccountsService.php/ || c ~ /BankAccountsService.php/) {print a; print b; print c; print d; p=1; next}} p' $TMPDIR/mt.txt | grep -n "<<<<<<<\|>>>>>>>\|=======" | head; git log --oneline $(git merge-base HEAD origin/develop)..HEAD | wc -l; git log --oneline $(git merge-base HEAD origin/develop)..HEAD | head`,
		`git fetch origin develop 2>&1 | tail -2; MB=$(git merge-base HEAD FETCH_HEAD); echo $MB; git diff --stat $MB HEAD | tail -3`,
		// Shell shapes agents use.
		`for f in $(git diff --name-only origin/develop...HEAD); do echo "== $f"; git diff origin/develop...HEAD -- "$f" | head -50; done`,
		`if git merge-base --is-ancestor origin/develop HEAD; then echo yes; else echo no; fi`,
		`diff <(git show origin/develop:a.php) <(git show HEAD:a.php) | head`,
		`(cd src && grep -rn foo . | wc -l)`,
		`git branch -a --contains HEAD; git tag --list 'v1*'; git remote -v; git stash list; git worktree list; git config --get remote.origin.url`,
		`cat $TMPDIR/x.diff | sed -n '100,200p' > /tmp/part.txt 2>/dev/null`,
		`git log --format='%H' -5 | xargs -I{} git show --stat {}`,
		`awk -F: '{print $1}' a.txt | sort | uniq -c | sort -rn | head`,
		"git diff `git merge-base HEAD origin/develop` HEAD --stat",
		`echo $((1+2))`,
		`glab api projects/1/merge_requests/2/notes | jq '.[].body'`,
	}
	for _, c := range allowed {
		if !ReviewShellCommand(c) {
			t.Errorf("must pass: %s", c)
		}
	}
	refused := []string{
		`git push origin HEAD`, `git checkout develop`, `git reset --hard`, `git commit -am x`, `git stash`, `git branch -D x`,
		`git branch newbranch`, `git tag v1`, `git remote add x y`, `git config user.name x`, `git worktree add x`, `git -c core.pager=sh log`,
		`git diff --output=src/a.php`, `git fetch --upload-pack=evil origin`, `git apply $TMPDIR/p.diff`, `git clean -fd`,
		`git log > src/out.txt`, `git log >> README.md`, `echo x > a.php`, `git diff > $TMPDIR/../src/a.php`, `cat x | tee a.php`,
		`curl -X POST https://gitlab/api`, `wget x`, `php -r 'unlink("a");'`, `python3 -c 'print(1)'`, `sh -c 'rm -rf src'`, `bash x.sh`,
		`eval "rm -rf src"`, `rm -rf src`, `mv a b`, `cp a b`, `X=rm; $X -rf src`, `$(echo rm) -rf src`, `PATH=. git log`, `GIT_DIR=x git log`,
		`git log $(rm -rf src)`, "git log `rm -rf src`", `diff <(rm x) a`, `cat <<EOF > a.php`, `sleep 100 &`,
		`find . -name '*.php' -delete`, `find . -exec rm {} \;`, `sed -i s/a/b/ src/a.php`, `awk '{system("rm x")}' f`,
		`awk '{print > "out.txt"}' f`, `awk '{print | "sh"}' f`, `awk -f prog.awk f`, `xargs rm < list`, `git log | xargs -I{} curl {}`,
		`glab api -X POST projects/1/notes`, `glab mr note 1 -m x`, `uniq a b`, `sort -o a.php b`, `rg --pre ./x foo`,
		`X=--output=src/a.php; git diff $X`, `echo 'unbalanced`, `git log &&`, ``,
	}
	for _, c := range refused {
		if ReviewShellCommand(c) {
			t.Errorf("must be refused: %s", c)
		}
	}
}
