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

func TestRenderTables(t *testing.T) {
	const open = "<div class=\"table-scroll\"><table>\n<thead><tr>"
	const middle = "</tr></thead>\n<tbody>\n"
	const close = "</tbody>\n</table></div>\n"
	cases := []struct {
		name, in, want string
	}{
		{
			"outer pipes and trimmed cells", "| Package | Owner |\n| --- | --- |\n| `internal/md` | **renderer** |",
			open + `<th scope="col" class="align-left">Package</th><th scope="col" class="align-left">Owner</th>` + middle +
				"<tr><td class=\"align-left\"><code>internal/md</code></td><td class=\"align-left\"><strong>renderer</strong></td></tr>\n" + close,
		},
		{
			"no outer pipes and alignment", "L | C | R | Default\n:- | :-: | -: | -\na | b | c | d",
			open + `<th scope="col" class="align-left">L</th><th scope="col" class="align-center">C</th><th scope="col" class="align-right">R</th><th scope="col" class="align-left">Default</th>` + middle +
				"<tr><td class=\"align-left\">a</td><td class=\"align-center\">b</td><td class=\"align-right\">c</td><td class=\"align-left\">d</td></tr>\n" + close,
		},
		{
			"empty headers and cells with uneven rows", "| A || C |\n| - | - | - |\n| x || z |\n| short |\n| a | b | c | ignored |",
			open + `<th scope="col" class="align-left">A</th><th scope="col" class="align-left"></th><th scope="col" class="align-left">C</th>` + middle +
				"<tr><td class=\"align-left\">x</td><td class=\"align-left\"></td><td class=\"align-left\">z</td></tr>\n" +
				"<tr><td class=\"align-left\">short</td><td class=\"align-left\"></td><td class=\"align-left\"></td></tr>\n" +
				"<tr><td class=\"align-left\">a</td><td class=\"align-left\">b</td><td class=\"align-left\">c</td></tr>\n" + close,
		},
		{
			"single column header only", "| Name |\n| - |",
			open + `<th scope="col" class="align-left">Name</th>` + middle + close,
		},
		{
			"leading outer pipe only and CRLF", "| Name\r\n| ---\r\n| value\r\n",
			open + `<th scope="col" class="align-left">Name</th>` + middle + "<tr><td class=\"align-left\">value</td></tr>\n" + close,
		},
		{
			"trailing outer pipe only", "Name |\n--- |\nvalue |",
			open + `<th scope="col" class="align-left">Name</th>` + middle + "<tr><td class=\"align-left\">value</td></tr>\n" + close,
		},
		{
			"escaped edge pipes", `\| Name \| | B` + "\n--- | ---\n" + `\| value \| | end`,
			open + `<th scope="col" class="align-left">| Name |</th><th scope="col" class="align-left">B</th>` + middle +
				"<tr><td class=\"align-left\">| value |</td><td class=\"align-left\">end</td></tr>\n" + close,
		},
		{
			"escaped pipes in text and code", "A | B\n- | -\na\\|b | `c\\|d`",
			open + `<th scope="col" class="align-left">A</th><th scope="col" class="align-left">B</th>` + middle +
				"<tr><td class=\"align-left\">a|b</td><td class=\"align-left\"><code>c|d</code></td></tr>\n" + close,
		},
		{
			"odd and even backslash runs", "A | B\n- | -\n" + `a\\|b | surplus` + "\n" + `a\\\|b | end`,
			open + `<th scope="col" class="align-left">A</th><th scope="col" class="align-left">B</th>` + middle +
				"<tr><td class=\"align-left\">a\\\\</td><td class=\"align-left\">b</td></tr>\n" +
				"<tr><td class=\"align-left\">a\\\\|b</td><td class=\"align-left\">end</td></tr>\n" + close,
		},
		{
			"unescaped pipes still split inline code", "A | B\n- | -\n`a|b`",
			open + `<th scope="col" class="align-left">A</th><th scope="col" class="align-left">B</th>` + middle +
				"<tr><td class=\"align-left\">`a</td><td class=\"align-left\">b`</td></tr>\n" + close,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.in, nil); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestRenderTableBoundaries(t *testing.T) {
	for _, suffix := range []string{
		"\nnext | paragraph", "# Heading | text", "- list | item",
		"1. list | item", "2. list | item", "---", "```\ncode | text\n```",
	} {
		t.Run(suffix, func(t *testing.T) {
			got := Render("before\nA | B\n- | -\nx | y\n"+suffix, nil)
			if !strings.HasPrefix(got, "<p>before</p>\n<div class=\"table-scroll\"><table>") {
				t.Fatalf("table did not interrupt preceding prose:\n%s", got)
			}
			if !strings.HasSuffix(got, "</table></div>\n"+Render(suffix, nil)) {
				t.Fatalf("following block consumed into table:\n%s", got)
			}
		})
	}
	got := Render("A | B\n- | -\nx | y\nfollowing prose", nil)
	if !strings.HasSuffix(got, "</table></div>\n<p>following prose</p>\n") {
		t.Errorf("non-pipe prose consumed into table:\n%s", got)
	}
}

func TestRenderNonTables(t *testing.T) {
	cases := []string{
		"A | B\nx | y", "A | B\n--- | nope", "A | B\n--- | --- | ---",
		"A | B\n::--- | ---", "A | B\n: | ---", "A | B\n- - | ---",
		"Name\n---", "Name\n-", `A\|B` + "\n--- | ---", "A | B\n---\\|---",
		"|\n|", "```\nA | B\n- | -\nx | y\n```", "# A | B\n- | -",
		"- A | B\n  - | -", "prose\n2024. A | B\n- | -",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := Render(in, nil); strings.Contains(got, "<table>") {
				t.Errorf("non-table recognized:\n%s", got)
			}
		})
	}
	if got := Render("A | B\n--- | nope", nil); got != "<p>A | B\n--- | nope</p>\n" {
		t.Errorf("failed recognition changed prose: %q", got)
	}
}

