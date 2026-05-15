package codex

import (
	"os"
	"path/filepath"
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
// pair, the pairs land after commonArgs (which already adds `--add-dir
// <root>`) and before `--ask-for-approval`, and on first-turn we don't
// prepend `resume`.
func TestExecuteArgsAppendsAddDirsBeforeApproval(t *testing.T) {
	args, err := executeArgs(agent.Request{
		Root:          "/bureaucracy",
		ClonePath:     "/bureaucracy/clone",
		NewSession:    true,
		AddDirs:       []string{"/tmp/moe-home", "/tmp/moe-devtmp"},
		Prompt:        "system",
		InitialPrompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"--add-dir /bureaucracy",
		"--add-dir /bureaucracy/clone",
		"--add-dir /tmp/moe-home",
		"--add-dir /tmp/moe-devtmp",
		"--ask-for-approval never",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q: %s", want, got)
		}
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
	args, err := executeArgs(agent.Request{
		Root:          "/bureaucracy",
		NewSession:    false,
		SessionID:     "sid-1",
		Prompt:        "system",
		InitialPrompt: "follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 2 || args[0] != "resume" || args[1] != "sid-1" {
		t.Fatalf("expected leading `resume sid-1`, got %v", args)
	}
	if args[len(args)-1] != "follow-up" {
		t.Errorf("InitialPrompt should be the last positional arg, got %q", args[len(args)-1])
	}
}

// TestExecuteOneShotArgsAppendsAddDirsBeforeUserPrompt: every AddDirs
// entry becomes a `--add-dir <dir>` pair sitting after commonArgs and
// before the trailing positional UserPrompt.
func TestExecuteOneShotArgsAppendsAddDirsBeforeUserPrompt(t *testing.T) {
	args, err := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		AddDirs:    []string{"/tmp/moe-home"},
		Prompt:     "system",
		UserPrompt: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
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
// in ~/.codex/config.toml aborts headless turns at the approval gate.
// The pin lands after the add-dirs and before the trailing UserPrompt.
func TestExecuteOneShotArgsPinsApprovalNever(t *testing.T) {
	args, err := executeOneShotArgs(agent.OneShotRequest{
		Root:       "/bureaucracy",
		Prompt:     "system",
		UserPrompt: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPair(args, "-c", "approval_policy=never") {
		t.Errorf("args missing `-c approval_policy=never` pair: %v", args)
	}
	// Sandbox stays on — pinning approval doesn't lift the sandbox.
	if !containsPair(args, "--sandbox", "workspace-write") {
		t.Errorf("args missing `--sandbox workspace-write` pair: %v", args)
	}
}

// TestExecuteArgsInteractiveUsesApprovalNever: the interactive path
// disables approval prompts with the first-class flag while keeping the
// sandbox boundary. The headless config pin must not leak into it.
func TestExecuteArgsInteractiveUsesApprovalNever(t *testing.T) {
	args, err := executeArgs(agent.Request{
		Root:       "/bureaucracy",
		ClonePath:  "/bureaucracy/clone",
		NewSession: true,
		Prompt:     "system",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if strings.Contains(got, "approval_policy=never") {
		t.Errorf("interactive path should not pin approval_policy=never: %s", got)
	}
	if !strings.Contains(got, "--ask-for-approval never") {
		t.Errorf("interactive path lost --ask-for-approval never: %s", got)
	}
	if !containsPair(args, "--sandbox", "workspace-write") {
		t.Errorf("interactive path lost `--sandbox workspace-write`: %v", args)
	}
	if strings.Contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("interactive path should keep sandbox enabled: %s", got)
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
