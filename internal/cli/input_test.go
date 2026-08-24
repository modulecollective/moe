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
	"github.com/modulecollective/moe/internal/runopen"
)

// askEntry is a thread position carrying a question, as it arrives from
// the gate.
func askEntry(slug, question string) pulseThreadEntry {
	return pulseThreadEntry{Existing: slug, Ask: question}
}

func seedParkedRun(t *testing.T, root, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, "moe", run.Options{ID: slug, Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func askOn(t *testing.T, root, projectID, slug, question string) {
	t.Helper()
	if _, err := input.Ask(root, projectID, slug, "moe/pulse-one", question,
		"dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func addOn(t *testing.T, root, projectID, slug, text string) {
	t.Helper()
	if _, err := input.Add(root, projectID, slug, text, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// The gate's three entry shapes have to stay distinguishable on the
// wire. The `run` key is the discriminator against a mint spec's `slug`,
// and the bare string keeps meaning exactly what it meant.
func TestThreadEntryUnmarshalsThreeShapes(t *testing.T) {
	var th pulseThread
	body := `{"runs": [
	  "already-parked",
	  {"slug": "fresh", "title": "Fresh", "why": "because"},
	  {"run": "change-auth", "ask": "Which policy — keep or adopt?"},
	  {"run": "no-question"}
	]}`
	if err := json.Unmarshal([]byte(body), &th); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(th.Runs) != 4 {
		t.Fatalf("runs = %d, want 4", len(th.Runs))
	}
	if got := th.Runs[0]; got.Existing != "already-parked" || got.Spec != nil || got.Ask != "" {
		t.Fatalf("string entry = %+v", got)
	}
	if got := th.Runs[1]; got.Spec == nil || got.Spec.Slug != "fresh" || got.Ask != "" {
		t.Fatalf("spec entry = %+v", got)
	}
	if got := th.Runs[2]; got.Existing != "change-auth" || got.Spec != nil || got.Ask != "Which policy — keep or adopt?" {
		t.Fatalf("ask entry = %+v", got)
	}
	// `{"run": "x"}` with no ask is the string form written long-hand.
	if got := th.Runs[3]; got.Existing != "no-question" || got.Ask != "" || got.Spec != nil {
		t.Fatalf("bare run entry = %+v", got)
	}
}

// Asking and holding are orthogonal: the ping lands on the run's record
// and the thread is not parked by the ask alone.
func TestGateAskOpensPingWithoutParking(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("change-auth", "Which compatibility policy?"),
		}}},
	}, io.Discard, &errb)

	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Park != "" {
		t.Fatalf("group park = %q, want the ask to hold nothing", groups[0].Park)
	}
	if len(groups[0].Runs) != 1 || groups[0].Runs[0].slug != "change-auth" {
		t.Fatalf("members = %+v, want the run at its position", groups[0].Runs)
	}

	f, err := input.Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := f.OpenPing()
	if !ok || e.Question != "Which compatibility policy?" {
		t.Fatalf("record = %+v, %v; stderr=%q", e, ok, errb.String())
	}
	if e.AskedBy != "moe/pulse-one" {
		t.Fatalf("asked_by = %q, want the sweep", e.AskedBy)
	}
}

// A survey that wants the answer first writes the park itself, and the
// two acts compose.
func TestGateAskKeepsExplicitThreadPark(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{
			Park: "waiting on the auth question",
			Runs: []pulseThreadEntry{askEntry("change-auth", "Which policy?")},
		}},
	}, io.Discard, io.Discard)

	if groups[0].Park != "waiting on the auth question" {
		t.Fatalf("park = %q, want the survey's own line", groups[0].Park)
	}
	f, _ := input.Load(root, "moe", "change-auth")
	if _, ok := f.OpenPing(); !ok {
		t.Fatal("question did not land alongside an explicit park")
	}
}

// An empty question is warn-and-skip like every other refusal in
// applyPulseGate.
func TestGateAskWithEmptyQuestionWarnsAndSkips(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "change-auth")

	var errb bytes.Buffer
	applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			{Existing: "change-auth", Ask: "   "},
		}}},
	}, io.Discard, &errb)

	if !strings.Contains(errb.String(), "pulse: input:") {
		t.Fatalf("stderr = %q, want a warn line", errb.String())
	}
	f, err := input.Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Notes) != 0 {
		t.Fatalf("record = %+v, want nothing written", f)
	}
}

