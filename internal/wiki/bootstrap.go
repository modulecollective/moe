package wiki

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsureManagedDocs is the closed-schema bootstrap step. For each
// cfg.ManagedDocs entry not yet on disk it creates the file with a
// "# <Title>\n\n" stub. Runs at session open before EnsureOpsStash so
// closed-schema invariants are satisfied for the rest of the turn —
// the agent's edits during the session land on top of the stub, and
// the per-turn commit ships the freshly-stubbed file alongside the
// agent's content.
//
// Returns true when at least one stub was created (the "fresh wiki"
// signal callers can use to adjust kickoff framing). Open-schema is a
// no-op — it returns (false, nil).
func EnsureManagedDocs(cfg Config) (bool, error) {
	if cfg.Mode != Closed {
		return false, nil
	}
	if len(cfg.ManagedDocs) == 0 {
		return false, fmt.Errorf("wiki: closed-schema requires ManagedDocs to be non-empty")
	}
	if err := os.MkdirAll(cfg.ContentDir, 0o755); err != nil {
		return false, fmt.Errorf("wiki: mkdir %s: %w", cfg.ContentDir, err)
	}
	stubbed := false
	for _, d := range cfg.ManagedDocs {
		path := filepath.Join(cfg.ContentDir, d.Filename)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return stubbed, fmt.Errorf("wiki: stat %s: %w", path, err)
		}
		body := fmt.Sprintf("# %s\n\n", d.Title)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return stubbed, fmt.Errorf("wiki: write stub %s: %w", path, err)
		}
		stubbed = true
	}
	return stubbed, nil
}
