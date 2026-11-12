package cli

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/serve"
)

// receiptRefusal is what refuseFailedReceipts' hook says. The angle
// brackets and ampersand prove the web escapes it.
const receiptRefusal = "receipt <refused> by policy & hook"

// refuseFailedReceipts installs a repo-scoped commit-msg hook that
// refuses only failed control receipts: requests are accepted, other
// receipts and unrelated commits land.
func refuseFailedReceipts(t *testing.T, root string) {
	t.Helper()
	hooks := t.TempDir()
	script := "#!/bin/sh\ncase \"$(head -n 1 \"$1\")\" in 'control: failed '*) echo '" + receiptRefusal + "' >&2; exit 1;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
}

// owedFinishFixture leaves the run with a finish its owner ran and saw
// fail, whose failed receipt the journal refused: the record reads it
// accepted, the local store failed.
func owedFinishFixture(t *testing.T) (string, *run.Metadata, *[]openSdlcStageInvocation, *[]pushFromCascadeInvocation, map[string]int) {
	t.Helper()
	root, md := controlFixture(t)
	exits := map[string]int{"code": 1}
	dispatched := stubOpenSdlcStage(t, exits)
	push := stubPushFromCascade(t, 0, nil)
	refuseFailedReceipts(t, root)
	inner := openSdlcStage
	openSdlcStage = func(s, p, r string, h bool, o, e io.Writer) int {
		if s == "design" {
			submitControl(t, root, runcontrol.KindFinish, "owed")
		}
		return inner(s, p, r, h, o, e)
	}
	t.Cleanup(func() { openSdlcStage = inner })
	if code := dispatchCascade("!!", "design", root, md, io.Discard, io.Discard); code != 1 {
		t.Fatalf("fixture: want code's failure, got %d", code)
	}
	if r, _ := controlRecord(t, root).Find("owed"); r.State != runcontrol.StateAccepted {
		t.Fatalf("fixture: the hook should have refused the receipt: %+v", r)
	}
	owed, err := runcontrol.Unrecorded(root, md.Project, md.ID)
	if s := owed["owed"]; err != nil || s.State != runcontrol.StateFailed || !strings.Contains(s.RecordingError, receiptRefusal) {
		t.Fatalf("fixture: local receipt = %+v, %v", owed, err)
	}
	return root, md, dispatched, push, exits
}

// journalState is everything a read must not move.
func journalState(t *testing.T, root string) string {
	t.Helper()
	ctl, _ := os.ReadFile(filepath.Join(root, runcontrol.Path("moe", "change-auth")))
	owed, _ := os.ReadFile(filepath.Join(root, ".moe", "control-unrecorded", "moe", "change-auth.json"))
	return gittest.Output(t, root, "rev-parse", "HEAD") + gittest.Output(t, root, "ls-files", "-s") +
		gittest.Output(t, root, "status", "--porcelain") + string(ctl) + string(owed)
}

func runShow(t *testing.T) (string, int) {
	t.Helper()
	var out, errs bytes.Buffer
	code := runControlShow([]string{"moe/change-auth"}, &out, &errs)
	return out.String() + errs.String(), code
}

