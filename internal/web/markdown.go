package web

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// Agent output is markdown-ish prose: inline code, fenced code blocks, lists, bold, links and file:line references.
// renderMarkdown turns the subset the dashboard needs into safe HTML (everything is escaped first), close to how
// GitHub renders a README, without pulling in a markdown library. blobBase + sha turn `path/to/file.php:123`
// into links to the file in GitLab at the reviewed commit.

var (
	fileRefRe    = regexp.MustCompile(`((?:[\w.-]+/)*[\w.-]+\.(?:php|phtml|js|ts|vue|sql|md|json|yml|yaml|twig|css|scss|html|go|py|sh))(?::(\d+))?`)
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	linkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	bareURLRe    = regexp.MustCompile(`(^|\s)(https?://[^\s<>"')»]+)`)
	orderedRe    = regexp.MustCompile(`^\d+[.)]\s+`)
)

// blobBase derives "https://host/group/project/-/blob" from an MR or issue web URL ("" when unknown).
func blobBase(webURL string) string {
	if idx := strings.Index(webURL, "/-/"); idx > 0 {
		return webURL[:idx] + "/-/blob"
	}
	return ""
}

// fileLink builds a GitLab link to path[:line] at sha ("" when the base or sha is unknown).
func fileLink(base, sha, path, line string) string {
	if base == "" || sha == "" || path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return ""
	}
	url := base + "/" + sha + "/" + path
	if line != "" {
		url += "#L" + line
	}
	return url
}

// renderMarkdown renders text as HTML inside <div class="md">.
func renderMarkdown(text, base, sha string) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="md">`)
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n")
	var para []string
	listOpen := "" // "ul" | "ol" | ""
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>" + renderInline(strings.Join(para, " "), base, sha) + "</p>")
		para = para[:0]
	}
	closeList := func() {
		if listOpen != "" {
			b.WriteString("</" + listOpen + ">")
			listOpen = ""
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flushPara()
			closeList()
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			class := ""
			if lang != "" {
				class = ` class="lang-` + html.EscapeString(lang) + `"`
			}
			b.WriteString("<pre><code" + class + ">" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>")
		case trimmed == "":
			flushPara()
			closeList()
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "• "):
			flushPara()
			if listOpen != "ul" {
				closeList()
				listOpen = "ul"
				b.WriteString("<ul>")
			}
			b.WriteString("<li>" + renderInline(trimmed[2:], base, sha) + "</li>")
		case orderedRe.MatchString(trimmed):
			flushPara()
			if listOpen != "ol" {
				closeList()
				listOpen = "ol"
				b.WriteString("<ol>")
			}
			b.WriteString("<li>" + renderInline(orderedRe.ReplaceAllString(trimmed, ""), base, sha) + "</li>")
		case strings.HasPrefix(trimmed, "#"):
			flushPara()
			closeList()
			b.WriteString("<p><strong>" + renderInline(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), base, sha) + "</strong></p>")
		default:
			if listOpen != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
				// continuation of a list item
				b.WriteString(" " + renderInline(trimmed, base, sha))
				continue
			}
			closeList()
			para = append(para, trimmed)
		}
	}
	flushPara()
	closeList()
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// renderInline escapes text and applies inline code, bold, links, bare URLs and file:line links.
func renderInline(text, base, sha string) string {
	// Protect inline code spans first: their content must not be touched by other rules.
	var codes []string
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		codes = append(codes, inner)
		return "\x00" + string(rune('A'+len(codes)-1)) + "\x00"
	})
	out := html.EscapeString(text)
	out = linkRe.ReplaceAllString(out, `<a href="$2" target="_blank" rel="noopener">$1</a>`)
	out = bareURLRe.ReplaceAllString(out, `$1<a href="$2" target="_blank" rel="noopener">$2</a>`)
	out = boldRe.ReplaceAllString(out, "<strong>$1</strong>")
	out = linkFileRefs(out, base, sha, false)
	for i, code := range codes {
		marker := "\x00" + string(rune('A'+i)) + "\x00"
		out = strings.Replace(out, marker, "<code>"+linkFileRefs(html.EscapeString(code), base, sha, true)+"</code>", 1)
	}
	return out
}

// linkFileRefs wraps repository paths (already HTML-escaped) into GitLab blob links. Outside code spans only
// references with a line number are linked, to avoid turning every mention of a file name into a link.
func linkFileRefs(escaped, base, sha string, insideCode bool) string {
	if base == "" || sha == "" {
		return escaped
	}
	return fileRefRe.ReplaceAllStringFunc(escaped, func(m string) string {
		sub := fileRefRe.FindStringSubmatch(m)
		path, line := sub[1], sub[2]
		if !insideCode && line == "" {
			return m
		}
		if !strings.Contains(path, "/") && !insideCode {
			return m
		}
		url := fileLink(base, sha, path, line)
		if url == "" {
			return m
		}
		return `<a class="file-ref" href="` + url + `" target="_blank" rel="noopener">` + m + `</a>`
	})
}
