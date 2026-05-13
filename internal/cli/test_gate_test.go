package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestParseTestCanvasSections splits the canvas by `## ` headings; the
// preamble before the first H2 is discarded; later H2s with the same
// title overwrite earlier ones (real canvases never re-use headings).
func TestParseTestCanvasSections(t *testing.T) {
	body := `# Test

## What was verified

ran tests

## What wasn't verified

UI rendering

## Fixes applied during this stage

(none)
`
	got := parseTestCanvasSections(body)
	if v := strings.TrimSpace(got["What was verified"]); v != "ran tests" {
		t.Fatalf("verified body = %q", v)
	}
	if v := strings.TrimSpace(got["What wasn't verified"]); v != "UI rendering" {
		t.Fatalf("unverified body = %q", v)
	}
	if v := strings.TrimSpace(got["Fixes applied during this stage"]); v != "(none)" {
		t.Fatalf("fixes body = %q", v)
	}
}

// TestTestSectionFilled drives the placeholder-aware section check.
// Blank lines and parenthetical placeholder lines don't count; any
// other non-blank line does.
func TestTestSectionFilled(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"whitespace only", "   \n\n\n", false},
		{"placeholder", "(agent fills: verified what)\n", false},
		{"placeholder with leading whitespace", "  (agent fills: verified what)\n", false},
		{"real content", "ran `go test ./...`, all green\n", true},
		{"mixed", "(placeholder)\nran tests, all green\n", true},
		{"single line no newline", "actual content", true},
	}
	for _, tc := range cases {
		if got := testSectionFilled(tc.body); got != tc.want {
			t.Errorf("%s: testSectionFilled(%q) = %v, want %v", tc.name, tc.body, got, tc.want)
		}
	}
}

// TestTestStageGateAcceptsFilledCanvas: a canvas with substantive
// content in both required sections satisfies the gate.
func TestTestStageGateAcceptsFilledCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, `# Test

## What was verified

ran `+"`go test ./...`"+`

## What wasn't verified

nothing — automated tests cover the change

## Fixes applied during this stage

(none)
`)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected gate to pass on filled canvas")
	}
}

// TestTestStageGateRefusesPlaceholderCanvas: a canvas that left the
// placeholder text in place is detected as theater.
func TestTestStageGateRefusesPlaceholderCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, testCanvasSkeleton)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected gate to refuse the unfilled skeleton")
	}
}

// TestTestStageGateRefusesEmptySection: filling only "What was
// verified" while leaving "What wasn't verified" empty is also
// theater — the design's silence-isn't-valid rule.
func TestTestStageGateRefusesEmptySection(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, `# Test

## What was verified

ran tests

## What wasn't verified

(agent fills: skipped surfaces)
`)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected gate to refuse: unverified section still a placeholder")
	}
}

// TestTestStageGateMissingCanvasIsUnsatisfied: a stage that never ran
// (no canvas on disk) reports unsatisfied; the work-turn check
// upstream is the authoritative "did the stage run" signal, but the
// gate must not produce an error here.
func TestTestStageGateMissingCanvasIsUnsatisfied(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected gate to refuse: canvas missing")
	}
}

// writeTestCanvas seeds the test stage's canvas file under root.
func writeTestCanvas(t *testing.T, root string, md *run.Metadata, body string) {
	t.Helper()
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, "test"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
