package claude

import (
	"slices"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
)

// TestSandboxSettingsJSONLiteral pins the exact `--settings` payload
// claude is launched with. Operator settings.json layouts depend on
// this exact key/value to merge correctly, so any drift is loud.
func TestSandboxSettingsJSONLiteral(t *testing.T) {
	want := `{"sandbox":{"enabled":true}}`
	if sandboxSettingsJSON != want {
		t.Fatalf("sandboxSettingsJSON = %q, want %q", sandboxSettingsJSON, want)
	}
}

// TestExecuteArgsIncludesAddDirsBeforeSettings pins the variadic-flag
// safety: each AddDirs entry becomes a `--add-dir <dir>` pair, the
// pairs sit before `--settings` (so the JSON payload isn't eaten as
// another directory), and the positional InitialPrompt — when set —
// lands at the very end after `--append-system-prompt`.
func TestExecuteArgsIncludesAddDirsBeforeSettings(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:     "sid-1",
		NewSession:    true,
		Root:          "/bureaucracy",
		AddDirs:       []string{"/tmp/moe-home", "/tmp/moe-devtmp"},
		Prompt:        "system",
		InitialPrompt: "go",
	})
	// First flag is --session-id (NewSession=true), then --add-dir Root,
	// then each AddDirs pair, then --settings, --append-system-prompt,
	// and the positional prompt last.
	want := []string{
		"--session-id", "sid-1",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/tmp/moe-home",
		"--add-dir", "/tmp/moe-devtmp",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
		"go",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteArgsAddsClonePathForCodeStages pins the cwd-inversion
// shape: when ClonePath is set, it lands as a `--add-dir` right after
// the bureaucracy root so the agent (sitting at cwd = root, the
// bureaucracy session worktree) can reach the source-tree workspace.
func TestExecuteArgsAddsClonePathForCodeStages(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:  "sid-1",
		NewSession: true,
		Root:       "/bureaucracy",
		ClonePath:  "/bureaucracy/.moe/clones/widget/req-1",
		Prompt:     "system",
	})
	want := []string{
		"--session-id", "sid-1",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/bureaucracy/.moe/clones/widget/req-1",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteArgsResumeWhenNotNewSession swaps --session-id for
// --resume on a returning session and omits InitialPrompt when empty.
func TestExecuteArgsResumeWhenNotNewSession(t *testing.T) {
	args := executeArgs(agent.Request{
		SessionID:  "sid-2",
		NewSession: false,
		Root:       "/bureaucracy",
		Prompt:     "system",
	})
	want := []string{
		"--resume", "sid-2",
		"--add-dir", "/bureaucracy",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
	}
	assertArgsEqual(t, args, want)
}

// TestExecuteOneShotArgsIncludesAddDirsBeforeSettings: the -p path
// must place AddDirs entries before --settings / --append-system-prompt
// and the positional UserPrompt — same variadic-flag rule as the
// interactive path. Otherwise claude eats the JSON settings blob or
// the user prompt as a directory.
func TestExecuteOneShotArgsIncludesAddDirsBeforeSettings(t *testing.T) {
	args := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		AddDirs:    []string{"/tmp/moe-home"},
		Prompt:     "system",
		UserPrompt: "user",
	})
	want := []string{
		"-p",
		"--permission-mode", "bypassPermissions",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--add-dir", "/bureaucracy",
		"--add-dir", "/tmp/moe-home",
		"--settings", `{"sandbox":{"enabled":true}}`,
		"--append-system-prompt", "system",
		"user",
	}
	assertArgsEqual(t, args, want)
}

// TestFilteredEnvDropsAPIKeys verifies the OAuth-precedence vars are
// stripped from the inherited environment so the spawned claude falls
// back to its OAuth Max-plan path. Other vars pass through unchanged
// and ExtraEnv is appended last.
func TestFilteredEnvDropsAPIKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-be-dropped")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-should-be-dropped")
	t.Setenv("ANTHROPIC_MODEL", "should-pass-through")
	t.Setenv("MOE_TEST_PASSTHROUGH", "yes")

	got := filteredEnv([]string{"EXTRA=1"})

	for _, kv := range got {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Errorf("ANTHROPIC_API_KEY leaked into env: %q", kv)
		}
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("ANTHROPIC_AUTH_TOKEN leaked into env: %q", kv)
		}
	}
	if !slices.Contains(got, "ANTHROPIC_MODEL=should-pass-through") {
		t.Errorf("ANTHROPIC_MODEL should pass through (non-secret routing var): %v", got)
	}
	if !slices.Contains(got, "MOE_TEST_PASSTHROUGH=yes") {
		t.Errorf("unrelated env var should pass through: %v", got)
	}
	if got[len(got)-1] != "EXTRA=1" {
		t.Errorf("ExtraEnv should be appended last; got tail %q", got[len(got)-1])
	}
}

// TestFilteredEnvEmptyExtra: even with no ExtraEnv, the filter still
// runs so an inherited ANTHROPIC_API_KEY is scrubbed. This is the
// case the old `if len(ExtraEnv) > 0` gate missed.
func TestFilteredEnvEmptyExtra(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-be-dropped")
	for _, kv := range filteredEnv(nil) {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatalf("ANTHROPIC_API_KEY leaked with nil ExtraEnv: %q", kv)
		}
	}
}

// TestPickCwdPrefersSessionCwd is the resume-invariant pin: claude
// buckets transcripts by EncodeCwd(cwd), so cwd has to be the same
// per-document path on every turn. SessionCwd is that stable path;
// Root is only the fallback for run-less callers (rebase_resolve)
// that have no session to resume. A regression that flipped the
// precedence (Root wins when SessionCwd is set) would route code
// stages back through the per-turn worktree UUID and silently break
// `--resume` again.
func TestPickCwdPrefersSessionCwd(t *testing.T) {
	cases := []struct {
		name       string
		sessionCwd string
		root       string
		want       string
	}{
		{name: "sessionCwd set", sessionCwd: "/sess/cwd", root: "/root", want: "/sess/cwd"},
		{name: "empty sessionCwd falls back to root", sessionCwd: "", root: "/root", want: "/root"},
		{name: "both empty stays empty", sessionCwd: "", root: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickCwd(tc.sessionCwd, tc.root); got != tc.want {
				t.Fatalf("pickCwd(%q, %q) = %q, want %q", tc.sessionCwd, tc.root, got, tc.want)
			}
		})
	}
}

// assertArgsEqual reports the entire mismatch when args differ — a
// single index error in a 15-element slice is easier to debug as the
// pair of slices than as a single failing field.
func assertArgsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(args) = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q\ngot:  %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}
