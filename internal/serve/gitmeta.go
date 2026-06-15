package serve

import (
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
)

// fileMeta is git-derived provenance for one bureaucracy file: when it
// first appeared, when it last changed, and which run produced the most
// recent change (parsed from MoE-* commit trailers). File mtime is the
// wrong source here — a clone/checkout clobbers it — so everything comes
// from history, which serve has in full against the live root.
type fileMeta struct {
	Created string // "2006-01-02", empty when unknown
	Updated string // dash.HumanAgo output, empty when unknown
	RunLink string // "/run/{project}/{run}", empty when no MoE-Run trailer
	Run     string // "{project}/{run}" label for the link
}

// gatherFileMeta derives provenance for rel (a path relative to the
// bureaucracy root) from git history. Up to three git subprocesses per
// call — keep it off index pages (see the design's N+1 caution); it
// belongs on hub/detail pages where the file count is a couple dozen.
func (s *Server) gatherFileMeta(now time.Time, rel string) fileMeta {
	var m fileMeta
	if out, err := git.Output(s.opts.Root, "log", "-1", "--format=%aI", "--", rel); err == nil {
		if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(out)); perr == nil {
			m.Updated = dash.HumanAgo(now, t)
		}
	}
	// First commit touching the path: oldest-first log, take line one.
	if out, err := git.Output(s.opts.Root, "log", "--reverse", "--format=%aI", "--", rel); err == nil {
		first := out
		if i := strings.IndexByte(first, '\n'); i >= 0 {
			first = first[:i]
		}
		if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(first)); perr == nil {
			m.Created = t.Format("2006-01-02")
		}
	}
	// Producing run: trailers on the most recent commit. Hand commits
	// carry no MoE-Run, so the link is simply omitted then.
	if body, err := git.Output(s.opts.Root, "log", "-1", "--format=%B", "--", rel); err == nil {
		if project, run := parseRunTrailers(body); project != "" && run != "" {
			m.Run = project + "/" + run
			m.RunLink = "/run/" + project + "/" + run
		}
	}
	return m
}

// parseRunTrailers pulls MoE-Project / MoE-Run out of a commit body.
// Same trailer-scan idiom as wiki/detect.go's lastCommitIsTwin.
func parseRunTrailers(body string) (project, run string) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok {
			run = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "MoE-Project:"); ok {
			project = strings.TrimSpace(v)
		}
	}
	return project, run
}
