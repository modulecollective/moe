package banner

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestStageEntryGolden pins the rendered shape. The bar glyph,
// workflow · stage separator, project + run anchor — anyone tweaking
// the shape lands on this test and re-eyeballs scrollback.
func TestStageEntryGolden(t *testing.T) {
	var buf bytes.Buffer
	StageEntry(&buf, "claude", "sdlc", "design", "moe", "nice-banners")
	got := buf.String()
	want := "▓▒░ MINISTRY OF EVERYTHING ░▒▓  [claude] sdlc · design  ·  moe nice-banners\n"
	if got != want {
		t.Fatalf("StageEntry =\n%q\nwant\n%q", got, want)
	}
}

// TestStageExitCompleteAndNoOp covers both branches of the committed
// flag — a clean commit lands `complete`; the "no document changes"
// no-op lands `no-op`. Flipped gradient marks frame the line.
func TestStageExitCompleteAndNoOp(t *testing.T) {
	var buf bytes.Buffer
	StageExit(&buf, "sdlc", "design", "moe", "nice-banners", true)
	if got, want := buf.String(), "░▒▓ design complete  ·  moe nice-banners ▓▒░\n"; got != want {
		t.Fatalf("StageExit(committed=true) =\n%q\nwant\n%q", got, want)
	}
	buf.Reset()
	StageExit(&buf, "sdlc", "design", "moe", "nice-banners", false)
	if got, want := buf.String(), "░▒▓ design no-op  ·  moe nice-banners ▓▒░\n"; got != want {
		t.Fatalf("StageExit(committed=false) =\n%q\nwant\n%q", got, want)
	}
}

func TestDashGolden(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 5, 14, 0, 13, 0, 0, time.UTC)
	Dash(&buf, now)
	if got, want := buf.String(), "▓▒░ MINISTRY OF EVERYTHING ░▒▓  dash  2026-05-14  00:13\n"; got != want {
		t.Fatalf("Dash =\n%q\nwant\n%q", got, want)
	}
}

// TestHookSectionPluralisation: the script-count noun matches the
// number, so the section header doesn't read "1 scripts".
func TestHookSectionPluralisation(t *testing.T) {
	cases := []struct {
		count int
		want  string
	}{
		{1, "▸ dev-env setup: 1 script in projects/moe/hooks/dev-env.d\n"},
		{3, "▸ dev-env setup: 3 scripts in projects/moe/hooks/dev-env.d\n"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		HookSection(&buf, "dev-env setup", tc.count, "projects/moe/hooks/dev-env.d")
		if got := buf.String(); got != tc.want {
			t.Errorf("count=%d: got %q, want %q", tc.count, got, tc.want)
		}
	}
}

func TestHookCacheHitGolden(t *testing.T) {
	var buf bytes.Buffer
	HookCacheHit(&buf, "dev-env", ".moe/dev-env.env")
	if got, want := buf.String(), "▸ dev-env cached (.moe/dev-env.env)\n"; got != want {
		t.Fatalf("HookCacheHit =\n%q\nwant\n%q", got, want)
	}
}

// TestHookStartHookDone pins the per-script header/footer shape:
// `→ <script>` opens, `← <script> (<seconds>s)` closes. One decimal
// place of seconds is enough — script timing isn't a profiler.
func TestHookStartHookDone(t *testing.T) {
	var buf bytes.Buffer
	HookStart(&buf, "10-tmpdir.sh")
	HookDone(&buf, "10-tmpdir.sh", 420*time.Millisecond)
	want := "→ 10-tmpdir.sh\n← 10-tmpdir.sh (0.4s)\n"
	if got := buf.String(); got != want {
		t.Fatalf("HookStart/HookDone =\n%q\nwant\n%q", got, want)
	}
}

// TestIndentStderrPassthroughNonTTY: a bytes.Buffer isn't a TTY, so the
// returned writer passes bytes through verbatim — test assertions on
// raw script output keep working.
func TestIndentStderrPassthroughNonTTY(t *testing.T) {
	var buf bytes.Buffer
	w := IndentStderr(&buf)
	in := "created postgres db myapp_dev_foo\nport 8080 reserved\n"
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != in {
		t.Fatalf("passthrough mode mutated output:\ngot  %q\nwant %q", got, in)
	}
}

// TestIndenterPrefixesLines exercises the indent transform directly
// (bypassing the IsTTY gate, which the non-TTY test already covers).
// Mid-line writes still get one indent per line, and a trailing
// newline doesn't conjure a phantom indent.
func TestIndenterPrefixesLines(t *testing.T) {
	var buf bytes.Buffer
	i := &indenter{w: &buf, atStart: true}
	if _, err := i.Write([]byte("alpha\nbeta")); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Write([]byte(" suffix\ngamma\n")); err != nil {
		t.Fatal(err)
	}
	want := "  alpha\n  beta suffix\n  gamma\n"
	if got := buf.String(); got != want {
		t.Fatalf("indenter =\n%q\nwant\n%q", got, want)
	}
	if strings.HasSuffix(buf.String(), "\n  ") {
		t.Fatalf("trailing newline conjured a phantom indent: %q", buf.String())
	}
}

// TestIndenterEmptyWrite: a zero-length write is a clean no-op — no
// indent fired, no error.
func TestIndenterEmptyWrite(t *testing.T) {
	var buf bytes.Buffer
	i := &indenter{w: &buf, atStart: true}
	n, err := i.Write(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Write(nil) = %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("Write(nil) wrote %q", buf.String())
	}
}
