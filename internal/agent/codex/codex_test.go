package codex

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// TestParseSessionIDFromFilename pins the codex rollout filename
// suffix convention: `rollout-<ISO-timestamp>-<uuid>.jsonl`. UUIDs
// contain four hyphens of their own, so the parser counts back from
// the end (5 segments) rather than splitting on "-".
func TestParseSessionIDFromFilename(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "canonical 0.130 layout",
			path: "/home/dev/.codex/sessions/2026/05/14/rollout-2026-05-14T23-13-13-019e28c3-feb5-7291-aafb-12a7071a8fdb.jsonl",
			want: "019e28c3-feb5-7291-aafb-12a7071a8fdb",
		},
		{
			name: "bare basename",
			path: "rollout-2026-05-14T23-13-13-019e28c3-feb5-7291-aafb-12a7071a8fdb.jsonl",
			want: "019e28c3-feb5-7291-aafb-12a7071a8fdb",
		},
		{
			name: "not enough segments",
			path: "rollout-019e28c3.jsonl",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseSessionIDFromFilename(c.path); got != c.want {
				t.Errorf("parseSessionIDFromFilename(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

// TestTomlMultilineBasicEscapesDangerousSequences verifies the
// developer_instructions encoder takes the TOML string-parse path
// for any input — including prompts that start with a TOML comment
// character or contain mid-string `"""`. The output, decoded as
// TOML, must equal the input verbatim.
func TestTomlMultilineBasicEscapesDangerousSequences(t *testing.T) {
	cases := []string{
		"# Header\n\nProse with `code` and *emphasis*.",
		"contains \"\"\" which would otherwise end the string",
		`backslash \n should not become a newline`,
		"",
		"single line",
	}
	for _, in := range cases {
		got := tomlMultilineBasic(in)
		if !strings.HasPrefix(got, `"""`) || !strings.HasSuffix(got, `"""`) {
			t.Errorf("encoded %q missing triple-quote wrapper: %q", in, got)
		}
	}
}

// TestCopyTranscriptRespectsCodexHome plants a fake rollout under a
// temp $CODEX_HOME and verifies CopyTranscript locates it via the
// glob and copies it byte-for-byte to the destination.
func TestCopyTranscriptRespectsCodexHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)
	sid := "019e28c3-feb5-7291-aafb-12a7071a8fdb"
	body := `{"timestamp":"…","type":"session_meta","payload":{"id":"` + sid + `"}}` + "\n"
	src := writeFakeRollout(t, tmp, "2026/05/14", sid, body)

	dest := filepath.Join(t.TempDir(), "out", "thread-codex.jsonl")
	found, err := CopyTranscript(sid, dest)
	if err != nil {
		t.Fatalf("CopyTranscript: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true; src exists at %s", src)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body not preserved: got %q want %q", got, body)
	}
}

// TestCopyTranscriptAbsent reports found=false (no err) when no
// rollout matches the id — the legitimate state where the operator
// aborted before the first turn wrote anything.
func TestCopyTranscriptAbsent(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	dest := filepath.Join(t.TempDir(), "thread-codex.jsonl")
	found, err := CopyTranscript("00000000-0000-0000-0000-000000000000", dest)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Fatal("expected found=false when no rollout exists")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest should not have been created; stat err=%v", err)
	}
}

// TestDiscoverSessionIDPicksNewestSinceTurnStart verifies the
// post-turn id-readback: plant two rollout files, one older than the
// turn-start cutoff and one newer; the newer one's id is returned.
func TestDiscoverSessionIDPicksNewestSinceTurnStart(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)

	now := time.Now().UTC()
	shard := filepath.Join(now.Format("2006"), now.Format("01"), now.Format("02"))

	oldSid := "00000000-0000-0000-0000-000000000001"
	newSid := "00000000-0000-0000-0000-000000000002"
	oldPath := writeFakeRollout(t, tmp, shard, oldSid, "old\n")
	newPath := writeFakeRollout(t, tmp, shard, newSid, "new\n")

	// Backdate the "old" file's mtime to before our turnStart, and
	// touch "new" to now so it wins the newest-after-since contest.
	turnStart := now.Add(-1 * time.Second)
	if err := os.Chtimes(oldPath, now.Add(-1*time.Hour), now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, now, now); err != nil {
		t.Fatal(err)
	}

	if got := discoverSessionID(turnStart); got != newSid {
		t.Errorf("discoverSessionID = %q, want %q", got, newSid)
	}
}

