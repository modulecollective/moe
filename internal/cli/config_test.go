package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/config"
)

func TestConfigGroupRegistered(t *testing.T) {
	cmd, ok := commands["config"]
	if !ok {
		t.Fatal(`expected top-level command "config" to be registered`)
	}
	var out, errb bytes.Buffer
	if code := cmd.Run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"list", "get", "set", "unset"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("config usage missing subcommand %q: %q", want, out.String())
		}
	}
}

func TestConfigListEmpty(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	// Every allowlist key prints, with `(unset)` when no value.
	for _, k := range config.Keys() {
		want := k + " = " + configUnsetMarker
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in list output: %q", want, out.String())
		}
	}
}

func TestConfigSetThenList(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"config", "set", "default_agent", "codex"}, &out, &errb); code != 0 {
		t.Fatalf("set exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"config", "list"}, &out, &errb); code != 0 {
		t.Fatalf("list exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "default_agent = codex") {
		t.Fatalf("expected default_agent = codex in list, got: %q", out.String())
	}
}

func TestConfigSetUnknownKeyRefuses(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "set", "no_such_key", "x"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on unknown key, got %d (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "unknown key") {
		t.Fatalf("expected unknown-key message, got: %q", errb.String())
	}
}

func TestConfigSetInvalidValueRefuses(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "set", "default_agent", "definitely-not-an-agent"}, &out, &errb)
	if code != 1 {
		t.Fatalf("expected exit=1 on bogus agent, got %d (stderr=%q)", code, errb.String())
	}
	// The file should not have been created — a failed validation
	// must not leave a half-written config behind.
	if _, err := os.Stat(filepath.Join(root, ".moe", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("config.json should not exist after a failed set; stat err=%v", err)
	}
}

func TestConfigGetUnset(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "get", "default_agent"}, &out, &errb)
	if code != 0 {
		t.Fatalf("expected exit=0 on unset key, got %d (stderr=%q)", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("expected empty stdout for unset key, got: %q", out.String())
	}
}

func TestConfigGetSet(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"config", "set", "default_agent", "claude"}, &out, &errb); code != 0 {
		t.Fatalf("set exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"config", "get", "default_agent"}, &out, &errb); code != 0 {
		t.Fatalf("get exit=%d stderr=%q", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "claude" {
		t.Fatalf("get default_agent = %q, want %q", strings.TrimSpace(out.String()), "claude")
	}
}

func TestConfigGetUnknownKey(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "get", "no_such_key"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on unknown key, got %d (stderr=%q)", code, errb.String())
	}
}

func TestConfigUnset(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	if code := Run([]string{"config", "set", "default_agent", "codex"}, &out, &errb); code != 0 {
		t.Fatalf("set exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"config", "unset", "default_agent"}, &out, &errb); code != 0 {
		t.Fatalf("unset exit=%d stderr=%q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"config", "get", "default_agent"}, &out, &errb); code != 0 {
		t.Fatalf("get exit=%d stderr=%q", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("after unset, get returned %q, want empty", out.String())
	}
	// The design says unset of the last key leaves an empty `{}` on
	// disk, not a missing file — one fewer special case in the
	// reader.
	b, err := os.ReadFile(filepath.Join(root, ".moe", "config.json"))
	if err != nil {
		t.Fatalf("config.json should still exist after unset: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("after unset, expected {} on disk, got: %v", raw)
	}
}

func TestConfigUnsetUnknownKey(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	var out, errb bytes.Buffer
	code := Run([]string{"config", "unset", "no_such_key"}, &out, &errb)
	if code != 2 {
		t.Fatalf("expected exit=2 on unknown key, got %d (stderr=%q)", code, errb.String())
	}
}

func TestConfigArityErrors(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")

	cases := []struct {
		name string
		args []string
	}{
		{"list with arg", []string{"config", "list", "stray"}},
		{"get no args", []string{"config", "get"}},
		{"get too many", []string{"config", "get", "a", "b"}},
		{"set no args", []string{"config", "set"}},
		{"set one arg", []string{"config", "set", "default_agent"}},
		{"set too many", []string{"config", "set", "a", "b", "c"}},
		{"unset no args", []string{"config", "unset"}},
		{"unset too many", []string{"config", "unset", "a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := Run(c.args, &out, &errb); code != 2 {
				t.Fatalf("expected exit=2, got %d (stderr=%q)", code, errb.String())
			}
		})
	}
}
