//go:build linux

package clonereclaim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"time"

	"github.com/modulecollective/moe/internal/procgroup"
	"github.com/modulecollective/moe/internal/runowner"
)

const cleanupSupervisorArg = "__moe_clone_cleanup_supervisor_v1"
const CleanupBudget = 5 * time.Minute
const cleanupOutputLimit = 2 << 20

type cleanupConfig struct {
	Identity    Identity
	Intent      CleanupIntent
	Environment []string
	Deadline    time.Time
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == cleanupSupervisorArg {
		os.Exit(cleanupSupervisor())
	}
}

// ExecuteCleanup is the descendant-supervision seam used by CLI hook
// orchestration. env must already contain the exact snapshot's addresses and
// mask inherited managed directory keys. No ambient environment is substituted.
// A timeout/output-limit/parent death without a durable completion holds rather
// than claiming that killing one process ended all cleanup workers.
func (r *Registry) ExecuteCleanup(ctx context.Context, h *runowner.Handle, plan CleanupPlan, env []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.Resources == nil {
		fresh, err := r.PlanCleanup(h, plan.Purpose, nil, plan.retry)
		if err != nil {
			return err
		}
		if fresh.Resources != nil {
			return errors.New("resource ownership changed before cleanup")
		}
		return nil
	}
	if plan.CacheCleared || plan.CompletedAttempt != "" {
		return r.ClearCompletedCache(h, plan)
	}
	intent, attempt, err := r.beginCleanup(h, plan)
	if err != nil {
		return err
	}
	defer attempt.Close()
	if plan.Resources.ResourceFree {
		if err := publishRecord(attempt, "teardown-complete.json", CleanupCompletion{Version: protocolVersion, Generation: intent.Generation, Resource: intent.Resource, Attempt: intent.ID}); err != nil {
			return err
		}
	} else {
		deadline := time.Now().Add(CleanupBudget)
		if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
			deadline = earlier
		}
		bounded, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		guard, err := h.SupervisorFile()
		if err != nil {
			return err
		}
		configRead, configWrite, err := os.Pipe()
		if err != nil {
			return err
		}
		defer configRead.Close()
		defer configWrite.Close()
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(bounded, self, cleanupSupervisorArg)
		procgroup.Isolate(cmd)
		cmd.WaitDelay = 2 * time.Second
		cmd.ExtraFiles = []*os.File{guard, configRead}
		out := &cleanupCapture{remaining: cleanupOutputLimit, cancel: cancel}
		errout := &cleanupCapture{remaining: cleanupOutputLimit, cancel: cancel}
		cmd.Stdout, cmd.Stderr = out, errout
		if err := cmd.Start(); err != nil {
			return err
		}
		configRead.Close()
		configErr := json.NewEncoder(configWrite).Encode(cleanupConfig{Identity: r.Identity, Intent: intent, Environment: env, Deadline: deadline.UTC()})
		configWrite.Close()
		waitErr := cmd.Wait()
		_, outErr := io.Copy(stdout, &out.buffer)
		_, errErr := io.Copy(stderr, &errout.buffer)
		if err := errors.Join(configErr, outErr, errErr); err != nil {
			return fmt.Errorf("cleanup output/config failure; retain %s: %w", attempt.Name(), err)
		}
		result, readErr := readRecord[CleanupCompletion](attempt, "teardown-complete.json")
		if readErr != nil {
			return &HoldError{Reason: "cleanup worker completion uncertain", Receipt: attempt.Name()}
		}
		if result.Version != protocolVersion || result.Generation != intent.Generation || result.Resource != intent.Resource || result.Attempt != intent.ID || result.ExitCode < 0 || result.ExitCode > 255 {
			return errors.New("cleanup worker receipt mismatch")
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("teardown exited %d; exact-run retry uses retained snapshot at %s", result.ExitCode, attempt.Name())
		}
		if waitErr != nil || out.exceeded || errout.exceeded {
			return fmt.Errorf("cleanup supervisor failed after completion; retain %s: %w", attempt.Name(), errors.Join(waitErr, bounded.Err()))
		}
	}
	completed, err := r.PlanCleanup(h, plan.Purpose, plan.Scripts, true)
	if err != nil {
		return err
	}
	return r.ClearCompletedCache(h, completed)
}

