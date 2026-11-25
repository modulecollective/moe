//go:build linux

package clonereclaim

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// Inventory includes both live paths and retained generations. Every parent is
// opened without following links. Invalid names are skipped with diagnostics;
// directory access failures and inventory limits fail the whole scan.
func Inventory(root string, stderr io.Writer) ([]string, error) {
	f, err := openAbsoluteDir(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	moe, err := childDir(f, ".moe", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer moe.Close()
	keys := map[string]bool{}
	for _, name := range []string{"clones", "clone-reclamation"} {
		base, err := childDir(moe, name, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		projects, err := base.ReadDir(4097)
		if err != nil && err != io.EOF {
			base.Close()
			return nil, err
		}
		if len(projects) > 4096 {
			base.Close()
			return nil, errors.New("project inventory limit exceeded")
		}
		for _, p := range projects {
			if !project.ValidID(p.Name()) {
				fmt.Fprintf(stderr, "clone gc inventory: skipping %q: invalid project ID\n", filepath.Join(root, ".moe", name, p.Name()))
				continue
			}
			dir, err := childDir(base, p.Name(), false)
			if err != nil {
				base.Close()
				return nil, err
			}
			runs, err := dir.ReadDir(8193)
			dir.Close()
			if err != nil && err != io.EOF {
				base.Close()
				return nil, err
			}
			if len(runs) > 8192 {
				base.Close()
				return nil, errors.New("run inventory limit exceeded")
			}
			for _, r := range runs {
				if !run.ValidID(r.Name()) {
					fmt.Fprintf(stderr, "clone gc inventory: skipping %q: invalid run ID\n", filepath.Join(root, ".moe", name, p.Name(), r.Name()))
					continue
				}
				keys[p.Name()+"/"+r.Name()] = true
				if len(keys) > 8192 {
					base.Close()
					return nil, errors.New("candidate inventory limit exceeded")
				}
			}
		}
		base.Close()
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
