package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"version"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "moe ") || !strings.Contains(got, Version) {
		t.Fatalf("unexpected version output: %q", got)
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr should be empty, got %q", errb.String())
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run(nil, &out, &errb)
	if code != 0 {
		t.Fatalf("expected zero exit when no args given, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "usage: moe") {
		t.Fatalf("expected usage in stdout, got %q", out.String())
	}
	if !strings.Contains(out.String(), "try 'moe dash'") {
		t.Fatalf("expected dash hint, got %q", out.String())
	}
	if errb.Len() != 0 {
		t.Fatalf("stderr should be empty, got %q", errb.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"bogus"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected non-zero exit on unknown command")
	}
	if !strings.Contains(errb.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr=%q", errb.String())
	}
}

func TestHelpListsRegisteredCommands(t *testing.T) {
	var out, errb bytes.Buffer
	code := Run([]string{"help"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"version", "help", "usage: moe"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help missing %q: %q", want, got)
		}
	}
}

func TestDispatchRoutesArgsToCommand(t *testing.T) {
	var gotArgs []string
	Register(&Command{
		Name:    "__testprobe",
		Summary: "test-only",
		Run: func(args []string, stdout, stderr io.Writer) int {
			gotArgs = append([]string(nil), args...)
			return 7
		},
	})
	defer delete(commands, "__testprobe")

	var out, errb bytes.Buffer
	code := Run([]string{"__testprobe", "a", "b", "--flag"}, &out, &errb)
	if code != 7 {
		t.Fatalf("exit=%d, want 7", code)
	}
	want := []string{"a", "b", "--flag"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args=%v want=%v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args[%d]=%q want %q", i, gotArgs[i], want[i])
		}
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register(&Command{Name: "version", Run: func([]string, io.Writer, io.Writer) int { return 0 }})
}
