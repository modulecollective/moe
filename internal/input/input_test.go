package input

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// seedRoot builds a bureaucracy with one project and one in-progress
// run. The record itself is what these tests exercise, so nothing here
// writes an inputs.json — Add and Ask do.
func seedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gittest.InitAt(t, root)
	write(t, root, "bureaucracy.conf", "")
	write(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed bureaucracy")
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func seedRun(t *testing.T, root, slug string) *run.Metadata {
	t.Helper()
	md, err := run.New(root, "moe", run.Options{ID: slug, Workflow: "sdlc"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func ask(t *testing.T, root, slug string) Entry {
	t.Helper()
	e, err := Ask(root, "moe", slug, "moe/pulse-one",
		"Which compatibility policy?", "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return e
}

func add(t *testing.T, root, slug, text string) Entry {
	t.Helper()
	e, err := Add(root, "moe", slug, text, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return e
}

// A note round-trips through the file and lands as one commit carrying
// the added trailer and no consent — it is the operator's own act.
func TestAddWritesRecordAndTrailer(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")

	e := add(t, root, "change-auth", "  The failing test is a known flake; skip it.  ")
	if e.ID != 1 || e.IsPing() || !e.Pending() {
		t.Fatalf("entry = %+v, want a pending note at id 1", e)
	}
	if e.Text != "The failing test is a known flake; skip it." {
		t.Fatalf("Text = %q, want it trimmed", e.Text)
	}

	f, err := Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Pending(); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("Pending() = %+v, want the one note", got)
	}
	if _, ok := f.OpenPing(); ok {
		t.Fatalf("a bare note reads as an open ping: %+v", f)
	}

	body, err := os.ReadFile(filepath.Join(root, Path("moe", "change-auth")))
	if err != nil {
		t.Fatal(err)
	}
	// An undelivered note says nothing about delivery rather than
	// carrying an empty key.
	if strings.Contains(string(body), "delivered_to") {
		t.Fatalf("inputs.json carries delivered_to while pending:\n%s", body)
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Added: moe/change-auth#1") {
		t.Fatalf("note commit missing its trailer:\n%s", msg)
	}
	if strings.Contains(msg, "MoE-Consent:") {
		t.Fatalf("note commit stamped consent:\n%s", msg)
	}
}

// A ping is a question with no text: it delivers nothing until answered.
func TestAskWritesOpenPingWithConsent(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")

	e := ask(t, root, "change-auth")
	if e.ID != 1 || !e.Open() || e.Pending() {
		t.Fatalf("entry = %+v, want an open ping at id 1", e)
	}

	f, err := Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	open, ok := f.OpenPing()
	if !ok || open.Question != "Which compatibility policy?" {
		t.Fatalf("OpenPing() = %+v, %v", open, ok)
	}
	if got := f.Pending(); len(got) != 0 {
		t.Fatalf("Pending() = %+v, want nothing to deliver", got)
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Asked: moe/change-auth#1") {
		t.Fatalf("ask commit missing its trailer:\n%s", msg)
	}
	if !strings.Contains(msg, "MoE-Consent: dynamic") {
		t.Fatalf("ask commit missing the sweep's consent:\n%s", msg)
	}
}

// Answering turns the ping into an ordinary pending entry that carries
// its own context.
func TestAnswerMakesThePingPending(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")

	e, err := Answer(root, "moe", "change-auth", 1, "Adopt the new default.", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsPing() || !e.Pending() || e.Open() {
		t.Fatalf("entry = %+v, want an answered ping", e)
	}
	f, _ := Load(root, "moe", "change-auth")
	if _, ok := f.OpenPing(); ok {
		t.Fatalf("ping still open after an answer: %+v", f)
	}
	if got := f.Pending(); len(got) != 1 || got[0].Question == "" || got[0].Text == "" {
		t.Fatalf("Pending() = %+v, want the question/answer pair", got)
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Answered: moe/change-auth#1") {
		t.Fatalf("answer commit missing its trailer:\n%s", msg)
	}
	// The answer is the operator's own act, so it carries no ride level.
	if strings.Contains(msg, "MoE-Consent:") {
		t.Fatalf("answer commit stamped consent:\n%s", msg)
	}
}

// One open ping per run, so a second ask is refused rather than
// overwriting or stacking. Notes are unlimited and unaffected.
func TestAskRefusesSecondOpenPingButNotesStack(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")

	_, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"Something else?", "dynamic", io.Discard, io.Discard)
	if !errors.Is(err, ErrOpenPing) {
		t.Fatalf("second Ask err = %v, want ErrOpenPing", err)
	}

	add(t, root, "change-auth", "one")
	add(t, root, "change-auth", "two")
	f, _ := Load(root, "moe", "change-auth")
	if len(f.Notes) != 3 {
		t.Fatalf("entries = %d, want the ask refused and both notes landed", len(f.Notes))
	}
}

// Once the first is answered the next question is allowed — the rule is
// one *open* ping, not one ever.
func TestAskAllowedAfterAnswer(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 0, "Adopt.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	e, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"And the migration?", "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("follow-up Ask: %v", err)
	}
	if e.ID != 2 {
		t.Fatalf("e.ID = %d, want 2", e.ID)
	}
}

// The terminal case, on all three write verbs: a run that has stopped
// has no next turn, so nothing written on it could ever be delivered.
func TestTerminalRunRefusesEveryWrite(t *testing.T) {
	root := seedRoot(t)
	md := seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")

	md.Status = run.StatusMerged
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	if _, err := Answer(root, "moe", "change-auth", 0, "too late", io.Discard, io.Discard); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Answer on merged run = %v, want ErrNotLive", err)
	}
	if _, err := Add(root, "moe", "change-auth", "too late", io.Discard, io.Discard); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Add on merged run = %v, want ErrNotLive", err)
	}
	if _, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"Anything?", "dynamic", io.Discard, io.Discard); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Ask on merged run = %v, want ErrNotLive", err)
	}
}

