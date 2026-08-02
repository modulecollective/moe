package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// TestParseTestCanvasSections splits the canvas by `## ` headings; the
// preamble before the first H2 is discarded.
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

// TestParseTestCanvasSectionsFirstHeadingWins: when a heading repeats,
// the first occurrence binds. Canvases quote the seeded skeleton as
// captured evidence, and the quote's headings land after the real ones.
func TestParseTestCanvasSectionsFirstHeadingWins(t *testing.T) {
	body := "# Test\n\n## Gate\n\nreal gate\n\n## Evidence\n\nthe skeleton the agent was handed:\n\n## Gate\n\nquoted gate\n"
	got := parseTestCanvasSections(body)
	if v := strings.TrimSpace(got["Gate"]); v != "real gate" {
		t.Fatalf("Gate body = %q, want the first occurrence", v)
	}
}

// TestTestStageGateAcceptsCanvasQuotingSkeleton is the regression for
// the phantom kickback that stranded moe/sdlc-for-b-repo-only-work: a
// legitimately-ready canvas that quotes the seeded skeleton — headings
// and a blocked ```json gate, nested inside its own fence — as evidence
// of what the agent was handed. Before first-heading-wins the quoted
// gate overrode the real one and the gate read blocked.
func TestTestStageGateAcceptsCanvasQuotingSkeleton(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	fence := "```"
	writeTestCanvas(t, root, md, `# Test

## Gate

`+fence+`json
{"status":"ready"}
`+fence+`

## What was verified

ran `+"`go test ./...`"+`

## What wasn't verified

nothing — automated tests cover the change

## Evidence

The seeded skeleton the agent was handed, captured by a fake that
copies $CANVAS before writing it:

`+fence+`
## Gate

`+fence+`json
{"status":"blocked"}
`+fence+`

Allowed values: "ready" or "blocked".

## What was verified

(agent fills: what you exercised)
`+fence+`

## Fixes applied during this stage

(none)
`)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected gate to pass: the real gate is ready, the blocked one is quoted evidence")
	}
	body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, "test")))
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := stageGateStatus(string(body)); status != "ready" || !ok {
		t.Fatalf("stageGateStatus = (%q, %v), want (\"ready\", true)", status, ok)
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

func TestStageGateStatus(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status string
		ok     bool
	}{
		{
			name:   "ready",
			body:   "# Review\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n",
			status: "ready",
			ok:     true,
		},
		{
			name:   "blocked",
			body:   "# Review\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n",
			status: "blocked",
			ok:     true,
		},
		{
			name: "missing",
			body: "# Review\n\n## Findings\n\nnone\n",
			ok:   false,
		},
		{
			name: "malformed",
			body: "# Review\n\n## Gate\n\n```json\n{\"status\":\n```\n",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := stageGateStatus(tc.body)
			if status != tc.status || ok != tc.ok {
				t.Fatalf("stageGateStatus = (%q, %v), want (%q, %v)", status, ok, tc.status, tc.ok)
			}
		})
	}
}

func TestReviewStageGateAcceptsReadyCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeStageCanvas(t, root, md, "review", `# Review

## Gate

`+"```json"+`
{"status":"ready"}
`+"```"+`
`)
	ok, err := reviewStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected review gate to pass on ready status")
	}
}

// reviewCanvasWithFollowups builds a ready review canvas whose
// "## Followups filed" section carries the given body.
func reviewCanvasWithFollowups(followups string) string {
	return "# Review\n\n## Gate\n\n```json\n{\"status\":\"ready\"}\n```\n\n## Followups filed\n\n" + followups + "\n"
}

