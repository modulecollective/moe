package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestWorkflowDispatcherRoutesToRegisteredWorkflow confirms that
// `moe workflow <name> <sub>` reaches the same per-workflow command as
// calling the workflow's Command() directly. The test registers a
// throwaway workflow so the assertion doesn't depend on the real sdlc/kb
// tables staying shaped a certain way.
func TestWorkflowDispatcherRoutesToRegisteredWorkflow(t *testing.T) {
	var gotArgs []string
	wf := NewWorkflow("__probe", "test-only probe workflow")
	wf.Register(&Command{
		Name:    "alpha",
		Summary: "A",
		Run: func(args []string, stdout, stderr io.Writer) int {
			gotArgs = append([]string(nil), args...)
			return 7
		},
	})
	RegisterWorkflow(wf)
	defer delete(workflows, "__probe")

	var out, errb bytes.Buffer
	for _, verb := range []string{"workflow", "wf"} {
		gotArgs = nil
		out.Reset()
		errb.Reset()
		code := Run([]string{verb, "__probe", "alpha", "p", "r"}, &out, &errb)
		if code != 7 {
			t.Fatalf("%s: exit=%d want=7 stderr=%q", verb, code, errb.String())
		}
		want := []string{"p", "r"}
		if len(gotArgs) != len(want) || gotArgs[0] != want[0] || gotArgs[1] != want[1] {
			t.Fatalf("%s: args=%v want=%v", verb, gotArgs, want)
		}
	}
}

// TestWorkflowDispatcherRejectsUnexposed confirms a workflow that
// opts out of `moe workflow` (ExposedViaCLI=false) stays invisible to
// the dispatcher through either alias. Uses a throwaway workflow so
// the test doesn't depend on any real workflow choosing to hide.
func TestWorkflowDispatcherRejectsUnexposed(t *testing.T) {
	wf := NewWorkflow("__hidden", "test-only hidden workflow")
	wf.ExposedViaCLI = false
	wf.Register(&Command{Name: "ping", Summary: "p", Run: nopRun})
	RegisterWorkflow(wf)
	defer delete(workflows, "__hidden")

	for _, verb := range []string{"workflow", "wf"} {
		var out, errb bytes.Buffer
		code := Run([]string{verb, "__hidden", "ping"}, &out, &errb)
		if code == 0 {
			t.Fatalf("%s: expected non-zero exit, got 0 (stdout=%q)", verb, out.String())
		}
		if !strings.Contains(errb.String(), `unknown workflow "__hidden"`) {
			t.Fatalf("%s: stderr should mention unknown workflow, got %q", verb, errb.String())
		}
	}
}

// TestWorkflowDispatcherNoArgsPrintsUsage confirms the zero-arg path
// shows the list of exposed workflows and stays quiet on stderr.
func TestWorkflowDispatcherNoArgsPrintsUsage(t *testing.T) {
	for _, verb := range []string{"workflow", "wf"} {
		var out, errb bytes.Buffer
		code := Run([]string{verb}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", verb, code, errb.String())
		}
		if !strings.Contains(out.String(), "usage: moe workflow") {
			t.Fatalf("%s: missing usage header: %q", verb, out.String())
		}
		for _, want := range []string{"sdlc", "kb", "quick", "idea"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s: usage missing workflow %q: %q", verb, want, out.String())
			}
		}
		if errb.Len() != 0 {
			t.Fatalf("%s: stderr should be empty, got %q", verb, errb.String())
		}
	}
}

// TestWorkflowDispatcherUnknownName prints "unknown workflow" and a
// listing of what's registered, matching the top-level dispatcher's
// error shape.
func TestWorkflowDispatcherUnknownName(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"workflow", "bogus"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit on unknown workflow")
	}
	if !strings.Contains(errb.String(), `unknown workflow "bogus"`) {
		t.Fatalf("stderr missing unknown-workflow hint: %q", errb.String())
	}
}