// A ping lands only on a run that already existed. A tagged idea
// resolves to a *different* run than the question was written against,
// so the ask drops with a line rather than landing somewhere the survey
// never looked.
func TestGateAskOnTaggedIdeaIsDropped(t *testing.T) {
	root := spawnFixture(t)
	seedTaggedIdea(t, root, "moe", "cleanup-foo")

	var errb bytes.Buffer
	groups := applyPulseGate(root, "moe", "pulse-one", pulseGate{
		Status: "ok",
		Threads: []pulseThread{{Runs: []pulseThreadEntry{
			askEntry("cleanup-foo", "Which policy?"),
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
	if len(f.Notes) != 0 {
		t.Fatalf("promoted run carries %+v, want no question", f.Notes)
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
			askEntry("batch", "Which policy?"),
		}}},
	}, io.Discard, &errb)

	if !strings.Contains(errb.String(), "chain head") {
		t.Fatalf("stderr = %q, want the chain-head refusal", errb.String())
	}
	f, _ := input.Load(root, "moe", "batch")
	if len(f.Notes) != 0 {
		t.Fatalf("chain head carries %+v, want no question", f.Notes)
	}
}

// A bare note reaches the next turn verbatim, and the section names the
// ids it rendered so the caller can stamp exactly those.
func TestOperatorInputSectionCarriesPendingNotes(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	addOn(t, root, "moe", "change-auth", "The failing test is a known flake; skip it and ship.")

	got, ids := operatorInputSection(root, md)
	if !strings.Contains(got, "## Operator input") {
		t.Fatalf("section = %q, want the heading", got)
	}
	if !strings.Contains(got, "known flake") {
		t.Fatalf("section = %q, want the note verbatim", got)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ids = %v, want [1]", ids)
	}
	// The bar the block has to carry, or a code stage reads a note as
	// licence to redo the design.
	if !strings.Contains(got, "not licence to rewrite") {
		t.Fatalf("section = %q, want the not-a-licence line", got)
	}
}

// An answered ping delivers as the pair: "B" alone is cryptic where
// "asked: which policy? — answer: B" is exact.
func TestOperatorInputSectionRendersAnsweredPingAsAPair(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	askOn(t, root, "moe", "change-auth", "Which compatibility policy?")
	if _, err := input.Answer(root, "moe", "change-auth", 0, "Adopt the new default.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	got, ids := operatorInputSection(root, md)
	if !strings.Contains(got, "asked: Which compatibility policy?") {
		t.Fatalf("section = %q, want the question", got)
	}
	if !strings.Contains(got, "answer: Adopt the new default.") {
		t.Fatalf("section = %q, want the answer", got)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want the answered ping delivered", ids)
	}
}

// An open ping is standing context, not a delivery: it renders every
// turn until answered and takes no stamp.
func TestOperatorInputSectionRendersOpenPingWithoutDelivering(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	askOn(t, root, "moe", "change-auth", "Which compatibility policy?")

	got, ids := operatorInputSection(root, md)
	if !strings.Contains(got, "Which compatibility policy?") {
		t.Fatalf("section = %q, want the open question", got)
	}
	if !strings.Contains(got, "No reply yet") {
		t.Fatalf("section = %q, want the unanswered line", got)
	}
	if !strings.Contains(got, "best judgment") {
		t.Fatalf("section = %q, want the proceed-anyway guidance", got)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want an open ping to deliver nothing", ids)
	}
}

// Delivered-once: a consumed note lives on the run page, not in the next
// prompt.
func TestOperatorInputSectionDropsDeliveredEntries(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	addOn(t, root, "moe", "change-auth", "skip the flake")
	if err := input.MarkDelivered(root, "moe", "change-auth", "code", []int{1}, "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, ids := operatorInputSection(root, md); got != "" || ids != nil {
		t.Fatalf("section = %q, ids = %v; want nothing after delivery", got, ids)
	}
}

func TestOperatorInputSectionEmptyWithoutRecord(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	if got, ids := operatorInputSection(root, md); got != "" || ids != nil {
		t.Fatalf("section = %q, ids = %v; want empty", got, ids)
	}
}

// The prompt assembly is where delivery actually has to happen: a
// section that exists but never reaches buildSystemPrompt delivers
// nothing.
func TestStagePromptCarriesOperatorInput(t *testing.T) {
	root := spawnFixture(t)
	md := seedParkedRun(t, root, "change-auth")
	addOn(t, root, "moe", "change-auth", "Ship behind the existing flag.")

	prompt, ids, err := buildSystemPrompt(root, md, "code", "" /*clone*/, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "## Operator input") {
		t.Fatalf("code prompt has no Operator input section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Ship behind the existing flag.") {
		t.Fatalf("code prompt does not carry the note:\n%s", prompt)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("delivered ids = %v, want [1]", ids)
	}
}

// An open ping stops nothing. The thread rides on the agent's best
// judgment, which is the whole orthogonality decision.
func TestSelfKickRidesWithAnOpenPing(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	tail := groomed.graph.Tail(threadRoot)
	proj, runID, err := splitProjectRun(tail)
	if err != nil {
		t.Fatal(err)
	}
	askOn(t, root, proj, runID, "Which compatibility policy?")

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("the ride was held by an open question; stderr=%q", errb.String())
	}
}

// A malformed record no longer holds anything either — nothing in the
// record is a floor, so a broken file costs a hint, not a stalled board.
func TestSelfKickRidesOverMalformedRecord(t *testing.T) {
	root, threadRoot, groomed, stages := selfKickFixture(t)
	proj, runID, err := splitProjectRun(threadRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, input.Path(proj, runID))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, input.Path(proj, runID)), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer withRideMode(rideDynamic)()
	var errb bytes.Buffer
	pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)

	if len(*stages) == 0 {
		t.Fatalf("an unreadable record held the ride; stderr=%q", errb.String())
	}
}

