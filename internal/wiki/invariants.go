package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// assertSchemaInvariantsPreFinalize is the guardrail the engine runs
// pre-finalize. It refuses a finalize that would add or remove docs the
// operator hasn't authorized: every cfg.ManagedDocs[i].Filename must be
// present, no other top-level .md may exist, no topics/ subdir may
// exist.
//
// Bootstrap needs no exception. A fresh wiki's stubs are written before
// finalize, so by the time this runs the docs are always on disk.
func assertSchemaInvariantsPreFinalize(cfg Config) error {
	if len(cfg.ManagedDocs) == 0 {
		return fmt.Errorf("wiki: engine requires ManagedDocs to be non-empty")
	}
	managed := make(map[string]bool, len(cfg.ManagedDocs))
	for _, d := range cfg.ManagedDocs {
		if d.Filename == "" {
			return fmt.Errorf("wiki: ManagedDoc has empty filename")
		}
		managed[d.Filename] = true
	}

	for _, d := range cfg.ManagedDocs {
		if _, err := os.Stat(filepath.Join(cfg.ContentDir, d.Filename)); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("wiki: missing managed doc %s", d.Filename)
			}
			return fmt.Errorf("wiki: stat %s: %w", d.Filename, err)
		}
	}

	// No other top-level .md, no topics/ subdir. Engine-managed files
	// (log.md, checkpoint.json) are exempt.
	entries, err := os.ReadDir(cfg.ContentDir)
	if err != nil {
		if os.IsNotExist(err) {
			// The per-doc stat above already proved the dir exists, so
			// this only fires on a race with something removing it.
			return nil
		}
		return fmt.Errorf("wiki: read %s: %w", cfg.ContentDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == topicsSubdir {
				return fmt.Errorf("wiki: must not contain a %s/ subdir", topicsSubdir)
			}
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if managed[name] {
			continue
		}
		// Engine-managed files (log.md / checkpoint.json)
		// are exempt — they're written by finalize, not by the agent.
		// history-summary.md is engine-aware but agent-written: reflect
		// instructs the agent to maintain it, so it sits alongside
		// log.md as a known-and-allowed top-level doc that isn't part
		// of ManagedDocs.
		if name == "log.md" || name == historySummaryName {
			continue
		}
		return fmt.Errorf("wiki: unexpected top-level doc %s", name)
	}
	return nil
}
