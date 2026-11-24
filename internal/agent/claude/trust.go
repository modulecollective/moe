package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/atomicfile"
	"github.com/modulecollective/moe/internal/git"
)

// Claude Code asks "is this one you trust?" for every --add-dir that
// sits in a git repository it has no trust record for, and an add-dir
// answer lasts one session. A trusted ancestor above a repository's
// root does not count, so the per-run clone and the dev-env scratch
// bureaucracy — each its own repository at a path minted per run —
// asked on every interactive stage open. seedTrust records trust for
// those directories, but only when the operator already trusts the
// bureaucracy they were made from: it derives trust, never grants it.
//
// The key Claude reads for a path is its repository's canonical root
// (a worktree resolves to its main repository), or the path itself
// outside any repository. The clone and MOE_HOME are repository roots
// with a .git directory, so their plain path is the key. A directory
// Claude keys differently gets an inert entry, and the dialog asks as
// before.

// configLockStale mirrors proper-lockfile's default staleness window,
// which Claude Code's config lock uses: a lock directory older than
// this belongs to a writer that died.
const configLockStale = 10 * time.Second

// lockWait bounds how long seedTrust waits on a live Claude writer
// before giving up and leaving the dialog to ask.
var lockWait = time.Second

// claudeConfigFile is where Claude Code keeps its global config,
// including projects[…].hasTrustDialogAccepted: .claude.json inside
// $CLAUDE_CONFIG_DIR when set, else in the home directory.
func claudeConfigFile() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// seedTrust stamps projects[<dir>].hasTrustDialogAccepted for each of
// dirs that lacks it, when Claude's config already trusts the
// bureaucracy at root. It writes nothing when the config file is
// missing, the bureaucracy is untrusted, or every dir is stamped.
//
// Claude rewrites the file under a `<file>.lock` directory lock
// (proper-lockfile), re-reading under it; seedTrust takes the same lock
// so neither writer loses the other's update. A lock held past
// lockWait is skipped with an error: the cost is one dialog.
func seedTrust(root string, dirs []string) error {
	path := claudeConfigFile()
	if path == "" {
		return nil
	}
	trustKeys := bureaucracyKeys(root)
	cfg, perm, err := readClaudeConfig(path)
	if cfg == nil || err != nil {
		return err
	}
	if len(untrusted(cfg, trustKeys, dirs)) == 0 {
		return nil
	}

	unlock, err := lockClaudeConfig(path)
	if err != nil {
		return err
	}
	defer unlock()

	// Re-read under the lock: Claude may have written since.
	cfg, perm, err = readClaudeConfig(path)
	if cfg == nil || err != nil {
		return err
	}
	todo := untrusted(cfg, trustKeys, dirs)
	if len(todo) == 0 {
		return nil
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := cfg["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("%s: projects: %w", path, err)
		}
	}
	for _, d := range todo {
		entry := map[string]json.RawMessage{}
		if raw, ok := projects[d]; ok {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("%s: projects[%s]: %w", path, d, err)
			}
		}
		entry["hasTrustDialogAccepted"] = json.RawMessage("true")
		b, err := marshalConfig(entry)
		if err != nil {
			return err
		}
		projects[d] = b
	}
	b, err := marshalConfig(projects)
	if err != nil {
		return err
	}
	cfg["projects"] = b
	body, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, perm)
}

// bureaucracyKeys are the config keys under which Claude would record
// trust for the bureaucracy at root: root itself, and — root being a
// session worktree — the main repository's root, which is the key
// Claude derives for any of its worktrees.
func bureaucracyKeys(root string) []string {
	keys := []string{filepath.Clean(root)}
	common, err := git.Output(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return keys
	}
	common = filepath.Clean(strings.TrimSpace(common))
	if filepath.Base(common) == ".git" {
		keys = append(keys, filepath.Dir(common))
	}
	return keys
}

// untrusted returns the dirs to stamp: none unless one of trustKeys is
// trusted, otherwise each non-empty dir whose entry is not yet true.
func untrusted(cfg map[string]json.RawMessage, trustKeys, dirs []string) []string {
	var projects map[string]struct {
		Trusted bool `json:"hasTrustDialogAccepted"`
	}
	if raw, ok := cfg["projects"]; ok {
		// A malformed projects value trusts nothing; the write path
		// reports it if we ever get that far.
		_ = json.Unmarshal(raw, &projects)
	}
	trusted := false
	for _, k := range trustKeys {
		if projects[k].Trusted {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if seen[d] || projects[d].Trusted {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// readClaudeConfig returns the config's top-level keys with their
// values verbatim, and the file's mode for the rewrite. A missing file
// returns nil, nil: Claude has not run here, so there is nothing to
// derive trust from.
func readClaudeConfig(path string) (map[string]json.RawMessage, fs.FileMode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]json.RawMessage{}
	}
	return cfg, info.Mode().Perm(), nil
}

// marshalConfig renders v the way Claude writes its config: two-space
// indent, no HTML escaping (so values round-trip byte-for-value).
func marshalConfig(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// lockClaudeConfig takes Claude Code's config lock: the directory
// `<path>.lock`, created atomically by mkdir. A lock older than
// configLockStale is a dead writer's and is taken over, as
// proper-lockfile does.
func lockClaudeConfig(path string) (func(), error) {
	lock := path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(lock); serr == nil && time.Since(info.ModTime()) > configLockStale {
			// Retry at once only if the takeover removed it. A stale
			// lock that will not go (a read-only home) must still run
			// into the deadline below, not spin here.
			if rerr := os.Remove(lock); rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s held by another writer", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