// A pending note is an operator mark under safe mode: the operator gave
// this run direction, so requiring a second approval would break the
// phone-to-motion loop the record exists for. Delivery consumes it.
func TestSafeModeAdmitsAThreadWithAPendingNote(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	setMode(t, root, "moe", project.ModeSafe)

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); !strings.Contains(got, "safe mode") {
			t.Fatalf("hold = %q, want the safe-mode hold before any note", got)
		}
	})

	addOn(t, root, "moe", id, "Go ahead — the API shape is settled.")
	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); got != "" {
			t.Fatalf("hold = %q, want the note to license the thread under safe", got)
		}
	})

	// Delivery consumes the licence; another note re-arms it.
	if err := input.MarkDelivered(root, "moe", id, "code", []int{1}, "", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); !strings.Contains(got, "safe mode") {
			t.Fatalf("hold = %q, want the licence consumed by delivery", got)
		}
	})
	addOn(t, root, "moe", id, "Second thoughts: use the other codec.")
	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); got != "" {
			t.Fatalf("hold = %q, want a fresh note to re-arm the licence", got)
		}
	})
}

// An open ping is not a mark: nobody has given the run anything yet.
func TestSafeModeStillHoldsWithOnlyAnOpenPing(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	setMode(t, root, "moe", project.ModeSafe)
	askOn(t, root, "moe", id, "Which policy?")

	asClock(func() {
		if got := holdFor(t, planFor(t, root), "moe/"+id); !strings.Contains(got, "safe mode") {
			t.Fatalf("hold = %q, want an unanswered question to license nothing", got)
		}
	})
}

// The mark is not an advance marker: it licenses the kick and says
// nothing about whether a stage was read, so the run's next pickup is
// still its first stage.
func TestPendingNoteIsNotAnAdvanceMarker(t *testing.T) {
	root := quietFixture(t)
	id := groomFixture(t, root, "fix-a")["fix-a"]
	addOn(t, root, "moe", id, "Go ahead.")
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
	// And the note commit does not wear the advance subject.
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if strings.HasPrefix(msg, "advance:") {
		t.Fatalf("the note landed as an advance marker:\n%s", msg)
	}
}

