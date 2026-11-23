//go:build linux

package clonereclaim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modulecollective/moe/internal/runowner"
)

const shellSupervisorArg = "__moe_clone_shell_supervisor_v1"

type shellStart struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	ID         string `json:"id"`
	Shell      string `json:"shell"`
}

type shellComplete struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Start      string `json:"start"`
	ExitCode   int    `json:"exit_code"`
}

type shellConfig struct {
	Identity Identity
	Start    shellStart
	Dir      string
	Env      []string
}

func init() {
	if len(os.Args) == 2 && os.Args[1] == shellSupervisorArg {
		os.Exit(shellSupervisor())
	}
}

// CheckShells discharges only exact durable supervisor completions. A free
// flock, elapsed time, or a locally invisible PID never clears a shell hold.
func (r *Registry) CheckShells(h *runowner.Handle) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	shells, err := privateChildDir(r.dir, "shells", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer shells.Close()
	entries, err := shells.ReadDir(4097)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 4096 {
		return errors.New("shell record scan limit exceeded")
	}
	for _, entry := range entries {
		if !validRandomID(entry.Name()) || !entry.IsDir() {
			return errors.New("unknown shell recovery entry")
		}
		attempt, err := privateChildDir(shells, entry.Name(), false)
		if err != nil {
			return err
		}
		start, readErr := readRecord[shellStart](attempt, "start.json")
		complete, completeErr := readRecord[shellComplete](attempt, "complete.json")
		attempt.Close()
		if readErr != nil {
			return fmt.Errorf("unreadable shell start %s: %w", entry.Name(), readErr)
		}
		if completeErr != nil {
			return fmt.Errorf("unresolved shell %s; host recovery required: %w", entry.Name(), completeErr)
		}
		if start.Version != protocolVersion || complete.Version != protocolVersion || start.ID != entry.Name() || start.Generation != r.Identity.Generation || complete.Generation != start.Generation || complete.Start != start.ID || complete.ExitCode < 0 || complete.ExitCode > 255 {
			return errors.New("contradictory shell completion; host recovery required")
		}
	}
	return nil
}