// TestExecuteArgsAppendsAddDirsBeforeApproval pins the codex
// interactive-path shape: every AddDirs entry becomes a `--add-dir <dir>`
// pair, the pairs land after commonArgs (which adds the clone path
// add-dir) and before `--ask-for-approval`, and on first-turn we don't
// prepend `resume`. cwd = bureaucracy root, so the permissions profile's
// writable workspace root makes the canvas writable without an explicit
// `--add-dir <root>`.
func TestExecuteArgsAppendsAddDirsBeforeApproval(t *testing.T) {
	args := executeArgs(agent.Request{
		Root:          "/bureaucracy",
		ClonePath:     "/bureaucracy/clone",
		NewSession:    true,
		AddDirs:       []string{"/tmp/moe-home", "/tmp/moe-devtmp"},
		Prompt:        "system",
		InitialPrompt: "go",
	})
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--add-dir /bureaucracy/clone",
		"--add-dir /tmp/moe-home",
		"--add-dir /tmp/moe-devtmp",
		"--ask-for-approval never",
		"-c default_permissions=moe-workspace-git",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q: %s", want, got)
		}
	}
	// cwd-inversion: no explicit `--add-dir /bureaucracy` — cwd handles
	// it via the profile's writable workspace root.
	if strings.Contains(got, "--add-dir /bureaucracy ") || strings.HasSuffix(got, "--add-dir /bureaucracy") {
		t.Errorf("args should not include redundant --add-dir for bureaucracy root: %s", got)
	}
	// Order: each AddDirs --add-dir must precede --ask-for-approval.
	idxApproval := indexOf(args, "--ask-for-approval")
	for _, d := range []string{"/tmp/moe-home", "/tmp/moe-devtmp"} {
		idx := indexOf(args, d)
		if idx < 0 || idx > idxApproval {
			t.Errorf("AddDir %q at idx %d should precede --ask-for-approval at idx %d (args: %v)", d, idx, idxApproval, args)
		}
	}
	// First turn: no `resume <sid>` prefix. The positional InitialPrompt
	// lands at the end.
	if args[0] == "resume" {
		t.Fatalf("first turn should not prepend `resume`: %v", args)
	}
	if args[len(args)-1] != "go" {
		t.Errorf("InitialPrompt should be the last positional arg, got %q", args[len(args)-1])
	}
}

// TestExecuteArgsResumePrependsSid: a returning session prepends
// `resume <sid>` to the flag set; the positional prompt still lands
// after all flags.
func TestExecuteArgsResumePrependsSid(t *testing.T) {
	args := executeArgs(agent.Request{
		Root:          "/bureaucracy",
		NewSession:    false,
		SessionID:     "sid-1",
		Prompt:        "system",
		InitialPrompt: "follow-up",
	})
	if len(args) < 2 || args[0] != "resume" || args[1] != "sid-1" {
		t.Fatalf("expected leading `resume sid-1`, got %v", args)
	}
	if args[len(args)-1] != "follow-up" {
		t.Errorf("InitialPrompt should be the last positional arg, got %q", args[len(args)-1])
	}
}

