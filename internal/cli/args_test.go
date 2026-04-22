package cli

import (
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reorderFlags(tc.in)
			if !equalStringSlices(got, tc.want) {
				t.Fatalf("reorderFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
