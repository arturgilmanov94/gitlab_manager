package web

import (
	"crypto/sha1"
	"fmt"
	"strings"
	"testing"
)

func TestRenderMarkdown(t *testing.T) {
	base := blobBase("https://gitlab.example.com/tn/core/tradernet/-/merge_requests/5")
	if base != "https://gitlab.example.com/tn/core/tradernet/-/blob" {
		t.Fatal(base)
	}
	out := string(renderMarkdown("Adds `FirdsService` in `src/Modules/Firds/Service.php:115` and <b>escapes</b>.\n\n"+
		"```php\n$x = 1 < 2;\n```\n\n- first **bold** item\n- second\n\n1. one\n2. two\n\nSee https://example.com/x and [doc](https://example.com/d).",
		base, "abc123"))
	for _, want := range []string{
		`<p>Adds <code>FirdsService</code> in <code><a class="file-ref" href="https://gitlab.example.com/tn/core/tradernet/-/blob/abc123/src/Modules/Firds/Service.php#L115" target="_blank" rel="noopener">src/Modules/Firds/Service.php:115</a></code> and &lt;b&gt;escapes&lt;/b&gt;.</p>`,
		`<pre><code class="lang-php">$x = 1 &lt; 2;</code></pre>`,
		`<ul><li>first <strong>bold</strong> item</li><li>second</li></ul>`,
		`<ol><li>one</li><li>two</li></ol>`,
		`<a href="https://example.com/x" target="_blank" rel="noopener">https://example.com/x</a>`,
		`<a href="https://example.com/d" target="_blank" rel="noopener">doc</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	// Without a sha there are no file links, and plain file mentions without a line stay text.
	plain := string(renderMarkdown("Look at src/A.php and src/B.php:3", "", ""))
	if strings.Contains(plain, "<a ") {
		t.Fatal(plain)
	}
	withSHA := string(renderMarkdown("Look at src/A.php and src/B.php:3", base, "abc"))
	if strings.Contains(withSHA, `blob/abc/src/A.php"`) || !strings.Contains(withSHA, `blob/abc/src/B.php#L3`) {
		t.Fatal(withSHA)
	}
	// Unterminated fence still renders the code.
	if !strings.Contains(string(renderMarkdown("```\ncode\n", "", "")), "<pre><code>code</code></pre>") {
		t.Fatal("unterminated fence")
	}
	if fileLink(base, "sha", "../etc/passwd", "") != "" || fileLink(base, "sha", "/abs", "") != "" {
		t.Fatal("unsafe paths must not link")
	}
}

func TestFormatDuration(t *testing.T) {
	for ms, want := range map[int64]string{0: "—", 48000: "48 с", 400000: "6 мин 40 с", 4320000: "1 ч 12 мин"} {
		if got := formatDuration(ms); got != want {
			t.Fatalf("%d: %q != %q", ms, got, want)
		}
	}
}

func TestMRDiffLink(t *testing.T) {
	mr := "https://gitlab.example.com/group/sub/project/-/merge_requests/42"
	// GitLab anchors every file of a diff with the SHA-1 of its path (Gitlab::Diff::File#file_hash).
	want := mr + "/diffs#" + fmt.Sprintf("%x", sha1.Sum([]byte("internal/web/server.go")))
	if got := mrDiffLink(mr, "internal/web/server.go"); got != want {
		t.Fatalf("mrDiffLink = %q, want %q", got, want)
	}
	if got := mrDiffLink(mr+"/", "internal/web/server.go"); got != want {
		t.Fatalf("a trailing slash in the MR url must not change the link: %q", got)
	}
	for _, bad := range [][2]string{{"", "a.go"}, {mr, ""}, {mr, "/etc/passwd"}, {mr, "../secrets.go"}} {
		if got := mrDiffLink(bad[0], bad[1]); got != "" {
			t.Fatalf("mrDiffLink(%q, %q) = %q", bad[0], bad[1], got)
		}
	}
}
