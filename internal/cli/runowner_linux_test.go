package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

const promptHolderEnv = "MOE_TEST_PROMPT_HOLDER_ROOT"

// TestRunOwnerPromptHolderProcess is the child half of the two-process
// regressions below, not a test of its own: it sits at the real post-design
// chain prompt for moe/advanced-run on a pty, with the answer relayed from
// its own stdin (EOF becomes a Ctrl-D at the prompt). The stage the answer
// would open is stubbed to report whether the run is still owned at the
// moment of handoff.
func TestRunOwnerPromptHolderProcess(t *testing.T) {
	root := os.Getenv(promptHolderEnv)
	if root == "" {
		t.Skip("helper process for the run-owner prompt regressions")
	}
	out, answers := os.Stdout, os.Stdin
	master, slave := cooldownPTY(t)
	os.Stdin = slave
	go func() {
		io.Copy(master, answers)
		master.Write([]byte{4})
	}()
	runStageSession = func(projectID, runID, docID string, _ stageSessionOpts, _, _ io.Writer) int {
		busy, err := runowner.Busy(root, projectID, runID)
		fmt.Fprintf(out, "STAGE %s/%s %s busy=%v err=%v held=%v\n", projectID, runID, docID, busy, err, heldRun(root, projectID, runID) != nil)
		return 0
	}
	md, err := run.Load(root, "moe", "advanced-run")
	if err != nil {
		t.Fatal(err)
	}
	code := promptNextStageOverride(root, md, "design", "", false, out, out)
	busy, err := runowner.Busy(root, "moe", "advanced-run")
	fmt.Fprintf(out, "EXIT %d busy=%v err=%v\n", code, busy, err)
}

type promptHolder struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan string
	seen  []string
}

func startPromptHolder(t *testing.T, root string) *promptHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunOwnerPromptHolderProcess$")
	cmd.Env = append(os.Environ(), promptHolderEnv+"="+root)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &promptHolder{cmd: cmd, in: in, lines: make(chan string, 256)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	return h
}

// waitLine is a barrier on the child's own output, not on elapsed time;
// the deadline only bounds a hung child.
func (h *promptHolder) waitLine(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatalf("child exited before %q; output:\n%s", want, strings.Join(h.seen, "\n"))
			}
			h.seen = append(h.seen, line)
			if strings.Contains(line, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %q from child; output:\n%s", want, strings.Join(h.seen, "\n"))
		}
	}
}

func (h *promptHolder) drain(t *testing.T) string {
	t.Helper()
	for line := range h.lines {
		h.seen = append(h.seen, line)
	}
	h.cmd.Wait()
	return strings.Join(h.seen, "\n")
}
