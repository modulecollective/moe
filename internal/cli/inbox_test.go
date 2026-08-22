package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// askEntry is a thread position carrying a question — the survey's
// structured park, as it arrives from the gate.
func askEntry(slug, question string, choices ...string) pulseThreadEntry {
	return pulseThreadEntry{Existing: slug, Ask: &pulseRunAsk{Question: question, Choices: choices}}
}

func seedParkedRun(t *testing.T, root, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, "moe", run.Options{ID: slug, Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// The gate's three entry shapes have to stay distinguishable on the
// wire. The `run` key is the discriminator against a mint spec's `slug`,
// and the bare string keeps meaning exactly what it meant.
func TestThreadEntryUnmarshalsThreeShapes(t *testing.T) {
	var th pulseThread
	body := `{"runs": [
	  "already-parked",
	  {"slug": "fresh", "title": "Fresh", "why": "because"},
	  {"run": "change-auth", "park": {"question": "Which policy?", "choices": ["Keep", "Adopt"]}},
	  {"run": "no-question"}
	]}`
	if err := json.Unmarshal([]byte(body), &th); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(th.Runs) != 4 {
		t.Fatalf("runs = %d, want 4", len(th.Runs))
	}
	if got := th.Runs[0]; got.Existing != "already-parked" || got.Spec != nil || got.Ask != nil {
		t.Fatalf("string entry = %+v", got)
	}
	if got := th.Runs[1]; got.Spec == nil || got.Spec.Slug != "fresh" || got.Ask != nil {
		t.Fatalf("spec entry = %+v", got)
	}
	ask := th.Runs[2]
	if ask.Existing != "change-auth" || ask.Spec != nil || ask.Ask == nil {
		t.Fatalf("ask entry = %+v", ask)
	}
	if ask.Ask.Question != "Which policy?" || len(ask.Ask.Choices) != 2 {
		t.Fatalf("ask payload = %+v", ask.Ask)
	}
	// `{"run": "x"}` with no park is the string form written long-hand.
	if got := th.Runs[3]; got.Existing != "no-question" || got.Ask != nil || got.Spec != nil {
		t.Fatalf("bare run entry = %+v", got)
	}
}

// The whole act in one: the question lands on the named run's record,
// and the thread it sits in is parked for this sweep.
func TestGateAskOpensRequestAndParksThread(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("change-auth", "Which compatibility policy?", "Preserve", "Adopt"),
		}}},
	}, io.Discard, &errb)

	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if !strings.Contains(groups[0].Park, "Which compatibility policy?") {
		t.Fatalf("group park = %q, want the question", groups[0].Park)
	}
	if len(groups[0].Runs) != 1 || groups[0].Runs[0].slug != "change-auth" {
		t.Fatalf("members = %+v, want the run at its position", groups[0].Runs)
	}

	f, err := input.Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	req, ok := f.Open()
	if !ok || req.Question != "Which compatibility policy?" {
		t.Fatalf("record = %+v, %v; stderr=%q", req, ok, errb.String())
	}
	if req.AskedBy != "moe/pulse-one" {
		t.Fatalf("asked_by = %q, want the sweep", req.AskedBy)
	}
}

// A thread-level park wins the wording — it is the survey's own sentence
// about the whole thread — but the question still lands.
func TestGateAskKeepsExplicitThreadPark(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{
			Park: "touches the release path",
			Runs: []pulseThreadEntry{askEntry("change-auth", "Which policy?", "Keep", "Adopt")},
		}},
	}, io.Discard, io.Discard)

	if groups[0].Park != "touches the release path" {
		t.Fatalf("park = %q, want the survey's own line", groups[0].Park)
	}
	if _, err := input.Load(root, "moe", "change-auth"); err != nil {
		t.Fatal(err)
	}
	f, _ := input.Load(root, "moe", "change-auth")
	if _, ok := f.Open(); !ok {
		t.Fatal("question did not land alongside an explicit park")
	}
}

// A malformed question is warn-and-skip like every other refusal in
// applyPulseGate — but the thread still parks, because holding is the
// safe direction and the survey asked for it.
func TestGateAskWithBadQuestionWarnsAndStillParks(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("change-auth", "Which policy?", "only-one"),
		}}},
	}, io.Discard, &errb)

	if groups[0].Park == "" {
		t.Fatal("thread did not park after a refused ask")
	}
	if !strings.Contains(errb.String(), "pulse: input:") {
		t.Fatalf("stderr = %q, want a warn line", errb.String())
	}
	f, err := input.Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Requests) != 0 {
		t.Fatalf("record = %+v, want nothing written", f)
	}
}

// v1 asks only on runs that already existed. A tagged idea resolves to a
// *different* run than the question was written against, so the ask
// drops with a line rather than landing somewhere the survey never
// looked.
func TestGateAskOnTaggedIdeaIsDropped(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "cleanup-foo")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("cleanup-foo", "Which policy?", "Keep", "Adopt"),
		}}},
	}, io.Discard, &errb)

	if !strings.Contains(errb.String(), "question dropped") {
		t.Fatalf("stderr = %q, want the drop line", errb.String())
	}
	destID := groups[0].Runs[0].mintedID
	if destID == "" {
		t.Fatalf("member = %+v, want the promoted run", groups[0].Runs[0])
	}
	f, err := input.Load(root, "moe", destID)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Requests) != 0 {
		t.Fatalf("promoted run carries %+v, want no question", f.Requests)
	}
}

