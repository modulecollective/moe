package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/run"
)

// Per-run clone inventory and guarded reclamation.
func init() {
	g := NewCommandGroup("clone", "per-run sandbox clone admin (.moe/clones/)")
	g.Register(&Command{
		Name:    "list",
		Summary: "list per-run sandbox clones under .moe/clones/ with their run status",
		Run:     runCloneList,
	})
	g.Register(&Command{
		Name:    "gc",
		Summary: "inspect terminal clone reclamation; --apply performs eligible cleanup",
		Run:     runCloneGC,
	})
	g.Register(&Command{Name: "recover", Summary: "interactively recover one stopped run clone", Run: runCloneRecover})
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
