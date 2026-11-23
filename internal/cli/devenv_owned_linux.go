//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func ownedRunRegistry(h *runowner.Handle, md *run.Metadata) (*clonereclaim.Registry, error) {
	if h == nil || md.Workspace != "" || !h.Owns(h.Root, md.Project, md.ID) {
		return nil, errors.New("per-run environment requires explicit run ownership")
	}
	r, err := clonereclaim.LoadRegistry(h)
	if err != nil {
		return nil, fmt.Errorf("per-run environment needs host enrollment/recovery: %w", err)
	}
	return r, nil
}

// devEnvSetupOwned is the per-run setup/refresh path. Callers pass their held
// guard; no borrower table, fresh allocation, or ambiguous legacy cache can
// substitute for durable ownership. Named workspaces use devEnvSetupEnv.
func devEnvSetupOwned(h *runowner.Handle, md *run.Metadata, stdout, stderr io.Writer) (map[string]string, bool, error) {
	r, err := ownedRunRegistry(h, md)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	if err := r.ValidateEnvironment(h); err != nil {
		return nil, false, err
	}
	if err := r.PersistCleared(h); err != nil {
		return nil, false, err
	}
	setup, err := ownedDevEnvScripts(h.Root, md.Project, devEnvDirRel)
	if err != nil {
		return nil, false, err
	}
	teardown, err := ownedDevEnvScripts(h.Root, md.Project, devEnvTeardownDirRel)
	if err != nil {
		return nil, false, err
	}
	if len(setup) > 0 && len(teardown) == 0 {
		return nil, false, errors.New("dev-env setup requires an executable teardown contract")
	}
	workTree := sandbox.Path(h.Root, md.Project, md.ID)
	var intent clonereclaim.SetupIntent
	var original []byte
	env, fresh, err := devEnvSetupWithLifecycle(h.Root, workTree, md, stdout, stderr, func() error {
		return devEnvTeardownOwned(context.Background(), h, md, "refresh", false, stdout, stderr)
	}, func(cache []byte) error {
		original = bytes.Clone(cache)
		var err error
		intent, err = r.BeginSetup(h, original, setup, teardown)
		return err
	})
	if err != nil || !fresh {
		return env, fresh, err
	}
	snapshot, err := r.RecordResources(h, original, setup, teardown, len(setup) == 0 && len(env) == 0)
	if err != nil {
		rollbackErr := rollbackDevEnvSetup(h.Root, workTree, md, env, stdout, stderr)
		var clearErr error
		if rollbackErr == nil {
			clearErr = devEnvClearCache(workTree)
		}
		return nil, false, errors.Join(err, rollbackErr, clearErr)
	}
	if err := r.CompleteSetup(h, intent, snapshot); err != nil {
		// Ownership already exists outside the clone. Keep it and its exact
		// cache for recovery rather than rolling back a committed allocation.
		return nil, false, err
	}
	return env, true, nil
}

// devEnvTeardownOwned is shared by terminal cleanup, cache refresh and rebind.
// Lifecycle/session/preservation preflight belongs to its guarded caller;
// this function never removes a clone, changes run status or ships code.
func devEnvTeardownOwned(ctx context.Context, h *runowner.Handle, md *run.Metadata, purpose string, retry bool, stdout, stderr io.Writer) error {
	r, err := ownedRunRegistry(h, md)
	if err != nil {
		return err
	}
	defer r.Close()
	scripts, err := ownedDevEnvScripts(h.Root, md.Project, devEnvTeardownDirRel)
	if err != nil {
		return err
	}
	plan, err := r.PlanCleanup(h, purpose, scripts, retry)
	if err != nil {
		return err
	}
	env := map[string]string{}
	if plan.Resources != nil {
		env, err = parseDevEnvLines(bytes.NewReader(plan.Resources.Cache), nil)
		if err != nil {
			return fmt.Errorf("saved resource cache cannot be decoded: %w", err)
		}
	}
	// Snapshot addresses override ambient values. Keys naming the harness's
	// managed roots must also be masked when absent from the original setup.
	for _, key := range devEnvWritableDirKeys {
		if _, ok := env[key]; !ok {
			env[key] = ""
		}
	}
	base := devEnvBaseEnv(h.Root, r.Identity.Path, md)
	return r.ExecuteCleanup(ctx, h, plan, append(base, mapToEnv(env)...), stdout, stderr)
}

func ownedDevEnvScripts(root, projectID, event string) ([]clonereclaim.Script, error) {
	rel := filepath.Join(project.Dir(projectID), "hooks", event)
	f, err := os.Open(filepath.Join(root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(1025)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 1024 {
		return nil, errors.New("dev-env hook inventory limit exceeded")
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlinked dev-env hook requires recovery")
		}
		if info.Mode()&0o111 != 0 {
			paths = append(paths, filepath.Join(rel, entry.Name()))
		}
	}
	sort.Strings(paths)
	return clonereclaim.ScriptIdentities(root, paths)
}
