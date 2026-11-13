//go:build linux || darwin

package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modulecollective/moe/internal/cliout"
)

// Screen modes: alternate screen, bracketed paste. Leaving undoes them
// and ends any synchronized-output block a failed paint left open.
const (
	enterModes = "\x1b[?1049h\x1b[?2004h\x1b[H\x1b[J"
	leaveModes = "\x1b[?2026l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l"
)

// Run is the console on the process's terminal. It refuses before
// touching anything unless stdout is a terminal and TERM is a real one,
// and restores the terminal — modes, screen, cursor — on every way out:
// quit, signal, error, or panic. A mutation still running at quit is
// waited for, and its outcome printed, after the terminal is back.
func Run(ctx context.Context, cfg Config, stdout *os.File, stderr io.Writer) error {
	if !cliout.IsTTY(stdout) {
		return errors.New("moe ui needs a terminal on stdout")
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return fmt.Errorf("moe ui needs a terminal that understands cursor movement (TERM=%q)", t)
	}
	tty, err := openTTY()
	if err != nil {
		return fmt.Errorf("moe ui: open terminal: %w", err)
	}
	saved, err := getTermios(tty)
	if err != nil {
		tty.Close()
		return fmt.Errorf("moe ui: %w", err)
	}

	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTSTP, syscall.SIGCONT)
	defer signal.Stop(sigs)

	entered := false
	enter := func() error {
		if err := setTermios(tty, rawTermios(saved)); err != nil {
			return err
		}
		entered = true
		_, err := io.WriteString(stdout, enterModes)
		return err
	}
	leave := func() {
		if !entered {
			return
		}
		entered = false
		_, _ = io.WriteString(stdout, leaveModes)
		_ = setTermios(tty, saved)
	}
	defer leave()
	if err := enter(); err != nil {
		return fmt.Errorf("moe ui: %w", err)
	}

	// The reader: one private /dev/tty, closed and joined on exit.
	input := make(chan []byte)
	readerDone := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(input)
		buf := make([]byte, 4096)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				select {
				case input <- append([]byte(nil), buf[:n]...):
				case <-quit:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		// Restore before closing: on a panic this runs ahead of the
		// deferred leave above, which would then find the tty closed.
		leave()
		close(quit)
		tty.Close()
		<-readerDone
	}()

	resize := make(chan struct{}, 1)
	cont := make(chan struct{}, 1)
	stop := make(chan struct{})
	tstp := make(chan struct{}, 1)
	sigDone := make(chan struct{})
	defer close(sigDone)
	go func() {
		for {
			select {
			case s := <-sigs:
				switch s {
				case syscall.SIGWINCH, syscall.SIGCONT:
					select {
					case resize <- struct{}{}:
					default:
					}
					if s == syscall.SIGCONT {
						select {
						case cont <- struct{}{}:
						default:
						}
					}
				case syscall.SIGTSTP:
					select {
					case tstp <- struct{}{}:
					default:
					}
				default:
					close(stop)
					return
				}
			case <-sigDone:
				return
			}
		}
	}()

	jobs := make(chan job, 1)
	results := make(chan jobResult, 1)
	go worker(cfg.Do, jobs, results)
	defer close(jobs)

	tio := loopIO{
		out:    stdout,
		input:  input,
		resize: resize,
		size:   func() (int, int, error) { return cliout.TerminalSize(stdout) },
		stop:   stop,
		tstp:   tstp,
		suspend: func() error {
			leave()
			select { // forget an earlier continue
			case <-cont:
			default:
			}
			// SIGSTOP, not SIGTSTP: it cannot be caught, so the shell gets
			// its terminal back. The stop lands asynchronously; re-entering
			// waits for the continue, or it could win the race and leave
			// the stopped process holding the terminal raw.
			if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
				return err
			}
			select {
			case <-cont:
			case <-stop:
				return errStopped
			}
			return enter()
		},
	}
	pending, loopErr := runLoop(ctx, cfg, tio, jobs, results)
	if cfg.Unreport != nil {
		cfg.Unreport(stdout)
	}
	leave()
	if pending != nil {
		fmt.Fprintf(stderr, "moe ui: waiting for %s to finish — quitting does not cancel or undo it\n", pending.label)
		r := <-results
		for r.progress {
			r = <-results
		}
		for i, res := range r.results {
			fmt.Fprintf(stderr, "moe ui: %s: %s\n", outcomeWord(res), r.job.steps[i].describe())
			if res.Message != "" {
				fmt.Fprintf(stderr, "  %s\n", sanitize(res.Message))
			}
			for _, l := range res.Lines {
				fmt.Fprintf(stderr, "  %s\n", sanitize(l))
			}
		}
	}
	if errors.Is(loopErr, errStopped) {
		return nil
	}
	return loopErr
}
