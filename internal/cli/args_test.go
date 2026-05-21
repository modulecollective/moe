package cli

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestReorderFlagsMovesFlagsAhead(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "flag after positional",
			in:   []string{"moe", "--from-idea=slug"},
			want: []string{"--from-idea=slug", "moe"},
		},
		{
			name: "interspersed mix",
			in:   []string{"--a", "pos1", "--b=2", "pos2", "-c"},
			want: []string{"--a", "--b=2", "-c", "pos1", "pos2"},
		},
		{
			name: "short flag",
			in:   []string{"pos", "-x"},
			want: []string{"-x", "pos"},
		},
		{
			name: "already-ordered stays put",
			in:   []string{"--x", "--y=z", "a", "b"},
			want: []string{"--x", "--y=z", "a", "b"},
		},
		{
			name: "empty",
			in:   []string{},
			want: []string{},
		},
		{
			name: "bare dash is positional",
			in:   []string{"pos", "-"},
			want: []string{"pos", "-"},
		},
		{
			name: "sentinel: everything after -- is positional",
			in:   []string{"--flag", "pos1", "--", "--not-a-flag", "pos2"},
			want: []string{"--flag", "--", "pos1", "--not-a-flag", "pos2"},
		},
		{
			name: "sentinel preserves relative order of trailing positionals",
			in:   []string{"a", "--", "--x", "b"},
			want: []string{"--", "a", "--x", "b"},
		},
		{
			name: "value-taking flag in space form keeps pair adjacent",
			in:   []string{"--id", "foo", "pos"},
			want: []string{"--id", "foo", "pos"},
		},
		{
			name: "value-taking flag pair moves ahead of positional together",
			in:   []string{"pos", "--id", "foo"},
			want: []string{"--id", "foo", "pos"},
		},
		{
			name: "bool flag does not swallow next token",
			in:   []string{"pos", "--bool", "more"},
			want: []string{"--bool", "pos", "more"},
		},
		{
			name: "unknown flag does not swallow next token",
			in:   []string{"pos", "--unknown", "foo"},
			want: []string{"--unknown", "pos", "foo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newReorderTestFlagSet()
			got := reorderFlags(fs, tc.in)
			if !equalStringSlices(got, tc.want) {
				t.Fatalf("reorderFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// newReorderTestFlagSet builds the minimum FlagSet the cases need:
// `--id` (value-taking) and `--bool` (bool). The legacy bare flags
// (`--a`, `--flag`, `-c`, `--x`, …) are intentionally left
// undeclared — they exercise the unknown-flag branch, which doesn't
// swallow a following token, preserving the pre-FlagSet-aware output.
func newReorderTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("reorder-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("id", "", "")
	fs.Bool("bool", false, "")
	return fs
}

func equalStringSlices(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestSplitProjectRun(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantProj string
		wantRun  string
		wantErr  bool
	}{
		{name: "kebab slug", in: "moe/slash-in-error-2026-05-21", wantProj: "moe", wantRun: "slash-in-error-2026-05-21"},
		{name: "at-latest sentinel", in: "moe/@latest", wantProj: "moe", wantRun: "@latest"},
		{name: "short ids", in: "p/r", wantProj: "p", wantRun: "r"},

		{name: "empty", in: "", wantErr: true},
		{name: "no slash", in: "moe", wantErr: true},
		{name: "leading slash", in: "/run", wantErr: true},
		{name: "trailing slash", in: "moe/", wantErr: true},
		{name: "two slashes", in: "moe/foo/bar", wantErr: true},
		{name: "embedded space in project", in: "mo e/run", wantErr: true},
		{name: "embedded space in run", in: "moe/r un", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proj, run, err := splitProjectRun(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitProjectRun(%q) = %q,%q, want error", tc.in, proj, run)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitProjectRun(%q) unexpected error: %v", tc.in, err)
			}
			if proj != tc.wantProj || run != tc.wantRun {
				t.Fatalf("splitProjectRun(%q) = %q,%q, want %q,%q",
					tc.in, proj, run, tc.wantProj, tc.wantRun)
			}
		})
	}
}
