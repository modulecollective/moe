package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// runStatus reads a run's on-disk status straight from run.json so a
// harvest test can prove the status flip never happened.
func runStatus(t *testing.T, root, projectID, runID string) string {
	t.Helper()
	md, err := run.Load(root, projectID, runID)
	if err != nil {
		t.Fatalf("load run %s/%s: %v", projectID, runID, err)
	}
	return md.Status
}

// TestHarvestCommandRegistered confirms the verb landed in every
// workflow group that can host an agent session — the capture
// workflows included, since `edit --chat` lets a session file captures
// on a run whose close deliberately skips harvest.
func TestHarvestCommandRegistered(t *testing.T) {
	for _, wf := range []string{"sdlc", "chat", "idea", "intent"} {
		g, ok := groups[wf]
		if !ok {
			t.Fatalf("group %q not registered", wf)
		}
		if g.Lookup("harvest") == nil {
			t.Errorf("%s: expected a harvest subcommand", wf)
		}
	}
}

// TestHarvestArgKindPerWorkflow pins the completion token kind: the
// capture workflows have their own candidate sources, and
// argProjectRun deliberately excludes their runs — so wiring them to it
// would leave `moe idea harvest <tab>` offering nothing.
func TestHarvestArgKindPerWorkflow(t *testing.T) {
	cases := map[string]argKind{
		"sdlc":              argProjectRun,
		dash.IdeaWorkflow:   argIdea,
		dash.IntentWorkflow: argIntent,
	}
	for wf, want := range cases {
		if got := harvestCommand(wf).argKind; got != want {
			t.Errorf("%s harvest argKind = %v, want %v", wf, got, want)
		}
	}
}

// TestHarvestFreshEntries is the happy path: fresh `- [ ]` entries fan
// out into ideas, the lines flip to `- [x]`, followups.md commits, and
// the run's status is left untouched.
func TestHarvestFreshEntries(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"- [ ] `chase-zlib` — Chase the zlib upgrade",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "harvested sdlc tele/ship-it") {
		t.Fatalf("missing harvest confirmation: %q", out.String())
	}

	for _, slug := range []string{"cleanup-foo", "chase-zlib"} {
		if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", slug, "run.json")); err != nil {
			t.Fatalf("expected idea %s: %v", slug, err)
		}
	}

	got := readFollowups(t, root, "tele", "ship-it")
	for _, want := range []string{
		"- [x] `cleanup-foo` — Clean up foo helper",
		"- [x] `chase-zlib` — Chase the zlib upgrade",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in followups.md after harvest:\n%s", want, got)
		}
	}

	// The rewritten file rides a harvest commit, and run.json status is
	// untouched — harvest does not flip the run.
	head := gitLog(t, root, "-1", "--format=%s%n%b")
	if !strings.Contains(head, "harvest: capture follow-ups, lore, and twin observations for tele/ship-it") {
		t.Fatalf("HEAD is not the harvest commit:\n%s", head)
	}
	for _, want := range []string{"MoE-Run: ship-it", "MoE-Project: tele", "MoE-Workflow: sdlc"} {
		if !strings.Contains(head, want) {
			t.Fatalf("harvest commit missing trailer %q:\n%s", want, head)
		}
	}
	if st := runStatus(t, root, "tele", "ship-it"); st != run.StatusInProgress {
		t.Fatalf("harvest must not flip run status, got %q", st)
	}
}

// TestHarvestIsIdempotent proves a re-run over an already-harvested
// (all-`[x]`) file is a clean no-op: no error, no new commit.
func TestHarvestIsIdempotent(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `cleanup-foo` — Clean up foo helper\n")

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

// TestHarvestOnClosedRun is the reported scenario: a stage re-run
// regenerates followups.md on an already-closed run; close refuses
// (terminal), but harvest still fans the entries out.
func TestHarvestOnClosedRun(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusClosed)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `late-idea` — Regenerated after close\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code != 0 {
		t.Fatalf("harvest on closed run should succeed, exit=%d stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "late-idea", "run.json")); err != nil {
		t.Fatalf("expected idea harvested from closed run: %v", err)
	}
	if st := runStatus(t, root, "tele", "ship-it"); st != run.StatusClosed {
		t.Fatalf("harvest must leave closed status intact, got %q", st)
	}
}

// TestHarvestRejectsStrayContent proves the close-time backstop surfaces
// identically through the harvest verb.
func TestHarvestRejectsStrayContent(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- clean up foo without a checkbox\n")

	var out, errb bytes.Buffer
	code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected non-zero on stray content, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "has content but no") {
		t.Fatalf("expected stray-content error, got: %q", errb.String())
	}
}

