package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

// TestDesignOnlyTaggedIdeaRidesOneStageThenHolds is the carry
// end-to-end, and the operator's note on this run made mechanical: a
// tag stamped from the phone must not move past design until a human
// looks, and parking it at code is the signal. One dynamic sweep
// promotes the idea and rides exactly the design turn; the next sweep
// finds the run held; the advance marker the run page's chip writes is
// what releases it.
//
// The fake `claude` writes the pulse gate on the pulse canvas and the
// design canvas on the run's, so the ride is real rather than stubbed.
func TestDesignOnlyTaggedIdeaRidesOneStageThenHolds(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	if _, err := createIdea(root, "moe", "worth-a-think", "# Worth a think\n\nA brief, not a design.\n", "", trailers.Block{}); err != nil {
		t.Fatalf("createIdea: %v", err)
	}
	if code := Run([]string{"idea", "tag", "moe/worth-a-think", "--design-only"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("idea tag --design-only failed")
	}

	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
ticks=$(printf '\140\140\140')
case "$canvas" in
  */documents/pulse/content.md)
    printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","loose":[{"slug":"worth-a-think","title":"Worth a think","why":"the operator tagged it"}]}' "$ticks" > "$canvas"
    ;;
  *)
    printf '\n## What I decided\n\nOne recommendation, named.\n' >> "$canvas"
    ;;
esac
exit 0
`)

	var out, errb bytes.Buffer
	defer withRideMode(rideDynamic)()
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil, &out, &errb); code != 0 {
		t.Fatalf("pulse exit=%d; stderr=%q", code, errb.String())
	}

	idea, err := run.Load(root, "moe", "worth-a-think")
	if err != nil {
		t.Fatal(err)
	}
	if idea.Status != run.StatusPromoted {
		t.Fatalf("idea status=%q, want promoted", idea.Status)
	}
	var promoted *run.Metadata
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == "sdlc" {
			promoted = md
		}
	}
	if promoted == nil {
		t.Fatal("no sdlc run minted")
	}
	// The shape a design-only spawn mints, reached from the other side.
	// Everything downstream reads only these two.
	if !promoted.DesignOnly || promoted.SpawnedBy == "" {
		t.Fatalf("promoted run design_only=%v spawned_by=%q, want the design-only spawn's shape",
			promoted.DesignOnly, promoted.SpawnedBy)
	}
	if !strings.Contains(errb.String(), "pulse: kicking moe/"+promoted.ID+" (dynamic, design only)") {
		t.Fatalf("pulse should ride the design turn and say the ride is short; stderr=%q", errb.String())
	}

	// Design landed; nothing walked to code.
	designDoc := filepath.Join(root, run.ContentPath("moe", promoted.ID, "design"))
	body, err := os.ReadFile(designDoc)
	if err != nil {
		t.Fatalf("design canvas: %v", err)
	}
	if !strings.Contains(string(body), "One recommendation, named.") {
		t.Fatalf("design turn never landed:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(root, run.ContentPath("moe", promoted.ID, "code"))); err == nil {
		t.Fatal("a code canvas exists — the ride was supposed to stop at design")
	}

	// The next sweep finds it held. This is the operator's brake: the
	// run sits at design until they read it.
	t.Chdir(root)
	groomed := groomChains(root, "moe", "pulse-groom", nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, io.Discard)
	if got := kickFloorHold(root, "moe/"+promoted.ID, groomed); got != designOnlyHeldReason {
		t.Fatalf("kickFloorHold = %q, want %q", got, designOnlyHeldReason)
	}

	// "Park it at code" is the advance chip — the same marker
	// runopen.MarkAdvanced writes — and it is what licenses the rest.
	if err := runopen.MarkAdvanced(root, "moe", promoted.ID, "design", io.Discard, io.Discard); err != nil {
		t.Fatalf("MarkAdvanced: %v", err)
	}
	groomed = groomChains(root, "moe", "pulse-groom", nil /*empty gate*/, nil /*kickoff edges*/, io.Discard, io.Discard)
	if got := kickFloorHold(root, "moe/"+promoted.ID, groomed); got != "" {
		t.Fatalf("kickFloorHold after advance = %q, want the hold released", got)
	}
}