// The three verbs, end to end through their argv entry points: what the
// operator types, and what they read back.
func TestInputVerbs(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedParkedRun(t, root, "change-auth")
	askOn(t, root, "moe", "change-auth", "Which compatibility policy?")

	var out, errb bytes.Buffer
	if code := runInputList(nil, &out, &errb); code != 0 {
		t.Fatalf("input list exited %d: %s", code, errb.String())
	}
	for _, want := range []string{
		"asked you:", "moe/change-auth", "Which compatibility policy?",
		"moe input answer moe/change-auth <text>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("listing missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := runInputAnswer([]string{"moe/change-auth", "Adopt", "the", "new", "default."}, &out, &errb); code != 0 {
		t.Fatalf("input answer exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "answered moe/change-auth#1") {
		t.Fatalf("answer output = %q", out.String())
	}

	out.Reset()
	if code := runInputAdd([]string{"moe/change-auth", "Also", "skip", "the", "flake."}, &out, &errb); code != 0 {
		t.Fatalf("input add exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "noted on moe/change-auth#2") {
		t.Fatalf("add output = %q", out.String())
	}

	// Both entries now sit in the given-and-waiting half; nothing is
	// asking the operator anything.
	out.Reset()
	if code := runInputList(nil, &out, &errb); code != 0 {
		t.Fatalf("second input list exited %d: %s", code, errb.String())
	}
	listing := out.String()
	if strings.Contains(listing, "asked you:") {
		t.Fatalf("listing still asks after an answer:\n%s", listing)
	}
	// The answered ping still lists by its question — that is what both
	// sides recognise it by — and the bare note by its text.
	if !strings.Contains(listing, "given, awaiting pickup:") ||
		!strings.Contains(listing, "Which compatibility policy?") ||
		!strings.Contains(listing, "Also skip the flake.") {
		t.Fatalf("listing = %q, want both entries pending", listing)
	}
}

// No text on the line means the prose is on stdin — the only way to
// write more than a sentence without fighting the shell's quoting.
func TestInputAddReadsStdin(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedParkedRun(t, root, "change-auth")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("Two paragraphs.\n\nThe second one.\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved })

	var out, errb bytes.Buffer
	if code := runInputAdd([]string{"moe/change-auth"}, &out, &errb); code != 0 {
		t.Fatalf("input add exited %d: %s", code, errb.String())
	}
	f, _ := input.Load(root, "moe", "change-auth")
	if len(f.Notes) != 1 || !strings.Contains(f.Notes[0].Text, "The second one.") {
		t.Fatalf("record = %+v, want the piped body", f.Notes)
	}
}

// The kickoff block is two lists and a bar: what the operator pushed,
// what the board asked, and the entry shape for asking.
func TestPendingInputBlock(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "pushed")
	seedParkedRun(t, root, "asked")
	addOn(t, root, "moe", "pushed", "Use the streaming path.\nIt is already benchmarked.")
	askOn(t, root, "moe", "asked", "Which compatibility policy?")

	got := pendingInputBlock(&pulseScan{root: root}, "moe")
	for _, want := range []string{
		"`moe/pushed` — Use the streaming path.",
		"`moe/asked` — Which compatibility policy?",
		`{"run": "<slug>", "ask": "…?"}`,
		"don't park one \"awaiting input\"",
		"stops nothing",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("block missing %q:\n%s", want, got)
		}
	}
	// Only the first line of a multi-line note: the run's own turn gets
	// the rest.
	if strings.Contains(got, "already benchmarked") {
		t.Fatalf("block relays the whole note:\n%s", got)
	}
}

func TestPendingInputBlockEmptyOnAQuietBoard(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "quiet")
	if got := pendingInputBlock(&pulseScan{root: root}, "moe"); got != "" {
		t.Fatalf("block = %q, want empty", got)
	}
}

// The ride boundary used to refuse on an open question. It doesn't any
// more: a ping holds nothing, so the chain rides and the turns read the
// unanswered line.
func TestChainKickRidesWithAQuestionOnAMember(t *testing.T) {
	root, stages, _ := kickFixture(t)

	spawnAndHead(t, root, "moe", "pulse-one", "batch", []pulseRunSpec{
		{Slug: "fix-one", Title: "One"},
		{Slug: "fix-two", Title: "Two"},
	}, os.Stderr)
	heads := runsWithWorkflow(t, root, "moe", chainWorkflow)
	if len(heads) != 1 {
		t.Fatalf("chain runs %v, want 1", heads)
	}
	askOn(t, root, "moe", "fix-two", "Which policy?")

	var errb bytes.Buffer
	if code := runChainKick([]string{"moe/" + heads[0]}, io.Discard, &errb); code != 0 {
		t.Fatalf("chain kick exited %d with a question open; stderr=%q", code, errb.String())
	}
	if len(*stages) == 0 {
		t.Fatalf("the ride was held by an open question; stderr=%q", errb.String())
	}
}

// The wiring, end to end through the real session machinery: a note
// reaches the turn's prompt, the turn's success stamps it delivered as
// its own journal commit, and the *next* turn's prompt no longer carries
// it. Delivered-once is the whole claim, and it lives in three places at
// once — the prompt builder, runStageSession's post-turn call, and the
// record — so nothing short of a real turn proves it.
func TestStageTurnDeliversTheNoteOnceAndStampsIt(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)

	promptFile := filepath.Join(t.TempDir(), "prompts.txt")
	t.Setenv("MOE_TEST_PROMPT_DUMP", promptFile)
	fakeClaudeOnPath(t, `#!/bin/sh
prompt=
next=0
for a in "$@"; do
  if [ "$next" = "1" ]; then prompt=$a; next=0; fi
  case "$a" in --append-system-prompt) next=1 ;; esac
done
printf '%s\n--END-PROMPT--\n' "$prompt" >> "$MOE_TEST_PROMPT_DUMP"
canvas=$(printf '%s' "$prompt" | awk '/Your canvas for this document is the single file:/ {getline; gsub(/^ +| +$/, ""); print; exit}')
if [ -n "$canvas" ]; then printf 'a turn happened\n' >> "$canvas"; fi
exit 0
`)

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/note-me"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	if _, err := input.Add(root, "tele", "note-me", "Skip the flake and ship.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := openSdlcDesign("tele", "note-me", true, "", &out, &errb); code != 0 {
		t.Fatalf("design turn exit=%d stderr=%q", code, errb.String())
	}

	prompts := readPromptDump(t, promptFile)
	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "Skip the flake and ship.") {
		t.Fatalf("the first turn's prompt did not carry the note:\n%s", prompts[0])
	}

	f, err := input.Load(root, "tele", "note-me")
	if err != nil {
		t.Fatal(err)
	}
	if f.Notes[0].DeliveredTo != "design" {
		t.Fatalf("entry = %+v, want it stamped delivered to design", f.Notes[0])
	}
	if !commitExists(t, root, "MoE-Input-Delivered: tele/note-me#1 design") {
		t.Fatal("no delivery commit landed in the journal")
	}

	// A second turn on the same doc: the note is history now, and the
	// canvas the first turn wrote is where the direction lives on.
	out.Reset()
	errb.Reset()
	if code := openSdlcDesign("tele", "note-me", true, "", &out, &errb); code != 0 {
		t.Fatalf("second design turn exit=%d stderr=%q", code, errb.String())
	}
	prompts = readPromptDump(t, promptFile)
	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	if strings.Contains(prompts[1], "Skip the flake and ship.") {
		t.Fatalf("a delivered note came back in the next prompt:\n%s", prompts[1])
	}
}

