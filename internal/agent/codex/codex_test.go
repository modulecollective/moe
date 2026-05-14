package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
