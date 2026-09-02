package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// writeTwinFeedback drops a feedback/twin.md alongside run.json without
// committing — sibling of writeLoreFeedback, same reasoning.
func writeTwinFeedback(t *testing.T, root, projectID, runID, body string) string {
	t.Helper()
	rel := run.FeedbackPath(projectID, runID, "twin")
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return abs
}

func readTwinFeedback(t *testing.T, root, projectID, runID string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, run.FeedbackPath(projectID, runID, "twin")))
	if err != nil {
		t.Fatalf("read feedback/twin.md: %v", err)
	}
	return string(body)
}

// TestHarvestTwinFeedbackMintsIdea is the channel's whole contract: an
// unchecked twin note becomes an idea run whose canvas carries the
// title, the provenance line, and the body, and the source line is
// marked with the resolved slug. Before this, twin notes sat in the
// file until a reflect pass read them; there is no pass now, so a note
// nobody schedules is a note nobody acts on.
func TestHarvestTwinFeedbackMintsIdea(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeTwinFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `architecture-clone-gc` — architecture.md still says reflect owns clone gc",
		"",
		"  operations.md moved gc to `moe clone gc`; architecture never",
		"  caught up.",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	canvas, err := os.ReadFile(filepath.Join(root, run.ContentPath("tele", "architecture-clone-gc", "idea")))
	if err != nil {
		t.Fatalf("read minted idea canvas: %v", err)
	}
	for _, want := range []string{
		"# architecture.md still says reflect owns clone gc",
		"Twin observation from run `tele/ship-it`.",
		"Acting on it edits `projects/tele/digital-twin/`.",
		"operations.md moved gc to `moe clone gc`",
	} {
		if !strings.Contains(string(canvas), want) {
			t.Errorf("idea canvas missing %q:\n%s", want, canvas)
		}
	}
	if got := readTwinFeedback(t, root, "tele", "ship-it"); !strings.Contains(got, "- [x] `architecture-clone-gc`") {
		t.Fatalf("twin note not marked harvested:\n%s", got)
	}
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("harvest left the tree dirty:\n%s", st)
	}
	if files := gitLog(t, root, "-1", "--name-only", "--format="); !strings.Contains(files, "projects/tele/runs/ship-it/feedback/twin.md") {
		t.Errorf("harvest commit missing the rewritten twin file:\n%s", files)
	}
}

// TestHarvestTwinFeedbackIsIdempotent: session-end harvest reruns on
// every exit, so an all-`[x]` file has to cost nothing.
func TestHarvestTwinFeedbackIsIdempotent(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeTwinFeedback(t, root, "tele", "ship-it", "- [ ] `one-note` — One observation\n\n  Body.\n")

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("first harvest: exit=%d stderr=%q", code, errb.String())
	}
	afterFirst := gitLog(t, root, "-1", "--format=%H")

	out.Reset()
	errb.Reset()
	if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("second harvest: exit=%d stderr=%q", code, errb.String())
	}
	if afterSecond := gitLog(t, root, "-1", "--format=%H"); afterSecond != afterFirst {
		t.Fatalf("idempotent re-run created a commit:\nfirst=%s second=%s", afterFirst, afterSecond)
	}
}

// TestHarvestRejectsLegacyTwinProse is the transition cost, made
// explicit. Twin notes used to be free-form prose separated by `---`.
// Under the checklist grammar such a file trips parseChecklist's
// stray-content backstop and fails loud, naming the file — the correct
// answer, since silently dropping an operator's note is the failure
// mode the backstop exists for.
func TestHarvestRejectsLegacyTwinProse(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeTwinFeedback(t, root, "tele", "ship-it",
		"architecture.md says X is invariant, but pkg/foo.go:42 does Y.\nDecide which is canon.\n")

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code == 0 {
		t.Fatalf("expected refusal, got exit=0\nstdout=%q", out.String())
	}
	got := errb.String()
	for _, want := range []string{"feedback/twin.md", "twin observation", "- [ ] `slug` — Title"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal missing %q:\n%s", want, got)
		}
	}
}

// TestParseTwinFeedbackRejectsProjectPrefix: parseChecklist's shared
// regex permits a `<project>/` prefix so followups can route across
// projects. A twin note has nowhere to route — the provenance line the
// harvest writes names *this* project's twin — so the prefix is a typo
// worth failing on, not a silent mis-file.
func TestParseTwinFeedbackRejectsProjectPrefix(t *testing.T) {
	_, _, err := parseTwinFeedback([]byte("- [ ] `claudia/some-note` — A note\n"))
	if err == nil {
		t.Fatal("expected an error for a prefixed twin slug")
	}
	if !strings.Contains(err.Error(), "must not contain '/'") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestParseTwinFeedbackValidatesTag pins the tag rule: a twin note
// carries the same optional workflow tag a followup does, validated the
// same way. Twin-ness rides in the harvested idea's body, so `(sdlc)` is
// the only tag a filer has reason to write and anything unregistered
// fails loud rather than being silently ignored.
func TestParseTwinFeedbackValidatesTag(t *testing.T) {
	_, todo, err := parseTwinFeedback([]byte("- [ ] `fix-arch` (sdlc) — Fix architecture.md\n"))
	if err != nil {
		t.Fatalf("(sdlc) tag rejected: %v", err)
	}
	if len(todo) != 1 || todo[0].promoteTo != "sdlc" {
		t.Fatalf("tag not captured: %+v", todo)
	}
	if _, _, err := parseTwinFeedback([]byte("- [ ] `fix-arch` (nosuchflow) — Fix architecture.md\n")); err == nil {
		t.Fatal("expected an unregistered tag to be rejected")
	}
}