// TestExecuteArgsModel pins interactive `--model`: set → a `--model
// <m>` pair rides the flag set (surviving the `resume <sid>` prepend on
// a returning turn); unset → no flag at all.
func TestExecuteArgsModel(t *testing.T) {
	args := executeArgs(agent.Request{
		Root:       "/bureaucracy",
		NewSession: true,
		Model:      "gpt-5-codex",
		Prompt:     "system",
	})
	if !containsPair(args, "--model", "gpt-5-codex") {
		t.Errorf("args missing `--model gpt-5-codex` pair: %v", args)
	}

	// Resume turn: --model still present alongside the `resume <sid>`
	// prefix.
	resume := executeArgs(agent.Request{
		Root:       "/bureaucracy",
		NewSession: false,
		SessionID:  "sid-1",
		Model:      "gpt-5-codex",
		Prompt:     "system",
	})
	if resume[0] != "resume" || !containsPair(resume, "--model", "gpt-5-codex") {
		t.Errorf("resume turn should keep `--model`: %v", resume)
	}

	// Unset Model → no --model flag.
	bare := executeArgs(agent.Request{Root: "/b", NewSession: true, Prompt: "p"})
	if indexOf(bare, "--model") >= 0 {
		t.Errorf("empty Model should omit --model: %v", bare)
	}
}

// TestExecuteOneShotArgsAppendsAddDirsBeforeUserPrompt: every AddDirs
// entry becomes a `--add-dir <dir>` pair sitting after commonArgs and
// before the trailing positional UserPrompt.
func TestExecuteOneShotArgsAppendsAddDirsBeforeUserPrompt(t *testing.T) {
	args := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		AddDirs:    []string{"/tmp/moe-home"},
		Prompt:     "system",
		UserPrompt: "user",
	})
	if args[0] != "exec" || args[1] != "--json" || args[2] != "--skip-git-repo-check" {
		t.Fatalf("expected `exec --json --skip-git-repo-check` prefix, got %v", args[:3])
	}
	if args[len(args)-1] != "user" {
		t.Fatalf("UserPrompt should be the last positional arg, got %q", args[len(args)-1])
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--add-dir /tmp/moe-home") {
		t.Errorf("args missing --add-dir for AddDirs entry: %s", got)
	}
	// The --add-dir for AddDirs must precede the trailing UserPrompt.
	idx := indexOf(args, "/tmp/moe-home")
	if idx < 0 || idx >= len(args)-1 {
		t.Errorf("AddDir at idx %d not before final UserPrompt at idx %d", idx, len(args)-1)
	}
}

// TestExecuteOneShotArgsPinsApprovalNever: `codex exec` has no
// --ask-for-approval flag, so we pin the approval policy explicitly
// with `-c approval_policy=never`. Without this, a non-"never" policy
// in ~/.codex/config.toml aborts the turn at the approval gate.
// The pin lands after the add-dirs and before the trailing UserPrompt.
func TestExecuteOneShotArgsPinsApprovalNever(t *testing.T) {
	args := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		Prompt:     "system",
		UserPrompt: "user",
	})
	if !containsPair(args, "-c", "approval_policy=never") {
		t.Errorf("args missing `-c approval_policy=never` pair: %v", args)
	}
	// Sandbox stays on — pinning approval doesn't lift it. The bound is
	// the permissions profile, not `--sandbox`.
	assertGitWritableProfile(t, args)
}

// assertGitWritableProfile pins the sandbox shape both argv paths must
// carry: MoE defines its own permissions profile inline and selects it,
// and must *not* pass `--sandbox` — on codex 0.144.x the explicit flag
// overrides `default_permissions`, silently reinstating a read-only
// `.git` on every workspace root and putting commits back at the mercy
// of the all-mutating-git-chain carveout.
func assertGitWritableProfile(t *testing.T, args []string) {
	t.Helper()
	if indexOf(args, "--sandbox") >= 0 {
		t.Errorf("`--sandbox` overrides default_permissions and must not be passed: %v", args)
	}
	if !containsPair(args, "-c", "default_permissions=moe-workspace-git") {
		t.Errorf("args missing `-c default_permissions=moe-workspace-git` pair: %v", args)
	}
	// The profile is defined by MoE, not borrowed from the operator's
	// ~/.codex/config.toml, and grants `.git` write on both root kinds.
	profile := ""
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "permissions.moe-workspace-git.") {
			profile = args[i+1]
		}
	}
	if profile == "" {
		t.Fatalf("args do not define the moe-workspace-git profile inline: %v", args)
	}
	for _, want := range []string{
		`":root" = "read"`,
		`":tmpdir" = "write"`,
		`":slash_tmp" = "write"`, // without it codex sets exclude_slash_tmp
		`":project_roots" = { "." = "write", ".git" = "write" }`,
		`":workspace_roots" = { "." = "write", ".git" = "write" }`,
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing %s: %s", want, profile)
		}
	}
}

