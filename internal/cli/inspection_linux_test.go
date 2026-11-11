package cli

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestInspectionDirectRexGating(t *testing.T) {
	_, terminal := cooldownPTY(t)
	old := os.Stdin
	os.Stdin = terminal
	t.Cleanup(func() { os.Stdin = old })
	t.Setenv("REX_SESSION", inspectionSession)
	t.Setenv("REX_BLOCK", inspectionOrigin)
	t.Setenv("TERM", "xterm-rex")
	t.Setenv("MOE_SERVE_AGENT", "")
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	t.Setenv("MOE_MARKDOWN_PAGER", "touch must-not-run")
	bin := t.TempDir()
	inspectionFile(t, bin+"/rex", "#!/bin/sh\nexit 99\n", 0700)
	t.Setenv("PATH", bin)
	md := stageMetadata("demo", "run", "sdlc")
	if newPromptInspection(t.TempDir(), md, "code", "canvas", false, terminal) == nil {
		t.Fatal("direct Rex not offered")
	}
	for _, tc := range []struct{ key, value string }{
		{"MOE_SERVE_AGENT", "1"}, {"TMUX", "/tmp/tmux"}, {"STY", "screen"}, {"TERM", "tmux-256color"}, {"TERM", "screen-256color"}, {"REX_BLOCK", ""}, {"REX_SESSION", ""},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if newPromptInspection(t.TempDir(), md, "code", "canvas", true, terminal) != nil {
				t.Fatal("suppressed environment offered viewer")
			}
		})
	}
	if newPromptInspection(t.TempDir(), md, "code", "canvas", true, io.Discard) != nil {
		t.Fatal("non-terminal output offered viewer")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdin = r
	if newPromptInspection(t.TempDir(), md, "code", "canvas", true, terminal) != nil {
		t.Fatal("non-terminal stdin offered viewer")
	}
	os.Stdin = terminal
	// --park must return before printing a menu, even in a direct Rex tty.
	var out strings.Builder
	if code := promptNextStageOverride(t.TempDir(), md, "code", "", true, &out, io.Discard); code != 0 {
		t.Fatalf("park exit %d", code)
	}
	if strings.Contains(out.String(), "view canvas") {
		t.Fatalf("park prompted: %s", &out)
	}
}