// The stale case: the web posts the id it rendered, so a tab sitting on
// a question that has since been answered and replaced answers nothing.
func TestAnswerRefusesStalePingID(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 1, "Adopt.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := Ask(root, "moe", "change-auth", "moe/pulse-two",
		"And the migration?", "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The stale tab still thinks #1 is open.
	if _, err := Answer(root, "moe", "change-auth", 1, "In place.", io.Discard, io.Discard); !errors.Is(err, ErrStalePing) {
		t.Fatalf("stale Answer = %v, want ErrStalePing", err)
	}
	f, _ := Load(root, "moe", "change-auth")
	if got := f.Notes[1].Text; got != "" {
		t.Fatalf("ping 2 text = %q, want it untouched", got)
	}
}

func TestEmptyProseIsRefused(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	if _, err := Add(root, "moe", "change-auth", "  \n ", io.Discard, io.Discard); !errors.Is(err, ErrEmpty) {
		t.Fatalf("blank Add = %v, want ErrEmpty", err)
	}
	if _, err := Ask(root, "moe", "change-auth", "moe/p", " ", "dynamic", io.Discard, io.Discard); !errors.Is(err, ErrEmpty) {
		t.Fatalf("blank Ask = %v, want ErrEmpty", err)
	}
	ask(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 0, "\t", io.Discard, io.Discard); !errors.Is(err, ErrEmpty) {
		t.Fatalf("blank Answer = %v, want ErrEmpty", err)
	}
}

func TestAnswerWithNothingOpen(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	if _, err := Answer(root, "moe", "change-auth", 0, "hello", io.Discard, io.Discard); !errors.Is(err, ErrNoOpenPing) {
		t.Fatalf("Answer with no record = %v, want ErrNoOpenPing", err)
	}
	add(t, root, "change-auth", "a plain note")
	if _, err := Answer(root, "moe", "change-auth", 0, "hello", io.Discard, io.Discard); !errors.Is(err, ErrNoOpenPing) {
		t.Fatalf("Answer against a note = %v, want ErrNoOpenPing", err)
	}
}

