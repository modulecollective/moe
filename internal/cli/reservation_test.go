package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

// Each helper is a separate owner: repolock forbids same-process contention.
// The winner stops inside its real writer's locked validation; the controller
// starts the contender before releasing it. No scheduling delay orders writes.
func TestReservationOrderingHelper(t *testing.T) {
	root := os.Getenv("MOE_RESERVATION_TEST_ROOT")
	if root == "" {
		return
	}
	op := os.Getenv("MOE_RESERVATION_TEST_OP")
	barrier := func() {
		if os.Getenv("MOE_RESERVATION_TEST_HOLD") == "1" {
			fmt.Println("locked")
			if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	fmt.Println("attempting")
	var err error
	switch op {
	case "design", "code":
		_, _, _, err = openStageSession(root, stageTurnInputs{Project: "tele", RunSlug: "work", DocID: op, LockPurpose: "test", ValidateEntry: func(md *run.Metadata) error {
			if err := sdlcSessionEntry(md); err != nil {
				return err
			}
			barrier()
			return nil
		}}, io.Discard, io.Discard)
		if err == nil {
			fmt.Println("admitted")
		}
	case "close":
		err = closeRunInProcess(root, "sdlc", sdlcCloseSubject, func(string, *run.Metadata) error { barrier(); return nil }, "tele", "work", true, io.Discard)
	case "advance":
		err = runopen.MarkAdvanced(root, "tele", "work", "design", runopen.AdvanceOptions{Validate: func(*run.Metadata) (string, error) { barrier(); return "design", nil }})
	default:
		t.Fatal("unknown operation", op)
	}
	if err != nil {
		fmt.Println("refused:", err)
	} else {
		fmt.Println("success")
	}
}

type reservationProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  *bufio.Reader
	stderr bytes.Buffer
}

func startReservationProcess(t *testing.T, root, op string, hold bool) *reservationProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	p := &reservationProcess{cmd: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReservationOrderingHelper$")}
	h := "0"
	if hold {
		h = "1"
	}
	p.cmd.Env = append(os.Environ(), "MOE_RESERVATION_TEST_ROOT="+root, "MOE_RESERVATION_TEST_OP="+op, "MOE_RESERVATION_TEST_HOLD="+h)
	var err error
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.lines = bufio.NewReader(out)
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	line, err := p.lines.ReadString('\n')
	if err != nil || line != "attempting\n" {
		t.Fatalf("helper start %q %v", line, err)
	}
	return p
}
func (p *reservationProcess) finish(t *testing.T) string {
	t.Helper()
	body, err := io.ReadAll(p.lines)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("helper: %v %s %s", err, body, &p.stderr)
	}
	return string(body)
}

func TestReservationMutationOrderings(t *testing.T) {
	for _, tc := range []struct {
		winner, contender string
		refuse            bool
	}{
		{"design", "close", true}, {"design", "advance", true},
		{"close", "design", true}, {"advance", "design", false},
		{"design", "design", true}, {"design", "code", true},
	} {
		t.Run(tc.winner+" before "+tc.contender, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "work", "sdlc", run.StatusInProgress)
			trailerstest.CommitWorkTurnAt(t, root, "tele", "work", "sdlc", "design", time.Now().Add(-time.Hour))
			first := startReservationProcess(t, root, tc.winner, true)
			line, err := first.lines.ReadString('\n')
			if err != nil || line != "locked\n" {
				t.Fatalf("winner barrier %q %v", line, err)
			}
			second := startReservationProcess(t, root, tc.contender, false)
			if _, err := first.input.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			result := first.finish(t)
			if !strings.Contains(result, "success") {
				t.Fatal(result)
			}
			result2 := second.finish(t)
			if strings.Contains(result2, "refused:") != tc.refuse {
				t.Fatalf("contender result: %s", result2)
			}
			if tc.refuse && strings.Contains(result2, "admitted") {
				t.Fatal("loser was admitted")
			}
			refs, err := session.RunReservations(root, "tele", "work")
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if tc.winner == "close" {
				expected = 0
			}
			if len(refs) != expected {
				t.Fatalf("reservations %v", refs)
			}
		})
	}
}

