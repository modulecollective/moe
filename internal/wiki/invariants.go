package wiki

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AssertModeInvariants is the guardrail the engine runs pre-finalize.
// For open-schema wikis it's a no-op — the agent is permitted to evolve
// the doc set freely. For closed-schema wikis (the twin) it refuses a
// finalize that would add or remove docs the operator hasn't authorized:
// every cfg.ManagedDocs[i].Filename must be present, no other top-level
// .md may exist, no topics/ subdir may exist.
//
// The bootstrap exception is handled by the caller: runWikiSession
// passes opts.Bootstrap=true on the first turn for a fresh closed-schema
// wiki (when the engine just wrote the stubs). On bootstrap turns the
// present-docs check skips — the stubs are about to land in the same
// commit and AssertModeInvariants is called pre-finalize, so the docs
// are always on disk by then anyway, but the flag exists so callers can
// skip the check explicitly when they know they just stubbed.
func AssertModeInvariants(cfg Config) error {
	return assertModeInvariants(cfg, false)
}

// AssertModeInvariantsBootstrap is AssertModeInvariants with the
// present-docs requirement relaxed: missing managed docs are tolerated
// because the engine is about to create them in this turn. Used by
// runWikiSession on the first turn for a fresh closed-schema wiki.
func AssertModeInvariantsBootstrap(cfg Config) error {
	return assertModeInvariants(cfg, true)
}

func assertModeInvariants(cfg Config, bootstrap bool) error {
	switch cfg.Mode {
	case Open:
		return nil
	case Closed:
		return assertClosedInvariants(cfg, bootstrap)
	default:
		return fmt.Errorf("wiki: unknown mode %d", cfg.Mode)
	}
}

func assertClosedInvariants(cfg Config, bootstrap bool) error {
	if len(cfg.ManagedDocs) == 0 {
		return fmt.Errorf("wiki: closed-schema requires ManagedDocs to be non-empty")
	}
	managed := make(map[string]bool, len(cfg.ManagedDocs))
	for _, d := range cfg.ManagedDocs {
		if d.Filename == "" {
			return fmt.Errorf("wiki: closed-schema ManagedDoc has empty filename")
		}
		managed[d.Filename] = true
	}

	if !bootstrap {
		for _, d := range cfg.ManagedDocs {
			if _, err := os.Stat(filepath.Join(cfg.ContentDir, d.Filename)); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("wiki: closed-schema missing managed doc %s", d.Filename)
				}
				return fmt.Errorf("wiki: stat %s: %w", d.Filename, err)
			}
		}
	}

	// No other top-level .md, no topics/ subdir. Engine-managed files
	// (log.md, checkpoint.json, .wiki-ops) are exempt.
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
			if name == TopicsSubdir {
				return fmt.Errorf("wiki: closed-schema must not contain a %s/ subdir", TopicsSubdir)
			}
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if managed[name] {
			continue
		}
		// Engine-managed files (log.md / checkpoint.json / .wiki-ops)
		// are exempt — they're written by finalize, not by the agent.
		if name == "log.md" {
			continue
		}
		return fmt.Errorf("wiki: closed-schema has unexpected top-level doc %s", name)
	}
	return nil
}
