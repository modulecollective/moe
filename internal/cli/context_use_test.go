package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The scanner's own tests live in internal/contextuse. What stays here
// is the verb: flag handling and the empty window.

func TestContextUseCommandEmptyBureaucracy(t *testing.T) {
	newMoeHome(t)
	t.Setenv("NO_COLOR", "1")
	var out, errb bytes.Buffer
	if code := Run([]string{"context-use"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "No stage transcripts in the window.") {
		t.Errorf("stdout = %q, want the empty case named", out.String())
	}
}

func TestContextUseCommandRejectsBadFlags(t *testing.T) {
	root := newTestBureaucracy(t)
	t.Setenv("MOE_HOME", root)
	for _, args := range [][]string{
		{"context-use", "--since", "banana"},
		{"context-use", "--by-backend", "--sessions"},
		{"context-use", "stray"},
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit=%d, want 2", args, code)
		}
	}
}