// A finish whose failure its owner knows but could not record reads as
// failed and unrecorded on every surface — never pending — while the
// record keeps saying accepted, its raw state still suppresses a fresh
// finish, and reading it changes nothing.
func TestOwedFinishReadsAsItsLocalOutcome(t *testing.T) {
	root, md, dispatched, push, _ := owedFinishFixture(t)
	before, ran := journalState(t, root), len(*dispatched)

	o := ObserveRuns(root, []*run.Metadata{md}, time.Now())["moe/change-auth"]
	if got := o.Summary(time.Now()); got != "finish failed · receipt not recorded" {
		t.Fatalf("summary = %q", got)
	}
	snap, err := GatherDashSnapshot(root, time.Now(), DashFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var board bytes.Buffer
	t.Setenv("NO_COLOR", "1")
	dash.Render(&board, time.Now(), nil, snap.Rows, 1, 1, false, dash.FactoryState{}, rand.New(rand.NewSource(1)))
	if !strings.Contains(board.String(), "[finish failed · receipt not recorded]") || strings.Contains(board.String(), "finish pending") {
		t.Fatalf("dash row:\n%s", board.String())
	}

	out, code := runShow(t)
	for _, want := range []string{"finish owed — failed", "code stopped (exit 1) — receipt not recorded",
		"recording failed when kept: ", receiptRefusal, "the next owner of this run records it before starting work"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if line := out[strings.Index(out, "finish owed"):]; strings.Count(strings.TrimRight(line, "\n"), "\n") != 0 {
		t.Errorf("the request line spans lines:\n%s", line)
	}
	if code != 0 || strings.Contains(out, "waiting for the owner") {
		t.Fatalf("show (exit %d) calls the failed finish pending:\n%s", code, out)
	}

	v, ok, err := serveRunControl(root, "moe", "change-auth")
	if err != nil || !ok || v.Summary != "finish failed · receipt not recorded" || len(v.Requests) != 1 ||
		!strings.Contains(v.Requests[0], "failed — code stopped (exit 1) — receipt not recorded") ||
		!slices.Contains(v.Lines, "receipt not recorded: the next owner of this run records it before starting work") {
		t.Fatalf("web view = %+v, %v, %v", v, ok, err)
	}
	if slices.Contains(v.Actions, "finish") || !slices.Contains(v.Actions, "pause") {
		t.Fatalf("actions must follow the record (an owed finish is still open there): %v", v.Actions)
	}

	if after := journalState(t, root); after != before || len(*dispatched) != ran || len(*push) != 0 {
		t.Fatalf("a status read moved state: dispatched %d→%d pushes=%d\nbefore:\n%s\nafter:\n%s", ran, len(*dispatched), len(*push), before, after)
	}
}

// Submitting again — the same id, a fresh one, from the terminal, the
// web, or through a live owner — reads the existing request back as the
// failure it was, creates nothing, and never says it is waiting.
func TestOwedFinishReadsBackOnResubmission(t *testing.T) {
	root, md, _, push, _ := owedFinishFixture(t)
	before := journalState(t, root)
	submit := func(args ...string) (string, string) {
		t.Helper()
		var out, errs bytes.Buffer
		if code := runControlSubmit(runcontrol.KindFinish, append(args, "moe/change-auth"), &out, &errs); code != 0 {
			t.Fatalf("finish %v: exit %d: %s", args, code, errs.String())
		}
		return out.String(), errs.String()
	}
	check := func(how, out, errs string) {
		t.Helper()
		if !strings.Contains(out, "moe/change-auth: existing request finish owed — failed") ||
			!strings.Contains(out, "receipt not recorded") ||
			strings.Contains(out+errs, "already satisfied") || strings.Contains(out+errs, "next boundary") {
			t.Errorf("%s readback:\nstdout: %s\nstderr: %s", how, out, errs)
		}
	}
	out, errs := submit("--id", "owed")
	check("same id", out, errs)
	out, errs = submit("--id", "fresh")
	check("fresh id", out, errs)
	if got, err := serveSubmitControl(root, "moe", "change-auth", "finish", "web-fresh", 0); err != nil ||
		!strings.Contains(got, "failed") || !strings.Contains(got, "receipt not recorded") {
		t.Errorf("web readback = %q, %v", got, err)
	}
	if _, err := serveSubmitControl(root, "moe", "change-auth", "pause", "owed", 0); err == nil {
		t.Error("a reused id with another intent was not refused")
	}

	// The lost-ack fallback's note must not promise the boundary either.
	r, _ := controlRecord(t, root).Find("owed")
	if rb, err := readBack(root, md, r, "saved; the owner's acknowledgement was lost"); err != nil || strings.Contains(rb.note, "next boundary") || !rb.owed {
		t.Errorf("lost-ack readback = %+v, %v", rb, err)
	}

	if after := journalState(t, root); after != before {
		t.Fatalf("resubmission changed the journal:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Through a live owner's endpoint.
	ownForTest(t, root)
	out, errs = submit("--id", "via-owner")
	check("live owner", out, errs)
	if f := controlRecord(t, root); len(f.Requests) != 1 || len(*push) != 0 {
		t.Fatalf("resubmission created a request or shipped: %+v pushes=%d", f.Requests, len(*push))
	}
}

// Once the receipt can be committed again, the next owner records it,
// the debt clears, surfaces read the journal's receipt, and nothing ever
// shipped under the old finish.
func TestOwedFinishClearsAfterRecovery(t *testing.T) {
	root, md, dispatched, push, exits := owedFinishFixture(t)
	ran := len(*dispatched)
	delete(exits, "code")
	var errs bytes.Buffer
	if code := chainKickRun(root, md.Project, md.ID, rideDynamic, true, io.Discard, &errs); code != exitYielded || len(*dispatched) != ran {
		t.Fatalf("kick with the receipt still refused: exit=%d dispatched %d→%d: %s", code, ran, len(*dispatched), errs.String())
	}
	gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	chainKickRun(root, md.Project, md.ID, rideDynamic, true, io.Discard, io.Discard)
	if len(*push) != 0 {
		t.Fatalf("shipped under the failed finish: pushes=%d", len(*push))
	}
	if owed, err := runcontrol.Unrecorded(root, md.Project, md.ID); err != nil || len(owed) != 0 {
		t.Fatalf("owed after recovery: %v, %v", owed, err)
	}
	out, _ := runShow(t)
	if !strings.Contains(out, "finish owed — failed — code stopped (exit 1)") || strings.Contains(out, "receipt not recorded") {
		t.Fatalf("show after recovery:\n%s", out)
	}
}

// Local evidence that is missing, spent, unreadable or nonsensical
// never reads as an outcome it does not prove, and never as a grant.
func TestLocalReceiptEvidenceEdges(t *testing.T) {
	root, md := controlFixture(t)
	submitControl(t, root, runcontrol.KindFinish, "f1")
	local := filepath.Join(root, ".moe", "control-unrecorded", "moe", "change-auth.json")
	put := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	observe := func() RunObservation {
		return ObserveRuns(root, []*run.Metadata{md}, time.Now())["moe/change-auth"]
	}

	if got := observe().Summary(time.Now()); got != "finish pending" {
		t.Fatalf("no debt: summary = %q", got)
	}

	// A completed outcome kept before the error was stored.
	put(`{"f1": {"State": "completed", "Boundary": "push", "Outcome": "shipped"}}`)
	o := observe()
	if got := o.Summary(time.Now()); got != "finish completed · receipt not recorded" {
		t.Fatalf("completed: summary = %q", got)
	}
	r, _ := o.Control.Find("f1")
	if got := o.describe(r); got != "completed at push — shipped — receipt not recorded" {
		t.Fatalf("completed legacy: %q", got)
	}

	// A local copy naming no request is reported, not synthesized.
	put(`{"ghost": {"State": "failed", "Outcome": "nope"}}`)
	o = observe()
	if got := o.Summary(time.Now()); got != "finish pending" || len(o.Control.Requests) != 1 ||
		!slices.Contains(o.localLines(), "local receipt ghost: names no recorded request; ignored") {
		t.Fatalf("unmatched: summary=%q lines=%v", got, o.localLines())
	}

	// A non-final local state proves nothing either way.
	put(`{"f1": {"State": "accepted"}}`)
	o = observe()
	if got := o.Summary(time.Now()); got != "finish outcome unknown" ||
		!strings.Contains(o.describe(r), "outcome unknown — its local receipt is invalid") {
		t.Fatalf("invalid: summary=%q describe=%q", got, o.describe(r))
	}

	// An unreadable store: known facts stay, nothing is offered, show fails.
	put(`{`)
	o = observe()
	if got := o.Summary(time.Now()); got != "local receipts unreadable · finish outcome unknown" {
		t.Fatalf("unreadable: summary = %q", got)
	}
	v, ok, err := serveRunControl(root, "moe", "change-auth")
	if err != nil || !ok || v.Actions != nil || !strings.HasPrefix(v.Lines[len(v.Lines)-1], "local receipts: unreadable — ") {
		t.Fatalf("unreadable web view = %+v, %v, %v", v, ok, err)
	}
	if out, code := runShow(t); code != 1 || strings.Contains(out, "waiting for the owner") || !strings.Contains(out, "outcome unknown") {
		t.Fatalf("unreadable show (exit %d):\n%s", code, out)
	}
	if _, ok := openFinish(root, md); ok {
		t.Fatal("an unreadable owed-receipt store granted a finish")
	}

	// A receipt the journal already carries wins over a leftover copy.
	put(`{}`) // Settle spends local copies, so it needs a readable store
	if err := runcontrol.Settle(root, md.Project, md.ID, "f1", runcontrol.Settlement{State: runcontrol.StateFailed, Outcome: "stopped"}, "static"); err != nil {
		t.Fatal(err)
	}
	put(`{"f1": {"State": "completed", "Outcome": "stale copy"}}`)
	o = observe()
	r, _ = o.Control.Find("f1")
	if got := o.Summary(time.Now()); got != "" || len(o.localLines()) != 0 || o.describe(r) != "failed — stopped" {
		t.Fatalf("durable wins: summary=%q lines=%v describe=%q", got, o.localLines(), o.describe(r))
	}
}

// The web reads the overlay through serve's real callbacks: the run
// page, its status fragment, the dash's status JSON, and the page a
// control POST redirects to.
func TestServeShowsOwedOutcomeThroughRealWiring(t *testing.T) {
	root, _, _, _, _ := owedFinishFixture(t)
	srv, err := serve.New(serveOptions(root, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	do := func(req *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	escaped := "receipt &lt;refused&gt; by policy &amp; hook"
	for _, path := range []string{"/run/moe/change-auth", "/run/moe/change-auth/status"} {
		rr := do(httptest.NewRequest("GET", path, nil))
		body := rr.Body.String()
		if rr.Code != 200 || !strings.Contains(body, "receipt not recorded") || !strings.Contains(body, escaped) ||
			strings.Contains(body, receiptRefusal) || strings.Contains(body, "waiting for the owner") {
			t.Errorf("GET %s (%d):\n%s", path, rr.Code, body)
		}
	}
	rr := do(httptest.NewRequest("GET", "/run-status", nil))
	if !strings.Contains(rr.Body.String(), `"moe/change-auth":"finish failed · receipt not recorded"`) {
		t.Errorf("run-status = %s", rr.Body.String())
	}
	post := httptest.NewRequest("POST", "/run/moe/change-auth/control", strings.NewReader(url.Values{"action": {"finish"}, "id": {"web-again"}}.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = do(post)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d: %s", rr.Code, rr.Body.String())
	}
	rr = do(httptest.NewRequest("GET", strings.TrimSuffix(rr.Header().Get("Location"), "#run-status"), nil))
	if !strings.Contains(rr.Body.String(), "failed — code stopped (exit 1) — receipt not recorded") {
		t.Errorf("redirect target lacks the outcome:\n%s", rr.Body.String())
	}
}
