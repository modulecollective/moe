package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

// testStageGate is the satisfiability gate registered on sdlc's test
// stage. It refuses to advance the run when the test canvas left the
// two load-bearing sections (What was verified, What wasn't verified)
// empty or sitting on their seeded placeholder paragraphs — the
// anti-theater move the design twin records: committing the skeleton
// without filling it should not count as "tested."
//
// The "Fixes applied" section is intentionally exempt — it's
// legitimately empty for clean runs.
func testStageGate(root string, md *run.Metadata) (bool, error) {
	canvas := filepath.Join(root, run.ContentPath(md.Project, md.ID, "test"))
	body, err := os.ReadFile(canvas)
	if err != nil {
		// A missing canvas means the stage hasn't run yet — the
		// work-turn check would have already short-circuited above
		// us. Tolerate so the gate doesn't manufacture an error for a
		// pre-stage run.
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	sections := parseTestCanvasSections(string(body))
	return testSectionFilled(sections["What was verified"]) &&
		testSectionFilled(sections["What wasn't verified"]), nil
}

// testStageGateFromCwd is the cwd-rooted convenience wrapper test code
// and dash callers can use without resolving the bureaucracy root by
// hand. Production code threads `root` through the Workflow.Next path,
// so it calls testStageGate directly; this helper is mainly for tests.
func testStageGateFromCwd(md *run.Metadata) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, err
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return false, err
	}
	return testStageGate(root, md)
}

// parseTestCanvasSections splits a markdown canvas keyed by `## ` H2
// headings into title → body strings. Bodies retain their interior
// newlines but lose the heading line itself. Used by testStageGate;
// kept here so it's testable in isolation.
func parseTestCanvasSections(body string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(body, "\n")
	currentTitle := ""
	var currentBody strings.Builder
	flush := func() {
		if currentTitle != "" {
			out[currentTitle] = currentBody.String()
		}
		currentBody.Reset()
	}
	for _, ln := range lines {
		trimmed := strings.TrimPrefix(ln, "## ")
		if trimmed != ln {
			flush()
			currentTitle = strings.TrimSpace(trimmed)
			continue
		}
		if currentTitle == "" {
			continue
		}
		currentBody.WriteString(ln)
		currentBody.WriteByte('\n')
	}
	flush()
	return out
}

// testSectionFilled returns true when section body has at least one
// substantive line — non-blank, not a parenthetical placeholder of the
// shape "(agent fills: …)" that the seeded skeleton ships with. Both
// fully-paren lines and lines whose first non-whitespace rune is `(`
// (i.e., a multi-line placeholder block) count as placeholders so a
// reformatted skeleton still reads as unfilled.
func testSectionFilled(body string) bool {
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "(") {
			continue
		}
		return true
	}
	return false
}
