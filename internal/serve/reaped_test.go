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
// actually looks, so it is where a dropped machine turn has to say so —
// which stage died, and the abandoned branch tip the transcript is still
// readable at. Before this the two facts lived in a stderr nobody keeps
// and a branch the reap deleted.
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
		`2026-08-23T23:46:03Z`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q:\n%s", want, body)
		}
	}
}

// The notice is the exception, not the furniture: a run that never lost
// a turn renders no warning at all.
func TestRunPageOmitsTheTombstoneWhenUnreaped(t *testing.T) {
	body := getRunPage(t, reapedServer(t, nil), "/run/alpha/src")
	if strings.Contains(body, `class="reaped"`) {
		t.Errorf("untombstoned run rendered a death notice:\n%s", body)
	}
}

// An unparseable timestamp still renders the notice. The sha is the part
// that has to reach the operator; dropping the whole warning over a bad
// clock field would lose exactly what the note exists to preserve.
func TestRunPageRendersATombstoneWithAnUnreadableTimestamp(t *testing.T) {
	s := reapedServer(t, &run.ReapNote{Doc: "code", At: "not a time", Tip: "deadbeefcafe"})
	body := getRunPage(t, s, "/run/alpha/src")
	if !strings.Contains(body, `<code>deadbee</code>`) {
		t.Errorf("run page dropped the tombstone over a bad timestamp:\n%s", body)
	}
}