// A chain placeholder has no stage to deliver an answer to, so it is not
// somewhere a question can live.
func TestGateAskOnChainHeadIsDropped(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "batch", Workflow: chainWorkflow}); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("batch", "Which policy?", "Keep", "Adopt"),
		}}},
	}, io.Discard, &errb)

	if !strings.Contains(errb.String(), "chain head") {
		t.Fatalf("stderr = %q, want the chain-head refusal", errb.String())
	}
	f, _ := input.Load(root, "moe", "batch")
	if len(f.Requests) != 0 {
		t.Fatalf("chain head carries %+v, want no question", f.Requests)
	}
}

// The stage floor: an open question refuses the turn and points at the
// verb that discharges it, rather than taking an answer nothing records.
func TestRefuseOnOpenInputHoldsStageEntry(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := refuseOnOpenInput(root, "moe", "change-auth", &errb); code == 0 {
		t.Fatal("stage entry allowed with a question open")
	}
	out := errb.String()
	if !strings.Contains(out, "Which policy?") {
		t.Fatalf("stderr = %q, want the question", out)
	}
	if !strings.Contains(out, "moe inbox answer moe/change-auth") {
		t.Fatalf("stderr = %q, want the answer command", out)
	}
}

// Once answered the floor lifts. Nothing else has to happen — the answer
// is the whole of the hold's release.
func TestRefuseOnOpenInputLiftsAfterAnswer(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, "moe", "change-auth", 0, 2, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if code := refuseOnOpenInput(root, "moe", "change-auth", io.Discard); code != 0 {
		t.Fatalf("stage entry still held after an answer (code %d)", code)
	}
}

// A run with no record pays nothing and passes.
func TestRefuseOnOpenInputPassesWithNoRecord(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")
	if code := refuseOnOpenInput(root, "moe", "change-auth", io.Discard); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

// The delivery half: the answer reaches the target run's later stage
// prompts, and the block says what an answer is and isn't.
func TestHumanInputsSectionCarriesTheAnswer(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which compatibility policy?", []string{"Preserve the old default", "Adopt the new default"},
		"dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, "moe", "change-auth", 0, 2, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	got := humanInputsSection(root, md)
	if !strings.Contains(got, "Which compatibility policy?") {
		t.Fatalf("section = %q, want the question", got)
	}
	if !strings.Contains(got, "Adopt the new default") {
		t.Fatalf("section = %q, want the selected answer", got)
	}
	if strings.Contains(got, "unanswered") {
		t.Fatalf("section = %q, want no unanswered marker", got)
	}
	// The bar the block has to carry, or a code stage reads an answer as
	// licence to redo the design.
	if !strings.Contains(got, "not licence to") {
		t.Fatalf("section = %q, want the not-a-licence line", got)
	}
}

func TestHumanInputsSectionEmptyWithoutRecord(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	if got := humanInputsSection(root, md); got != "" {
		t.Fatalf("section = %q, want empty", got)
	}
}

// The prompt assembly is where delivery actually has to happen: a
// section that exists but never reaches buildSystemPrompt delivers
// nothing.
func TestStagePromptCarriesHumanInputs(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which compatibility policy?", []string{"Preserve", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, "moe", "change-auth", 0, 1, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	prompt, err := buildSystemPrompt(root, md, "code", "" /*clone*/, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "## Human inputs") {
		t.Fatalf("code prompt has no Human inputs section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Preserve") {
		t.Fatalf("code prompt does not carry the selected answer:\n%s", prompt)
	}
}

// The kick floor is thread-wide: the question lands on the run whose
// future agent needs the answer, which is routinely a member queued
// behind the head, and riding the thread would walk straight into it.
func TestSelfKickHoldsThreadOnQueuedMembersQuestion(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	// The tail, not the root: a floor that only asked the head would let
	// this ride start.
	tail := groomed.graph.Tail(threadRoot)
	if tail == threadRoot {
		t.Fatalf("fixture thread has no queued member (tail = root = %s)", threadRoot)
	}
	proj, runID, err := splitProjectRun(tail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Ask(root, proj, runID, "moe/pulse-one",
		"Which compatibility policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("the ride started with a question open on %s; stderr=%q", tail, errb.String())
	}
	out := errb.String()
	if !strings.Contains(out, "awaiting input") || !strings.Contains(out, "Which compatibility policy?") {
		t.Fatalf("stderr = %q, want the hold to name the question", out)
	}
	if !strings.Contains(out, tail) {
		t.Fatalf("stderr = %q, want the member named", out)
	}
}

// Answering releases it. Nothing else runs in between — the answer is
// the whole of the release, and the next sweep's kick carries the thread.
func TestSelfKickResumesAfterAnswer(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	tail := groomed.graph.Tail(threadRoot)
	proj, runID, err := splitProjectRun(tail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Ask(root, proj, runID, "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, proj, runID, 0, 2, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("the thread stayed held after its question was answered; stderr=%q", errb.String())
	}
}

// A malformed record holds too. It is machine-written, so a violation is
// a bug the operator should see rather than a reason to start work whose
// held-ness nothing can now answer.
func TestSelfKickHoldsOnMalformedRecord(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	proj, runID, err := splitProjectRun(threadRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, input.Path(proj, runID)), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) != 0 {
		t.Fatalf("the ride started over an unreadable record; stderr=%q", errb.String())
	}
	if !strings.Contains(errb.String(), "unreadable input record") {
		t.Fatalf("stderr = %q, want the malformed-record hold", errb.String())
	}
}

// An answered question is an operator mark under safe mode: the operator
// read a concrete question on a concrete run and supplied the missing
// decision, so asking for a second approval would make the inbox fail
// its purpose.
func TestSafeModeAdmitsAThreadWithAnAnsweredQuestion(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	setMode(t, root, "moe", project.ModeSafe)

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); !strings.Contains(got, "safe mode") {
			t.Fatalf("hold = %q, want the safe-mode hold before any answer", got)
		}
	})

	if _, err := input.Ask(root, "moe", id, "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, "moe", id, 0, 2, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); got != "" {
			t.Fatalf("hold = %q, want the answer to license the thread under safe", got)
		}
	})
}

