package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runcontrol"
	"github.com/modulecollective/moe/internal/runowner"
)

func runControl(t *testing.T, kind string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	var code int
	switch kind {
	case "show":
		code = runControlShow(args, &out, &errb)
	case "resume":
		code = runControlResume(args, &out, &errb)
	default:
		code = runControlSubmit(runcontrol.Kind(kind), args, &out, &errb)
	}
	return code, out.String(), errb.String()
}

// Handoff holds the run and names the conversation to pick up: the last
// stage that actually ran, with its backend — not the next pending one.
func TestControlHandoffNamesLastConversation(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	md := seedParkedRun(t, root, "change-auth")
	doc, _, err := run.EnsureDocument(root, md, "design")
	if err != nil {
		t.Fatal(err)
	}
	doc.SetSession("codex", "01a1cee2-107e-7f51-992f-2785354a6dba")
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}

	code, out, errs := runControl(t, "handoff", "moe/change-auth", "--id", "h1")
	if code != 0 {
		t.Fatalf("handoff exited %d: %s", code, errs)
	}
	for _, want := range []string{"handoff h1 on moe/change-auth", "moe sdlc design moe/change-auth --agent codex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("handoff output missing %q:\n%s", want, out)
		}
	}

	code, out, _ = runControl(t, "show", "moe/change-auth")
	if code != 0 {
		t.Fatal("show failed")
	}
	for _, want := range []string{
		"owner: none", "hold: handoff h1 — held, ready to resume",
		"release: moe control resume moe/change-auth",
		"conversation: moe sdlc design moe/change-auth --agent codex",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show missing %q:\n%s", want, out)
		}
	}

	// A pause on top is already satisfied.
	_, out, _ = runControl(t, "pause", "moe/change-auth")
	if !strings.Contains(out, "already satisfied by handoff h1") {
		t.Fatalf("second hold output:\n%s", out)
	}
}

// Resume releases the hold and starts nothing; while another invocation
// owns the run it refuses and the hold stays.
func TestControlResumeReleasesOnly(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedParkedRun(t, root, "change-auth")
	if code, _, errs := runControl(t, "pause", "moe/change-auth", "--id", "p1"); code != 0 {
		t.Fatalf("pause: %s", errs)
	}

	h, err := runowner.Acquire(root, "moe", "change-auth")
	if err != nil {
		t.Fatal(err)
	}
	code, _, errs := runControl(t, "resume", "moe/change-auth")
	if code == 0 || !strings.Contains(errs, "owned by another moe invocation") {
		t.Fatalf("contended resume = %d: %s", code, errs)
	}
	if f, _ := runcontrol.Load(root, "moe", "change-auth"); !f.Held() {
		t.Fatal("contended resume released the hold")
	}
	h.Release()

	code, out, errs := runControl(t, "resume", "moe/change-auth")
	if code != 0 || !strings.Contains(out, "released pause p1") {
		t.Fatalf("resume = %d:\n%s%s", code, out, errs)
	}
	if f, _ := runcontrol.Load(root, "moe", "change-auth"); f.Held() {
		t.Fatal("resume left the hold")
	}
	if busy, _ := runowner.Busy(root, "moe", "change-auth"); busy {
		t.Fatal("resume kept the run owned")
	}
	_, out, _ = runControl(t, "resume", "moe/change-auth")
	if !strings.Contains(out, "was not held") {
		t.Fatalf("idle resume output:\n%s", out)
	}
}

// Controls are for operator-paced stage workflows; others are refused
// with the reason, and record-level refusals surface as errors.
func TestControlRefusals(t *testing.T) {
	root := spawnFixture(t)
	t.Chdir(root)
	seedParkedRun(t, root, "change-auth")
	if _, err := run.New(root, "moe", run.Options{ID: "pulse-one", Workflow: pulseWorkflow}); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runControl(t, "finish", "moe/pulse-one")
	if code == 0 || !strings.Contains(errs, "operator-paced stage workflows") {
		t.Fatalf("finish on a pulse = %d: %s", code, errs)
	}
	code, _, errs = runControl(t, "park", "moe/change-auth")
	if code == 0 || !strings.Contains(errs, "no owner to park") {
		t.Fatalf("park with no owner = %d: %s", code, errs)
	}
	runControl(t, "pause", "moe/change-auth")
	code, _, errs = runControl(t, "finish", "moe/change-auth")
	if code == 0 || !strings.Contains(errs, "resume it first") {
		t.Fatalf("finish while held = %d: %s", code, errs)
	}
}
