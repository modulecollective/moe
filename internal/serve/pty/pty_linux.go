//go:build linux

// Package pty opens a Linux pseudo-terminal pair and starts a child
// process on its slave side. Linux only — moe serve runs on a fly.io
// Linux box, so we don't carry the cross-platform plumbing a
// general-purpose PTY library (creack/pty) would. A stub in
// pty_other.go errors at runtime on non-Linux so the rest of moe
// still builds (the operator runs serve on Linux only).
//
// The package is a thin shim around golang.org/x/sys/unix: open
// /dev/ptmx, unlock the slave (TIOCSPTLCK), resolve the slave path
// (TIOCGPTN), then start the child with setsid + setctty so it
// receives the slave as its controlling terminal. ~80 LOC we own
// end-to-end rather than pulling a community library.
package pty

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Pty is the parent's side of a PTY pair: the master file the parent
// reads/writes through, plus the child *exec.Cmd it's wired to.
type Pty struct {
	master *os.File
	cmd    *exec.Cmd
}

// Start opens a PTY pair, attaches cmd's stdio to the slave, and
// starts cmd. The slave fd is closed in the parent after fork so the
// kernel's controlling-terminal teardown does the right thing when
// the child exits: read on the master returns EIO and the read loop
// can shut down cleanly.
//
// cmd must be a freshly constructed *exec.Cmd; Start sets Stdin,
// Stdout, Stderr, and SysProcAttr's Setsid / Setctty / Ctty fields.
// Callers should not pre-populate those.
func Start(cmd *exec.Cmd) (*Pty, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("pty: open /dev/ptmx: %w", err)
	}

	// Linux Openpt-equivalent: unlock the slave end, then read the
	// device number to construct its path.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("pty: TIOCSPTLCK: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("pty: TIOCGPTN: %w", err)
	}
	slavePath := "/dev/pts/" + strconv.Itoa(n)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("pty: open %s: %w", slavePath, err)
	}

	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	// Ctty is interpreted as the *child's* fd index after dup2 of
	// Stdin/Stdout/Stderr — not the parent's slave fd. Since all
	// three point at the slave, any of 0/1/2 works; 0 is the
	// convention.
	cmd.SysProcAttr.Ctty = 0

	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, fmt.Errorf("pty: start %s: %w", cmd.Path, err)
	}

	// Child has its own copy of slave; drop the parent's reference
	// so EOF arrives on the master when the child finally exits.
	slave.Close()

	// Default window size. The chain prompt fits in 80 cols; resize
	// is exposed via SetSize if a caller ever needs it.
	_ = SetSize(master, 24, 80)

	return &Pty{master: master, cmd: cmd}, nil
}

// File returns the master *os.File. Callers read/write through it
// directly — the package doesn't buffer.
func (p *Pty) File() *os.File { return p.master }

// Cmd returns the wrapped *exec.Cmd so callers can Wait on it.
func (p *Pty) Cmd() *exec.Cmd { return p.cmd }

// SetSize updates the master's window size. Pre-spawn or live.
func SetSize(master *os.File, rows, cols uint16) error {
	ws := &unix.Winsize{Row: rows, Col: cols}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
}

// Close sends SIGTERM to the child (if still running) and releases
// the master fd. The master close itself SIGHUPs the child via the
// kernel's controlling-terminal teardown — SIGTERM gives it a
// chance to drain first.
func (p *Pty) Close() error {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	return p.master.Close()
}
