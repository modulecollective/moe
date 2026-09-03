package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
)

// unmarkedIn returns the subjects of the project-scoped commits in
// base..tip that machineAuthored reads as the operator's. It is the
// gate's own predicate, asked one commit at a time so a failure names
// what went unstamped instead of just saying "something did".
func unmarkedIn(t *testing.T, root, projectID, base, tip string) []string {
	t.Helper()
	out, err := git.Output(root, "log", base+".."+tip, "--format=%x00%B", "--", "projects/"+projectID)
	if err != nil {
		t.Fatalf("git log %s..%s: %v", base, tip, err)
	}
	var unmarked []string
	for body := range strings.SplitSeq(out, "\x00") {
		if strings.TrimSpace(body) == "" {
			continue
		}
		if machineAuthored(body) {
			continue
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
		unmarked = append(unmarked, subject)
	}
	return unmarked
}

// commitsIn counts the project-scoped commits in base..tip, so a test
// asserting "nothing in the range is unmarked" can also prove the range
// wasn't empty.
func commitsIn(t *testing.T, root, projectID, base, tip string) int {
	t.Helper()
	out, err := git.Output(root, "log", base+".."+tip, "--format=%H", "--", "projects/"+projectID)
	if err != nil {
		t.Fatalf("git log %s..%s: %v", base, tip, err)
	}
	return len(strings.Fields(out))
}

// pendingNoteText is the operator note the fixture below leaves pending
// on the idea. Distinctive enough to find in a rendered prompt.
const pendingNoteText = "Prefer the mechanical fix over the clever one."

// promotedRunID returns the id of the sdlc run the sweep promoted — the
// one sdlc run in the project that isn't the hand-built source.
func promotedRunID(t *testing.T, root, sourceID string) string {
	t.Helper()
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, md := range mds {
		if md.Project == "moe" && md.Workflow == "sdlc" && md.ID != sourceID {
			found = append(found, md.ID)
		}
	}
	if len(found) != 1 {
		t.Fatalf("sdlc runs besides %s = %v, want exactly the promoted one", sourceID, found)
	}
	return found[0]
}

// TestSweepLandsOnlyMachineMarkedCommits is the fixture-honesty
// regression, and the one test in this package that measures the
// heartbeat's predicate against commits production actually writes
// rather than ones a fixture stamped.
//
// The gate's sweep-exit walk refuses to advance its cursors when any
// commit in dispatch..exit fails machineAuthored. That is only a
// refusal on *operator* commits if a sweep's own commits are marked —
// and they were not: `Open run`, `work: start session for pulse`,
// `work: update pulse` and `Close pulse run` carried neither
// MoE-Consent nor MoE-Spawned-By, so every sweep refused, both cursors
// froze, and a quiet board cost a sweep every tick. The shipped
// heartbeat suite could not see it because its fixtures stamp the
// sweep's own commits with `MoE-Consent: dynamic`, a trailer production
// never wrote there.
//
// So this drives a real sweep — fake claude on PATH, in-process, under
// a dynamic ride, the same idiom TestTaggedFollowupHarvestPromoteGroom
// AndKick uses — and asks the gate's own question about the range it
// left behind. Its value is entirely in *not* being a fixture: any new
// journal commit a sweep learns to write shows up here unstamped.
//
// The sweep alone is not the whole range a sweep leaves behind. A groom
// that roots a ride owns every commit that ride's stage turns write too,
// so the thread here goes unparked and the fixture services the kick:
// the promoted run's design turn runs for real, delivers a note the
// promotion carried onto it, and nominates the run's close. That puts
// `input: carried`, `work: start session for design`, `work: update
// design`, `input: delivered` and the close inside base..tip — commits
// only an end-to-end dispatch can put there, and the ones a stamp
// regression at a stage emit site would show up in.
func TestSweepLandsOnlyMachineMarkedCommits(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "moe")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	// Operator-side setup: a run with a tagged follow-up, harvested into
	// an idea by hand. All of it lands below the measured range — a
	// hand-harvest is exactly the operator act the gate must keep
	// refusing.
	source, err := run.New(root, "moe", run.Options{ID: "source-run", Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	writeFollowups(t, root, "moe", source.ID, strings.Join([]string{
		"- [ ] `tagged-fix` (sdlc) — Apply the mechanical fix",
		"",
		"  The idea canvas is the only promotion seed.",
		"",
	}, "\n"))
	if err := harvestFollowups(root, "moe", source.ID, "sdlc", true); err != nil {
		t.Fatalf("harvestFollowups: %v", err)
	}
	if err := run.StageAndCommit(root, "test: land harvested audit line", run.FollowupsPath("moe", source.ID)); err != nil {
		t.Fatalf("commit harvested followup: %v", err)
	}
	// A note pending on the idea, still below the range: `input: note` is
	// the operator's own act and carries no consent trailer, which is
	// exactly where an unstamped commit belongs. Promotion then carries
	// it onto the destination run and the design turn delivers it — two
	// more emit sites the guard gets to see, both inside the range.
	if _, err := input.Add(root, "moe", "tagged-fix", pendingNoteText); err != nil {
		t.Fatalf("input.Add: %v", err)
	}

	// The gate would record this tip as the sweep's dispatch base.
	base, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}

	// The survey promotes the tagged idea (a run open plus the idea's own
	// status bump, and the carry of the pending note) and leaves the
	// thread unparked, so the sweep closes itself and self-kicks the
	// promoted run. The design turn that kick dispatches nominates the
	// run's close, which stops the cascade there — one real stage turn,
	// serviced, and no code stage to script. The `*)` fallback stays
	// `exit 1`: a cascade that walked past the nomination would fail the
	// pulse, and the exit-0 assertion below is what catches it.
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
    printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","threads":[{"runs":[{"slug":"tagged-fix","title":"Apply the mechanical fix","why":"captured followup clears the bar"}]}]}' "$ticks" > "$canvas"
    exit 0
    ;;
  */documents/design/content.md)
    printf '%s\n' '# Design' '' '## What I checked' '' 'The mechanical fix already landed upstream, so the run is moot.' '' '## Gate' '' "${ticks}json" '{"status":"close"}' "$ticks" > "$canvas"
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`)

	var out, errb bytes.Buffer
	defer withRideMode(rideDynamic)()
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil, &out, &errb); code != 0 {
		t.Fatalf("pulse exit=%d stderr=%q", code, errb.String())
	}

	tip, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	// Guard against a vacuous pass: an empty range has no unmarked
	// commits either. The floor is the sweep's own four (open, start,
	// turn, close), the promotion's two, the carry, and the ride's four
	// (design start, design turn, delivered, close).
	if n := commitsIn(t, root, "moe", base, tip); n < 11 {
		t.Fatalf("sweep landed %d project commits, want at least the open/start/turn/close + promote pair + carry "+
			"+ the ride's design start/turn/delivered/close; stderr=%q", n, errb.String())
	}
	if idea, err := run.Load(root, "moe", "tagged-fix"); err != nil {
		t.Fatal(err)
	} else if idea.Status != run.StatusPromoted {
		t.Fatalf("idea status=%q, want promoted — the promotion commits must be inside the measured range", idea.Status)
	}
	// The ride really ran and really concluded. Without these a kick that
	// silently stopped short — a tightened floor, a refused dispatch —
	// would still pass the unmarked check, on a range with no stage-turn
	// commits in it at all.
	promoted := promotedRunID(t, root, source.ID)
	if md, err := run.Load(root, "moe", promoted); err != nil {
		t.Fatal(err)
	} else if md.Status != run.StatusClosed {
		t.Fatalf("promoted run status=%q, want closed — the kicked design turn never nominated its close", md.Status)
	}
	f, err := input.Load(root, "moe", promoted)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Notes) != 1 || f.Notes[0].DeliveredTo != "design" {
		t.Fatalf("carried input = %+v, want one note delivered to design — "+
			"the delivery commit is the emit site this range exists to measure", f.Notes)
	}
	// And the delivery was substantive, not just stamped: the turn's
	// prompt snapshot is what the agent actually read.
	snap, err := os.ReadFile(filepath.Join(root, run.PromptPathFor("claude", "moe", promoted, "design")))
	if err != nil {
		t.Fatalf("design prompt snapshot: %v", err)
	}
	if !strings.Contains(string(snap), pendingNoteText) {
		t.Errorf("the design turn's prompt never carried the note it was marked as delivering")
	}
	if unmarked := unmarkedIn(t, root, "moe", base, tip); len(unmarked) != 0 {
		t.Errorf("the sweep's own range holds commits the gate reads as the operator's:\n  %s\n"+
			"every one of these freezes both cursors at sweep exit", strings.Join(unmarked, "\n  "))
	}
	// And the inverse, so the assertion above can't pass by the predicate
	// having gone blind: an operator commit in the same range still reads
	// as one.
	journalCommit(t, root, "moe", "operator: a note", "")
	after, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	if unmarked := unmarkedIn(t, root, "moe", base, after); len(unmarked) != 1 || unmarked[0] != "operator: a note" {
		t.Errorf("unmarked with an operator commit in range = %v, want exactly [operator: a note]", unmarked)
	}
}

// TestStageCommitsStampConsentOnlyInsideAWalk pins both halves of the
// stamp rule at the emit sites a ride passes through on every hop.
// Inside a walk the commit carries the ride level; outside one the
// message is byte-identical to what it was before the stamp landed,
// which is what keeps an operator's own `moe sdlc code` out of the
// machine-marked set.
func TestStageCommitsStampConsentOnlyInsideAWalk(t *testing.T) {
	for _, tc := range []struct {
		name string
		ride bool
		want string
	}{
		{name: "operator", ride: false},
		{name: "walk", ride: true, want: "MoE-Consent: static"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTestBureaucracy(t)
			md := &run.Metadata{ID: "stamp-me", Project: "tele", Workflow: "sdlc",
				Documents: map[string]*run.Document{}}
			if _, _, err := run.EnsureDocument(root, md, "code"); err != nil {
				t.Fatal(err)
			}
			if err := run.Save(root, md); err != nil {
				t.Fatal(err)
			}
			if tc.ride {
				t.Cleanup(withRideMode(rideStatic))
			}

			check := func(what string) {
				t.Helper()
				body := gitLogFormat(t, root, 1, "HEAD", "%B")
				switch {
				case tc.want == "" && strings.Contains(body, "MoE-Consent:"):
					t.Errorf("%s outside a walk carries a consent trailer:\n%s", what, body)
				case tc.want != "" && !strings.Contains(body, tc.want):
					t.Errorf("%s inside a walk is missing %q:\n%s", what, tc.want, body)
				}
			}

			if err := commitSessionStart(root, md, "code"); err != nil {
				t.Fatal(err)
			}
			check("work: start session for code")

			canvas := filepath.Join(root, run.ContentPath("tele", "stamp-me", "code"))
			if err := os.WriteFile(canvas, []byte("# hello\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := commitTurn(root, md, "code", 0); err != nil {
				t.Fatal(err)
			}
			check("work: update code")

			if err := commitAdvance(root, md, "code"); err != nil {
				t.Fatal(err)
			}
			check("advance: code")
		})
	}
}

// TestCloseStampsConsentOnlyInsideAWalk is the same pin for a close,
// which is more than one commit: enterTerminal harvests follow-ups
// first, so an `Open run` per filed idea lands beside the close itself.
// The whole close is one act, so the range it leaves behind is asserted
// whole rather than one HEAD body — that is also the shape the gate
// reads it in.
func TestCloseStampsConsentOnlyInsideAWalk(t *testing.T) {
	for _, tc := range []struct {
		name string
		ride bool
	}{
		{name: "operator"},
		{name: "walk", ride: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "close-me", "sdlc", run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			t.Setenv("NO_COLOR", "1")
			writeFollowups(t, root, "tele", "close-me", "- [ ] `harvested-fix` — Something the stage spotted\n")
			if err := run.StageAndCommit(root, "test: file a follow-up", run.FollowupsPath("tele", "close-me")); err != nil {
				t.Fatal(err)
			}
			reg, ok := lookupCloseRegistration("sdlc")
			if !ok {
				t.Fatal("sdlc has no registered close")
			}
			base, err := git.HEAD(root)
			if err != nil {
				t.Fatal(err)
			}
			if tc.ride {
				t.Cleanup(withRideMode(rideDynamic))
			}
			var errb bytes.Buffer
			if err := closeRunInProcess(root, "sdlc", reg.subject, reg.cleanup,
				"tele", "close-me", true /*skipEdit*/, &errb); err != nil {
				t.Fatalf("close: %v stderr=%q", err, errb.String())
			}
			tip, err := git.HEAD(root)
			if err != nil {
				t.Fatal(err)
			}
			// The close commit plus the harvested idea's open — if the
			// harvest didn't run, the assertions below prove much less.
			n := commitsIn(t, root, "tele", base, tip)
			if n < 2 {
				t.Fatalf("close landed %d project commits, want the close plus the harvested idea's open", n)
			}
			unmarked := unmarkedIn(t, root, "tele", base, tip)
			if tc.ride && len(unmarked) != 0 {
				t.Errorf("a ride's close left commits the gate reads as the operator's:\n  %s",
					strings.Join(unmarked, "\n  "))
			}
			if !tc.ride && len(unmarked) != n {
				t.Errorf("an operator close stamped %d of %d commits as machine-authored; "+
					"outside a walk the messages must be byte-identical to before the stamp landed", n-len(unmarked), n)
			}
		})
	}
}