// TestReviewStageGateEnforcesFollowupsSection: a ready canvas carrying
// the "## Followups filed" heading passes only when the section is
// filled — a real "None" statement or slug rows — and is refused while
// it still sits on the seeded placeholder.
func TestReviewStageGateEnforcesFollowupsSection(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	cases := []struct {
		name      string
		followups string
		want      bool
	}{
		{
			"placeholder refused",
			"(agent fills: one row per followup filed in the run's followups.md — `slug` — why it's deferred; or an explicit \"None — every finding was fixed in place, blocks the gate, or wasn't worth deferring.\")",
			false,
		},
		{
			"explicit none passes",
			"None — every finding was fixed in place, blocks the gate, or wasn't worth deferring.",
			true,
		},
		{
			"slug rows pass",
			"`pulse-noop-auto-closes-skeleton` — deferred to its own run",
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeStageCanvas(t, root, md, "review", reviewCanvasWithFollowups(tc.followups))
			ok, err := reviewStageGate(root, md)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want {
				t.Fatalf("reviewStageGate = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestReviewStageGateRefusesBlockedAndMalformedCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	for _, body := range []string{
		reviewCanvasSkeleton,
		"# Review\n\n## Gate\n\n```json\n{\"status\":\"blocked\"}\n```\n",
		"# Review\n\n## Gate\n\n```json\n{\"status\":\n```\n",
	} {
		writeStageCanvas(t, root, md, "review", body)
		ok, err := reviewStageGate(root, md)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("expected review gate to refuse canvas:\n%s", body)
		}
	}
}

// TestTestStageGateAcceptsFilledCanvas: a canvas with substantive
// content in both required sections satisfies the gate.
func TestTestStageGateAcceptsFilledCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, `# Test

## Gate

`+"```json"+`
{"status":"ready"}
`+"```"+`

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

## Gate

`+"```json"+`
{"status":"ready"}
`+"```"+`

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

func TestTestStageGateRequiresReadyStatus(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, `# Test

## Gate

`+"```json"+`
{"status":"blocked"}
`+"```"+`

## What was verified

ran tests

## What wasn't verified

nothing
`)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected gate to refuse: status is blocked")
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

// TestTestGateShipNone drives the no-ship declaration reader. The field
// only counts on a ready gate, only for the exact value "none", and
// every unreadable shape reports false — push treats a true here as
// permission to close the run and delete its sandbox.
func TestTestGateShipNone(t *testing.T) {
	gate := func(payload string) string {
		return "# Test\n\n## Gate\n\n```json\n" + payload + "\n```\n\n## What was verified\n\nran tests\n"
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"ready and ship none", gate(`{"status":"ready","ship":"none"}`), true},
		{"field order and spacing", gate(`{ "ship": "none", "status": "ready" }`), true},
		{"ready without ship", gate(`{"status":"ready"}`), false},
		{"ready with ship push", gate(`{"status":"ready","ship":"push"}`), false},
		{"ready with empty ship", gate(`{"status":"ready","ship":""}`), false},
		{"blocked with ship none", gate(`{"status":"blocked","ship":"none"}`), false},
		{"ship none without status", gate(`{"ship":"none"}`), false},
		{"malformed json", gate(`{"status":"ready","ship":`), false},
		{"no gate section", "# Test\n\n## What was verified\n\nran tests\n", false},
		{"unfilled skeleton", testCanvasSkeleton, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
			writeTestCanvas(t, root, md, tc.body)
			if got := testGateShipNone(root, md); got != tc.want {
				t.Fatalf("testGateShipNone = %v, want %v for canvas:\n%s", got, tc.want, tc.body)
			}
		})
	}
}

// TestTestGateShipNoneMissingCanvas: a run whose test stage never wrote
// a canvas — an operator pushing straight from code — carries no
// signal, so push keeps its existing behaviour.
func TestTestGateShipNoneMissingCanvas(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	if testGateShipNone(root, md) {
		t.Fatal("a missing test canvas must not read as a no-ship declaration")
	}
}

// TestTestStageGateIgnoresShipField: the advance gate parses status only,
// so the new field rides along without changing whether test advances.
func TestTestStageGateIgnoresShipField(t *testing.T) {
	root := t.TempDir()
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc"}
	writeTestCanvas(t, root, md, `# Test

## Gate

`+"```json"+`
{"status":"ready","ship":"none"}
`+"```"+`

## What was verified

ran `+"`go test ./...`"+`

## What wasn't verified

nothing — the run ships no project change
`)
	ok, err := testStageGate(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected the advance gate to ignore `ship` and pass on the ready status")
	}
}

// writeTestCanvas seeds the test stage's canvas file under root.
func writeTestCanvas(t *testing.T, root string, md *run.Metadata, body string) {
	t.Helper()
	writeStageCanvas(t, root, md, "test", body)
}

func writeStageCanvas(t *testing.T, root string, md *run.Metadata, stage, body string) {
	t.Helper()
	path := filepath.Join(root, run.ContentPath(md.Project, md.ID, stage))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
