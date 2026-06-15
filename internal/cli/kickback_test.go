package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

const blockedReviewCanvas = `# Review

## Gate

` + "```json" + `
{"status":"blocked"}
` + "```" + `

## Findings

The retry loop in foo.go:42 never resets the counter — it spins forever.

## Evidence Reviewed

diff of foo.go; ran go test ./internal/foo.
`

// stubKickback swaps openKickbackSession for a recorder and restores it
// on cleanup. Returns pointers the caller asserts against after driving
// the prompt.
func stubKickback(t *testing.T) (ran *bool, doc *string) {
	t.Helper()
	var fired bool
	var gotDoc string
	old := openKickbackSession
	openKickbackSession = func(_ *run.Metadata, document, _, _ string, _, _ io.Writer) int {
		fired = true
		gotDoc = document
		return 0
	}
	t.Cleanup(func() { openKickbackSession = old })
	return &fired, &gotDoc
}

func sdlcGroup(t *testing.T) *CommandGroup {
	t.Helper()
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestBuildKickbackKickoff: the agent-facing kickoff names the blocked
// stage, inlines the canvas verbatim (findings and all), and tells the
// agent the chain will re-offer that stage after the fix.
func TestBuildKickbackKickoff(t *testing.T) {
	got := buildKickbackKickoff("sdlc", "review", blockedReviewCanvas)
	for _, want := range []string{
		"`moe sdlc review` closed with a blocked gate",
		"the review stage found a problem",
		"The retry loop in foo.go:42 never resets the counter", // inlined findings
		"re-offer `moe sdlc review`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kickoff missing %q:\n%s", want, got)
		}
	}
}

// TestPromptKickbackDefaultsToCode: blank (reflex Enter) and an explicit
// `y` both kick back to code — the common "the reviewer is right, go fix
// the code" path is a single keystroke.
func TestPromptKickbackDefaultsToCode(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{name: "blank", input: ""},
		{name: "explicit-y", input: "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran, doc := stubKickback(t)
			feedStdin(t, tc.input)
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			var stdout, stderr bytes.Buffer
			if code := promptKickback(sdlcGroup(t), nil, md, "review", blockedReviewCanvas, &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			if !*ran {
				t.Fatalf("expected a kickback dispatch, got none (stdout=%q)", stdout.String())
			}
			if *doc != "code" {
				t.Fatalf("kickback document = %q, want code", *doc)
			}
		})
	}
}

