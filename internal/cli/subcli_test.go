package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestSDLCRegistered verifies the init() in sdlc.go actually wired the
// sdlc workflow into both registries and exposed it as the top-level
// `moe sdlc` command. Guards against a future refactor silently
// dropping either half of the group + workflow pair.
func TestSDLCRegistered(t *testing.T) {
	if _, err := LookupWorkflow("sdlc"); err != nil {
		t.Fatal(err)
	}
	g, err := LookupGroup("sdlc")
	if err != nil {
		t.Fatal(err)
	}
	if g.Summary == "" {
		t.Fatal("sdlc group summary should not be empty")
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
// contract: every CommandGroup that paired with a Workflow installs a
// top-level `moe <name>` Command via RegisterGroup. A regression here
// would break the only way operators invoke these workflows.
func TestStageWorkflowsRegisteredAsTopLevel(t *testing.T) {
	for _, name := range []string{"sdlc", "kb", "quick", "twin"} {
		if _, ok := commands[name]; !ok {
			t.Fatalf("expected top-level command %q to be registered", name)
		}
	}
}

// TestWorkflowRegisterStagePanicsOnDuplicate pins the duplicate-name
// panic on the new RegisterStage method — the only invariant left on
// Workflow itself after the split.
func TestWorkflowRegisterStagePanicsOnDuplicate(t *testing.T) {
	w := NewWorkflow("test-dup-stages")
	w.RegisterStage("alpha")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate stage registration")
		}
	}()
	w.RegisterStage("alpha")
}

func nopRun(args []string, stdout, stderr io.Writer) int { return 0 }
