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
// from a shape it doesn't understand — and every one of these used to
// decode to something *quieter* than what the survey wrote. The seed's
// case is the first row: a misspelled `ask` reads as a bare position, so
// the question the survey wrote for the operator is never opened and
// nothing on stderr says so.
func TestPulseGateRefusesUnknownKeys(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		// want is a substring of the refusal reason, which the apply
		// site interpolates into its warn — the operator reads that
		// instead of diffing the fence by eye.
		want string
	}{{
		name:    "typo'd ask on an ask entry",
		payload: `{"status":"ok","threads":[{"runs":[{"run":"change-auth","sak":"Which policy?"}]}]}`,
		want:    `unknown field "sak"`,
	}, {
		// The v1 spelling, `{"run": …, "park": {"question": …}}`. It is
		// no more decodable than a typo and fails the same way: a survey
		// still writing it gets told, rather than having the question
		// swallowed and the position read as bare.
		name:    "the v1 park-object ask",
		payload: `{"status":"ok","threads":[{"runs":[{"run":"change-auth","park":{"question":"Which policy?"}}]}]}`,
		want:    `unknown field "park"`,
	}, {
		name:    "typo'd park on the thread",
		payload: `{"status":"ok","threads":[{"runs":["change-auth"],"prak":"the ordering is a guess"}]}`,
		want:    `unknown field "prak"`,
	}, {
		name:    "ask on a mint spec, where it does not exist",
		payload: `{"status":"ok","threads":[{"runs":[{"slug":"fresh","title":"Fresh","ask":"q?"}]}]}`,
		want:    `unknown field "ask"`,
	}, {
		name:    "typo'd key on a loose spec",
		payload: `{"status":"ok","loose":[{"slug":"fresh","titel":"Fresh"}]}`,
		want:    `unknown field "titel"`,
	}, {
		name:    "typo'd key at the top level",
		payload: `{"status":"ok","loos":[{"slug":"fresh"}]}`,
		want:    `unknown field "loos"`,
	}, {
		// `{"run": ""}` used to fall through to the spec branch and drop
		// the entry — and any question on it — behind an unusable-slug
		// warn. Same silence, refused the same way.
		name:    "empty run",
		payload: `{"status":"ok","threads":[{"runs":[{"run":"","ask":"q?"}]}]}`,
		want:    `empty "run"`,
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

// A refusal names the entry it came from. One typo in a long thread is
// otherwise a hunt.
func TestPulseGateRefusalNamesTheEntry(t *testing.T) {
	root := spawnFixture(t)
	writePulseGate(t, root, "moe", "pulse-x", `{"status":"ok","threads":[{"runs":[`+
		`"tidy-1",{"run":"change-auth","sak":"Which policy?"}]}]}`)

	_, err := readPulseGate(root, "moe", "pulse-x")
	if err == nil {
		t.Fatal("gate parsed, want a refusal")
	}
	if !strings.Contains(err.Error(), `"change-auth"`) {
		t.Errorf("refusal = %q, want it to name the change-auth entry", err)
	}
}

// The grammar the stage fragment teaches keeps parsing. Strictness is a
// backstop against typos, not a new rule the survey has to learn — a
// gate that says what it means goes through untouched, question and all.
func TestPulseGateStillParsesTheTaughtGrammar(t *testing.T) {
	root := spawnFixture(t)
	writePulseGate(t, root, "moe", "pulse-x", `{"status":"ok",`+
		`"loose":[{"slug":"fix-ci","workflow":"sdlc","title":"Fix CI","why":"red","design":"# seed\n"}],`+
		`"threads":[{"onto":"already-parked","head":"","park":"a guess","runs":[`+
		`"tidy-1",`+
		`{"slug":"fresh","chore":"","why":"because"},`+
		`{"run":"change-auth","ask":"Which policy — keep or adopt?"},`+
		`{"run":"no-question"},`+
		`{"run":"blank-ask","ask":""}]}]}`)

	gate, err := readPulseGate(root, "moe", "pulse-x")
	if err != nil {
		t.Fatalf("gate did not parse: %v", err)
	}
	runs := gate.Threads[0].Runs
	if len(runs) != 5 {
		t.Fatalf("runs = %+v, want 5", runs)
	}
	if runs[0].Existing != "tidy-1" || runs[0].Ask != "" || runs[0].Spec != nil {
		t.Errorf("runs[0] = %+v, want the bare string position", runs[0])
	}
	if runs[1].Spec == nil || runs[1].Spec.Slug != "fresh" {
		t.Errorf("runs[1] = %+v, want an inline mint spec", runs[1])
	}
	if ask := runs[2]; ask.Existing != "change-auth" || ask.Ask != "Which policy — keep or adopt?" {
		t.Errorf("runs[2] = %+v, want the ask form intact", ask)
	}
	// The two long-hand bare positions: no `ask` key, and an empty one.
	// Blank is grammatical, so it decodes; it just says nothing, which is
	// what a position with no question is.
	if runs[3].Existing != "no-question" || runs[3].Ask != "" {
		t.Errorf("runs[3] = %+v, want a bare position", runs[3])
	}
	if runs[4].Existing != "blank-ask" || runs[4].Ask != "" {
		t.Errorf("runs[4] = %+v, want a bare position", runs[4])
	}
}

// The seed, end to end: a sweep writes a gate whose ask entry misspells
// `ask`. The whole gate is refused, so nothing is minted, the run stays
// on the dash's ACTIVE list for review, and the sweep reports out
// non-zero. The stderr line names the offending key — the difference
// between an operator who fixes a typo and one who diffs a fence by eye.
//
// Before this change the same gate exited 0: the entry decoded as a bare
// position, and the question the survey wrote for the operator was
// simply gone — nothing asked, nothing said.
func TestPulseSurveyRefusesGateWithATypodAsk(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	trailerstest.SeedProject(t, root, "moe")

	orig := openPulse
	openPulse = func(projectID, runID string, headless bool, pi *pulseInterrupt, stdout, stderr io.Writer) surveyOutcome {
		writePulseGate(t, root, projectID, runID, `{"status":"ok",`+
			`"loose":[{"slug":"fix-a","title":"Fix a","why":"because"}],`+
			`"threads":[{"runs":[{"run":"change-auth","sak":"Which policy?"}]}]}`)
		return surveyOutcome{code: 0, agentStarted: true}
	}
	t.Cleanup(func() { openPulse = orig })

	var errb bytes.Buffer
	if code := runPulseSurvey(root, "moe", "" /*emitRun*/, nil /*pi*/, io.Discard, &errb); code == 0 {
		t.Fatalf("survey exit=0 on a gate with an unknown key — the question it wrote was dropped; stderr=%q", errb.String())
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
		"hint: moe pulse new moe ",
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
