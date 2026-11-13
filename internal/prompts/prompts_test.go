package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Read-only prompts carry the short-comment rules and, when the file exists, the developer's own style.
func TestCommentStyleRule(t *testing.T) {
	defer func(old string) { CommentStyleFile = old }(CommentStyleFile)
	CommentStyleFile = filepath.Join(t.TempDir(), "style.md")
	if r := readOnlyRules(); !strings.Contains(r, "GITLAB COMMENT STYLE") || strings.Contains(r, "developer's comment style") {
		t.Fatalf("no file: %s", r)
	}
	if err := os.WriteFile(CommentStyleFile, []byte("пишу коротко, без воды\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := readOnlyRules(); !strings.Contains(r, "--- developer's comment style ---\nпишу коротко, без воды\n--- end ---") {
		t.Fatalf("with file: %s", r)
	}
	if !strings.Contains(string(ReviewSchema), `"comment"`) {
		t.Fatal("findings must have a comment field")
	}
}
