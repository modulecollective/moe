package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestResolveSRTDecisionTable covers the five rows of the design's
// MOE_SANDBOX decision table. A fake `srt` binary on a controlled PATH
// lets us flip the "srt installed" axis without touching the host.
func TestResolveSRTDecisionTable(t *testing.T) {
	withSRT := t.TempDir()
	fake := filepath.Join(withSRT, "srt")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()

	cases := []struct {
		name      string
		envVal    string
		path      string // value for PATH
		wantWrap  bool
		wantError bool
	}{
		{"unset-installed", "", withSRT, true, false},
		{"unset-missing", "", empty, false, false},
		{"off-installed", "off", withSRT, false, false},
		{"off-missing", "off", empty, false, false},
		{"on-installed", "on", withSRT, true, false},
		{"one-installed", "1", withSRT, true, false},
		{"on-missing", "on", empty, false, true},
		{"one-missing", "1", empty, false, true},
		{"garbage", "yes", withSRT, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			root := t.TempDir()
			srt, cfg, err := resolveSRT(root, tc.envVal)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got srt=%q cfg=%q", srt, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantWrap {
				if srt == "" {
					t.Fatalf("expected wrap, got empty srt")
				}
				if cfg == "" {
					t.Fatalf("expected wrap, got empty cfg")
				}
			} else {
				if srt != "" || cfg != "" {
					t.Fatalf("expected direct, got srt=%q cfg=%q", srt, cfg)
				}
			}
		})
	}
}

// TestEnsureSRTSettingsGeneratesValidJSON confirms the lazy write
// produces a well-formed settings file with <root> substituted for the
// absolute path. The resulting JSON must parse and expose the minimum
// keys srt expects (filesystem.{allowWrite,denyWrite,denyRead},
// network.{allowedDomains,deniedDomains}) — srt's schema validator
// rejects configs missing denyWrite or deniedDomains.
func TestEnsureSRTSettingsGeneratesValidJSON(t *testing.T) {
	root := t.TempDir()
	p, err := ensureSRTSettings(root)
	if err != nil {
		t.Fatalf("ensureSRTSettings: %v", err)
	}
	if want := filepath.Join(root, ".moe", "srt-settings.json"); p != want {
		t.Fatalf("path: got %q want %q", p, want)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), absRoot) {
		t.Fatalf("settings missing abs root %q: %s", absRoot, b)
	}
	if strings.Contains(string(b), "<root>") {
		t.Fatalf("settings still contains literal <root>: %s", b)
	}

	// Use a map so "key present but empty" is distinguishable from "key
	// absent" — srt's validator treats them differently.
	var parsed map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("settings is not valid JSON: %v\n%s", err, b)
	}
	for _, key := range []string{"allowWrite", "denyWrite", "denyRead"} {
		if _, ok := parsed["filesystem"][key]; !ok {
			t.Fatalf("filesystem.%s key missing: %s", key, b)
		}
	}
	for _, key := range []string{"allowedDomains", "deniedDomains"} {
		if _, ok := parsed["network"][key]; !ok {
			t.Fatalf("network.%s key missing: %s", key, b)
		}
	}
	// First entry of allowWrite should be the absolute root so srt lets
	// claude write into the clone and the transcript dir.
	var allowWrite []string
	if err := json.Unmarshal(parsed["filesystem"]["allowWrite"], &allowWrite); err != nil {
		t.Fatal(err)
	}
	if len(allowWrite) == 0 || allowWrite[0] != absRoot {
		t.Fatalf("allowWrite[0]: got %v want %q", allowWrite, absRoot)
	}
}

// TestShellJoinSurvivesSrtArgMangling covers the arg forms that broke
// moe against srt 1.0.0: multi-line strings, shell metacharacters,
// embedded single quotes, and empty strings. srt's default mode runs
// commandArgs.join(' ') through `sh -c` with zero escaping, so shellJoin
// has to produce something sh can parse back into the original argv.
func TestShellJoinSurvivesSrtArgMangling(t *testing.T) {
	cases := [][]string{
		{"echo", "hello"},
		{"claude", "--append-system-prompt", "line1\nline2\nline3"},
		{"sh", "-c", "echo $HOME && ls /"},
		{"echo", "it's a \"quoted\" string"},
		{"echo", ""},
		{"echo", "has space", "simple"},
	}
	for _, argv := range cases {
		cmd := shellJoin(argv)
		// Re-parse by invoking sh -c with an argv-printer and comparing.
		parsed, err := roundtripViaSh(cmd, len(argv))
		if err != nil {
			t.Fatalf("roundtrip %v: %v (cmd=%q)", argv, err, cmd)
		}
		if len(parsed) != len(argv) {
			t.Fatalf("argc: got %d want %d (cmd=%q parsed=%v)", len(parsed), len(argv), cmd, parsed)
		}
		for i := range argv {
			if parsed[i] != argv[i] {
				t.Fatalf("argv[%d]: got %q want %q (cmd=%q)", i, parsed[i], argv[i], cmd)
			}
		}
	}
}

// roundtripViaSh runs `sh -c 'printf %s\\0 "$@"' _ <cmd>` and splits on
// NULs. The printf trick preserves every arg verbatim, including
// newlines, so we can assert shellJoin produced a sh-parseable string.
func roundtripViaSh(cmd string, _ int) ([]string, error) {
	wrapped := `set -- ` + cmd + `; for a; do printf '%s\0' "$a"; done`
	out, err := exec.Command("sh", "-c", wrapped).Output()
	if err != nil {
		return nil, err
	}
	s := string(out)
	if s == "" {
		return nil, nil
	}
	// Trailing NUL means the last field is empty — strip it.
	s = strings.TrimSuffix(s, "\x00")
	return strings.Split(s, "\x00"), nil
}

// TestEnsureSRTSettingsIdempotent ensures a second call is a no-op —
// the operator can hand-edit the generated file without having it
// silently overwritten on the next sandboxed run.
func TestEnsureSRTSettingsIdempotent(t *testing.T) {
	root := t.TempDir()
	p, err := ensureSRTSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	custom := []byte(`{"custom":"edit"}`)
	if err := os.WriteFile(p, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := ensureSRTSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p {
		t.Fatalf("path changed: %q → %q", p, p2)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(custom) {
		t.Fatalf("operator edit overwritten: %s", got)
	}
}

// TestEnsureSRTSettingsCreatesMoeDir confirms we lazily create the
// .moe parent when it is missing — on a freshly initialized bureaucracy
// that has never clone-sandboxed, the dir won't exist yet.
func TestEnsureSRTSettingsCreatesMoeDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only path expectations")
	}
	root := t.TempDir()
	if _, err := os.Stat(filepath.Join(root, ".moe")); err == nil {
		t.Fatal("precondition: .moe should not exist yet")
	}
	if _, err := ensureSRTSettings(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".moe", "srt-settings.json")); err != nil {
		t.Fatalf(".moe/srt-settings.json missing: %v", err)
	}
}
