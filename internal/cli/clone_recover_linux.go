//go:build linux

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"reflect"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/workspace"
)

func runCloneRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clone recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { moePrintln(stderr, "usage: moe clone recover <project>/<run>") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	p, id, err := splitCloneRef(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 2
	}
	if !stdinIsTerminal() || serveAgentSuppress() || os.Getenv("MOE_SESSION_ROOT") != "" {
		moePrintln(stderr, "clone recover requires an interactive host terminal after stopping old binaries and unmanaged users")
		return 1
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	h, err := runowner.Acquire(root, p, id)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	defer h.Release()
	endpoint, err := startRunEndpoint(root, p, id)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	defer endpoint.Close()
	endpoint.Update(func(s *runsock.Status) { s.Phase = "clone recovery" })
	if err := recoverClone(h, bufio.NewReader(os.Stdin), stdout, stderr); err != nil {
		moePrintf(stderr, "clone recover: %v\n", err)
		return 1
	}
	return 0
}

func recoverClone(h *runowner.Handle, input *bufio.Reader, stdout, stderr io.Writer) error {
	md, err := cloneRunState(h)
	if err != nil {
		return err
	}
	if md.Workspace != "" {
		return recoverNamedShell(h, md, input, stdout)
	}
	r, physical, legacy, err := clonereclaim.RecoveryRegistry(h)
	if err != nil {
		return err
	}
	defer func() {
		if r != nil {
			r.Close()
		}
	}()
	scripts, err := ownedDevEnvScripts(h.Root, h.Project, devEnvTeardownDirRel)
	if err != nil {
		return err
	}
	setup, err := ownedDevEnvScripts(h.Root, h.Project, devEnvDirRel)
	if err != nil {
		return err
	}
	moePrintf(stdout, "Recover %s/%s at %s\nPhysical inode %d:%d; mount %s; namespace %s; generation %s\n", h.Project, h.Run, physical.Path, physical.Device, physical.Inode, physical.Mount, physical.Namespace, physical.Generation)
	if r != nil {
		if err := r.ValidateEnvironment(h); err != nil {
			moePrintf(stdout, "Hold: %v\n", err)
		}
		moePrintf(stdout, "Retained evidence: %s\n", r.Receipt())
	} else {
		moePrintln(stdout, "Legacy clone: no enrollment. Cached resources will be adopted only with a valid owner and teardown contract.")
	}
	confirm := func(message, exact string) error {
		moePrintf(stdout, "%s\nType %q to continue: ", message, exact)
		line, err := input.ReadString('\n')
		if err != nil {
			return err
		}
		if line != exact+"\n" {
			return errors.New("confirmation declined")
		}
		return nil
	}
	if err := confirm("Confirm ALL old MoE binaries, shells, descendants and unmanaged writers using this clone/resources have stopped. Recovery does not delete the clone.", h.Project+"/"+h.Run+" stopped"); err != nil {
		return err
	}
	fresh, err := cloneRunState(h)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh, md) {
		return errors.New("run changed while confirmation was pending")
	}
	check, actual, isLegacy, err := clonereclaim.RecoveryRegistry(h)
	if check != nil {
		check.Close()
	}
	if err != nil {
		return err
	}
	if actual != physical || isLegacy != legacy {
		return errors.New("clone changed while confirmation was pending")
	}
	current, err := ownedDevEnvScripts(h.Root, h.Project, devEnvTeardownDirRel)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, scripts) {
		return errors.New("teardown hooks changed while confirmation was pending")
	}
	if legacy {
		r, err = clonereclaim.Enroll(h)
		if err != nil {
			return err
		}
		// Enroll may create only metadata; no external resource is ever inferred
		// from a pathname. Strict ownership validation lives in RecordResources.
		cache, exists, err := r.CacheBytes(h)
		if err != nil {
			return err
		}
		if exists {
			if _, err := r.RecordResources(h, cache, setup, scripts, len(setup) == 0 && len(cacheEnvironmentLines(cache)) == 0); err != nil {
				return err
			}
		}
	} else {
		if err := r.ConfirmHostView(h, physical); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(physical.Path); errors.Is(err, os.ErrNotExist) {
		moePrintf(stdout, "host view recovered for %s/%s; authorized removal remains pending; inspect with `moe clone gc %s/%s`\n", h.Project, h.Run, h.Project, h.Run)
		return nil
	} else if err != nil {
		return err
	}
	if err := r.RepairSetup(h); err != nil {
		return err
	}
	if err := r.ConfirmStopped(h); err != nil {
		return err
	}
	cache, exists, err := r.CacheBytes(h)
	if err != nil {
		return err
	}
	if exists {
		if err := r.AdoptCache(h, setup, scripts, len(setup) == 0 && len(cacheEnvironmentLines(cache)) == 0); err != nil {
			return err
		}
	}
	original, changed, err := r.ScriptChange(h, scripts)
	if err != nil {
		return err
	}
	if changed {
		moePrintln(stdout, "Teardown digest change against retained ORIGINAL resource addresses:")
		for i, s := range scripts {
			moePrintf(stdout, "%s: %s -> %s\n", s.Path, original[i].Digest, s.Digest)
		}
		if err := confirm("Approve these exact replacement teardown digests? Missing hooks must be restored.", h.Project+"/"+h.Run+" approve teardown"); err != nil {
			return err
		}
		fresh, err := cloneRunState(h)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(fresh, md) {
			return errors.New("run changed while script approval was pending")
		}
		if err := r.ApproveScripts(h, scripts); err != nil {
			return err
		}
	}
	// A healthy owned cache remains warm. Only an actual cleanup debt is retried;
	// the original addresses/supervisor contract apply to active runs as well.
	if err := r.ValidateEnvironment(h); err != nil {
		if err := devEnvTeardownOwned(context.Background(), h, md, "refresh", true, stdout, stderr); err != nil {
			return err
		}
	}
	moePrintf(stdout, "recovered %s/%s; clone retained; evidence: %s\n", h.Project, h.Run, r.Receipt())
	return nil
}

