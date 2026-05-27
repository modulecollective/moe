package serve

import (
	"strings"
	"testing"
)

// TestSanitizePTYTail covers the noise the activity log used to leak
// from a live claude/codex PTY: CSI moves, OSC title sets, BEL/BS,
// spinner overwrites, and runs of redraw blank lines.
func TestSanitizePTYTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "csi_sgr_strip",
			in:   "hello \x1b[31mred\x1b[0m world",
			want: "hello red world",
		},
		{
			name: "csi_cursor_strip",
			in:   "before\x1b[1Aabove\x1b[2Kerased",
			want: "beforeaboveerased",
		},
		{
			name: "csi_private_mode_strip",
			in:   "\x1b[?25lhidden\x1b[?25h",
			want: "hidden",
		},
		{
			name: "osc_bel_terminated",
			in:   "\x1b]0;moe — alpha/foo\x07payload",
			want: "payload",
		},
		{
			name: "osc_st_terminated",
			in:   "\x1b]0;title\x1b\\after",
			want: "after",
		},
		{
			name: "bel_and_bs_stripped",
			in:   "ding\x07dong\x08back",
			want: "dingdongback",
		},
		{
			name: "carriage_return_overwrite",
			in:   "Loading...\rDone.",
			want: "Done.",
		},
		{
			name: "carriage_return_multiline",
			in:   "step 1\nLoading...\rstep 2 done\nstep 3\n",
			want: "step 1\nstep 2 done\nstep 3\n",
		},
		{
			name: "collapses_blank_line_runs",
			in:   "first\n\n\n\nsecond\n",
			want: "first\n\nsecond\n",
		},
		{
			name: "single_blank_line_preserved",
			in:   "first\n\nsecond\n",
			want: "first\n\nsecond\n",
		},
		{
			name: "mixed_real_world_fragment",
			in: "Working...\r" +
				"\x1b[2K\rDone in 2.3s\n" +
				"\x1b[1mbold\x1b[0m line\n" +
				"\x07\n" +
				"\n" +
				"\n" +
				"next: moe sdlc design alpha/foo\n",
			want: "Done in 2.3s\nbold line\n\nnext: moe sdlc design alpha/foo\n",
		},
		{
			name: "empty_input",
			in:   "",
			want: "",
		},
		{
			name: "plain_text_unchanged",
			in:   "nothing to strip here\n",
			want: "nothing to strip here\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizePTYTail(c.in)
			if got != c.want {
				t.Errorf("sanitizePTYTail(%q) =\n  got:  %q\n  want: %q", c.in, got, c.want)
			}
		})
	}
}

// TestSanitizePTYTailDoesNotKeepCSIPattern guards against
// regressions where the "174m*oz" pattern (orphan CSI fragments)
// would leak back into the rendered log. Asserts that no ESC byte
// survives sanitization of an ANSI-heavy input.
func TestSanitizePTYTailDropsAllEscapes(t *testing.T) {
	in := "\x1b[31mhi\x1b[0m\x1b]0;title\x07\x1b[?25l\x1b(B"
	got := sanitizePTYTail(in)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("escape survived sanitization: %q", got)
	}
}