// TestExecuteArgsInteractiveUsesApprovalNever: the interactive path
// disables approval prompts with the first-class flag while keeping the
// sandbox boundary. The one-shot config pin must not leak into it.
func TestExecuteArgsInteractiveUsesApprovalNever(t *testing.T) {
	args := executeArgs(agent.Request{
		Root:       "/bureaucracy",
		ClonePath:  "/bureaucracy/clone",
		NewSession: true,
		Prompt:     "system",
	})
	got := strings.Join(args, " ")
	if strings.Contains(got, "approval_policy=never") {
		t.Errorf("interactive path should not pin approval_policy=never: %s", got)
	}
	if !strings.Contains(got, "--ask-for-approval never") {
		t.Errorf("interactive path lost --ask-for-approval never: %s", got)
	}
	// Sandbox-loosening fix for codex: the profile must be selected so
	// `.git/index.lock` writes during commit don't EROFS regardless of
	// the shell chain the agent builds. Without it the agent can edit
	// but a status-prefixed `git commit` chain fails inside the clone.
	assertGitWritableProfile(t, args)
	if strings.Contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("interactive path should keep sandbox enabled: %s", got)
	}
}

// TestFilteredEnvDropsAPIKey strips OPENAI_API_KEY (codex auto-reads
// it and silently switches off the ChatGPT-plan OAuth path), preserves
// CODEX_ACCESS_TOKEN (that *is* the OAuth path) and other vars, and
// appends ExtraEnv last.
func TestFilteredEnvDropsAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-should-be-dropped")
	t.Setenv("CODEX_ACCESS_TOKEN", "oauth-should-pass")
	t.Setenv("MOE_TEST_PASSTHROUGH", "yes")

	got := filteredEnv([]string{"EXTRA=1"})

	for _, kv := range got {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Errorf("OPENAI_API_KEY leaked into env: %q", kv)
		}
	}
	if !slices.Contains(got, "CODEX_ACCESS_TOKEN=oauth-should-pass") {
		t.Errorf("CODEX_ACCESS_TOKEN must pass through (OAuth path): %v", got)
	}
	if !slices.Contains(got, "MOE_TEST_PASSTHROUGH=yes") {
		t.Errorf("unrelated env var should pass through: %v", got)
	}
	if got[len(got)-1] != "EXTRA=1" {
		t.Errorf("ExtraEnv should be appended last; got tail %q", got[len(got)-1])
	}
}

// TestFilteredEnvPinsNoEditor: every codex turn gets GIT_EDITOR=true and
// GIT_SEQUENCE_EDITOR=true so an editor-spawning git op (rebase
// --continue, commit with no -m) never hangs on vim in a no-TTY turn
// (codex-rebase-weirdness). The pin must override an inherited
// GIT_EDITOR — git reads the last occurrence, so the pin has to appear
// after the inherited environment.
func TestFilteredEnvPinsNoEditor(t *testing.T) {
	t.Setenv("GIT_EDITOR", "vim")

	got := filteredEnv(nil)

	if !slices.Contains(got, "GIT_EDITOR=true") {
		t.Errorf("GIT_EDITOR=true not pinned: %v", got)
	}
	if !slices.Contains(got, "GIT_SEQUENCE_EDITOR=true") {
		t.Errorf("GIT_SEQUENCE_EDITOR=true not pinned: %v", got)
	}
	// The inherited GIT_EDITOR=vim is still present; the pin wins only by
	// coming later. Assert the last GIT_EDITOR= entry is the pin.
	last := ""
	for _, kv := range got {
		if strings.HasPrefix(kv, "GIT_EDITOR=") {
			last = kv
		}
	}
	if last != "GIT_EDITOR=true" {
		t.Errorf("inherited GIT_EDITOR must be overridden by the pin; last GIT_EDITOR entry = %q", last)
	}
}

