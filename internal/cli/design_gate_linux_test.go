package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestDesignGateTTYTail(t *testing.T) {
	for _, verdict := range []string{"ready", "blocked", "close"} {
		t.Run(verdict, func(t *testing.T) {
			root := isolateCascadeMoeHome(t)
			md := stageMetadata("tele", "r", "sdlc")
			writeStageCanvas(t, root, md, "design", strings.Replace(designCanvasSkeleton, `"blocked"`, `"`+verdict+`"`, 1))
			closes := stubGroupCloseCommand(t, "sdlc", 0)
			master, terminal := cooldownPTY(t)
			old := os.Stdin
			os.Stdin = terminal
			t.Cleanup(func() { os.Stdin = old })
			if !stdinIsTerminal() {
				t.Fatal("test did not get a TTY")
			}
			if _, err := io.WriteString(master, "n\n"); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			if code := promptNextStageOverride(root, md, "design", "", false, &out, &errs); code != 0 {
				t.Fatalf("exit %d: %s", code, &errs)
			}
			got := out.String()
			switch verdict {
			case "ready":
				if !strings.Contains(got, "run now?") {
					t.Fatalf("no build offer: %s", got)
				}
			case "blocked":
				if !strings.Contains(got, "parked at design") || strings.Contains(got, "run now?") || strings.Contains(got, "advance") {
					t.Fatalf("unready TTY tail: %s", got)
				}
			case "close":
				if len(*closes) != 1 || strings.Contains(got, "run now?") {
					t.Fatalf("close TTY tail: closes=%v %s", *closes, got)
				}
			}
		})
	}
}
