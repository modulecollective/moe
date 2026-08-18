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
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
	"github.com/modulecollective/moe/internal/wiki"
)

// capturingFakeClaudeScript writes the canvas and then files one
// followup and one lore entry in the run's own scratch files — the shape
// every stage prompt's capture sections invite. The run dir is derived
// from the canvas path in the prompt (three levels up from
// documents/<doc>/content.md) because the session's cwd is the
// bureaucracy worktree, not the run dir.
const capturingFakeClaudeScript = `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
[ -n "$canvas" ] || exit 1
printf 'sharpened in conversation\n' >> "$canvas"

rundir=$(dirname "$(dirname "$(dirname "$canvas")")")
printf '%s\n' '- [ ] {{followup}} — Spotted mid-conversation' > "$rundir/followups.md"
mkdir -p "$rundir/feedback"
printf '%s\n' \
  '- [ ] {{lore}} — A portable fact' \
  '' \
  '  applies-when: a pid check runs from inside an agent sandbox' \
  '' \
  '  A stage sandbox gets its own PID namespace, so a host pid is absent.' \
  > "$rundir/feedback/lore.md"
exit 0
`

// quietFakeClaudeScript writes only the canvas — a session that captures
// nothing, which is what most session ends look like.
const quietFakeClaudeScript = `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
[ -n "$canvas" ] || exit 1
printf 'another turn\n' >> "$canvas"
exit 0
`

func capturingFakeClaude(t *testing.T, followupSlug, loreSlug string) {
	t.Helper()
	bt := "`"
	fakeClaudeOnPath(t, strings.NewReplacer(
		"{{followup}}", bt+followupSlug+bt,
		"{{lore}}", bt+loreSlug+bt,
	).Replace(capturingFakeClaudeScript))
}

// seedCaptureRun stands up a bureaucracy with one document-only capture
// run, ready for an `edit --chat` session.
func seedCaptureRun(t *testing.T, workflow, runID string) string {
	t.Helper()
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	gittest.Run(t, root, "add", "bureaucracy.conf")
	gittest.Run(t, root, "commit", "-m", "mark bureaucracy")
	trailerstest.SeedRun(t, root, "tele", runID, workflow, run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	return root
}

// assertHarvested is the shared claim of the three session-end tests:
// both captures left the run dir, and both were marked so a re-run
// won't repeat them.
func assertHarvested(t *testing.T, root, runID, followupSlug, loreSlug string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, run.Dir("tele", followupSlug), "run.json")); err != nil {
		t.Errorf("followup never reached an idea run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, wiki.LoreDirRel, loreSlug+".md")); err != nil {
		t.Errorf("lore entry never reached lore/: %v", err)
	}
	if got := readFollowups(t, root, "tele", runID); !strings.Contains(got, "- [x] `"+followupSlug+"`") {
		t.Errorf("followup not marked harvested:\n%s", got)
	}
	if got := readLoreFeedback(t, root, "tele", runID); !strings.Contains(got, "- [x] `"+loreSlug+"`") {
		t.Errorf("lore entry not marked harvested:\n%s", got)
	}
}

// TestIntentChatHarvestsAtSessionEnd is the run's central claim, driven
// end-to-end through the real session machinery: an `intent edit --chat`
// turn that files a followup and a lore entry has both fanned out by the
// time the operator is back at the shell. Before this change the entries
// were committed to the journal and stranded — intent close is a capture
// close and never harvests, and no manual verb reached an intent run.
//
// Both captures matter: the stranding incident that opened this run
// included a lore entry, so a session-end harvest that only did
// followups would still lose half of it.
func TestIntentChatHarvestsAtSessionEnd(t *testing.T) {
	root := seedCaptureRun(t, dash.IntentWorkflow, "sharpen-me")
	capturingFakeClaude(t, "chase-the-thing", "sandbox-pid-hidden")

	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "edit", "--chat", "tele/sharpen-me"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	assertHarvested(t, root, "sharpen-me", "chase-the-thing", "sandbox-pid-hidden")

	// A clean tree is not hygiene here — it's what lets the operator
	// close the intent afterwards. Capture close runs the strict
	// requireCleanTree with no scratch-path exemptions, so a harvest that
	// left rewrites uncommitted would wedge the close.
	if st := gittest.Output(t, root, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("session-end harvest left the tree dirty:\n%s", st)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"intent", "close", "tele/sharpen-me"}, &out, &errb); code != 0 {
		t.Fatalf("intent close after a harvested session: exit=%d stderr=%q", code, errb.String())
	}
}