// SuperviseShell starts an exclusive shell without changing its TTY/process
// group. The separate subreaper owns the inherited guard until the shell and
// all adopted descendants have exited, including setsid/double-fork jobs.
// The caller must already serve its observable owner endpoint. phase runs in
// the caller after the shell exits while adopted jobs may still be running.
func (r *Registry) SuperviseShell(h *runowner.Handle, shell, dir string, env []string, stdin, stdout, stderr *os.File, phase func()) (int, error) {
	if err := r.CheckShells(h); err != nil {
		return 1, err
	}
	if filepath.Clean(dir) != r.Identity.Path {
		return 1, errors.New("shell directory differs from enrolled clone")
	}
	start, attempt, err := r.beginShell(shell)
	if err != nil {
		return 1, err
	}
	defer attempt.Close()
	guard, err := h.SupervisorFile()
	if err != nil {
		return 1, err
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer configRead.Close()
	defer configWrite.Close()
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	defer eventRead.Close()
	defer eventWrite.Close()
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(self, shellSupervisorArg)
	cmd.ExtraFiles = []*os.File{guard, configRead, eventWrite}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	configRead.Close()
	eventWrite.Close()
	configErr := json.NewEncoder(configWrite).Encode(shellConfig{Identity: r.Identity, Start: start, Dir: dir, Env: env})
	configWrite.Close()
	eventsDone := make(chan error, 1)
	go func() {
		var b [1]byte
		for {
			n, err := eventRead.Read(b[:])
			if n > 0 && phase != nil {
				phase()
			}
			if err == io.EOF {
				eventsDone <- nil
				return
			}
			if err != nil {
				eventsDone <- err
				return
			}
		}
	}()
	waitErr := cmd.Wait()
	eventErr := <-eventsDone
	if err := errors.Join(configErr, eventErr); err != nil {
		return 1, err
	}
	complete, err := readRecord[shellComplete](attempt, "complete.json")
	if err != nil {
		return 1, fmt.Errorf("shell lifetime unresolved; retain %s: %w", attempt.Name(), errors.Join(err, waitErr))
	}
	if complete.Version != protocolVersion || complete.Generation != start.Generation || complete.Start != start.ID || complete.ExitCode < 0 || complete.ExitCode > 255 {
		return 1, errors.New("invalid shell completion")
	}
	if err := r.CheckShells(h); err != nil {
		return 1, err
	}
	if waitErr != nil {
		if exited, ok := errors.AsType[*exec.ExitError](waitErr); !ok || exited.ExitCode() != complete.ExitCode {
			return 1, waitErr
		}
	}
	return complete.ExitCode, nil
}

func (r *Registry) beginShell(shell string) (shellStart, *os.File, error) {
	var start shellStart
	shells, err := privateChildDir(r.dir, "shells", true)
	if err != nil {
		return start, nil, err
	}
	defer shells.Close()
	id, err := randomID()
	if err != nil {
		return start, nil, err
	}
	attempt, err := privateChildDir(shells, id, true)
	if err != nil {
		return start, nil, err
	}
	start = shellStart{Version: protocolVersion, Generation: r.Identity.Generation, ID: id, Shell: shell}
	if err := publishRecord(attempt, "start.json", start); err != nil {
		attempt.Close()
		return start, nil, err
	}
	return start, attempt, nil
}

// The supervisor performs no Git/CLI work and owns no unrelated subprocesses.
// Its Wait4 loop therefore cannot steal a status from another CLI child.
func shellSupervisor() int {
	guard := os.NewFile(3, "run-owner")
	config := os.NewFile(4, "shell-config")
	events := os.NewFile(5, "shell-events")
	for _, f := range []*os.File{guard, config, events} {
		syscall.CloseOnExec(int(f.Fd()))
	}
	defer guard.Close()
	defer config.Close()
	defer events.Close()
	var cfg shellConfig
	decoder := json.NewDecoder(io.LimitReader(config, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, "shell supervisor:", err)
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
	if r.Identity != cfg.Identity || !validRandomID(cfg.Start.ID) || cfg.Start.Generation != r.Identity.Generation || cfg.Start.Version != protocolVersion || cfg.Dir != r.Identity.Path {
		fmt.Fprintln(os.Stderr, "shell supervisor identity mismatch")
		return 1
	}
	shells, err := privateChildDir(r.dir, "shells", false)
	if err != nil {
		return 1
	}
	defer shells.Close()
	attempt, err := privateChildDir(shells, cfg.Start.ID, false)
	if err != nil {
		return 1
	}
	defer attempt.Close()
	recorded, err := readRecord[shellStart](attempt, "start.json")
	if err != nil || recorded != cfg.Start {
		fmt.Fprintln(os.Stderr, "shell start record mismatch")
		return 1
	}
	// Linux PR_SET_CHILD_SUBREAPER: adopted orphans remain waitable even
	// after leaving the original process group or double-forking.
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "subreaper:", errno)
		return 1
	}
	// Consume terminal interrupts in the supervisor; the shell retains its
	// normal disposition and foreground-terminal job control.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(signals)
	cmd := exec.Command(cfg.Start.Shell)
	cmd.Dir, cmd.Env = cfg.Dir, cfg.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	status := 0
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			status = exitErr.ExitCode()
			if status < 0 {
				status = 128 + int(exitErr.Sys().(syscall.WaitStatus).Signal())
			}
		} else {
			fmt.Fprintln(os.Stderr, "shell:", err)
			status = 1
		}
	}
	if _, err := events.Write([]byte{'w'}); err != nil && !errors.Is(err, syscall.EPIPE) {
		return 1
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
			fmt.Fprintln(os.Stderr, "shell descendants:", err)
			return 1
		}
	}
	if err := publishRecord(attempt, "complete.json", shellComplete{Version: protocolVersion, Generation: cfg.Start.Generation, Start: cfg.Start.ID, ExitCode: status}); err != nil {
		fmt.Fprintln(os.Stderr, "shell completion:", err)
		return 1
	}
	return status
}