// Delivery stamps only the ids the prompt carried, and only the entries
// still pending — the mid-turn-add race's whole point.
func TestMarkDeliveredStampsOnlyRenderedPendingIDs(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	add(t, root, "change-auth", "first")
	// The turn rendered #1 and started. #2 lands mid-turn.
	add(t, root, "change-auth", "second")

	if err := MarkDelivered(root, "moe", "change-auth", "code", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	f, _ := Load(root, "moe", "change-auth")
	if f.Notes[0].DeliveredTo != "code" {
		t.Fatalf("entry 1 = %+v, want delivered to code", f.Notes[0])
	}
	if !f.Notes[1].Pending() {
		t.Fatalf("entry 2 = %+v, want it still pending for the next turn", f.Notes[1])
	}

	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	if !strings.Contains(msg, "MoE-Input-Delivered: moe/change-auth#1 code") {
		t.Fatalf("delivery commit missing its trailer:\n%s", msg)
	}
	// Not a stage turn — stamping MoE-Document would read as one.
	if strings.Contains(msg, "MoE-Document:") {
		t.Fatalf("delivery commit stamped a document trailer:\n%s", msg)
	}

	// Re-marking is a no-op: no second commit, nothing overwritten.
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	if err := MarkDelivered(root, "moe", "change-auth", "review", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if after := gittest.Output(t, root, "rev-parse", "HEAD"); after != before {
		t.Fatalf("re-marking wrote a commit: %s → %s", before, after)
	}
}

// An open ping is never delivered — it has nothing to deliver — so a
// caller that names it writes no commit.
func TestMarkDeliveredIgnoresOpenPingAndEmptyList(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	ask(t, root, "change-auth")
	before := gittest.Output(t, root, "rev-parse", "HEAD")
	for _, ids := range [][]int{nil, {1}} {
		if err := MarkDelivered(root, "moe", "change-auth", "code", ids, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if after := gittest.Output(t, root, "rev-parse", "HEAD"); after != before {
		t.Fatalf("MarkDelivered wrote a commit with nothing to stamp: %s → %s", before, after)
	}
}

// A record that violates the invariants refuses loudly. It is
// machine-written, so a violation is a bug to see rather than noise to
// route around.
func TestLoadRefusesMalformedRecord(t *testing.T) {
	cases := map[string]string{
		"two open pings": `{"notes":[{"id":1,"question":"a?"},{"id":2,"question":"b?"}]}`,
		"sparse ids":     `{"notes":[{"id":7,"text":"hi"}]}`,
		"empty entry":    `{"notes":[{"id":1}]}`,
		"blank text":     `{"notes":[{"id":1,"text":"  "}]}`,
		"delivered ping": `{"notes":[{"id":1,"question":"a?","delivered_to":"code"}]}`,
		"not json":       `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "change-auth")
			write(t, root, Path("moe", "change-auth"), body)
			if _, err := Load(root, "moe", "change-auth"); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Load = %v, want ErrMalformed", err)
			}
		})
	}
}

// A run with no record is the common case and costs nothing.
func TestLoadMissingRecordIsEmpty(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	f, err := Load(root, "moe", "change-auth")
	if err != nil {
		t.Fatalf("Load on a run with no record: %v", err)
	}
	if _, ok := f.OpenPing(); ok || len(f.Notes) != 0 || len(f.Pending()) != 0 {
		t.Fatalf("f = %+v, want empty", f)
	}
}

// Carry copies every undelivered shape and leaves both source history
// and destination-local history intact. The destination owns fresh ids:
// an idea's ids are meaningful only within the idea's record.
func TestCarryCopiesUndeliveredEntriesWithDenseDestinationIDs(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "idea")
	seedRun(t, root, "destination")

	add(t, root, "idea", "already consumed")
	if err := MarkDelivered(root, "moe", "idea", "idea", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	add(t, root, "idea", "operator note")
	answered := ask(t, root, "idea")
	if _, err := Answer(root, "moe", "idea", answered.ID, "Keep the old policy.", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	open := ask(t, root, "idea")
	add(t, root, "destination", "destination-local note")

	n, err := Carry(root, "moe", "idea", "moe", "destination", "dynamic", io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if n != 3 {
		t.Fatalf("Carry count = %d, want 3", n)
	}

	dst, err := Load(root, "moe", "destination")
	if err != nil {
		t.Fatal(err)
	}
	if len(dst.Notes) != 4 {
		t.Fatalf("destination notes = %+v, want its note plus three carried entries", dst.Notes)
	}
	for i, e := range dst.Notes {
		if e.ID != i+1 {
			t.Fatalf("destination entry %d has id %d", i, e.ID)
		}
		if e.Delivered() {
			t.Fatalf("carried destination entry is already delivered: %+v", e)
		}
	}
	if got := dst.Notes[1]; got.Text != "operator note" || got.IsPing() {
		t.Fatalf("carried note = %+v", got)
	}
	if got := dst.Notes[2]; got.Question != answered.Question || got.Text != "Keep the old policy." || got.AskedBy != answered.AskedBy {
		t.Fatalf("carried answered ping = %+v", got)
	}
	if got := dst.Notes[3]; got.Question != open.Question || got.Text != "" || got.AskedBy != open.AskedBy || !got.Open() {
		t.Fatalf("carried open ping = %+v", got)
	}

	src, err := Load(root, "moe", "idea")
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Notes) != 4 || src.Notes[0].DeliveredTo != "idea" || src.Notes[1].ID != 2 || src.Notes[3].ID != 4 {
		t.Fatalf("source record changed: %+v", src.Notes)
	}
	msg := gittest.Output(t, root, "log", "-1", "--format=%B")
	for _, want := range []string{
		"input: carried moe/idea#2,3,4 → moe/destination#2,3,4",
		"MoE-Run: destination",
		"MoE-Project: moe",
		"MoE-Workflow: sdlc",
		"MoE-Consent: dynamic",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("carry commit missing %q:\n%s", want, msg)
		}
	}
}

// No record and an all-delivered record are both ordinary no-ops: no
// destination file rewrite and no bookkeeping-only journal commit.
func TestCarryNoOpsWithoutUndeliveredInput(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "idea")
	seedRun(t, root, "destination")

	head := gittest.Output(t, root, "rev-parse", "HEAD")
	if n, err := Carry(root, "moe", "no-record", "moe", "destination", "", io.Discard, io.Discard); err != nil || n != 0 {
		t.Fatalf("Carry(no record) = %d, %v", n, err)
	}
	if got := gittest.Output(t, root, "rev-parse", "HEAD"); got != head {
		t.Fatalf("no-record carry committed: before %s after %s", head, got)
	}

	add(t, root, "idea", "done")
	if err := MarkDelivered(root, "moe", "idea", "idea", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	head = gittest.Output(t, root, "rev-parse", "HEAD")
	if n, err := Carry(root, "moe", "idea", "moe", "destination", "", io.Discard, io.Discard); err != nil || n != 0 {
		t.Fatalf("Carry(all delivered) = %d, %v", n, err)
	}
	if got := gittest.Output(t, root, "rev-parse", "HEAD"); got != head {
		t.Fatalf("all-delivered carry committed: before %s after %s", head, got)
	}
}

// A destination can already have an open ping on the twin path. The
// source's machine question is re-askable there, while carrying it would
// violate the run-addressed answer invariant, so only that entry drops.
func TestCarryDropsAnOpenPingCollisionAndCarriesTheRest(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "idea")
	seedRun(t, root, "destination")

	sourceOpen := ask(t, root, "idea")
	add(t, root, "idea", "still carry me")
	destinationOpen := ask(t, root, "destination")
	add(t, root, "destination", "already here")

	var stderr strings.Builder
	n, err := Carry(root, "moe", "idea", "moe", "destination", "", io.Discard, &stderr)
	if err != nil {
		t.Fatalf("Carry: %v", err)
	}
	if n != 1 {
		t.Fatalf("Carry count = %d, want 1", n)
	}
	if !strings.Contains(stderr.String(), "dropping open ping moe/idea#1") ||
		!strings.Contains(stderr.String(), "moe/destination already has one open") {
		t.Fatalf("collision warning = %q", stderr.String())
	}

	dst, err := Load(root, "moe", "destination")
	if err != nil {
		t.Fatal(err)
	}
	if len(dst.Notes) != 3 || dst.Notes[2].ID != 3 || dst.Notes[2].Text != "still carry me" {
		t.Fatalf("destination record = %+v", dst.Notes)
	}
	if got, ok := dst.OpenPing(); !ok || got.ID != destinationOpen.ID || got.Question != destinationOpen.Question {
		t.Fatalf("destination open ping = %+v, %v", got, ok)
	}
	if src, err := Load(root, "moe", "idea"); err != nil || len(src.Notes) != 2 || src.Notes[0] != sourceOpen {
		t.Fatalf("source record = %+v, %v", src.Notes, err)
	}
	if got := gittest.Output(t, root, "log", "-1", "--format=%s"); !strings.Contains(got, "moe/idea#2 → moe/destination#3") {
		t.Fatalf("carry subject = %q", got)
	}
}

// Scan is the queue: everything still live on in-progress runs, oldest
// first, with terminal runs dropped even though their history stays on
// disk.
//
// The slugs are chosen against the alphabetical tie-break: `zulu` is
// written first and must list first, so a Scan that ignored the commit
// time would fail rather than accidentally pass.
func TestScanListsLiveEntriesOldestFirstAndDropsTerminalRuns(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "alpha")
	seedRun(t, root, "zulu")
	gone := seedRun(t, root, "gone")

	// Pin the commits apart: LastFileActivity reads %ct, so two writes
	// inside one second would tie and fall through to the slug order the
	// slugs above are chosen to contradict.
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-01T10:00:00+00:00")
	ask(t, root, "zulu")
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-02T10:00:00+00:00")
	add(t, root, "alpha", "ship it")
	t.Setenv("GIT_COMMITTER_DATE", "2026-08-03T10:00:00+00:00")
	ask(t, root, "gone")
	os.Unsetenv("GIT_COMMITTER_DATE")

	gone.Status = run.StatusClosed
	if err := run.Save(root, gone); err != nil {
		t.Fatal(err)
	}

	waiting, errs := Scan(root, "")
	if len(errs) != 0 {
		t.Fatalf("Scan errs = %v", errs)
	}
	var got []string
	for _, w := range waiting {
		got = append(got, w.Run)
	}
	if len(got) != 2 || got[0] != "zulu" || got[1] != "alpha" {
		t.Fatalf("Scan = %v, want [zulu alpha] — oldest first, terminal dropped", got)
	}
	if !waiting[0].Entry.Open() || waiting[1].Entry.Open() {
		t.Fatalf("Scan = %+v, want zulu's ping open and alpha's note pending", waiting)
	}
	if got := waiting[0].Ref(); got != "moe/zulu#1" {
		t.Fatalf("Ref() = %q", got)
	}
	// The terminal run keeps its history for its own page.
	f, err := Load(root, "moe", "gone")
	if err != nil || len(f.Notes) != 1 {
		t.Fatalf("terminal run's history = %+v, %v", f, err)
	}
}

// A delivered note drops out of the queue: it needs nothing from anyone.
func TestScanDropsDeliveredEntries(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "change-auth")
	add(t, root, "change-auth", "ship it")
	if err := MarkDelivered(root, "moe", "change-auth", "code", []int{1}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if waiting, _ := Scan(root, ""); len(waiting) != 0 {
		t.Fatalf("Scan = %+v, want the delivered note gone", waiting)
	}
}

func TestScanFiltersByProject(t *testing.T) {
	root := seedRoot(t)
	write(t, root, "projects/other/project.json", `{"id":"other"}`)
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed second project")
	seedRun(t, root, "mine")
	if _, err := run.New(root, "other", run.Options{ID: "theirs", Workflow: "sdlc"}); err != nil {
		t.Fatal(err)
	}
	ask(t, root, "mine")
	if _, err := Ask(root, "other", "theirs", "other/pulse",
		"Which?", "dynamic", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	waiting, _ := Scan(root, "moe")
	if len(waiting) != 1 || waiting[0].Project != "moe" {
		t.Fatalf("Scan(moe) = %+v, want only moe's", waiting)
	}
}

// One unreadable record degrades to one missing row, not an empty queue.
func TestScanSkipsMalformedRecordWithError(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "good")
	seedRun(t, root, "bad")
	ask(t, root, "good")
	write(t, root, Path("moe", "bad"), `{`)

	waiting, errs := Scan(root, "")
	if len(waiting) != 1 || waiting[0].Run != "good" {
		t.Fatalf("Scan = %+v, want just the readable one", waiting)
	}
	if len(errs) != 1 {
		t.Fatalf("Scan errs = %v, want one", errs)
	}
}

// FirstLine is what the multi-entry surfaces list; the full prose stays
// for the prompt.
func TestFirstLine(t *testing.T) {
	e := Entry{Text: "skip the flake\nit fails on ARM only\n"}
	if got := e.FirstLine(); got != "skip the flake" {
		t.Fatalf("note FirstLine() = %q", got)
	}
	// A ping is identified by its question, answered or not — the answer
	// alone reads as a fragment on a list of many runs.
	answered := Entry{Question: "which one?", Text: "the second"}
	if got := answered.FirstLine(); got != "which one?" {
		t.Fatalf("answered ping FirstLine() = %q", got)
	}
	if got := (Entry{Question: "which one?"}).FirstLine(); got != "which one?" {
		t.Fatalf("open ping FirstLine() = %q", got)
	}
}
