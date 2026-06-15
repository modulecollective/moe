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
			name: "bare url autolink trims trailing punct",
			in:   "go to https://example.com/page.",
			want: []string{`<a href="https://example.com/page">https://example.com/page</a>`},
			deny: []string{"page.</a>"},
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