// TestPromptKickbackDToDesign: `d` routes the kickback to design instead
// of code — the escape hatch for "this is a design miss, not an impl bug."
func TestPromptKickbackDToDesign(t *testing.T) {
	ran, doc := stubKickback(t)
	feedStdin(t, "d")
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	var stdout, stderr bytes.Buffer
	if code := promptKickback(sdlcGroup(t), nil, md, "test", blockedReviewCanvas, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !*ran || *doc != "design" {
		t.Fatalf("expected kickback to design, ran=%v doc=%q", *ran, *doc)
	}
}

// TestPromptKickbackNParks: `n` declines without dispatching — the run
// stays parked at the blocked stage for the operator to return to.
func TestPromptKickbackNParks(t *testing.T) {
	ran, _ := stubKickback(t)
	feedStdin(t, "n")
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	var stdout, stderr bytes.Buffer
	if code := promptKickback(sdlcGroup(t), nil, md, "review", blockedReviewCanvas, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if *ran {
		t.Fatalf("`n` must not dispatch a kickback")
	}
}

// TestPromptKickbackScuttle: `x` dispatches the scuttle (close) command
// with [project/run] and never fires a kickback.
func TestPromptKickbackScuttle(t *testing.T) {
	ran, _ := stubKickback(t)
	var scuttleRan bool
	var scuttleArgs []string
	scuttle := &Command{
		Name: "close",
		Run: func(args []string, _, _ io.Writer) int {
			scuttleRan = true
			scuttleArgs = append([]string(nil), args...)
			return 0
		},
	}
	feedStdin(t, "x")
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	var stdout, stderr bytes.Buffer
	if code := promptKickback(sdlcGroup(t), scuttle, md, "review", blockedReviewCanvas, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if *ran {
		t.Fatalf("`x` must not dispatch a kickback")
	}
	if !scuttleRan {
		t.Fatalf("expected scuttle to dispatch on `x`")
	}
	if got, want := strings.Join(scuttleArgs, " "), "tele/fix-it"; got != want {
		t.Fatalf("scuttle args = %q, want %q", got, want)
	}
}

// TestPromptKickbackPrintsCanvasAndLabel: the blocked canvas is printed
// verbatim above the [Y/n/d/x] menu so the operator reads the findings
// before choosing, and the legend names every option.
func TestPromptKickbackPrintsCanvasAndLabel(t *testing.T) {
	stubKickback(t)
	feedStdin(t, "n")
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	scuttle := &Command{Name: "close", Run: func(_ []string, _, _ io.Writer) int { return 0 }}
	var stdout, stderr bytes.Buffer
	if code := promptKickback(sdlcGroup(t), scuttle, md, "review", blockedReviewCanvas, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, blockedReviewCanvas) {
		t.Errorf("canvas not printed verbatim:\n%s", got)
	}
	if !strings.Contains(got, "review blocked — kick back to fix? [Y/n/d/x]") {
		t.Errorf("expected kickback label, got:\n%s", got)
	}
	if !strings.Contains(got, "Y = kick back to code · n = decline (park) · d = kick back to design · x = scuttle (close)") {
		t.Errorf("expected full legend, got:\n%s", got)
	}
	if i, j := strings.Index(got, blockedReviewCanvas), strings.Index(got, "[Y/n/d/x]"); i < 0 || j < 0 || i >= j {
		t.Errorf("canvas should appear above the prompt; canvas=%d prompt=%d", i, j)
	}
}

// TestPromptKickbackNoDesignDropsD: a workflow group without a design
// command drops `d` from the label, and typing `d` collapses to a
// decline (no dispatch). Pins the design feature-gate so the menu stays
// honest for a future sdlc-shaped workflow without a design stage.
func TestPromptKickbackNoDesignDropsD(t *testing.T) {
	ran, _ := stubKickback(t)
	g := NewCommandGroup("sdlc-nodesign", "")
	g.Register(&Command{Name: "code", Run: func(_ []string, _, _ io.Writer) int { return 0 }})
	feedStdin(t, "d")
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	var stdout, stderr bytes.Buffer
	if code := promptKickback(g, nil, md, "review", blockedReviewCanvas, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "[Y/n]") {
		t.Errorf("expected [Y/n] label without /d or /x, got:\n%s", got)
	}
	if strings.Contains(got, "/d") {
		t.Errorf("design-less group must not offer /d, got:\n%s", got)
	}
	if *ran {
		t.Errorf("`d` with no design command must decline, not dispatch")
	}
}

// TestPromptNextStageOverrideKickbackRouting drives the routing decision
// at the chain-prompt entry point. Against non-terminal stdin the
// blocked branch prints a back-pointing nudge (`next: moe sdlc code …`),
// while a ready gate keeps today's forward nudge (`next: moe sdlc
// test …`). This is the guard that the kickback reshaping only fires on
// `blocked`, and that the non-TTY caller never auto-walks forward past a
// block.
func TestPromptNextStageOverrideKickbackRouting(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{name: "blocked kicks back", status: "blocked", want: "next: moe sdlc code tele/fix-it (review blocked — kick back to fix)"},
		{name: "ready walks forward", status: "ready", want: "next: moe sdlc test tele/fix-it"},
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devnull.Close() })
	oldStdin := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() { os.Stdin = oldStdin })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "review"))
			if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
				t.Fatal(err)
			}
			body := "# Review\n\n## Gate\n\n```json\n{\"status\":\"" + tc.status + "\"}\n```\n\n## Findings\n\nx\n"
			if err := os.WriteFile(canvas, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
			var stdout, stderr bytes.Buffer
			if code := promptNextStageOverride(root, md, "review", "", &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("want %q, got: %q", tc.want, stdout.String())
			}
		})
	}
}

// TestPromptNextStageOverrideRecoveryReoffersGate pins the no-double-
// kickback contract directly. A kickback opens code/design with
// NextStageOverride set to the blocked stage; when that recovery turn
// closes, the chain re-enters promptNextStageOverride with
// justFinished="code" and override="review". Even with the blocked
// review canvas still on disk, the prompt must re-offer the review gate
// (`next: moe sdlc review …`) — NOT reshape into another kickback. This
// guards the override-set / justFinished-not-review-test path that stops
// the post-fix loop from recursing into a second kickback offer.
func TestPromptNextStageOverrideRecoveryReoffersGate(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devnull.Close() })
	oldStdin := os.Stdin
	os.Stdin = devnull
	t.Cleanup(func() { os.Stdin = oldStdin })

	root := t.TempDir()
	// The blocked review canvas is still on disk from the gate that
	// kicked back — if the guard regressed, the prompt would re-read it
	// and re-offer a kickback instead of the gate.
	canvas := filepath.Join(root, run.ContentPath("tele", "fix-it", "review"))
	if err := os.MkdirAll(filepath.Dir(canvas), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canvas, []byte(blockedReviewCanvas), 0o644); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "fix-it", Project: "tele", Workflow: "sdlc", Status: run.StatusInProgress}
	var stdout, stderr bytes.Buffer
	// Recovery turn: justFinished="code", override="review".
	if code := promptNextStageOverride(root, md, "code", "review", &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	got := stdout.String()
	if want := "next: moe sdlc review tele/fix-it"; !strings.Contains(got, want) {
		t.Fatalf("recovery turn must re-offer the review gate; want %q, got: %q", want, got)
	}
	if strings.Contains(got, "kick back to fix") {
		t.Fatalf("recovery turn must not reshape into a kickback, got: %q", got)
	}
}
