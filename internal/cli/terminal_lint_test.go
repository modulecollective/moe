package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTerminalStatusWriteIsCentralised pins the invariant that the
// only place writing a terminal run status is enterTerminal in
// terminal.go. Every other code path that ends a run must flow
// through enterTerminal so harvest stays glued to the state
// transition. A new caller that flips status by hand is exactly the
// regression that put us here in the first place: harvest hung off a
// command instead of the transition, and a new terminal site silently
// reintroduced the bug.
//
// The grep is deliberately dumb. If a future caller has a genuinely
// different reason to write md.Status = run.Status{Merged,Closed} it
// can override with a //nolint:terminal-lint comment on the line, but
// the default is to fail the build and force the conversation.
func TestTerminalStatusWriteIsCentralised(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	terminalWritePattern := regexp.MustCompile(
		`md\.Status\s*=\s*run\.Status(Merged|Closed)\b`,
	)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// Test files set up arbitrary fixtures (faked status flips,
		// state-machine probes) that don't represent runtime code
		// paths; they're outside the invariant.
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// terminal.go is the one allowed home for production writes.
		if e.Name() == "terminal.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !terminalWritePattern.MatchString(line) {
				continue
			}
			if strings.Contains(line, "//nolint:terminal-lint") {
				continue
			}
			t.Errorf("%s:%d: terminal status written outside terminal.go: %s\n"+
				"       route this through enterTerminal so follow-up harvest stays attached to the transition,\n"+
				"       or annotate the line with //nolint:terminal-lint and explain why",
				e.Name(), i+1, strings.TrimSpace(line))
		}
	}
}
