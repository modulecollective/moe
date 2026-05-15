// Package config is the operator-local key/value layer behind
// `moe config`. The on-disk file is .moe/config.json under the
// bureaucracy root — same neighbourhood as queue.json and the
// clones/ tree, operator-local and never committed.
//
// The keyspace is closed: every accepted key has an entry in the
// allowlist below, with a per-key validator that runs at Set time.
// A future key joins via a code change here plus a one-line entry,
// not by writing arbitrary JSON. The design (set-default-model
// run) calls this out as the discipline that keeps `moe config` from
// drifting into a free-form config-knobs surface.
//
// Concurrency: read-modify-write with temp+rename for atomicity
// against partial writes. No file lock — MoE is single-operator,
// and two concurrent `moe config set` invocations racing on the
// same key is a non-issue worth solving (last write wins).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/agent"
)

// Config is the on-disk shape of .moe/config.json. Each accepted
// key is a field with `omitempty` so an unset key round-trips
// cleanly (read-modify-write doesn't re-introduce a `"": ""` line).
type Config struct {
	DefaultAgent string `json:"default_agent,omitempty"`
}

// Path returns the absolute path to .moe/config.json under root.
func Path(root string) string {
	return filepath.Join(root, ".moe", "config.json")
}

// Read loads .moe/config.json. A missing file is a normal state
// (operator has never run `moe config set`) and returns an empty
// Config with no error. An empty file is treated the same.
func Read(root string) (Config, error) {
	p := Path(root)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", p, err)
	}
	if len(b) == 0 {
		return Config{}, nil
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", p, err)
	}
	return c, nil
}

// Write persists c to .moe/config.json. The .moe directory is
// created lazily on first write — operators on a fresh checkout
// don't need to pre-create anything. Write uses temp+rename so a
// concurrent reader never sees a half-written file.
func Write(root string, c Config) error {
	p := Path(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", filepath.Dir(p), err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	b = append(b, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename %s: %w", p, err)
	}
	return nil
}

// keyEntry binds an operator-facing key string to the typed field
// it reads/writes on a Config plus the per-key validator that runs
// at Set time. The keyspace lives entirely in this table so adding
// or removing a key is a one-place change.
type keyEntry struct {
	get      func(Config) string
	set      func(*Config, string)
	validate func(string) error
}

// keys is the closed allowlist of operator-settable keys. A new key
// joins by adding an entry here and a matching JSON-tagged field on
// Config. Keep entries sorted by name for stable `moe config list`
// output.
var keys = map[string]keyEntry{
	"default_agent": {
		get: func(c Config) string { return c.DefaultAgent },
		set: func(c *Config, v string) { c.DefaultAgent = v },
		// Validate at set-time against the agent registry so a typo
		// surfaces at the keystroke, not at the next stage turn.
		validate: func(v string) error {
			if _, err := agent.Get(v); err != nil {
				return err
			}
			return nil
		},
	},
}

// Keys returns the allowlisted key names in sorted order. The CLI
// layer uses this for `moe config list` and for the
// unknown-key error message on `set` / `unset` / `get`.
func Keys() []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Known reports whether key is in the allowlist.
func Known(key string) bool {
	_, ok := keys[key]
	return ok
}

// ErrUnknownKey is returned by Get / Set / Unset when key is not in
// the allowlist. Callers can errors.Is against it to render a
// "valid keys are: ..." message without parsing strings.
var ErrUnknownKey = errors.New("config: unknown key")

// Get returns the current value for key on c. An unset key (the
// zero value of its field) returns an empty string with ok=true —
// the caller distinguishes unset from set-to-empty if it cares.
// An unknown key returns ("", false).
func Get(c Config, key string) (string, bool) {
	e, ok := keys[key]
	if !ok {
		return "", false
	}
	return e.get(c), true
}

// Set validates value against key's per-key validator and writes
// it onto c. Returns ErrUnknownKey wrapped with the key name for
// an unknown key; returns the validator's error verbatim for an
// invalid value.
func Set(c *Config, key, value string) error {
	e, ok := keys[key]
	if !ok {
		return fmt.Errorf("%w: %q (valid: %v)", ErrUnknownKey, key, Keys())
	}
	if err := e.validate(value); err != nil {
		return err
	}
	e.set(c, value)
	return nil
}

// Unset clears key on c back to its zero value. Returns
// ErrUnknownKey for an unknown key. An already-unset key is not an
// error — `moe config unset` is idempotent.
func Unset(c *Config, key string) error {
	e, ok := keys[key]
	if !ok {
		return fmt.Errorf("%w: %q (valid: %v)", ErrUnknownKey, key, Keys())
	}
	e.set(c, "")
	return nil
}
