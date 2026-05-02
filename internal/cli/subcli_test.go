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
	if !strings.Contains(got, "usage: moe wf <subcommand>") {
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
		if !strings.Contains(out.String(), "usage: moe wf <subcommand>") {
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
		t.Fatal("expected non-zero exit on unknown subcommand")
	}
	if !strings.Contains(errb.String(), `unknown wf subcommand "bogus"`) {
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
			t.Fatal("expected panic on duplicate subcommand registration")
		}
	}()
	w.Register(&Command{Name: "alpha", Summary: "A2", Run: nopRun})
}

// TestSDLCRegistered verifies the init() in sdlc.go actually wired the
// sdlc workflow into the registry and exposed it as the top-level
// `moe sdlc` command. Guards against a future refactor silently
// dropping either registration.
func TestSDLCRegistered(t *testing.T) {
	wf, err := LookupWorkflow("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	if wf.Summary == "" {
		t.Fatal("sdlc workflow summary should not be empty")
	}
	var out, errb bytes.Buffer
	// `moe sdlc` (no subcommand) should print sub-usage and exit 0.
	code := Run([]string{"sdlc"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"new", "design", "code", "push"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("sdlc usage missing subcommand %q: %q", want, out.String())
		}
	}
}

// TestStageWorkflowsRegisteredAsTopLevel guards the dual-registration
// contract: every stage-laddered workflow installs a top-level `moe
// <name>` Command alongside RegisterWorkflow. A future refactor that
// silently drops the Register(wf.Command()) call would break the only
// way operators invoke these workflows. Mirror image of the old "all
// workflows reachable under moe workflow" test that lived in the
// dispatcher.
func TestStageWorkflowsRegisteredAsTopLevel(t *testing.T) {
	for _, name := range []string{"sdlc", "kb", "quick", "twin"} {
		if _, ok := commands[name]; !ok {
			t.Fatalf("expected top-level command %q to be registered", name)
		}
	}
}

func nopRun(args []string, stdout, stderr io.Writer) int { return 0 }
