package serve

import (
	"errors"
	"strings"
	"testing"
)

// tracesServer wires a read-only run at alpha/src whose GatherRunTraces
// callback returns the given traces.
func tracesServer(t *testing.T, traces RunTraces, gatherErr error) *Server {
	t.Helper()
	root := t.TempDir()
	seedRun(t, root, "alpha", "src", "sdlc")
	return newSafeTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		GatherRunTraces: func(string, string) (RunTraces, error) {
			return traces, gatherErr
		},
	})
}

// TestRunPageRendersTraces: the three sections a run's residue gets on
// its page. A landed follow-up links to the idea run it became and
// badges that run's current status ("did it land, and where is it
// now"); a lore entry links to the promoted file; the twin note renders
// whole with one chip naming the pass that folded it in.
func TestRunPageRendersTraces(t *testing.T) {
	s := tracesServer(t, RunTraces{
		Followups: []RunTrace{
			{Slug: "still-open", Title: "Not harvested yet"},
			{Done: true, Slug: "landed", Title: "Promoted last close",
				Body: "Why: foo reaches into bar.", TargetURL: "/run/alpha/landed", TargetStatus: "closed"},
			{Done: true, Slug: "vanished", Title: "Dropped by hand"},
			{Done: true, Raw: "- [x] never matched the grammar"},
		},
		Lore: []RunTrace{
			{Done: true, Slug: "portable-fact", Title: "A portable fact", TargetURL: "/lore/portable-fact"},
		},
		TwinNote: &TwinNoteTrace{
			Body:       "architecture.md understates the serve seam.",
			Reflected:  true,
			ReflectRun: "reflect-2026-07",
		},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/src")

	for _, want := range []string{
		`<h2>follow-ups</h2>`,
		`href="/run/alpha/landed">landed</a>`,
		`<span class="badge">closed</span>`,
		`Why: foo reaches into bar.`,
		// Unharvested and hand-dropped entries render, unlinked.
		`>still-open</span>`,
		`>vanished</span>`,
		`- [x] never matched the grammar`,
		`<h2>lore</h2>`,
		`href="/lore/portable-fact">portable-fact</a>`,
		`<h2>twin notes</h2>`,
		`folded in by`,
		`href="/run/alpha/reflect-2026-07">reflect-2026-07</a>`,
		`architecture.md understates the serve seam.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q\n%s", want, body)
		}
	}
	// The open entry has no target, so nothing may link it.
	if strings.Contains(body, `href="/run/alpha/still-open"`) {
		t.Errorf("open follow-up must not link:\n%s", body)
	}
}

// TestRunPagePendingTwinNote: a note no reflect pass has sealed past
// says so, and offers no link — there's no pass to point at yet.
func TestRunPagePendingTwinNote(t *testing.T) {
	s := tracesServer(t, RunTraces{
		TwinNote: &TwinNoteTrace{Body: "A fresh observation."},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/src")

	if !strings.Contains(body, "awaiting next reflect pass") {
		t.Errorf("pending note missing its chip\n%s", body)
	}
	if strings.Contains(body, "folded in") {
		t.Errorf("pending note must not claim it was folded in\n%s", body)
	}
}

// TestRunPageTracesDegradeNotFail: no callback wired, and a gather that
// errors, both leave the page as it was — the canvas links and meta
// line are still worth serving. A broken trace file must cost its
// section, not the page.
func TestRunPageTracesDegradeNotFail(t *testing.T) {
	for name, s := range map[string]*Server{
		"no callback": func() *Server {
			root := t.TempDir()
			seedRun(t, root, "alpha", "src", "sdlc")
			return newSafeTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})
		}(),
		"gather errors": tracesServer(t, RunTraces{}, errors.New("followups.md is a directory")),
	} {
		body := getRunPage(t, s, "/run/alpha/src")
		for _, absent := range []string{"<h2>follow-ups</h2>", "<h2>lore</h2>", "<h2>twin notes</h2>"} {
			if strings.Contains(body, absent) {
				t.Errorf("%s: page should carry no %q section\n%s", name, absent, body)
			}
		}
	}
}

// TestRunPageEmptyTracesRenderNoSections: the common case — a run that
// left nothing behind gets no empty-state noise.
func TestRunPageEmptyTracesRenderNoSections(t *testing.T) {
	s := tracesServer(t, RunTraces{}, nil)
	body := getRunPage(t, s, "/run/alpha/src")
	for _, absent := range []string{"<h2>follow-ups</h2>", "<h2>lore</h2>", "<h2>twin notes</h2>"} {
		if strings.Contains(body, absent) {
			t.Errorf("empty traces should not render %q\n%s", absent, body)
		}
	}
}
