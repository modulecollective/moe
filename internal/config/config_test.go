package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	// Importing the agent registry packages registers the agents the
	// default_agent validator needs to recognise. Without these, every
	// Set("default_agent", ...) test would fail with an empty
	// registry. The CLI binary pulls them in transitively; for the
	// unit test we have to pull them in directly.
	_ "github.com/modulecollective/moe/internal/agent/claude"
	_ "github.com/modulecollective/moe/internal/agent/codex"
)

func TestReadMissingFileIsEmpty(t *testing.T) {
	root := t.TempDir()
	c, err := Read(root)
	if err != nil {
		t.Fatalf("Read on missing file: %v", err)
	}
	if c != (Config{}) {
		t.Fatalf("Read on missing file = %+v, want zero Config", c)
	}
}

func TestWriteThenRead(t *testing.T) {
	root := t.TempDir()
	want := Config{DefaultAgent: "codex"}
	if err := Write(root, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
}

func TestWriteOmitsEmptyKeys(t *testing.T) {
	// An unset DefaultAgent should round-trip as `{}`, not
	// `{"default_agent": ""}`. omitempty on the field is what makes
	// `unset` of the last key leave a clean object behind.
	root := t.TempDir()
	if err := Write(root, Config{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("empty Config wrote %v, want {}", raw)
	}
}

func TestSetThenGet(t *testing.T) {
	var c Config
	if err := Set(&c, "default_agent", "claude"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok := Get(c, "default_agent")
	if !ok {
		t.Fatalf("Get returned ok=false for known key")
	}
	if v != "claude" {
		t.Fatalf("Get = %q, want %q", v, "claude")
	}
}

func TestSetUnknownKey(t *testing.T) {
	var c Config
	err := Set(&c, "no_such_key", "x")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Set on unknown key err = %v, want ErrUnknownKey", err)
	}
}

func TestSetInvalidValue(t *testing.T) {
	var c Config
	err := Set(&c, "default_agent", "definitely-not-an-agent")
	if err == nil {
		t.Fatalf("Set with bogus agent name should error")
	}
}

func TestUnset(t *testing.T) {
	c := Config{DefaultAgent: "codex"}
	if err := Unset(&c, "default_agent"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if v, _ := Get(c, "default_agent"); v != "" {
		t.Fatalf("after Unset, value = %q, want \"\"", v)
	}
}

func TestUnsetIdempotent(t *testing.T) {
	var c Config
	if err := Unset(&c, "default_agent"); err != nil {
		t.Fatalf("Unset on already-empty key: %v", err)
	}
}

func TestUnsetUnknownKey(t *testing.T) {
	var c Config
	err := Unset(&c, "no_such_key")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Unset on unknown key err = %v, want ErrUnknownKey", err)
	}
}

func TestGetUnknownKey(t *testing.T) {
	v, ok := Get(Config{}, "no_such_key")
	if ok {
		t.Fatalf("Get on unknown key returned ok=true (v=%q)", v)
	}
}

func TestKeysIsSorted(t *testing.T) {
	ks := Keys()
	for i := 1; i < len(ks); i++ {
		if ks[i-1] > ks[i] {
			t.Fatalf("Keys() not sorted: %v", ks)
		}
	}
	if len(ks) == 0 {
		t.Fatalf("Keys() empty; expected at least default_agent")
	}
}

func TestKnown(t *testing.T) {
	if !Known("default_agent") {
		t.Fatalf("Known(default_agent) = false")
	}
	if Known("nope") {
		t.Fatalf("Known(nope) = true")
	}
}

func TestRoundTripMatchesDesignShape(t *testing.T) {
	// The design pins the on-disk shape: flat JSON object,
	// snake_case keys. Lock both with a literal-bytes check so a
	// future field rename can't drift silently.
	root := t.TempDir()
	if err := Write(root, Config{DefaultAgent: "codex"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(raw, map[string]string{"default_agent": "codex"}) {
		t.Fatalf("on-disk shape = %v, want {\"default_agent\":\"codex\"}", raw)
	}
}

func TestPath(t *testing.T) {
	root := "/tmp/moe-root"
	got := Path(root)
	want := filepath.Join(root, ".moe", "config.json")
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}
