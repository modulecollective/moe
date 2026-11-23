//go:build linux

package cli

import (
	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func execOwnedShell(h *runowner.Handle, md *run.Metadata, path string, extra []string, stderr io.Writer) int {
	var r *clonereclaim.Registry
	var err error
	if md.Workspace == "" {
		r, err = clonereclaim.LoadRegistry(h)
	} else {
		r, err = clonereclaim.BeginNamedShell(h, path)
	}
	if err != nil {
		moePrintf(stderr, "shell: %v\n", err)
		return 1
	}
	defer r.Close()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	interrupts := make(chan os.Signal, 8)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Stop(interrupts)
	publishPhase(h.Root, md, "shell running", "", "")
	status, err := r.SuperviseShell(h, shell, path, append(os.Environ(), extra...), os.Stdin, os.Stdout, os.Stderr, func() { publishPhase(h.Root, md, "waiting for shell jobs", "", "") })
	if err != nil {
		moePrintf(stderr, "shell: %v\n", err)
		return 1
	}
	return status
}
