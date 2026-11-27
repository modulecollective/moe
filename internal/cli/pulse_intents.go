package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers"
)

// The pulse keeps intents: it opens one when work has gathered around an
// outcome nobody named, links runs to the intent each serves, and closes
// one whose done condition it can show met. All three ride the survey's
// gate, applied after the turn in the order applyPulseGate spells out.
// The harness checks shape, existence and the one-try rule; the bars
// (when to open, when to close) are prompt text in the stage fragment,
// because a harness check would have to read a done condition out of
// markdown.

// openPulseIntents opens the intents the gate's `intents` list asks for.
// Each opens under its exact slug: an intent is named, not dated, so a
// slug that is already taken — an open or closed intent, or any other
// run — skips with a warning rather than minting a sibling.
func openPulseIntents(root, spawnedBy string, acts []pulseIntentAct, stderr io.Writer) {
	for _, act := range acts {
		if act.Open == "" {
			continue
		}
		projectID, slug, _ := splitProjectRun(act.Open)
		if run.Slugify(slug) != slug {
			moePrintf(stderr, "pulse: intent: open %q has an unusable slug — skipping\n", act.Open)
			continue
		}
		if err := requireProject(root, projectID); err != nil {
			moePrintf(stderr, "pulse: intent: open %q: %v — skipping\n", act.Open, err)
			continue
		}
		if md, err := run.Load(root, projectID, slug); err == nil {
			moePrintf(stderr, "pulse: intent: open %q: slug already names a %s %s — skipping\n", act.Open, md.Status, md.Workflow)
			continue
		}
		seed := "# " + strings.TrimSpace(act.Title) + "\n\n" + strings.TrimSpace(act.Body) + "\n"
		_, err := runopen.Open(root, projectID, run.Options{
			ID:        slug,
			Workflow:  dash.IntentWorkflow,
			SeedDocs:  map[string]string{dash.IntentDocID: seed},
			SpawnedBy: spawnedBy,
			Trailers:  trailers.Block{SpawnedBy: spawnedBy, Consent: spawnConsent(spawnedBy)},
		})
		if err != nil {
			moePrintf(stderr, "pulse: intent: open %q: %v — skipping\n", act.Open, err)
			continue
		}
		moePrintf(stderr, "pulse: opened intent %s (%s)\n", act.Open, strings.TrimSpace(act.Why))
	}
}

// closePulseIntents closes the intents the gate's `intents` list asks
// for, each through the capture close with the sweep recorded as
// closed_by and the why in the commit body. An intent a machine already
// closed once is refused by name: the operator reopened it, and the next
// survey would otherwise close it again on the same evidence.
func closePulseIntents(root, spawnedBy string, acts []pulseIntentAct, stderr io.Writer) {
	for _, act := range acts {
		if act.Close == "" {
			continue
		}
		projectID, slug, _ := splitProjectRun(act.Close)
		md, err := run.Load(root, projectID, slug)
		if err != nil {
			moePrintf(stderr, "pulse: intent: close %q: %v — skipping\n", act.Close, err)
			continue
		}
		if md.Workflow != dash.IntentWorkflow {
			moePrintf(stderr, "pulse: intent: close %q names a %s run, not an intent — skipping\n", act.Close, md.Workflow)
			continue
		}
		err = runopen.CloseCaptureWith(root, projectID, slug, runopen.CloseOptions{
			ClosedBy: spawnedBy,
			Why:      act.Why,
			Consent:  spawnConsent(spawnedBy),
		})
		switch {
		case errors.Is(err, runopen.ErrMachineCloseSpent):
			moePrintf(stderr, "pulse: intent: close %q refused — a pulse closed it once and the operator reopened it; only the operator closes it now\n", act.Close)
		case errors.Is(err, runopen.ErrNotCapture):
			moePrintf(stderr, "pulse: intent: close %q: intent is already %s — skipping\n", act.Close, md.Status)
		case err != nil:
			moePrintf(stderr, "pulse: intent: close %q: %v — skipping\n", act.Close, err)
		default:
			moePrintf(stderr, "pulse: closed intent %s (%s)\n", act.Close, strings.TrimSpace(act.Why))
		}
	}
}

