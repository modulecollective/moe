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
	return newTestServer(t, Options{
		Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo",
		GatherRunTraces: func(string, string) (RunTraces, error) {
			return traces, gatherErr
		},
	})
}

// TestRunPageRendersTraces: the three sections a run's residue gets on
// its page. All three read the same way now: a landed follow-up or twin
// note links to the idea run it became and badges that run's current
// status ("did it land, and where is it now"); a lore entry links to the
// promoted file. Bodies sit behind a disclosure in every section.
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
			{Done: true, Slug: "portable-fact", Title: "A portable fact",
				Body: "Why: it bites every project the same way.", TargetURL: "/lore/portable-fact"},
		},
		Twin: []RunTrace{
			{Done: true, Slug: "arch-serve-seam", Title: "architecture.md understates the serve seam",
				Body: "The component list predates the split.", TargetURL: "/run/alpha/arch-serve-seam", TargetStatus: "in_progress"},
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
		`href="/run/alpha/arch-serve-seam">arch-serve-seam</a>`,
		`<span class="badge">in_progress</span>`,
		`The component list predates the split.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q\n%s", want, body)
		}
	}
	// The open entry has no target, so nothing may link it.
	if strings.Contains(body, `href="/run/alpha/still-open"`) {
		t.Errorf("open follow-up must not link:\n%s", body)
	}
	// All three trace disclosures start closed — the reading pattern is
	// scan the section, expand the one you want.
	if strings.Contains(body, "<details class=\"trace-body\" open") {
		t.Errorf("trace bodies must default collapsed:\n%s", body)
	}
	if strings.Count(body, `<details class="trace-body">`) != 3 {
		t.Errorf("want a disclosure on each of the follow-up, lore, and twin bodies:\n%s", body)
	}
}

// TestRunPageUnharvestedTwinNote: a note still waiting on a harvest
// renders unchecked and unlinked — there is no idea to point at yet.
func TestRunPageUnharvestedTwinNote(t *testing.T) {
	s := tracesServer(t, RunTraces{
		Twin: []RunTrace{{Slug: "fresh-observation", Title: "A fresh observation"}},
	}, nil)
	body := getRunPage(t, s, "/run/alpha/src")

	if !strings.Contains(body, `>fresh-observation</span>`) {
		t.Errorf("unharvested note missing its entry\n%s", body)
	}
	if strings.Contains(body, `href="/run/alpha/fresh-observation"`) {
		t.Errorf("unharvested note must not link\n%s", body)
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
			return newTestServer(t, Options{Addr: "127.0.0.1:0", Root: root, MoeBin: "/bin/echo"})
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