// TestHarvestEditorPreflight pins the editor gating, matching close:
// the pop fires by default when an unchecked entry exists, and --no-edit
// skips it.
func TestHarvestEditorPreflight(t *testing.T) {
	t.Run("pops by default on unchecked entry", func(t *testing.T) {
		root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
		t.Setenv("MOE_HOME", root)
		t.Setenv("NO_COLOR", "1")
		editorCalls := markerEditor(t)

		writeFollowups(t, root, "tele", "ship-it", "- [ ] `chase-it` — Chase the thing\n")

		var out, errb bytes.Buffer
		if code := Run([]string{"sdlc", "harvest", "tele/ship-it"}, &out, &errb); code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
		if n := editorCalls(); n != 1 {
			t.Fatalf("expected exactly one editor pop, got %d", n)
		}
	})

	t.Run("--no-edit skips the pop", func(t *testing.T) {
		root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
		t.Setenv("MOE_HOME", root)
		t.Setenv("NO_COLOR", "1")
		editorCalls := markerEditor(t)

		writeFollowups(t, root, "tele", "ship-it", "- [ ] `chase-it` — Chase the thing\n")

		var out, errb bytes.Buffer
		if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
			t.Fatalf("exit=%d stderr=%q", code, errb.String())
		}
		if n := editorCalls(); n != 0 {
			t.Fatalf("expected no editor pop under --no-edit, got %d", n)
		}
	})
}

// TestHarvestRejectsWrongWorkflow guards the workflow match: a sdlc run
// can't be harvested through another workflow's verb.
func TestHarvestRejectsWrongWorkflow(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"chat", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb)
	if code == 0 {
		t.Fatalf("expected refusal harvesting a sdlc run as chat, stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "is a sdlc run, not chat") {
		t.Fatalf("expected wrong-workflow error, got: %q", errb.String())
	}
}

// TestHarvestPromotesLore is the gap the verb shipped with: lore was
// left out with a note that a separate verb could cover it if it ever
// bit. A conversational session that stranded a lore entry alongside
// its followups is that bite — so the one verb fans out both, in
// enterTerminal's order and with its staging (the promoted lore/ file
// rides the same commit as the rewritten scratch files).
func TestHarvestPromotesLore(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeFollowups(t, root, "tele", "ship-it", "- [ ] `chase-zlib` — Chase the zlib upgrade\n")
	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `sandbox-pid-hidden` — A sandbox pid check false-negatives",
		"",
		"  applies-when: code decides liveness from a recorded pid inside a sandbox",
		"",
		"  A stage sandbox gets its own PID namespace.",
		"",
	}, "\n"))

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "harvest", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}

	if entry := readLoreEntry(t, root, "sandbox-pid-hidden"); !strings.Contains(entry, "own PID namespace") {
		t.Fatalf("promoted lore entry missing its body:\n%s", entry)
	}
	if got := readLoreFeedback(t, root, "tele", "ship-it"); !strings.Contains(got, "- [x] `sandbox-pid-hidden`") {
		t.Fatalf("lore entry not marked harvested:\n%s", got)
	}
	// Both scratch files and the promoted lore file ride one commit —
	// which is also what leaves the tree clean for a later close.
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("harvest left the tree dirty:\n%s", st)
	}
	files := gitLog(t, root, "-1", "--name-only", "--format=")
	for _, want := range []string{
		"projects/tele/runs/ship-it/followups.md",
		"projects/tele/runs/ship-it/feedback/lore.md",
		"lore/sandbox-pid-hidden.md",
	} {
		if !strings.Contains(files, want) {
			t.Errorf("harvest commit missing %s:\n%s", want, files)
		}
	}
}

// TestHarvestLoreIsIdempotent: a second pass over an all-`[x]` lore
// file is a clean no-op. Session-end harvest reruns on every session
// exit, so "nothing new" has to cost nothing — no error, no commit, no
// re-promoted entry.
func TestHarvestLoreIsIdempotent(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `one-fact` — One portable fact",
		"",
		"  applies-when: always",
		"",
		"  The body.",
		"",
	}, "\n"))

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

// TestHarvestAcceptsCaptureRuns: the idea refusal the verb shipped with
// ("the run *is* the capture") died with `idea edit --chat`. A capture
// run's close never harvests, so a refusal here would leave a session's
// captures with no path out at all.
func TestHarvestAcceptsCaptureRuns(t *testing.T) {
	for _, wf := range []string{dash.IdeaWorkflow, dash.IntentWorkflow} {
		t.Run(wf, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "sharpen-me", wf, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")

			writeFollowups(t, root, "tele", "sharpen-me", "- [ ] `spotted-this` — Spotted mid-conversation\n")

			var out, errb bytes.Buffer
			if code := Run([]string{wf, "harvest", "--no-edit", "tele/sharpen-me"}, &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if _, err := os.Stat(filepath.Join(root, "projects", "tele", "runs", "spotted-this", "run.json")); err != nil {
				t.Fatalf("expected the capture run's followup to fan out: %v", err)
			}
		})
	}
}
