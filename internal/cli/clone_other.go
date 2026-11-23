//go:build !linux

package cli

import (
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/atomicfile"
	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func cloneInventory(root string) ([]string, error) {
	entries, err := listClones(root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.project+"/"+e.run)
	}
	return out, nil
}
func collectClone(root, p, id string, apply, retry bool, now time.Time, stdout, stderr io.Writer) cloneResult {
	return cloneResult{Project: p, Run: id, Path: sandbox.Path(root, p, id), Outcome: "held", Reason: "unsupported_platform"}
}
func disposeClone(h *runowner.Handle, apply, retry bool, purpose string, now time.Time, stdout, stderr io.Writer) cloneResult {
	return collectClone(h.Root, h.Project, h.Run, apply, retry, now, stdout, stderr)
}
func runCloneRecover(args []string, stdout, stderr io.Writer) int {
	moePrintln(stderr, "clone recovery requires the verified Linux host view")
	return 1
}
func releasePerRunClone(h *runowner.Handle, md *run.Metadata) error {
	return errors.New("unattended clone cleanup requires verified Linux confinement; clone retained")
}
func ensureRunClone(h *runowner.Handle) (string, error) {
	if h == nil {
		return "", errors.New("missing owner")
	}
	if err := clonereclaim.CheckRunRecovery(h); err != nil {
		return "", err
	}
	return sandbox.EnsureRetained(h.Root, h.Project, h.Run)
}
func devEnvSetupOwned(h *runowner.Handle, md *run.Metadata, stdout, stderr io.Writer) (map[string]string, bool, error) {
	return nil, false, errors.New("owned per-run environments require the verified Linux host view")
}
func execOwnedShell(h *runowner.Handle, md *run.Metadata, path string, extra []string, stderr io.Writer) int {
	receipt := filepath.Join(h.Root, ".moe", "clone-reclamation", h.Project, h.Run, "unsupported-shell.json")
	if err := atomicfile.Write(receipt, []byte("{\"status\":\"host recovery required after unsupported shell supervision\"}\n"), 0o600); err != nil {
		moePrintf(stderr, "shell: %v\n", err)
		return 1
	}
	moePrintf(stderr, "shell: descendant completion cannot be certified on this platform; recovery hold retained at %s\n", receipt)
	return execShell(path, extra, stderr)
}
