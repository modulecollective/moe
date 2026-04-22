package executor

import (
	"encoding/json"
	"os"
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
// keys srt expects (filesystem, network).
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

	var parsed struct {
		Filesystem struct {
			AllowWrite []string `json:"allowWrite"`
			DenyRead   []string `json:"denyRead"`
		} `json:"filesystem"`
		Network struct {
			AllowedDomains []string `json:"allowedDomains"`
		} `json:"network"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("settings is not valid JSON: %v\n%s", err, b)
	}
	if len(parsed.Filesystem.AllowWrite) == 0 {
		t.Fatal("allowWrite is empty")
	}
	if len(parsed.Filesystem.DenyRead) == 0 {
		t.Fatal("denyRead is empty")
	}
	if len(parsed.Network.AllowedDomains) == 0 {
		t.Fatal("allowedDomains is empty")
	}
	// First entry of allowWrite should be the absolute root so srt lets
	// claude write into the clone and the transcript dir.
	if parsed.Filesystem.AllowWrite[0] != absRoot {
		t.Fatalf("allowWrite[0]: got %q want %q", parsed.Filesystem.AllowWrite[0], absRoot)
	}
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
