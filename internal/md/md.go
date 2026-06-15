// Package md is a tiny, stdlib-only Markdown→HTML renderer. It is
// deliberately *not* a CommonMark implementation: it targets the subset
// the bureaucracy corpus actually uses (ATX headings, paragraphs,
// one-level lists, fenced code, horizontal rules; inline code, bold,
// italic, links, bare-URL autolinks, and `[[wikilinks]]`). Markup it
// doesn't understand — GFM tables, images, nested lists past one level —
// renders as its literal escaped text rather than as broken HTML.
//
// Every text node is HTML-escaped and the result is returned as
// template.HTML. The content is the operator's own markdown, but
// escaping is non-negotiable: a stray `<script>` in a doc must never
// execute.
package md

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// headingRe matches an ATX heading: 1–6 leading '#' then at least one
// space then the heading text.
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// listItemRe matches a single list item line: optional indentation, a
// bullet (`-`/`*`) or ordered (`1.`) marker, a space, then the content.
var listItemRe = regexp.MustCompile(`^\s*([-*]|\d{1,9}\.)\s+(.*)$`)

// wikilinkSlugRe is the conservative wikilink target: a bare slug of
// word chars, dots, slashes, and hyphens — no spaces, quotes, or
// operators. This keeps bash test syntax (`[[ -n "$v" ]]`) and regex
// character classes (`[[:space:]]`) from being mistaken for wikilinks.
var wikilinkSlugRe = regexp.MustCompile(`^[\w./-]+$`)

// Render converts src to safe HTML. resolve, when non-nil, rewrites a
// relative link target (e.g. "topics/claude-code.md") to the route that
// serves it; it receives the raw target and returns the replacement
// href (return "" to leave the target unchanged). Absolute links
// (scheme://…, mailto:, root-relative "/…", and "#anchors") bypass
// resolve untouched.
func Render(src string, resolve func(target string) string) string {
	lines := strings.Split(src, "\n")
	var b strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			i++
			continue
		}

		// Fenced code block — collect verbatim until the closing fence
		// (or EOF). Content is escaped but never inline-parsed, so an
		// ASCII diagram inside a fence survives intact.
		if strings.HasPrefix(trimmed, "```") {
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // consume closing fence
			}
			b.WriteString("<pre><code>")
			b.WriteString(html.EscapeString(strings.Join(code, "\n")))
			b.WriteString("</code></pre>\n")
			continue
		}

		// ATX heading.
		if m := headingRe.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			text := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(m[2]), "#"))
			fmt.Fprintf(&b, "<h%d>%s</h%d>\n", level, renderInline(text, resolve), level)
			i++
			continue
		}

		// Horizontal rule.
		if isHR(trimmed) {
			b.WriteString("<hr>\n")
			i++
			continue
		}

		// List block — a run of consecutive item lines. The first
		// item's marker decides ordered vs unordered; nesting collapses
		// to one level (the documented degrade).
		if m := listItemRe.FindStringSubmatch(line); m != nil {
			tag := "ul"
			if isOrderedMarker(m[1]) {
				tag = "ol"
			}
			fmt.Fprintf(&b, "<%s>\n", tag)
			for i < len(lines) {
				im := listItemRe.FindStringSubmatch(lines[i])
				if im == nil {
					break
				}
				content := im[2]
				i++
				// Lazy continuation: an indented, non-blank line that
				// isn't itself a list item belongs to this item (wrapped
				// prose, common in the corpus). A nested "- sub" line
				// matches listItemRe and so falls through to the outer
				// loop as a sibling — the documented one-level collapse.
				for i < len(lines) {
					cl := lines[i]
					if strings.TrimSpace(cl) == "" || listItemRe.MatchString(cl) {
						break
					}
					if !strings.HasPrefix(cl, " ") && !strings.HasPrefix(cl, "\t") {
						break
					}
					content += "\n" + strings.TrimSpace(cl)
					i++
				}
				b.WriteString("<li>")
				b.WriteString(renderInline(content, resolve))
				b.WriteString("</li>\n")
			}
			fmt.Fprintf(&b, "</%s>\n", tag)
			continue
		}

		// Paragraph — gather lines until a blank line or the next block
		// start. Joined with newlines; HTML collapses them to spaces, so
		// unfenced multi-line ASCII reflows (a known, authoring-fixable
		// casualty).
		var para []string
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" || isBlockStart(lines[i]) {
				break
			}
			para = append(para, lines[i])
			i++
		}
		b.WriteString("<p>")
		b.WriteString(renderInline(strings.Join(para, "\n"), resolve))
		b.WriteString("</p>\n")
	}
	return b.String()
}

