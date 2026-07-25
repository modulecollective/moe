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
// The bootstrap exception is handled by the caller: runWikiSession
// passes opts.Bootstrap=true on the first turn for a fresh wiki (when
// the engine just wrote the stubs). On bootstrap turns the present-docs
// check skips — the stubs are about to land in the same commit and
// assertSchemaInvariantsPreFinalize is called pre-finalize, so the docs
// are always on disk by then anyway, but the flag exists so callers can
// skip the check explicitly when they know they just stubbed.
func assertSchemaInvariantsPreFinalize(cfg Config) error {
	return assertSchemaInvariants(cfg, false)
}

// assertSchemaInvariantsBootstrap is assertSchemaInvariantsPreFinalize
// with the present-docs requirement relaxed: missing managed docs are
// tolerated because the engine is about to create them in this turn.
// Used by runWikiSession on the first turn for a fresh wiki.
func assertSchemaInvariantsBootstrap(cfg Config) error {
	return assertSchemaInvariants(cfg, true)
}

func assertSchemaInvariants(cfg Config, bootstrap bool) error {
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

	if !bootstrap {
		for _, d := range cfg.ManagedDocs {
			if _, err := os.Stat(filepath.Join(cfg.ContentDir, d.Filename)); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("wiki: missing managed doc %s", d.Filename)
				}
				return fmt.Errorf("wiki: stat %s: %w", d.Filename, err)
			}
		}
	}

	// No other top-level .md, no topics/ subdir. Engine-managed files
	// (log.md, checkpoint.json) are exempt.
	entries, err := os.ReadDir(cfg.ContentDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing on disk yet — bootstrap covers this case;
			// non-bootstrap with a missing dir is already caught above
			// by the per-doc stat.
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
