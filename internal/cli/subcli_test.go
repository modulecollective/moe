package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestWorkflowNoArgsPrintsUsage(t *testing.T) {
	w := NewWorkflow("wf", "test workflow")
	w.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})
	w.Register(&Command{Name: "beta", Summary: "B", Run: nopRun})

	var out, errb bytes.Buffer
	code := w.Command().Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "usage: moe wf <stage>") {
		t.Fatalf("missing usage header: %q", got)
	}
	for _, want := range []string{"alpha", "beta", "A", "B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q: %q", want, got)
		}
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr should be empty, got %q", errb.String())
	}
}

func TestWorkflowHelpFlags(t *testing.T) {
	w := NewWorkflow("wf", "test workflow")
	w.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})

	for _, arg := range []string{"-h", "--help", "help"} {
		var out, errb bytes.Buffer
		code := w.Command().Run([]string{arg}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", arg, code, errb.String())
		}
		if !strings.Contains(out.String(), "usage: moe wf <stage>") {
			t.Fatalf("%s: expected usage in stdout, got %q", arg, out.String())
		}
	}
}

func TestWorkflowUnknownStage(t *testing.T) {
	w := NewWorkflow("wf", "test workflow")
	w.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})

	var out, errb bytes.Buffer
	code := w.Command().Run([]string{"bogus"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit on unknown stage")
	}
	if !strings.Contains(errb.String(), `unknown wf stage "bogus"`) {
		t.Fatalf("stderr=%q", errb.String())
	}
}

func TestWorkflowRoutesArgsToStage(t *testing.T) {
	w := NewWorkflow("wf", "test workflow")
	var gotArgs []string
	w.Register(&Command{
		Name:    "alpha",
		Summary: "A",
		Run: func(args []string, stdout, stderr io.Writer) int {
			gotArgs = append([]string(nil), args...)
			return 7
		},
	})

	var out, errb bytes.Buffer
	code := w.Command().Run([]string{"alpha", "p", "r", "--flag"}, &out, &errb)
	if code != 7 {
		t.Fatalf("exit=%d want=7 stderr=%q", code, errb.String())
	}
	want := []string{"p", "r", "--flag"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args=%v want=%v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args[%d]=%q want %q", i, gotArgs[i], want[i])
		}
	}
}

func TestWorkflowRegisterPanicsOnDuplicate(t *testing.T) {
	w := NewWorkflow("wf", "test workflow")
	w.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate stage registration")
		}
	}()
	w.Register(&Command{Name: "alpha", Summary: "A2", Run: nopRun})
}

// TestSDLCRegistered verifies the init() in sdlc.go actually wired a
// top-level `sdlc` command into the global table. Guards against a
// future refactor silently dropping the registration.
func TestSDLCRegistered(t *testing.T) {
	cmd, ok := commands["sdlc"]
	if !ok {
		t.Fatal(`expected top-level command "sdlc" to be registered`)
	}
	if cmd.Summary == "" {
		t.Fatal("sdlc command summary should not be empty")
	}
	var out, errb bytes.Buffer
	// `moe sdlc` (no args) should print sub-usage and exit 0.
	code := cmd.Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"design", "code"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("sdlc usage missing stage %q: %q", want, out.String())
		}
	}
}

func nopRun(args []string, stdout, stderr io.Writer) int { return 0 }