// isBlockStart reports whether line opens a block-level construct, used
// to terminate paragraph gathering.
func isBlockStart(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") {
		return true
	}
	if headingRe.MatchString(line) {
		return true
	}
	if isHR(trimmed) {
		return true
	}
	if listItemRe.MatchString(line) {
		return true
	}
	return false
}

// isHR reports whether a trimmed line is a horizontal rule: three or
// more of a single '-', '*', or '_' and nothing else.
func isHR(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	c := trimmed[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c {
			return false
		}
	}
	return true
}

func isOrderedMarker(marker string) bool {
	return marker != "-" && marker != "*"
}

// renderInline renders inline markup within a single text run and
// returns escaped HTML. It scans left to right, handling the highest-
// priority construct at each position and escaping anything else.
func renderInline(s string, resolve func(string) string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		// Inline code span.
		if s[i] == '`' {
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				b.WriteString("<code>")
				b.WriteString(html.EscapeString(s[i+1 : i+1+j]))
				b.WriteString("</code>")
				i += 1 + j + 1
				continue
			}
		}

		// Wikilink — render as a styled span; resolution to a target
		// route is a follow-up. Detected conservatively (bare slug only).
		if strings.HasPrefix(s[i:], "[[") {
			if slug, n, ok := matchWikilink(s[i:]); ok {
				b.WriteString(`<span class="wikilink">`)
				b.WriteString(html.EscapeString(slug))
				b.WriteString("</span>")
				i += n
				continue
			}
		}

		// Link [text](url).
		if s[i] == '[' {
			if text, url, n, ok := matchLink(s[i:]); ok {
				href := url
				if resolve != nil && isRelativeLink(url) {
					if h := resolve(url); h != "" {
						href = h
					}
				}
				if !safeHref(href) {
					// Disallowed scheme (javascript:, data:, …): drop the
					// anchor, keep the visible label as inert inline text —
					// the renderer's usual "degrade to text" behaviour.
					b.WriteString(renderInline(text, resolve))
					i += n
					continue
				}
				b.WriteString(`<a href="`)
				b.WriteString(html.EscapeString(href))
				b.WriteString(`">`)
				b.WriteString(renderInline(text, resolve))
				b.WriteString("</a>")
				i += n
				continue
			}
		}

		// Bold (checked before italic so `**` isn't consumed as two `*`).
		if strings.HasPrefix(s[i:], "**") {
			if inner, n, ok := matchDelim(s[i:], "**"); ok {
				b.WriteString("<strong>")
				b.WriteString(renderInline(inner, resolve))
				b.WriteString("</strong>")
				i += n
				continue
			}
		}

		// Italic.
		if s[i] == '*' {
			if inner, n, ok := matchDelim(s[i:], "*"); ok {
				b.WriteString("<em>")
				b.WriteString(renderInline(inner, resolve))
				b.WriteString("</em>")
				i += n
				continue
			}
		}

		// Bare-URL autolink.
		if hasURLPrefix(s[i:]) {
			url, n := scanURL(s[i:])
			b.WriteString(`<a href="`)
			b.WriteString(html.EscapeString(url))
			b.WriteString(`">`)
			b.WriteString(html.EscapeString(url))
			b.WriteString("</a>")
			i += n
			continue
		}

		// Default: escape one byte. UTF-8 continuation bytes (>=0x80)
		// are never the special ASCII chars, so writing them raw is safe.
		switch s[i] {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&#34;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

// matchWikilink expects s to start with "[[". On a conservative match
// it returns the slug, the byte length consumed (including the "]]"),
// and true.
func matchWikilink(s string) (slug string, n int, ok bool) {
	end := strings.Index(s, "]]")
	if end < 0 {
		return "", 0, false
	}
	inner := s[2:end]
	if !wikilinkSlugRe.MatchString(inner) {
		return "", 0, false
	}
	return inner, end + 2, true
}

// matchLink expects s to start with '['. It matches a `[text](url)`
// link with no nested brackets in the text and returns the text, the
// url, the bytes consumed, and true.
func matchLink(s string) (text, url string, n int, ok bool) {
	closeText := strings.IndexByte(s, ']')
	if closeText < 0 || closeText+1 >= len(s) || s[closeText+1] != '(' {
		return "", "", 0, false
	}
	closeURL := strings.IndexByte(s[closeText+2:], ')')
	if closeURL < 0 {
		return "", "", 0, false
	}
	text = s[1:closeText]
	url = s[closeText+2 : closeText+2+closeURL]
	return text, url, closeText + 2 + closeURL + 1, true
}

// matchDelim expects s to start with delim and returns the content up
// to the next unescaped delim, the bytes consumed, and true. A space
// immediately after the opening delim disqualifies the match (so
// "2 * 3 * 4" isn't italicised), as does empty content.
func matchDelim(s, delim string) (inner string, n int, ok bool) {
	if len(s) <= len(delim) || s[len(delim)] == ' ' {
		return "", 0, false
	}
	rest := s[len(delim):]
	close := strings.Index(rest, delim)
	if close <= 0 {
		return "", 0, false
	}
	return rest[:close], len(delim) + close + len(delim), true
}

// isRelativeLink reports whether url is a relative target the resolve
// hook should get a crack at — i.e. not a scheme URL, mailto, root-
// relative path, or in-page anchor.
func isRelativeLink(url string) bool {
	if url == "" {
		return false
	}
	if strings.HasPrefix(url, "/") || strings.HasPrefix(url, "#") {
		return false
	}
	if strings.HasPrefix(url, "mailto:") {
		return false
	}
	return !strings.Contains(url, "://")
}

// safeHref reports whether href is safe to emit as an <a href>. Targets
// with no scheme (relative, root-relative, or #anchor) pass; scheme
// targets pass only for http/https/mailto. This drops javascript:,
// data:, vbscript:, file:, etc.
func safeHref(href string) bool {
	switch schemeOf(href) {
	case "", "http", "https", "mailto":
		return true
	default:
		return false
	}
}

// schemeOf returns the lowercased URL scheme of href (without the ':'),
// or "" if href is relative. A scheme is an ASCII letter followed by
// letters/digits/'+'/'-'/'.' up to a ':'. ASCII control chars and spaces
// are skipped while scanning so a browser-stripped `java\tscript:` or a
// leading-space ` javascript:` can't smuggle a scheme past the check.
func schemeOf(href string) string {
	var sb strings.Builder
	for i := 0; i < len(href); i++ {
		c := href[i]
		if c <= ' ' { // C0 controls + space: browsers strip/trim these
			continue
		}
		if c == ':' {
			if sb.Len() == 0 {
				return ""
			}
			return strings.ToLower(sb.String())
		}
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isCont := isLetter || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
		if sb.Len() == 0 && !isLetter {
			return "" // first scheme char must be a letter; else relative
		}
		if !isCont {
			return "" // hit '/', '?', '#', or a path char before ':'
		}
		sb.WriteByte(c)
	}
	return ""
}

func hasURLPrefix(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// scanURL reads a bare URL from the front of s, stopping at whitespace
// or a delimiter, and trims trailing sentence punctuation.
func scanURL(s string) (url string, n int) {
	end := strings.IndexAny(s, " \t\n<>()[]\"'")
	if end < 0 {
		end = len(s)
	}
	url = s[:end]
	for len(url) > 0 && strings.ContainsRune(".,;:!?", rune(url[len(url)-1])) {
		url = url[:len(url)-1]
	}
	return url, len(url)
}