type cleanupCapture struct {
	buffer    bytes.Buffer
	remaining int
	cancel    context.CancelFunc
	exceeded  bool
}

func (c *cleanupCapture) Write(p []byte) (int, error) {
	if len(p) > c.remaining {
		c.exceeded = true
		c.cancel()
		return 0, errors.New("cleanup output limit exceeded")
	}
	c.remaining -= len(p)
	return c.buffer.Write(p)
}

func cleanupSupervisor() int {
	guard := os.NewFile(3, "run-owner")
	config := os.NewFile(4, "cleanup-config")
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	defer guard.Close()
	defer config.Close()
	var cfg cleanupConfig
	decoder := json.NewDecoder(io.LimitReader(config, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup configuration:", err)
		return 1
	}
	config.Close()
	h, err := runowner.AdoptSupervisor(cfg.Identity.Root, cfg.Identity.Project, cfg.Identity.Run, guard)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer h.Release()
	r, err := LoadRegistry(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer r.Close()
	if r.Identity != cfg.Identity || !validRandomID(cfg.Intent.Resource) || !validRandomID(cfg.Intent.ID) {
		return 1
	}
	resource, err := r.resourceDir(cfg.Intent.Resource, false)
	if err != nil {
		return 1
	}
	defer resource.Close()
	snapshot, err := readRecord[Resources](resource, "ownership.json")
	if err != nil || r.validateResources(snapshot) != nil {
		return 1
	}
	attempts, err := privateChildDir(resource, "attempts", false)
	if err != nil {
		return 1
	}
	defer attempts.Close()
	attempt, err := privateChildDir(attempts, cfg.Intent.ID, false)
	if err != nil {
		return 1
	}
	defer attempt.Close()
	intent, err := readRecord[CleanupIntent](attempt, "intent.json")
	if err != nil || !reflect.DeepEqual(intent, cfg.Intent) || intent.Version != protocolVersion || intent.Generation != r.Identity.Generation || intent.Resource != snapshot.ID || intent.CacheDigest != snapshot.Digest || !r.scriptsApproved(snapshot, intent.Scripts) || !cleanupPurpose(intent.Purpose) {
		return 1
	}
	pgid, err := syscall.Getpgid(0)
	if err != nil || pgid != os.Getpid() {
		return 1
	}
	if cfg.Deadline.IsZero() || time.Until(cfg.Deadline) <= 0 || time.Until(cfg.Deadline) > CleanupBudget {
		return 1
	}
	// The supervisor retains its own deadline after the outer CLI dies. On
	// expiry there is deliberately no completion receipt: escaped workers
	// require stopped-use recovery, never a PID-based guess.
	timer := time.AfterFunc(time.Until(cfg.Deadline), func() { syscall.Kill(-os.Getpid(), syscall.SIGKILL) })
	defer timer.Stop()
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "cleanup subreaper:", errno)
		return 1
	}
	status := 0
	for _, script := range intent.Scripts {
		current, err := ScriptIdentities(r.Identity.Root, []string{script.Path})
		if err != nil || len(current) != 1 || current[0] != script {
			fmt.Fprintln(os.Stderr, "cleanup script changed before launch")
			return 1
		}
		cmd := exec.Command(r.Identity.Root + string(os.PathSeparator) + script.Path)
		cmd.Dir, cmd.Env = r.Identity.Path, cfg.Environment
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if exited, ok := errors.AsType[*exec.ExitError](err); ok {
				status = exited.ExitCode()
				if status < 0 {
					status = 128 + int(exited.Sys().(syscall.WaitStatus).Signal())
				}
			} else {
				fmt.Fprintln(os.Stderr, "cleanup launch:", err)
				status = 1
			}
		}
		for {
			_, err := syscall.Wait4(-1, nil, 0, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				break
			}
			if err != nil {
				return 1
			}
		}
		if status != 0 {
			break
		}
	}
	if err := publishRecord(attempt, "teardown-complete.json", CleanupCompletion{Version: protocolVersion, Generation: intent.Generation, Resource: intent.Resource, Attempt: intent.ID, ExitCode: status}); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup receipt:", err)
		return 1
	}
	return status
}
