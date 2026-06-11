package cli

import (
	"regexp"
	"testing"
)

// TestSummariesAreProviderAgnostic pins the help surface to the
// glossary's word: moe dispatches stage turns to whichever backend the
// agent ladder resolves (claude or codex), so no command one-liner may
// promise a specific backend. These strings drifted once already —
// every session-opening verb said "Claude Code" long after the agent
// seam landed — so the registry walk keeps `moe help` honest
// structurally instead of by review. Flag descriptions that enumerate
// backend *values* ("claude/codex") aren't reachable through the
// registry and stay legitimate places to name backends.
func TestSummariesAreProviderAgnostic(t *testing.T) {
	re := regexp.MustCompile(`(?i)claude code`)
	for name, c := range commands {
		if re.MatchString(c.Summary) {
			t.Errorf("command %q summary names a specific backend: %q", name, c.Summary)
		}
	}
	for gname, g := range groups {
		for sname, c := range g.commands {
			if re.MatchString(c.Summary) {
				t.Errorf("subcommand %q %q summary names a specific backend: %q", gname, sname, c.Summary)
			}
		}
	}
}