// applyPulseServes writes the gate's `serves` links onto run.json, all
// in one journal commit carrying the sweep's consent. A run may be named
// by the slug this gate's `loose` used (minted maps it to the id the
// harness opened). A link to an unknown run, to anything but a sdlc run
// or an idea, to a run with an open session, or to an intent that is not
// open, warns and skips; the rest still apply. A link that already reads
// the same is not rewritten.
func applyPulseServes(root, spawnedBy string, serves []pulseServes, minted map[string]string, stderr io.Writer) {
	if len(serves) == 0 {
		return
	}
	err := repolock.With(root, repolock.Options{Purpose: "pulse-serves"}, func() error {
		var paths []string
		changed := map[string]*run.Metadata{}
		for _, sv := range serves {
			md, err := resolveServesRun(root, sv.Run, minted, changed)
			if err != nil {
				moePrintf(stderr, "pulse: serves: %v — skipping\n", err)
				continue
			}
			if sv.Intent != "" {
				if err := requireOpenIntent(root, sv.Intent); err != nil {
					moePrintf(stderr, "pulse: serves: %s → %s: %v — skipping\n", sv.Run, sv.Intent, err)
					continue
				}
			}
			if md.Intent == sv.Intent {
				continue
			}
			md.Intent = sv.Intent
			key := md.Project + "/" + md.ID
			if _, seen := changed[key]; !seen {
				paths = append(paths, filepath.Join(run.Dir(md.Project, md.ID), "run.json"))
			}
			changed[key] = md
		}
		if len(paths) == 0 {
			return nil
		}
		// A failed save or commit puts every run.json back, so the
		// repo-wide dirty-tree gate does not wedge the closes that follow.
		restore := func(err error) error {
			_ = git.Run(root, append([]string{"reset", "-q", "--"}, paths...)...)
			_ = git.Run(root, append([]string{"checkout", "HEAD", "--"}, paths...)...)
			return err
		}
		for _, md := range changed {
			if err := run.Save(root, md); err != nil {
				return restore(err)
			}
		}
		msg := "Link " + dash.Plural(len(paths), "run") + " to intents\n\n" +
			trailers.Block{SpawnedBy: spawnedBy, Consent: spawnConsent(spawnedBy)}.String()
		if err := run.StageAndCommit(root, msg, paths...); err != nil {
			return restore(err)
		}
		moePrintf(stderr, "pulse: linked %d run(s) to intents\n", len(paths))
		return nil
	})
	if err != nil {
		moePrintf(stderr, "pulse: serves: %v\n", err)
	}
}

// resolveServesRun loads the run a `serves` entry names, preferring one
// this apply already changed so two entries on one run compose.
func resolveServesRun(root, ref string, minted map[string]string, changed map[string]*run.Metadata) (*run.Metadata, error) {
	projectID, slug, _ := splitProjectRun(ref)
	if id, ok := minted[projectID+"/"+slug]; ok {
		slug = id
	}
	if md, ok := changed[projectID+"/"+slug]; ok {
		return md, nil
	}
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", ref, err)
	}
	if md.Workflow != "sdlc" && md.Workflow != dash.IdeaWorkflow {
		return nil, fmt.Errorf("run %s is a %s run; only sdlc runs and ideas serve an intent", ref, md.Workflow)
	}
	// A session commits its own run.json and rebases onto main at close;
	// a first turn's edit sits beside `intent`, so a link written now
	// would conflict that close. The run stays in Unmapped while it is
	// active (unmappedBlock), so a later survey links it.
	if err := session.RequireUnreserved(root, md.Project, md.ID); err != nil {
		return nil, fmt.Errorf("run %s has an open session; linking it later", ref)
	}
	return md, nil
}

// requireOpenIntent refuses anything but an in-progress intent.
func requireOpenIntent(root, ref string) error {
	projectID, slug, _ := splitProjectRun(ref)
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		return err
	}
	if md.Workflow != dash.IntentWorkflow {
		return fmt.Errorf("%s is a %s run, not an intent", ref, md.Workflow)
	}
	if md.Status != run.StatusInProgress {
		return fmt.Errorf("intent %s is %s", ref, md.Status)
	}
	return nil
}