// TestIdeaChatHarvestsAtSessionEnd is the sibling surface. The two
// `edit --chat` verbs are deliberately the same session shape, so the
// rule has to hold on both — and idea was additionally hard-refused by
// the manual verb, which left it with no path at all.
func TestIdeaChatHarvestsAtSessionEnd(t *testing.T) {
	root := seedCaptureRun(t, dash.IdeaWorkflow, "shelf-note")
	capturingFakeClaude(t, "chase-the-other-thing", "another-portable-fact")

	var out, errb bytes.Buffer
	if code := Run([]string{"idea", "edit", "--chat", "tele/shelf-note"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	assertHarvested(t, root, "shelf-note", "chase-the-other-thing", "another-portable-fact")
}

// TestChatHarvestsAtSessionEnd covers the perpetual surface. chat
// harvests at close too, so the claim here is about timing: the captures
// land while the thread is still open, instead of waiting for an archive
// that may be weeks away or never come. Unlike the two capture verbs
// this one runs with a sandbox clone, so it also proves the harvest sits
// clear of the boundary check.
func TestChatHarvestsAtSessionEnd(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	// SeedRun rewrites project.json to a bare `{"id":...}`, so the
	// submodule seed has to land after it or the sandbox clone finds no
	// remote to clone from.
	trailerstest.SeedRun(t, root, "tele", "ponder", chatWorkflow, run.StatusInProgress)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	noEditor(t)
	capturingFakeClaude(t, "chase-a-third-thing", "a-third-fact")

	var out, errb bytes.Buffer
	if code := openChat("tele", "ponder", "", &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	assertHarvested(t, root, "ponder", "chase-a-third-thing", "a-third-fact")

	// Harvest is not a terminal transition — the thread stays open.
	if st := runStatus(t, root, "tele", "ponder"); st != run.StatusInProgress {
		t.Fatalf("session-end harvest flipped run status to %q", st)
	}
}

// TestSessionEndHarvestIsIdempotent: a second session on the same run
// must not re-fan what the first one already harvested. Conversational
// runs get many session ends, so "nothing new" is the common case and
// has to stay a free no-op.
func TestSessionEndHarvestIsIdempotent(t *testing.T) {
	root := seedCaptureRun(t, dash.IntentWorkflow, "sharpen-me")
	capturingFakeClaude(t, "chase-once", "fact-once")

	var out, errb bytes.Buffer
	if code := Run([]string{"intent", "edit", "--chat", "tele/sharpen-me"}, &out, &errb); code != 0 {
		t.Fatalf("first session: exit=%d stderr=%q", code, errb.String())
	}
	firstLore := readLoreEntry(t, root, "fact-once")

	fakeClaudeOnPath(t, quietFakeClaudeScript)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"intent", "edit", "--chat", "tele/sharpen-me"}, &out, &errb); code != 0 {
		t.Fatalf("second session: exit=%d stderr=%q", code, errb.String())
	}

	ideas := 0
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Workflow == dash.IdeaWorkflow {
			ideas++
		}
	}
	if ideas != 1 {
		t.Fatalf("re-harvest fanned the same followup out %d times, want 1", ideas)
	}
	if got := readLoreEntry(t, root, "fact-once"); got != firstLore {
		t.Fatalf("re-harvest rewrote the promoted lore entry:\nfirst:\n%s\nsecond:\n%s", firstLore, got)
	}
}