func TestRenderTableInlineAndResolvers(t *testing.T) {
	in := "| **Header** | `alpha/fix-it` |\n| - | - |\n" +
		"| *text* & <script> | [doc](topics/doc.md) abc1234 |\n" +
		"| [bad](javascript:evil) | [data](data:text/html,evil) |"
	got := RenderWithReferences(in, func(target string) string {
		if target == "topics/doc.md" {
			return "/knowledge/" + target
		}
		return ""
	}, func(ref Reference) string {
		return "/reference/" + ref.Text
	})
	for _, want := range []string{
		`<th scope="col" class="align-left"><strong>Header</strong></th>`,
		`<code><a href="/reference/alpha/fix-it">alpha/fix-it</a></code>`,
		`<td class="align-left"><em>text</em> &amp; &lt;script&gt;</td>`,
		`<a href="/knowledge/topics/doc.md">doc</a> <a href="/reference/abc1234">abc1234</a>`,
		`<td class="align-left">bad</td><td class="align-left">data</td>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, deny := range []string{"<script>", `href="javascript:`, `href="data:`} {
		if strings.Contains(got, deny) {
			t.Errorf("unsafe cell HTML %q in:\n%s", deny, got)
		}
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

func TestRenderWithReferences(t *testing.T) {
	fullSHA := "abcdef0123456789abcdef0123456789abcdef01"
	resolve := func(ref Reference) string {
		switch ref.Kind {
		case ReferenceCommit:
			return "/commit/" + ref.Text
		case ReferenceRun:
			if strings.Contains(ref.Text, "/") {
				return "/run/" + ref.Text
			}
			return "/run/current/" + ref.Text
		default:
			return ""
		}
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"short sha in heading", "# abc1234", `<h1><a href="/commit/abc1234">abc1234</a></h1>`},
		{"full sha in paragraph", fullSHA, `<a href="/commit/` + fullSHA + `">` + fullSHA + `</a>`},
		{"qualified run in list", "- alpha/fix-it", `<li><a href="/run/alpha/fix-it">alpha/fix-it</a></li>`},
		{"bare run in whole code span", "`fix-it`", `<code><a href="/run/current/fix-it">fix-it</a></code>`},
		{"sha in whole code span", "`abc1234`", `<code><a href="/commit/abc1234">abc1234</a></code>`},
		{"sha range keeps one code span", "`abc1234..def5678`", `<code><a href="/commit/abc1234">abc1234</a>..<a href="/commit/def5678">def5678</a></code>`},
		{"surrounding html escaped", "abc1234 & <done>", `<a href="/commit/abc1234">abc1234</a> &amp; &lt;done&gt;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderWithReferences(tc.in, nil, resolve)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, got)
			}
		})
	}
}

func TestRenderWithReferencesLeavesExcludedTextInert(t *testing.T) {
	resolve := func(ref Reference) string {
		return "/resolved/" + ref.Text
	}
	cases := []struct {
		name string
		in   string
		want string
		deny string
	}{
		{"fenced code", "```\nabc1234 alpha/fix-it\n```", "abc1234 alpha/fix-it", `/resolved/`},
		{"authored link label", "[abc1234 alpha/fix-it](/target)", `<a href="/target">abc1234 alpha/fix-it</a>`, `/resolved/`},
		{"bare url", "https://example.com/abc1234", `<a href="https://example.com/abc1234">https://example.com/abc1234</a>`, `/resolved/`},
		{"ordinary bare slug", "fix-it", "<p>fix-it</p>", `/resolved/`},
		{"uppercase sha", "ABC1234", "<p>ABC1234</p>", `/resolved/`},
		{"numeric id", "1234567", "<p>1234567</p>", `/resolved/`},
		{"sha too short", "abc123", "<p>abc123</p>", `/resolved/`},
		{"sha too long", "abcdef0123456789abcdef0123456789abcdef012", "<p>abcdef0123456789abcdef0123456789abcdef012</p>", `/resolved/`},
		{"sha prefix boundary", "xabc1234", "<p>xabc1234</p>", `/resolved/`},
		{"sha suffix boundary", "abc1234g", "<p>abc1234g</p>", `/resolved/`},
		{"run prefix boundary", "prefix/alpha/fix-it", "<p>prefix/alpha/fix-it</p>", `/resolved/`},
		{"run suffix boundary", "alpha/fix-it/more", "<p>alpha/fix-it/more</p>", `/resolved/`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderWithReferences(tc.in, nil, resolve)
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, got)
			}
			if strings.Contains(got, tc.deny) {
				t.Errorf("unexpected %q in:\n%s", tc.deny, got)
			}
		})
	}
}

func TestRenderWithReferencesResolverDeclineAndUnsafeHref(t *testing.T) {
	for _, href := range []string{"", "javascript:alert(1)"} {
		got := RenderWithReferences("abc1234", nil, func(Reference) string { return href })
		if got != "<p>abc1234</p>\n" {
			t.Errorf("href %q: got %q", href, got)
		}
	}
	if got := Render("abc1234 alpha/fix-it", nil); got != "<p>abc1234 alpha/fix-it</p>\n" {
		t.Errorf("Render wrapper changed reference behavior: %q", got)
	}
}
