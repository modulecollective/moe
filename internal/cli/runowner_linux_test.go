package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
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

// TestUnansweredPromptHoldsItsRunAgainstAKick is the watchroll/cleanup
// incident: a stage turn has settled (no session branch left), its
// next-stage prompt is waiting, and a dynamic pulse in another process
// sweeps the board. The real kick floor and the real chain-kick
// admission must both leave the run alone — no stage started, no journal
// motion — until the prompt's invocation is gone. Every case then proves
// pickup works again, so a fix that disabled kicking cannot pass.
func TestUnansweredPromptHoldsItsRunAgainstAKick(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		kill         bool
		wantStage    bool
		wantOut      string
	}{
		{name: "decline", answer: "n\n", wantOut: "EXIT 0 busy=false"},
		{name: "eof", wantOut: "EXIT 0 busy=false"},
		{name: "interrupt-by-kill", kill: true},
		{name: "advance", answer: "a\n", wantOut: "advanced past design"},
		{name: "run", answer: "y\n", wantStage: true, wantOut: "EXIT 0 busy=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, threadRoot, groomed, stages := advancedKickFixture(t)
			defer withRideMode(rideDynamic)()
			for _, stage := range []string{"design", "build"} {
				if openSessionStage(root, groomed.byKey[threadRoot]) != "" {
					t.Fatalf("fixture has a live %s session; the prompt must be the only occupant", stage)
				}
			}
			head, err := git.HEAD(root)
			if err != nil {
				t.Fatal(err)
			}

			h := startPromptHolder(t, root)
			h.waitLine(t, "run now?")

			var errb bytes.Buffer
			pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)
			if len(*stages) != 0 {
				t.Fatalf("kick drove %v while the prompt was waiting", kickStages(*stages))
			}
			if !strings.Contains(errb.String(), threadRoot+" is owned by an active moe invocation") {
				t.Fatalf("kick stderr = %q, want the owner hold", errb.String())
			}
			// Admission is the authoritative check: a plan stamped before
			// the prompt opened still cannot start the run.
			errb.Reset()
			if code := chainKickRun(root, "moe", "advanced-run", rideDynamic, false, io.Discard, &errb); code == 0 || len(*stages) != 0 {
				t.Fatalf("chain kick admitted a held run: code=%d stages=%v stderr=%q", code, kickStages(*stages), errb.String())
			}
			if after, _ := git.HEAD(root); after != head {
				t.Fatalf("journal moved while held: %s -> %s", head, after)
			}
			if md, err := run.Load(root, "moe", "advanced-run"); err != nil || md.Status != run.StatusInProgress {
				t.Fatalf("run state changed while held: %+v %v", md, err)
			}

			if tc.kill {
				if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			} else {
				io.WriteString(h.in, tc.answer)
				h.in.Close()
			}
			out := h.drain(t)
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Fatalf("child output missing %q:\n%s", tc.wantOut, out)
			}
			if got := strings.Count(out, "STAGE "); tc.wantStage != (got == 1) || got > 1 {
				t.Fatalf("stage handoffs = %d, want stage=%v:\n%s", got, tc.wantStage, out)
			}
			if tc.wantStage && !strings.Contains(out, "STAGE moe/advanced-run build busy=true err=<nil> held=true") {
				t.Fatalf("the chosen stage did not start under continuous ownership:\n%s", out)
			}

			errb.Reset()
			pulseSelfKick(root, wantKick(groomed, groomedThread{Root: threadRoot}), io.Discard, &errb)
			if got := kickStages(*stages); len(got) == 0 || got[0] != "advanced-run:build" {
				t.Fatalf("pickup after the invocation left: drove %v, stderr=%q", got, errb.String())
			}
		})
	}
}