func recoverNamedShell(h *runowner.Handle, md *run.Metadata, input *bufio.Reader, stdout io.Writer) error {
	held, err := runHoldsWorkspace(h.Root, md)
	if err != nil {
		return err
	}
	if !held {
		return errors.New("named shell recovery requires the run's current claim")
	}
	registries, err := clonereclaim.NamedShellRecovery(h, workspace.Path(h.Root, md.Project, md.Workspace))
	if err != nil {
		return err
	}
	defer func() {
		for _, r := range registries {
			r.Close()
		}
	}()
	for _, r := range registries {
		moePrintf(stdout, "Named shell lifetime %s; generation %s; inode %d:%d; evidence: %s\n", r.Identity.Path, r.Identity.Generation, r.Identity.Device, r.Identity.Inode, r.Receipt())
	}
	exact := h.Project + "/" + h.Run + " stopped"
	moePrintf(stdout, "Confirm all old binaries, shells, descendants and unmanaged writers have stopped. The named workspace stays enrolled only in its existing lifecycle.\nType %q to continue: ", exact)
	line, err := input.ReadString('\n')
	if err != nil {
		return err
	}
	if line != exact+"\n" {
		return errors.New("confirmation declined")
	}
	fresh, err := cloneRunState(h)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh, md) {
		return errors.New("run changed during named shell recovery")
	}
	held, err = runHoldsWorkspace(h.Root, md)
	if err != nil || !held {
		return errors.New("named workspace claim changed during recovery")
	}
	for _, r := range registries {
		if err := r.ConfirmStopped(h); err != nil {
			return err
		}
	}
	moePrintf(stdout, "recovered shell lifetimes for %s/%s; named workspace retained\n", h.Project, h.Run)
	return nil
}

func cacheEnvironmentLines(cache []byte) map[string]string {
	// Used only to classify an EMPTY cache as resource-free; malformed exports
	// still fail strict ownership validation and cannot become disposable proof.
	env, err := parseDevEnvLines(bytes.NewReader(cache), nil)
	if err != nil {
		return map[string]string{"invalid": ""}
	}
	return env
}
