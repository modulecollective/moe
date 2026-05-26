//go:build !linux

// Non-Linux stub for the pty package. Start returns an error so the
// serve HTTP handlers surface "PTY unsupported on this OS" rather
// than failing to compile. moe serve is Linux-only by design (the
// fly box); other moe verbs still build on macOS / Windows.
package pty

import (
	"errors"
	"os"
	"os/exec"
)

// errUnsupported is returned by every entry point on non-Linux.
var errUnsupported = errors.New("pty: only supported on Linux")

// Pty has the same shape as the Linux type so callers compile
// everywhere. All methods are no-ops or error returns.
type Pty struct{}

// Start always returns errUnsupported.
func Start(cmd *exec.Cmd) (*Pty, error) { return nil, errUnsupported }

// File returns nil; never called because Start errors first.
func (p *Pty) File() *os.File { return nil }

// Cmd returns nil; never called because Start errors first.
func (p *Pty) Cmd() *exec.Cmd { return nil }

// Close is a no-op on non-Linux.
func (p *Pty) Close() error { return nil }

// SetSize always returns errUnsupported.
func SetSize(master *os.File, rows, cols uint16) error { return errUnsupported }
