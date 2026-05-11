package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// Mirror image of TestWorkflowNoArgsPrintsUsage — CommandGroup is the
// dispatch shape extracted from Workflow, so the dispatcher contract is
// pinned the same way.

func TestCommandGroupNoArgsPrintsUsage(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	g.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})
	g.Register(&Command{Name: "beta", Summary: "B", Run: nopRun})

	var out, errb bytes.Buffer
	code := g.Command().Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "usage: moe g <subcommand>") {
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

func TestCommandGroupHelpFlags(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	g.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})

	for _, arg := range []string{"-h", "--help", "help"} {
		var out, errb bytes.Buffer
		code := g.Command().Run([]string{arg}, &out, &errb)
		if code != 0 {
			t.Fatalf("%s: exit=%d stderr=%q", arg, code, errb.String())
		}
		if !strings.Contains(out.String(), "usage: moe g <subcommand>") {
			t.Fatalf("%s: expected usage in stdout, got %q", arg, out.String())
		}
	}
}

func TestCommandGroupUnknownSubcommand(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	g.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})

	var out, errb bytes.Buffer
	code := g.Command().Run([]string{"bogus"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit on unknown subcommand")
	}
	if !strings.Contains(errb.String(), `unknown g subcommand "bogus"`) {
		t.Fatalf("stderr=%q", errb.String())
	}
}

func TestCommandGroupRoutesArgsToSubcommand(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	var gotArgs []string
	g.Register(&Command{
		Name:    "alpha",
		Summary: "A",
		Run: func(args []string, stdout, stderr io.Writer) int {
			gotArgs = append([]string(nil), args...)
			return 7
		},
	})

	var out, errb bytes.Buffer
	code := g.Command().Run([]string{"alpha", "p", "r", "--flag"}, &out, &errb)
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

func TestCommandGroupRegisterPanicsOnDuplicate(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	g.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate subcommand registration")
		}
	}()
	g.Register(&Command{Name: "alpha", Summary: "A2", Run: nopRun})
}

// Hidden subcommands stay reachable by exact name but drop out of the
// usage listing. Mirrors the same rule on Command at the top level.
func TestCommandGroupHidesHiddenFromUsage(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	g.Register(&Command{Name: "alpha", Summary: "A", Run: nopRun})
	g.Register(&Command{Name: "secret", Summary: "S", Run: nopRun, Hidden: true})

	var out, errb bytes.Buffer
	code := g.Command().Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "alpha") {
		t.Fatalf("usage missing alpha: %q", out.String())
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("hidden subcommand leaked into usage: %q", out.String())
	}
	// Hidden subcommand still dispatches by exact name.
	var got bytes.Buffer
	if code := g.Command().Run([]string{"secret"}, &got, &errb); code != 0 {
		t.Fatalf("hidden secret should still dispatch; got exit=%d stderr=%q", code, errb.String())
	}
}

func TestCommandGroupLookup(t *testing.T) {
	g := NewCommandGroup("g", "test group")
	alpha := &Command{Name: "alpha", Summary: "A", Run: nopRun}
	g.Register(alpha)

	if got := g.Lookup("alpha"); got != alpha {
		t.Fatalf("Lookup(alpha) = %v, want %p", got, alpha)
	}
	if got := g.Lookup("missing"); got != nil {
		t.Fatalf("Lookup(missing) = %v, want nil", got)
	}
}
