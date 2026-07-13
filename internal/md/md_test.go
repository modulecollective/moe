package md

import (
	"strings"
	"testing"
)

func TestRenderBlocks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string // substrings that must all appear
		deny []string // substrings that must not appear
	}{
		{
			name: "atx headings",
			in:   "# Title\n## Sub\n###### Deep",
			want: []string{"<h1>Title</h1>", "<h2>Sub</h2>", "<h6>Deep</h6>"},
			deny: []string{"#"},
		},
		{
			name: "paragraph",
			in:   "first line\nsecond line\n\nnext para",
			want: []string{"<p>first line\nsecond line</p>", "<p>next para</p>"},
		},
		{
			name: "unordered list",
			in:   "- one\n- two\n* three",
			want: []string{"<ul>", "<li>one</li>", "<li>two</li>", "<li>three</li>", "</ul>"},
		},
		{
			name: "ordered list",
			in:   "1. first\n2. second",
			want: []string{"<ol>", "<li>first</li>", "<li>second</li>", "</ol>"},
		},
		{
			name: "wrapped list item folds continuation into one li",
			in:   "- [Claude Code](topics/claude-code.md) — the CLI\n  extensions and notes\n- next",
			want: []string{"<li>", "the CLI\nextensions and notes</li>", "<li>next</li>"},
			deny: []string{"<p>extensions"},
		},
		{
			name: "hard-wrapped number does not interrupt paragraph",
			in:   "released in\n2024. The model shipped.",
			want: []string{"<p>released in\n2024. The model shipped.</p>"},
			deny: []string{"<ol>", "<li>"},
		},
		{
			name: "1. still interrupts a paragraph (author intent)",
			in:   "steps\n1. install",
			want: []string{"<ol>", "<li>install</li>"},
		},
		{
			name: "bullet still interrupts a paragraph",
			in:   "steps\n- install",
			want: []string{"<ul>", "<li>install</li>"},
		},
		{
			name: "wrapped ordered continuation folds into li keeping number",
			in:   "- model released in\n  2024. Shipped.\n- next",
			want: []string{"<li>", "released in\n2024. Shipped.</li>", "<li>next</li>"},
			deny: []string{"<ol>"},
		},
		{
			name: "nested bullet continuation still collapses to sibling",
			in:   "- item\n  - sub\n- next",
			want: []string{"<li>item</li>", "<li>sub</li>", "<li>next</li>"},
		},
		{
			name: "ordered list beginning past 1 gets start",
			in:   "3. c\n4. d",
			want: []string{`<ol start="3">`, "<li>c</li>", "<li>d</li>"},
		},
		{
			name: "plain ordered list emits no start",
			in:   "1. a\n2. b",
			want: []string{"<ol>", "<li>a</li>"},
			deny: []string{"start="},
		},
		{
			name: "atx heading keeps glued trailing hash",
			in:   "# What is F#",
			want: []string{"<h1>What is F#</h1>"},
		},
		{
			name: "atx heading strips spaced closing sequence",
			in:   "# Title ##",
			want: []string{"<h1>Title</h1>"},
		},
		{
			name: "atx heading of only hashes is empty",
			in:   "# #",
			want: []string{"<h1></h1>"},
		},
		{
			name: "fenced code preserves content verbatim",
			in:   "```\n  col1   col2\n  a      b\n```",
			want: []string{"<pre><code>  col1   col2\n  a      b</code></pre>"},
			deny: []string{"<p>"},
		},
		{
			name: "horizontal rule",
			in:   "above\n\n---\n\nbelow",
			want: []string{"<hr>", "<p>above</p>", "<p>below</p>"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Render(tc.in, nil)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("unexpected %q in:\n%s", d, got)
				}
			}
		})
	}
}

