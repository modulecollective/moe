package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/input"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
)

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
	if _, err := input.Ask(root, projectID, slug, "moe/pulse-one", question, "dynamic"); err != nil {
		t.Fatal(err)
	}
}

func addOn(t *testing.T, root, projectID, slug, text string) {
	t.Helper()
	if _, err := input.Add(root, projectID, slug, text); err != nil {
		t.Fatal(err)
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
	if _, err := input.Answer(root, "moe", "change-auth", 0, "Adopt the new default."); err != nil {
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
	if err := input.MarkDelivered(root, "moe", "change-auth", "code", []int{1}, ""); err != nil {
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

	prompt, ids, err := buildSystemPrompt(root, md, "code", "" /*clone*/, false, "claude")
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

// The kickoff block is two lists: what the operator pushed and what the
// board asked.
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
		"don't treat one as waiting on input",
		"stops nothing",
		"a run parked at a spent review asked",
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
	// The survey has no ask form; the block reports questions, it must
	// not coach the survey on asking them.
	for _, coach := range []string{"do not ask", "second question"} {
		if strings.Contains(got, coach) {
			t.Fatalf("block coaches asking (%q):\n%s", coach, got)
		}
	}
}

func TestPendingInputBlockEmptyOnAQuietBoard(t *testing.T) {
	root := spawnFixture(t)
	seedParkedRun(t, root, "quiet")
	if got := pendingInputBlock(&pulseScan{root: root}, "moe"); got != "" {
		t.Fatalf("block = %q, want empty", got)
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
	if _, err := input.Add(root, "tele", "note-me", "Skip the flake and ship."); err != nil {
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
	if _, err := input.Add(root, "tele", "note-me", "Skip the flake and ship."); err != nil {
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

// A note on a tagged idea is not the survey's to weigh: the harness
// promotes the idea on the next dynamic sweep and the promotion carries
// the note onto the run. So the entry drops out of the waiting list —
// and with nothing else live, the whole block does. A note on an
// untagged idea stays, as the operator's inbox it always was.
func TestPendingInputBlockLeavesOutTaggedIdeas(t *testing.T) {
	root := spawnFixture(t)
	if _, err := run.New(root, "moe", run.Options{ID: "an-idea", Workflow: "idea"}); err != nil {
		t.Fatal(err)
	}
	addOn(t, root, "moe", "an-idea", "Do the design pass and park for me to look.")

	sc, ok := newPulseScan(root)
	if !ok {
		t.Fatalf("newPulseScan(%q) failed", root)
	}
	if got := pendingInputBlock(sc, "moe"); !strings.Contains(got, "`moe/an-idea`") {
		t.Fatalf("block dropped a note on an untagged idea:\n%s", got)
	}

	if err := runopen.TagIdea(root, "moe", "an-idea", "sdlc", false); err != nil {
		t.Fatal(err)
	}
	sc, ok = newPulseScan(root)
	if !ok {
		t.Fatalf("newPulseScan(%q) failed", root)
	}
	if got := pendingInputBlock(sc, "moe"); got != "" {
		t.Fatalf("block = %q, want empty — the tagged idea's note rides its promotion", got)
	}
}

// `moe pulse note` writes the pulse record, from args or stdin, and
// `moe input list` shows pending notes first under "for the pulse" —
// but not under a project filter, since a pulse note names no project.
func TestPulseNoteVerbAndListing(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedParkedRun(t, root, "change-auth")
	addOn(t, root, "moe", "change-auth", "skip the flake")

	var out, errb bytes.Buffer
	if code := runPulseNote([]string{"do", "the", "lightpool", "fix", "next"}, &out, &errb); code != 0 {
		t.Fatalf("pulse note exited %d: %s", code, errb.String())
	}
	if got := out.String(); got != "note #1 saved for the pulse — the next sweep reads it\n" {
		t.Fatalf("pulse note output = %q", got)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("bump Go everywhere\n\nstart with moe\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved })
	out.Reset()
	if code := runPulseNote(nil, &out, &errb); code != 0 {
		t.Fatalf("pulse note from stdin exited %d: %s", code, errb.String())
	}
	f, _ := input.LoadPulse(root)
	if len(f.Notes) != 2 || f.Notes[1].Text != "bump Go everywhere\n\nstart with moe" {
		t.Fatalf("pulse record = %+v", f.Notes)
	}

	out.Reset()
	if code := runInputList(nil, &out, &errb); code != 0 {
		t.Fatalf("input list exited %d: %s", code, errb.String())
	}
	want := "for the pulse:\n  #1 — do the lightpool fix next\n  #2 — bump Go everywhere\n\ngiven, awaiting pickup:\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Fatalf("listing = %q, want it to open with %q", out.String(), want)
	}
	out.Reset()
	if code := runInputList([]string{"moe"}, &out, &errb); code != 0 {
		t.Fatalf("filtered input list exited %d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "for the pulse") {
		t.Fatalf("project-filtered listing shows pulse notes:\n%s", out.String())
	}
}
