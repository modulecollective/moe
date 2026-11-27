package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// The gate grammar is closed. A key nobody defined is a parse error that
// refuses the whole gate, because the harness can't tell a typo apart
// from a shape it doesn't understand.
func TestPulseGateRefusesUnknownKeys(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		// want is a substring of the refusal reason, which the apply
		// site interpolates into its warn — the operator reads that
		// instead of diffing the fence by eye.
		want string
	}{{
		name:    "the retired threads key",
		payload: `{"status":"ok","threads":[{"runs":["moe/change-auth"]}]}`,
		want:    `unknown field "threads"`,
	}, {
		name:    "the retired park key",
		payload: `{"status":"ok","park":"the ordering is a guess"}`,
		want:    `unknown field "park"`,
	}, {
		name:    "typo'd key on a loose spec",
		payload: `{"status":"ok","loose":[{"slug":"moe/fresh","titel":"Fresh"}]}`,
		want:    `unknown field "titel"`,
	}, {
		name:    "typo'd key at the top level",
		payload: `{"status":"ok","loos":[{"slug":"moe/fresh"}]}`,
		want:    `unknown field "loos"`,
	}, {
		name:    "typo'd key on next",
		payload: `{"status":"ok","next":{"run":"moe/x","why":"w","rapair":"ci"}}`,
		want:    `unknown field "rapair"`,
	}, {
		name:    "next without a project",
		payload: `{"status":"ok","next":{"run":"x","why":"w"}}`,
		want:    `expected <project>/<run>`,
	}, {
		name:    "next without a reason",
		payload: `{"status":"ok","next":{"run":"moe/x"}}`,
		want:    `"why" is required`,
	}, {
		name:    "next with an unknown repair",
		payload: `{"status":"ok","next":{"run":"moe/x","why":"w","repair":"lint"}}`,
		want:    `repair must be "ci"`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := spawnFixture(t)
			writePulseGate(t, root, "moe", "pulse-x", tc.payload)

			gate, err := readPulseGate(root, "moe", "pulse-x")
			if err == nil {
				t.Fatalf("gate parsed %s, want a refusal (got %+v)", tc.payload, gate)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to name %s", err, tc.want)
			}
		})
	}
}

// A sweep whose gate carries an unknown key is refused whole: nothing is
// minted, the run stays on the dash's ACTIVE list for review, and the
// sweep reports out non-zero with the offending key named.
func TestPulseSurveyRefusesGateWithATypodAsk(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, admission *pulseAdmission, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok",`+
			`"loose":[{"slug":"moe/fix-a","title":"Fix a","why":"because"}],`+
			`"next":{"run":"moe/fix-a","why":"because","sak":"typo"}}`)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil /*pi*/, io.Discard, &errb); code == 0 {
		t.Fatalf("survey exit=0 on a gate with an unknown key — stderr=%q", errb.String())
	}
	if open := openPulseRuns(t, root, "moe"); len(open) != 1 {
		t.Fatalf("open pulse runs = %v, want one left open by the refused gate", open)
	}
	// The refusal is all-or-nothing: the well-formed `loose` spec beside
	// the typo doesn't get minted either. Nothing in the fence said what
	// the writer meant, so there is no trustworthy remainder.
	mds, err := run.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, md := range mds {
		if md.Workflow != pulseWorkflow {
			t.Errorf("run %s (%s) minted off a refused gate, want none", md.ID, md.Workflow)
		}
	}
	if got := errb.String(); !strings.Contains(got, `unknown field "sak"`) {
		t.Errorf("stderr = %q, want it to name the unknown key", got)
	}
	// The refusal also names the way out. There is no reopen-and-fix
	// door for a sweep, so an operator told only "leaving the run open
	// for review" is told to do nothing in particular; the recovery is
	// close-then-run-another, in that order.
	for _, want := range []string{
		"hint: moe pulse close moe/",
		"hint: moe pulse new ",
	} {
		if got := errb.String(); !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want it to carry %q", got, want)
		}
	}
}

// spawnWhy reads gates written under older grammars and is documented to
// answer "" rather than fail for anything it can't match. A gate from
// the spawn/chain era already answered "" (its lists decoded empty, so
// nothing matched); under a strict decode it errors, and spawnWhy
// swallows that to the same "". No provenance regression.
func TestSpawnWhyStillTolerantOfOldGrammar(t *testing.T) {
	root := spawnFixture(t)
	writePulseGate(t, root, "moe", "pulse-x", `{"status":"ok",`+
		`"spawn":[{"slug":"fix-ci","why":"red CI"}],`+
		`"chain":[{"head":"h","runs":["fix-ci"]}]}`)

	if why := spawnWhy(root, "moe/pulse-x", "fix-ci"); why != "" {
		t.Errorf("spawnWhy = %q, want \"\" for a gate written under the old grammar", why)
	}
}