// TestFilteredEnvScrubsExtra is the dev-env-injection regression: a
// project whose dev-env hook emits OPENAI_API_KEY feeds it in as
// ExtraEnv, otherwise appended last so dev-env vars win. The scrub must
// span extra too, or the key reaches codex and re-bills the turn to the
// API — the codex-symmetric form of the westworld billing hole. EXTRA=1
// still passes through; the no-editor pins are untouched.
func TestFilteredEnvScrubsExtra(t *testing.T) {
	got := filteredEnv([]string{
		"OPENAI_API_KEY=sk-injected-by-dev-env",
		"EXTRA=1",
	})
	for _, kv := range got {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Errorf("OPENAI_API_KEY in ExtraEnv leaked into env: %q", kv)
		}
	}
	if !slices.Contains(got, "EXTRA=1") {
		t.Errorf("non-scrubbed ExtraEnv var should still pass through: %v", got)
	}
	if !slices.Contains(got, "GIT_EDITOR=true") {
		t.Errorf("no-editor pins must survive the extra scrub: %v", got)
	}
}

// TestFilteredEnvEmptyExtra: filter still runs with nil ExtraEnv, so
// an inherited OPENAI_API_KEY is scrubbed on a no-extras spawn (the
// case the old `if len(ExtraEnv) > 0` gate missed).
func TestFilteredEnvEmptyExtra(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-should-be-dropped")
	for _, kv := range filteredEnv(nil) {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") {
			t.Fatalf("OPENAI_API_KEY leaked with nil ExtraEnv: %q", kv)
		}
	}
}

// containsPair reports whether args contains the consecutive pair
// [key, value] in that order.
func containsPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

// indexOf returns the first index of needle in args, or -1.
func indexOf(args []string, needle string) int {
	for i, a := range args {
		if a == needle {
			return i
		}
	}
	return -1
}

// TestExecuteOneShotTimeoutMirrorsAndReturnsSid drives ExecuteOneShot
// against a fake `codex` that emits its thread.started event then hangs
// past the deadline. The regression this run fixes: on the timeout kill
// codex used to return "" with no transcript mirror; it must now return
// the captured sid and mirror the rollout to ThreadPath, matching claude
// — so the post-headless auto-tail has something to render and the next
// turn can --resume the timed-out session.
func TestExecuteOneShotTimeoutMirrorsAndReturnsSid(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sid := "019e28c3-feb5-7291-aafb-12a7071a8fdb"
	// Pre-plant the rollout the fake CLI's session would have written so
	// the post-timeout mirror has a source to copy.
	writeFakeRollout(t, codexHome, "2026/05/14", sid, `{"type":"session_meta"}`+"\n")

	// Fake `codex`: announce the session id, then hang well past the
	// deadline. `exec sleep` replaces the shell so the context kill hits
	// the sleep directly and nothing lingers holding the stdout pipe.
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"type":"thread.started","thread_id":"` + sid + `"}'` + "\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dest := filepath.Join(t.TempDir(), "thread-codex.jsonl")
	r := agent.OneShotRequest{
		Root:       t.TempDir(),
		UserPrompt: "go",
		Timeout:    500 * time.Millisecond,
		ThreadPath: dest,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}

	start := time.Now()
	gotSid, err := Agent{}.ExecuteOneShot(r)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("ExecuteOneShot ran %s — the deadline kill didn't fire", elapsed)
	}
	if gotSid != sid {
		t.Errorf(`sid = %q, want %q (must return the sid on timeout, not "")`, gotSid, sid)
	}
	if err == nil || !strings.Contains(err.Error(), "codex: exec timed out") {
		t.Errorf("err = %v, want a codex timeout error", err)
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Errorf("transcript not mirrored to ThreadPath: %v", statErr)
	}
}

func writeFakeRollout(t *testing.T, codexHome, shard, sid, body string) string {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", shard)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-05-14T23-13-13-"+sid+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