// The mark is not an advance marker: it clears the human-input hold and
// says nothing about whether a stage was read, so the run's next pickup
// is still its first stage.
func TestAnsweredQuestionIsNotAnAdvanceMarker(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	if _, err := input.Ask(root, "moe", id, "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Answer(root, "moe", id, 0, 1, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	md, err := run.Load(root, "moe", id)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := wf.Next(root, md)
	if err != nil {
		t.Fatal(err)
	}
	if next != wf.Stages()[0] {
		t.Fatalf("next stage = %q, want the ladder untouched at %q", next, wf.Stages()[0])
	}
	// And the answer commit does not wear the advance subject.
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if strings.HasPrefix(msg, "advance:") {
		t.Fatalf("the answer landed as an advance marker:\n%s", msg)
	}
}

// The two verbs, end to end through their argv entry points: what the
// operator types, and what they read back.
func TestInboxListAndAnswerVerbs(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which compatibility policy?", []string{"Preserve", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := runInboxList(nil, &out, &errb); code != 0 {
		t.Fatalf("inbox list exited %d: %s", code, errb.String())
	}
	listing := out.String()
	for _, want := range []string{
		"moe/change-auth", "Which compatibility policy?",
		"1. Preserve", "2. Adopt",
		"moe inbox answer moe/change-auth <1-2>",
	} {
		if !strings.Contains(listing, want) {
			t.Fatalf("listing missing %q:\n%s", want, listing)
		}
	}

	out.Reset()
	if code := runInboxAnswer([]string{"moe/change-auth", "2"}, &out, &errb); code != 0 {
		t.Fatalf("inbox answer exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "answered moe/change-auth#1 — Adopt") {
		t.Fatalf("answer output = %q", out.String())
	}

	out.Reset()
	if code := runInboxList(nil, &out, &errb); code != 0 {
		t.Fatalf("second inbox list exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nothing waiting") {
		t.Fatalf("listing after the answer = %q", out.String())
	}
}

func TestInboxAnswerRefusesOutOfRangeChoice(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")
	if _, err := input.Ask(root, "moe", "change-auth", "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	if code := runInboxAnswer([]string{"moe/change-auth", "7"}, io.Discard, &errb); code == 0 {
		t.Fatal("inbox answer accepted a choice out of range")
	}
	if !strings.Contains(errb.String(), "out of range") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// The ride boundary. A question on the *second* fix in a batch refuses
// the whole kick up front: the stage floor would have caught it, but
// only after fix-one had already shipped, and a half-ridden chain is
// what the up-front ask exists to prevent.
func TestChainKickRefusesWithAQuestionOnAMember(t *testing.T) {
	root, stages, pushes := kickFixture(t)

	spawnAndHead(t, root, "moe", "pulse-one", "batch", []pulseRunSpec{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, os.Stderr)
	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain runs %v, want 1", heads)
	}
	if _, err := input.Ask(root, "moe", "fix-two", "moe/pulse-one",
		"Which policy?", []string{"Keep", "Adopt"}, "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/" + heads[0]}, io.Discard, &errb); code == 0 {
		t.Fatalf("chain kick rode a chain with a question open; stderr=%q", errb.String())
	}
	if len(*stages) != 0 || len(*pushes) != 0 {
		t.Fatalf("the ride started: stages=%v pushes=%d", kickStages(*stages), len(*pushes))
	}
	if !strings.Contains(errb.String(), "moe/fix-two is awaiting input") {
		t.Fatalf("stderr = %q, want the member named", errb.String())
	}
}
