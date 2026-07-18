package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

// The pulse's backlog-hygiene read covers *open* ideas. The settled
// record — runs that closed without code, runs that merged a fix — is
// reachable only by scanning every run.json by hand, which most sweeps
// won't do. That gap is what turns one observation into three ideas:
// nothing at filing time tells the surveying agent the question was
// already answered.
//
// So the harness hands it the list. Two uses, mapping to the two ways
// the 2026-07-18 chains refiled settled work: a finding that matches a
// *merged* run's fix usually means you are re-observing pre-fix
// behaviour (the classic stale-binary read at a chain tail); a finding
// that matches a *closed* run is a settled drop, and reopening it needs
// new evidence named in the entry.

const (
	// settledRunsWindow bounds the block by run age. The refile loop
	// this targets is chain churn — minted and settled within days — so
	// a fortnight covers the observed failure while keeping the block
	// short enough to read.
	settledRunsWindow = 14 * 24 * time.Hour
	// settledRunsCap bounds the row count on a busy fortnight. Newest
	// first, so what falls off the end is the oldest.
	settledRunsCap = 20
)

// settledRun is one row of the block.
type settledRun struct {
	id      string
	created time.Time
	wf      string
	status  string
	title   string
}

// settledRunsBlock renders the recently-settled-runs context block, or
// "" when there is nothing to say. Best-effort like its siblings in
// pulseKickoffWithContext: a failed scan drops the block rather than
// failing the sweep.
//
// Selection is terminal-and-recent: `closed` or `merged`, this project,
// non-pulse workflow, created inside the window. `promoted` is
// deliberately absent — a promoted idea's successor run carries the
// story and will list itself when it settles, so including both would
// double-report one thread. Pulses are excluded because every pulse is
// a closed run and they would crowd out everything else.
//
// Known approximation: created-within-window stands in for
// settled-within-window, because run.json records no terminal
// timestamp. A long-lived run closed yesterday won't list. Walking the
// journal for close/merge commits would be exact and costs a git call
// per pulse; the proxy covers the failure actually observed.
func settledRunsBlock(root, projectID string) string {
	mds, err := run.Scan(root)
	if err != nil {
		return ""
	}
	cutoff := time.Now().Local().Add(-settledRunsWindow)

	var rows []settledRun
	for _, md := range mds {
		if md.Project != projectID || md.Workflow == "pulse" {
			continue
		}
		if md.Status != run.StatusClosed && md.Status != run.StatusMerged {
			continue
		}
		created, err := time.ParseInLocation("2006-01-02", md.Created, time.Local)
		if err != nil || created.Before(cutoff) {
			continue
		}
		rows = append(rows, settledRun{
			id:      md.ID,
			created: created,
			wf:      md.Workflow,
			status:  md.Status,
			title:   settledRunTitle(root, md),
		})
	}
	if len(rows) == 0 {
		return ""
	}

	// Newest first; ID breaks ties, since Created is date-only and a
	// busy day would otherwise order non-deterministically.
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].created.Equal(rows[j].created) {
			return rows[i].created.After(rows[j].created)
		}
		return rows[i].id < rows[j].id
	})
	if len(rows) > settledRunsCap {
		rows = rows[:settledRunsCap]
	}

	var sb strings.Builder
	sb.WriteString("Recently settled runs (last 14 days, newest first) — the decisions already made, " +
		"which the open backlog does not show you:\n\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "- `%s` (%s, %s) — %s\n", r.id, r.wf, r.status, r.title)
	}
	sb.WriteString("\nBefore filing, check a finding against this list. A finding that matches a `merged` " +
		"run's fix usually means you are re-observing pre-fix behaviour, not a live bug — verify against " +
		"current code before filing. A finding that matches a `closed` run is a settled drop: it stays " +
		"settled unless you have new evidence, and the entry must name that evidence.")
	return sb.String()
}

// settledRunTitle reads a run's headline from its canvases — the idea's
// H1 first (the framing the run was minted with), then the design's.
// Falls back to the slug, which is already a compressed title.
func settledRunTitle(root string, md *run.Metadata) string {
	for _, docID := range []string{"idea", "design"} {
		if _, ok := md.Documents[docID]; !ok {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, run.ContentPath(md.Project, md.ID, docID)))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			if h, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
				if h = strings.TrimSpace(h); h != "" {
					return h
				}
			}
		}
	}
	return md.ID
}