// openIntentStatusPaths is the pulse turn's ExtraStagePaths: the
// status.md of every open intent in the session worktree, relative to
// it. Intent canvases are the operator's and stay off the list; a
// status.md missing on disk is left out so `git add` never fails on it.
func openIntentStatusPaths(workRoot string, _ *run.Metadata) ([]string, error) {
	mds, err := run.Scan(workRoot)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, md := range mds {
		if md.Workflow != dash.IntentWorkflow || md.Status != run.StatusInProgress {
			continue
		}
		rel := run.StatusPath(md.Project, md.ID)
		if _, err := os.Stat(filepath.Join(workRoot, rel)); err == nil {
			out = append(out, rel)
		}
	}
	return out, nil
}

// openIntentsBlock is the kickoff's view of every open intent: its
// qualified slug, title, marks and canvas path, then the work linked to
// it with each run's status. It is what the pulse keeps Status from.
func openIntentsBlock(sc *pulseScan) string {
	linked := map[string][]*run.Metadata{}
	for _, md := range sc.mds {
		if md.Intent != "" {
			linked[md.Intent] = append(linked[md.Intent], md)
		}
	}
	var lines []string
	for _, md := range sc.mds {
		if md.Workflow != dash.IntentWorkflow || md.Status != run.StatusInProgress {
			continue
		}
		key := md.Project + "/" + md.ID
		lines = append(lines, fmt.Sprintf("- `%s` — %s%s\n  canvas: %s\n  status: %s",
			key, intentTitle(sc.root, md.Project, md.ID), intentMarks(md),
			filepath.Join(sc.root, run.ContentPath(md.Project, md.ID, dash.IntentDocID)),
			filepath.Join(sc.root, run.StatusPath(md.Project, md.ID))))
		work := linked[key]
		sortRunsByKey(work)
		for _, w := range work {
			lines = append(lines, fmt.Sprintf("  - `%s/%s` (%s, %s) — %s", w.Project, w.ID, w.Workflow, w.Status, settledRunTitle(sc.root, w)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Open intents, each with the work linked to it. Keep each one's `status.md` as the stage guidance says:\n" +
		strings.Join(lines, "\n")
}

// unmappedWindow is how far back Unmapped looks for new work. It is
// wider than one pulse so a run whose link was skipped for an open
// session is offered again once that session lands.
const unmappedWindow = 7 * 24 * time.Hour

// unmappedBlock lists the recent runs and ideas that serve no intent yet
// and moved since the last pulse — the ones a `serves` entry could link.
// It is what makes mapping cheap without a backfill: only new work is
// asked about. "New" is created within unmappedWindow of the last pulse
// (of now, for a first-ever pulse, which skips the activity test). A run
// the pulse leaves unlinked comes back while it stays active in that
// window; the prompt says to leave the rest.
func unmappedBlock(sc *pulseScan, last time.Time, runID string) string {
	since := time.Now()
	if !last.IsZero() {
		since = last
	}
	day := since.Add(-unmappedWindow).Local().Format("2006-01-02")
	var picked []*run.Metadata
	for _, md := range sc.mds {
		if md.Intent != "" || md.ID == runID || md.Created < day {
			continue
		}
		if md.Workflow != "sdlc" && md.Workflow != dash.IdeaWorkflow {
			continue
		}
		if md.Workflow == dash.IdeaWorkflow && md.Status == run.StatusPromoted {
			continue
		}
		if !last.IsZero() && !sc.idx.LastActivity[md.Project+"/"+md.ID].After(last) {
			continue
		}
		picked = append(picked, md)
	}
	if len(picked) == 0 {
		return ""
	}
	sortRunsByKey(picked)
	lines := make([]string, 0, len(picked))
	for _, md := range picked {
		lines = append(lines, fmt.Sprintf("- `%s/%s` (%s, %s) — %s", md.Project, md.ID, md.Workflow, md.Status, settledRunTitle(sc.root, md)))
	}
	return "Unmapped: runs and ideas opened this week that moved since the last pulse and serve no intent. Link the ones that serve one with `serves`; leave the rest:\n" +
		strings.Join(lines, "\n")
}

func sortRunsByKey(mds []*run.Metadata) {
	sort.Slice(mds, func(i, j int) bool {
		if mds[i].Project != mds[j].Project {
			return mds[i].Project < mds[j].Project
		}
		return mds[i].ID < mds[j].ID
	})
}
