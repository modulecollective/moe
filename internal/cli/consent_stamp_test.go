package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
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

	// The gate would record this tip as the sweep's dispatch base.
	base, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}

	// The survey promotes the tagged idea (a run open plus the idea's own
	// status bump) and parks the thread, so the sweep grooms and closes
	// without rooting a ride the fake claude would have to service.
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
    printf '%s\n' '# Pulse' '' '## Gate' '' "${ticks}json" '{"status":"ok","threads":[{"park":"holding for the operator","runs":[{"slug":"tagged-fix","title":"Apply the mechanical fix","why":"captured followup clears the bar"}]}]}' "$ticks" > "$canvas"
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
	// commits either. The sweep's own four plus the promotion's two are
	// the floor.
	if n := commitsIn(t, root, "moe", base, tip); n < 6 {
		t.Fatalf("sweep landed %d project commits, want at least the open/start/turn/close + promote pair; stderr=%q", n, errb.String())
	}
	if idea, err := run.Load(root, "moe", "tagged-fix"); err != nil {
		t.Fatal(err)
	} else if idea.Status != run.StatusPromoted {
		t.Fatalf("idea status=%q, want promoted — the promotion commits must be inside the measured range", idea.Status)
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
			if err := commitTurn(root, md, "code"); err != nil {
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
			var out, errb bytes.Buffer
			if err := closeRunInProcess(root, "sdlc", reg.subject, reg.cleanup,
				"tele", "close-me", true /*skipEdit*/, &out, &errb); err != nil {
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
