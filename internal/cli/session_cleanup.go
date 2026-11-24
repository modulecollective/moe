package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/procgroup"
	"github.com/modulecollective/moe/internal/project"
)

// sessionCleanupDirRel is the project hook event session GC fires when
// an unregistered session directory (rule 5) refuses host removal.
// MoE knows nothing about how a file came to be unremovable — a
// container that wrote through a bind mount as root is the case that
// prompted it — so the project that made the mess supplies the repair.
// The hook's contract is narrow: make $MOE_SESSION_DIR removable by
// the host user. MoE then retries the removal itself.
const sessionCleanupDirRel = "session-cleanup.d"

// sessionCleanupTimeout bounds each script. It runs from the serve
// heartbeat as well as the verb, so a hung script must not wedge the
// tick. Generous enough for a container image pull. A var so tests can
// shorten it.
var sessionCleanupTimeout = 2 * time.Minute

// blockedSessionDir is a rule-5 directory whose first removal failed
// with a permission error and is waiting on project hooks.
type blockedSessionDir struct {
	dir string
	err error
}

// sessionResidueProjects names the projects whose state directories
// survive under a failed removal. RemoveAll deletes children before
// parents, so a surviving `projects/<p>/` holds something the host
// could not unlink — those are the projects whose hooks get a turn.
// Only projects that exist in the bureaucracy count; the name is a
// directory entry, so it cannot carry a separator.
func sessionResidueProjects(root, dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, "projects"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, project.Dir(e.Name()))); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// runSessionCleanupHooks runs every residue project's
// session-cleanup.d scripts against dir, in project then script order.
// ran reports whether any script existed. A failing script stops that
// project's remaining scripts but not other projects': each project
// repairs its own files.
func runSessionCleanupHooks(root, dir string, out io.Writer) (ran bool, err error) {
	var errs []error
	for _, p := range sessionResidueProjects(root, dir) {
		hookDirRel := filepath.Join(project.Dir(p), "hooks", sessionCleanupDirRel)
		hookDir := filepath.Join(root, hookDirRel)
		scripts, lerr := listExecutables(hookDir)
		if lerr != nil {
			errs = append(errs, lerr)
			continue
		}
		if len(scripts) == 0 {
			continue
		}
		ran = true
		banner.HookSection(out, sessionCleanupDirRel, len(scripts), hookDirRel)
		for _, script := range scripts {
			if serr := runSessionCleanupScript(root, dir, p, filepath.Join(hookDir, script), script, out); serr != nil {
				errs = append(errs, fmt.Errorf("%s: %w", filepath.Join(hookDirRel, script), serr))
				break
			}
		}
	}
	return ran, errors.Join(errs...)
}

func runSessionCleanupScript(root, dir, projectID, path, name string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionCleanupTimeout)
	defer cancel()
	banner.HookStart(out, name)
	start := time.Now()
	cmd := exec.CommandContext(ctx, path)
	// The group kill reaches a script's children (a docker client, a
	// sleep) so the deadline returns rather than waiting on whoever
	// still holds the output pipe.
	procgroup.Isolate(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"MOE_PROJECT="+projectID,
		"MOE_BUREAUCRACY="+root,
		"MOE_SESSION_DIR="+dir,
	)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	banner.HookDone(out, name, time.Since(start))
	if ctx.Err() != nil {
		return fmt.Errorf("timed out after %s: %w", sessionCleanupTimeout, err)
	}
	return err
}