func TestCloseRefusalPreservesSandboxAndClaim(t *testing.T) {
	root := seedCloseFixture(t, "tele", "work", "sdlc", run.StatusInProgress)
	s, stop, err := session.Reserve(root, "tele", "work", "design", false)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	writeDeadClaim(t, s, false)
	before := gittest.HeadSHA(t, root)
	claimBefore, err := os.ReadFile(session.ClaimPath(root, "tele", "work", "design"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = closeRunInProcess(root, "sdlc", sdlcCloseSubject, func(string, *run.Metadata) error { called = true; return nil }, "tele", "work", true, io.Discard)
	var conflict *session.UnresolvedError
	if !errors.As(err, &conflict) || called {
		t.Fatalf("close %v cleanup %v", err, called)
	}
	if before != gittest.HeadSHA(t, root) {
		t.Fatal("close committed")
	}
	claimAfter, _ := os.ReadFile(session.ClaimPath(root, "tele", "work", "design"))
	if !bytes.Equal(claimBefore, claimAfter) {
		t.Fatal("close changed claim")
	}
	if _, err := os.Stat(s.WorktreePath); err != nil {
		t.Fatal("close removed worktree", err)
	}
	t.Setenv("MOE_HOME", root)
	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/work"}, &out, &errb); code == 0 || !strings.Contains(errb.String(), s.Branch) {
		t.Fatalf("CLI close %d: %s", code, &errb)
	}
}

func TestStageAdmissionRevalidatesFreshStatusAndRefusesClaimFailure(t *testing.T) {
	for _, status := range []string{run.StatusClosed, run.StatusInProgress, run.StatusPushed} {
		t.Run(status, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "work", "sdlc", status)
			in := stageTurnInputs{Project: "tele", RunSlug: "work", DocID: "code", LockPurpose: "test", ValidateEntry: sdlcSessionEntry}
			_, _, cancel, err := openStageSession(root, in, io.Discard, io.Discard)
			if status == run.StatusClosed {
				if err == nil {
					t.Fatal("closed run admitted")
				}
				if err := session.RequireUnreserved(root, "tele", "work"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := cancel(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	root := seedCloseFixture(t, "tele", "work", "sdlc", run.StatusInProgress)
	if _, err := repolock.EnsureDir(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".moe", "claims"), []byte("block claim directory"), 0600); err != nil {
		t.Fatal(err)
	}
	started := false
	code := runStageTurn(root, stageTurnInputs{Project: "tele", RunSlug: "work", DocID: "code", LockPurpose: "test", BuildSpec: func(string) (stageTurnSpec, error) { started = true; return stageTurnSpec{}, nil }}, io.Discard, io.Discard)
	if code == 0 || started {
		t.Fatalf("claim failure launched: code %d started %v", code, started)
	}
	if err := session.RequireUnreserved(root, "tele", "work"); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionRevalidatesAfterAutoPull(t *testing.T) {
	for _, workflow := range []string{"sdlc", "idea"} {
		t.Run(workflow, func(t *testing.T) {
			f := newSyncFixture(t)
			trailerstest.SeedRun(t, f.root, "moe", "work", workflow, run.StatusInProgress)
			f.initBureaucracyOrigin()
			md, err := run.Load(f.root, "moe", "work")
			if err != nil {
				t.Fatal(err)
			}
			md.Status = run.StatusClosed
			body, err := json.Marshal(md)
			if err != nil {
				t.Fatal(err)
			}
			f.advanceBureaucracyOrigin(filepath.Join(run.Dir("moe", "work"), "run.json"), string(body), "remote close")
			check := sdlcSessionEntry
			if workflow == "idea" {
				check = inProgressSessionEntry("idea")
			}
			_, _, _, err = openStageSession(f.root, stageTurnInputs{Project: "moe", RunSlug: "work", DocID: "design", LockPurpose: "test", ValidateEntry: check}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("entry after remote close: %v", err)
			}
			if err := session.RequireUnreserved(f.root, "moe", "work"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