// A turn that fails marks nothing, so the next attempt redelivers —
// the guarantee that makes marking a separate post-turn commit rather
// than a rider on the turn's own.
func TestFailedStageTurnLeavesTheNotePending(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	seedSdlcOneShotProject(t, root, "tele")
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	stubEditor(t)
	suppressNextStagePrompt(t)
	fakeClaudeOnPath(t, "#!/bin/sh\nexit 3\n")

	var out, errb bytes.Buffer
	if code := runNew("sdlc", []string{"tele/note-me"}, &out, &errb); code != 0 {
		t.Fatalf("runNew exit=%d stderr=%q", code, errb.String())
	}
	if _, err := input.Add(root, "tele", "note-me", "Skip the flake and ship.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := openSdlcDesign("tele", "note-me", true, "", &out, &errb); code == 0 {
		t.Fatalf("the design turn unexpectedly succeeded; stderr=%q", errb.String())
	}

	f, err := input.Load(root, "tele", "note-me")
	if err != nil {
		t.Fatal(err)
	}
	if !f.Notes[0].Pending() {
		t.Fatalf("entry = %+v, want it still pending after a failed turn", f.Notes[0])
	}
}

// readPromptDump splits the fake agent's appended system prompts.
func readPromptDump(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("prompt dump missing: %v", err)
	}
	var out []string
	for _, p := range strings.Split(string(body), "\n--END-PROMPT--\n") {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// commitExists reports whether any commit on HEAD's history carries the
// given trailer line.
func commitExists(t *testing.T, root, trailer string) bool {
	t.Helper()
	return strings.Contains(gittest.Output(t, root, "log", "--format=%B"), trailer)
}

// A note on a tagged idea has no turn of its own to reach — the promote
// is what delivers it — so the block tells the survey to read it as
// promote signal rather than a pending pickup.
func TestPendingInputBlockNamesTaggedIdeasAsPromoteSignal(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "an-idea", Workflow: "idea"}); err != nil {
		t.Fatal(err)
	}
	addOn(t, root, "moe", "an-idea", "Do the design pass and park for me to look.")

	// Untagged: operator-fenced, so the survey can't promote it and the
	// sentence would be advice it can't take.
	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatalf("newPulseScan(%q) failed", root)
	}
	if got := pendingInputBlock(sc, "moe"); strings.Contains(got, "signal to promote") {
		t.Fatalf("block reads an untagged idea as promote signal:\n%s", got)
	}

	if err := runopen.TagIdea(root, "moe", "an-idea", "sdlc", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	sc, ok = newPulseScan(root)
	if !ok {
		t.Fatalf("newPulseScan(%q) failed", root)
	}
	got := pendingInputBlock(sc, "moe")
	if !strings.Contains(got, "signal to promote") {
		t.Fatalf("block missing the tagged-idea sentence:\n%s", got)
	}
}