func TestRenderInline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		deny []string
	}{
		{
			name: "code bold italic",
			in:   "a `code` b **bold** c *italic* d",
			want: []string{"<code>code</code>", "<strong>bold</strong>", "<em>italic</em>"},
		},
		{
			name: "absolute link passes through",
			in:   "see [docs](https://example.com/x)",
			want: []string{`<a href="https://example.com/x">docs</a>`},
		},
		{
			name: "mailto link passes through",
			in:   "mail [me](mailto:a@b.com)",
			want: []string{`<a href="mailto:a@b.com">me</a>`},
		},
		{
			name: "root-relative and anchor links pass through",
			in:   "[home](/projects/moe) and [top](#section)",
			want: []string{`<a href="/projects/moe">home</a>`, `<a href="#section">top</a>`},
		},
		{
			name: "javascript scheme dropped to inert label",
			in:   "[click](javascript:alert(1))",
			want: []string{"click"},
			deny: []string{`href="javascript`, "<a "},
		},
		{
			name: "javascript scheme case-insensitive",
			in:   "[x](JavaScript:alert(1))",
			deny: []string{`href="JavaScript`, `href="javascript`, "<a "},
		},
		{
			name: "data scheme dropped",
			in:   "[x](data:text/html,<script>alert(1)</script>)",
			deny: []string{`href="data:`, "<a "},
		},
		{
			name: "leading-space javascript scheme dropped",
			in:   "[x]( javascript:alert(1))",
			deny: []string{"javascript", "<a "},
		},
		{
			name: "tab-obfuscated javascript scheme dropped",
			in:   "[x](java\tscript:alert(1))",
			deny: []string{"javascript", "<a "},
		},
		{
			name: "bare url autolink trims trailing punct",
			in:   "go to https://example.com/page.",
			want: []string{`<a href="https://example.com/page">https://example.com/page</a>`},
			deny: []string{"page.</a>"},
		},
		{
			name: "url in link text does not nest an anchor",
			in:   "[see https://a.com](https://b.com)",
			want: []string{`<a href="https://b.com">see https://a.com</a>`},
			deny: []string{`href="https://a.com"`},
		},
		{
			name: "unsafe-scheme degrade still autolinks a url in the label",
			in:   "[go https://a.com now](javascript:alert(1))",
			want: []string{`<a href="https://a.com">https://a.com</a>`},
			deny: []string{`href="javascript`},
		},
		{
			name: "html is escaped",
			in:   "x <script>alert(1)</script> y",
			want: []string{"&lt;script&gt;", "&lt;/script&gt;"},
			deny: []string{"<script>"},
		},
		{
			name: "wikilink bare slug becomes span",
			in:   "ride [[reflect-shows-in-dash]] now",
			want: []string{`<span class="wikilink">reflect-shows-in-dash</span>`},
		},
		{
			name: "dotted and typed wikilink slugs",
			in:   "[[services.ports]] and [[feedback_no-config-knobs]]",
			want: []string{`<span class="wikilink">services.ports</span>`, `<span class="wikilink">feedback_no-config-knobs</span>`},
		},
		{
			name: "bash test syntax is not a wikilink",
			in:   "guard with `[[ -n \"$v\" ]]` and [[:space:]]",
			deny: []string{`class="wikilink"`},
		},
		{
			name: "italic not triggered by spaced asterisks",
			in:   "2 * 3 * 4",
			deny: []string{"<em>"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Render(tc.in, nil)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("unexpected %q in:\n%s", d, got)
				}
			}
		})
	}
}

// TestRelativeLinkResolution mirrors the knowledge index.md shape: a
// curated landing page that links to topics/*.md with relative file
// links. The renderer must rewrite those to serve routes so the
// rendered index is clickable; absolute links must pass through.
func TestRelativeLinkResolution(t *testing.T) {
	in := "- [Claude Code](topics/claude-code.md) — the CLI\n" +
		"- [external](https://anthropic.com)"
	resolve := func(target string) string {
		// Map "topics/<x>.md" to the knowledge topic route.
		if strings.HasPrefix(target, "topics/") && strings.HasSuffix(target, ".md") {
			name := strings.TrimSuffix(strings.TrimPrefix(target, "topics/"), ".md")
			return "/projects/moe/knowledge/" + name
		}
		return ""
	}
	got := Render(in, resolve)
	if !strings.Contains(got, `<a href="/projects/moe/knowledge/claude-code">Claude Code</a>`) {
		t.Errorf("relative .md link not rewritten:\n%s", got)
	}
	if !strings.Contains(got, `<a href="https://anthropic.com">external</a>`) {
		t.Errorf("absolute link should pass through:\n%s", got)
	}
}
