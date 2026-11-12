package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Chain prompts no longer present artifacts. In the environment that used
// to offer a Rex viewer — a direct Rex terminal on both ends, viewer
// settings exported, a rex binary on PATH — the menu advertises no view
// keys, the canvas still prints, and v/f decline like any unknown answer
// without touching rex. Output goes to a real PTY so a redirected writer
// can't make the negative check pass trivially.
func TestStageDecisionLaunchesNoViewerInRex(t *testing.T) {
	for _, answer := range []string{"v", "f"} {
		var dispatched int
		var args []string
		for _, surface := range decisionSurfaces(&dispatched, &args) {
			t.Run(surface.name+"/answer="+answer, func(t *testing.T) {
				root, md := decisionFixture(t)
				bin := t.TempDir()
				calls := filepath.Join(t.TempDir(), "rex-calls")
				script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n"
				if err := os.WriteFile(filepath.Join(bin, "rex"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				t.Setenv("REX_SESSION", "s-1")
				t.Setenv("REX_BLOCK", "b-1")
				t.Setenv("TERM", "xterm-rex")
				t.Setenv("MOE_SERVE_AGENT", "")
				t.Setenv("TMUX", "")
				t.Setenv("STY", "")
				t.Setenv("MOE_MARKDOWN_PAGER", "touch "+filepath.Join(root, "pager-ran"))
				t.Setenv("MOE_DIFF_VIEWER", "touch "+filepath.Join(root, "diff-ran"))

				master, terminal := cooldownPTY(t)
				old := os.Stdin
				os.Stdin = terminal
				t.Cleanup(func() { os.Stdin = old })
				if !stdinIsTerminal() {
					t.Fatal("PTY stdin not seen as a terminal")
				}
				if _, err := io.WriteString(master, answer+"\n"); err != nil {
					t.Fatal(err)
				}
				var screen bytes.Buffer
				drained := make(chan struct{})
				go func() {
					_, _ = io.Copy(&screen, master)
					close(drained)
				}()

				dispatched = 0
				var errOut bytes.Buffer
				code := surface.run(t, root, md, terminal, &errOut)
				terminal.Close()
				<-drained

				if code != 0 || dispatched != 0 {
					t.Fatalf("code=%d dispatched=%d (%v) screen=%s err=%s", code, dispatched, args, &screen, &errOut)
				}
				got := screen.String()
				if !strings.Contains(got, surface.canvas) || !strings.Contains(got, "?") {
					t.Fatalf("canvas or question missing: %q", got)
				}
				for _, gone := range []string{"v = view canvas", "f = source diff", "/v", "/f]"} {
					if strings.Contains(got, gone) {
						t.Fatalf("menu still offers %q: %q", gone, got)
					}
				}
				for _, ran := range []string{calls, filepath.Join(root, "pager-ran"), filepath.Join(root, "diff-ran")} {
					if _, err := os.Stat(ran); !os.IsNotExist(err) {
						t.Fatalf("%s exists (err=%v): something launched", ran, err)
					}
				}
			})
		}
	}
}
