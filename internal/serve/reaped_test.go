package serve

import (
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/run"
)

// reapedServer wires a read-only run at alpha/src carrying the given
// tombstone (nil for none).
func reapedServer(t *testing.T, note *run.ReapNote) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "src", "sdlc")
	md, err := run.Load(root, "alpha", "src")
	if err != nil {
		t.Fatal(err)
	}
	md.Reaped = note
	if err := run.Save(root, md); err != nil {
		t.Fatal(err)
	}
	return newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})
}

// TestRunPageRendersTheReapTombstone: the run page is where the operator
// actually looks: name the unfinished stage, cleanup time and evidence
// command even when the run has no archived diagnostics.
func TestRunPageRendersTheReapTombstone(t *testing.T) {
	s := reapedServer(t, &run.ReapNote{
		Doc: "design",
		At:  "2026-08-23T23:46:03Z",
		Tip: "81c2d5a719d6f5a1c0a2b3c4d5e6f708192a3b4c",
	})
	body := getRunPage(t, s, "/run/alpha/src")

	for _, want := range []string{
		`class="reaped"`,
		`<strong>design</strong>`,
		// Short sha, the form the operator pastes into `git show`.
		`<code>81c2d5a</code>`,
		`the last machine turn at <strong>design</strong> was unfinished;`,
		`the heartbeat abandoned its session branch <span title="2026-08-23T23:46:03Z">`,
		`<code>moe sdlc log alpha/src design</code>`,
		`recorded outcome, when available`,
		`Reaping alone does not establish the cause`,
		`Session evidence`,
		`until Git GC prunes it`,
		`an operator touch on`,
		`the thread releases it, and opening any stage here clears it outright`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"died", "crash", "timeout", "transcript survives"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("run page claims %q:\n%s", unwanted, body)
		}
	}
}

// The notice is the exception, not the furniture: a run that never lost
// a turn renders no warning at all.
func TestRunPageOmitsTheTombstoneWhenUnreaped(t *testing.T) {
	body := getRunPage(t, reapedServer(t, nil), "/run/alpha/src")
	if strings.Contains(body, `class="reaped"`) {
		t.Errorf("untombstoned run rendered an unfinished-turn notice:\n%s", body)
	}
}

// An unparseable timestamp still renders the notice. The sha is the part
// that has to reach the operator; dropping the whole warning over a bad
// clock field would lose exactly what the note exists to preserve.
func TestRunPageRendersATombstoneWithAnUnreadableTimestamp(t *testing.T) {
	s := reapedServer(t, &run.ReapNote{Doc: "code", At: "not a time", Tip: "deadbeefcafe"})
	body := getRunPage(t, s, "/run/alpha/src")
	for _, want := range []string{
		`<code>deadbee</code>`,
		`<strong>code</strong> was unfinished;`,
		`<code>moe sdlc log alpha/src code</code>`,
		`abandoned its session branch at not a time`,
		`recorded outcome, when available`,
		`the thread releases it, and opening any stage here clears it outright`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q with a bad timestamp:\n%s", want, body)
		}
	}
}
