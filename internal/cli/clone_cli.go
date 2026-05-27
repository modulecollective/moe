package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

// `moe clone` owns the per-run sandbox clones under
// `.moe/clones/<project>/<run>/`. `list` prints what's on disk for the
// operator to eyeball; `gc` reconciles against the run registry,
// removing clones whose owning run has reached a terminal status
// (merged / closed / promoted) or whose run.json is gone entirely.
// Pushed and in-progress runs are skipped — their clones are still
// load-bearing for `moe shell`.
//
// The verb is the recovery surface the sandbox-remove warning points at
// when a container-written file blocks `os.RemoveAll` (rootless docker
// maps container-root → host nobody:nogroup, which the moe process
// can't unlink). For each orphan `gc` first tries an in-process
// RemoveAll; on EACCES it falls back to a one-shot `docker run --rm -v
// <clone>:/x alpine rm -rf /x` so the cleanup runs as the same UID that
// wrote the file. If docker isn't on PATH or the container call also
// fails, it leaves the clone in place and prints the exact command the
// operator can copy.

func init() {
	g := NewCommandGroup("clone", "list or garbage-collect per-run sandbox clones under .moe/clones/")
	g.Register(&Command{
		Name:    "list",
		Summary: "list per-run sandbox clones under .moe/clones/ with their run status",
		Run:     runCloneList,
	})
	g.Register(&Command{
		Name:    "gc",
		Summary: "remove orphan per-run sandbox clones under .moe/clones/",
		Run:     runCloneGC,
	})
	RegisterGroup(g)
}

func runCloneList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clone list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe clone list")
		moePrintln(stderr, "")
		moePrintln(stderr, "Prints one line per directory under .moe/clones/ as")
		moePrintln(stderr, "<project>/<run>\\t<status>\\t<path>. Status comes from the run registry;")
		moePrintln(stderr, "clones whose run.json is gone print (missing).")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	entries, err := listClones(root)
	if err != nil {
		moePrintf(stderr, "clone list: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		moePrintln(stdout, "clone list: no clones")
		return 0
	}
	for _, e := range entries {
		moePrintf(stdout, "%s/%s\t%s\t%s\n", e.project, e.run, e.status, e.path)
	}
	return 0
}

func runCloneGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("clone gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe clone gc")
		moePrintln(stderr, "")
		moePrintln(stderr, "Removes sandbox clones whose run reached a terminal status (merged,")
		moePrintln(stderr, "closed, promoted) or whose run.json is no longer on disk. In-progress")
		moePrintln(stderr, "and pushed runs are left alone — their clones are still in use.")
		moePrintln(stderr, "")
		moePrintln(stderr, "Falls back to a container-driven rm when a clone holds container-written")
		moePrintln(stderr, "files the moe process can't unlink; on fallback failure, prints the exact")
		moePrintln(stderr, "`docker run --rm -v` line for the operator to run by hand.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	orphans, err := findOrphanClones(root)
	if err != nil {
		moePrintf(stderr, "clone gc: %v\n", err)
		return 1
	}
	if len(orphans) == 0 {
		moePrintln(stdout, "clone gc: no orphan clones")
		return 0
	}

	failed := 0
	for _, o := range orphans {
		if err := removeOrphanClone(o.path); err != nil {
			moePrintf(stderr, "clone gc: %s/%s: %v\n", o.project, o.run, err)
			failed++
			continue
		}
		moePrintf(stdout, "removed %s/%s\n", o.project, o.run)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// orphanClone describes a clone directory that no live run owns.
type orphanClone struct {
	project string
	run     string
	path    string
}

// cloneEntry is one row of `moe clone list` output: a clone directory
// paired with the status its owning run is at, or "(missing)" when
// run.json is gone.
type cloneEntry struct {
	project string
	run     string
	status  string
	path    string
}

// listClones walks `.moe/clones/<project>/<run>/` and pairs each clone
// with the status of its owning run. Clones without a matching run.json
// get status "(missing)". Result is sorted (project, run) so output is
// stable.
func listClones(root string) ([]cloneEntry, error) {
	clonesRoot := filepath.Join(root, ".moe", "clones")
	entries, err := os.ReadDir(clonesRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", clonesRoot, err)
	}

	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	status := make(map[string]string, len(mds))
	for _, md := range mds {
		status[md.Project+"/"+md.ID] = md.Status
	}

	var out []cloneEntry
	for _, projEnt := range entries {
		if !projEnt.IsDir() {
			continue
		}
		project := projEnt.Name()
		projDir := filepath.Join(clonesRoot, project)
		runEnts, err := os.ReadDir(projDir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", projDir, err)
		}
		for _, runEnt := range runEnts {
			if !runEnt.IsDir() {
				continue
			}
			runID := runEnt.Name()
			s, ok := status[project+"/"+runID]
			if !ok {
				s = "(missing)"
			}
			out = append(out, cloneEntry{
				project: project,
				run:     runID,
				status:  s,
				path:    filepath.Join(projDir, runID),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].project != out[j].project {
			return out[i].project < out[j].project
		}
		return out[i].run < out[j].run
	})
	return out, nil
}

// findOrphanClones returns clone directories under .moe/clones/ whose
// owning run is at a terminal status or whose run.json is missing.
// In-progress and pushed runs are skipped. Result is sorted by
// (project, run) so the output order is stable.
func findOrphanClones(root string) ([]orphanClone, error) {
	clonesRoot := filepath.Join(root, ".moe", "clones")
	entries, err := os.ReadDir(clonesRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", clonesRoot, err)
	}

	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("scan runs: %w", err)
	}
	status := make(map[string]string, len(mds))
	for _, md := range mds {
		status[md.Project+"/"+md.ID] = md.Status
	}

	var out []orphanClone
	for _, projEnt := range entries {
		if !projEnt.IsDir() {
			continue
		}
		project := projEnt.Name()
		projDir := filepath.Join(clonesRoot, project)
		runEnts, err := os.ReadDir(projDir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", projDir, err)
		}
		for _, runEnt := range runEnts {
			if !runEnt.IsDir() {
				continue
			}
			runID := runEnt.Name()
			key := project + "/" + runID
			s, ok := status[key]
			switch {
			case !ok:
				// No run.json on disk for this clone — orphan.
			case s == run.StatusMerged, s == run.StatusClosed, s == run.StatusPromoted:
				// Terminal status — orphan.
			default:
				// in_progress / pushed / anything unknown — leave alone.
				continue
			}
			out = append(out, orphanClone{
				project: project,
				run:     runID,
				path:    filepath.Join(projDir, runID),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].project != out[j].project {
			return out[i].project < out[j].project
		}
		return out[i].run < out[j].run
	})
	return out, nil
}

// removeOrphanClone tries os.RemoveAll first. On a permission error
// (typical when a container wrote files as a UID the host can't
// unlink), it falls back to a one-shot `docker run --rm -v <path>:/x
// alpine rm -rf /x` so the cleanup runs as container-root.
//
// If docker isn't on PATH or the container rm also fails, returns an
// error whose message names the exact command the operator can run by
// hand. Stays stdlib + a single `docker` exec — no compose
// introspection, no project runtime registry; the AGENTS.md
// stdlib-only stance allows a runtime probe but not a hard dep.
func removeOrphanClone(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if err := os.RemoveAll(abs); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrPermission) {
		return err
	}
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return fmt.Errorf("permission denied; docker not on PATH for fallback. try: %s",
			dockerRmRecipe(abs))
	}
	// One-shot container as root, bind-mounts the clone, rm -rf the
	// in-container view, then exits. --rm so the container itself
	// leaves no trace.
	cmd := exec.Command("docker", "run", "--rm", "-v", abs+":/x", "alpine", "rm", "-rf", "/x")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("permission denied; docker fallback failed: %v: %s. try: %s",
			err, strings.TrimSpace(string(out)), dockerRmRecipe(abs))
	}
	// The bind-mount target itself isn't removed by container-side rm
	// — the host-side dir survives even when empty. RemoveAll once
	// more on the (now empty) host path finishes the job.
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("docker fallback emptied clone but host remove still failed: %w", err)
	}
	return nil
}

// dockerRmRecipe returns the exact shell command the operator can run
// by hand when both the in-process and docker-fallback removals fail.
// Kept as a single helper so the warning text and the gc-failure text
// stay in lockstep.
func dockerRmRecipe(abs string) string {
	return fmt.Sprintf("docker run --rm -v %s:/x alpine rm -rf /x", abs)
}
