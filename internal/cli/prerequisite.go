package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
)

// filing is one entry a run's followups.md has extracted, resolved to
// where it landed. Harvest rewrites a checked line to the id the entry
// actually landed at, so the source's own file is the relationship and
// the scan resolves it: checked line → idea → promoted run → status.
// An entry linked to a live owner rewrites to that owner, so the walk
// starts at the run itself.
type filing struct {
	// Ref is `<project>/<id>` as the checked line resolved it.
	Ref string
	// Tag is the workflow tag on the line, "" for untagged.
	Tag string
	// Owner is the run doing the work — the promoted run, or the live
	// owner the entry linked to. Nil for an idea not yet promoted.
	Owner *run.Metadata
	// Settled is true once the work can no longer change: the owner merged
	// or closed, or the idea closed without promotion. Pushed is not
	// settled; a pushed PR is not a landed prerequisite.
	Settled bool
	// How is the settling status, `merged` or `closed`.
	How string
	// At approximates when it settled: the latest commit naming the
	// settling run. Intent links carry no MoE-Run trailer, so nothing a
	// terminal run routinely receives afterwards moves it.
	At time.Time
}

// Key is the name a printed line should use: the owner when there is
// one, since that is where the work happened.
func (f filing) Key() string {
	if f.Owner != nil {
		return f.Owner.Project + "/" + f.Owner.ID
	}
	return f.Ref
}

// runFilings resolves every checked entry in md's followups.md against
// the scan. An entry whose target is not on the board is dropped: it
// cannot settle, and nothing here can say where it went.
func runFilings(sc *pulseScan, md *run.Metadata) []filing {
	body, err := os.ReadFile(filepath.Join(sc.root, run.FollowupsPath(md.Project, md.ID)))
	if err != nil {
		return nil
	}
	var out []filing
	for line := range strings.SplitSeq(string(body), "\n") {
		m := followupDoneCaptureRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := m[2]
		if !strings.Contains(ref, "/") {
			ref = md.Project + "/" + ref
		}
		target := sc.byKey[ref]
		if target == nil {
			continue
		}
		f := filing{Ref: ref, Tag: m[3]}
		if target.Workflow == dash.IdeaWorkflow {
			switch target.Status {
			case run.StatusPromoted:
				f.Owner = sc.byKey[sc.idx.PromotedTo[ref]]
			case run.StatusClosed:
				f.Settled, f.How, f.At = true, run.StatusClosed, sc.idx.LastActivity[ref]
			}
		} else {
			f.Owner = target
		}
		if o := f.Owner; o != nil && (o.Status == run.StatusMerged || o.Status == run.StatusClosed) {
			f.Settled, f.How, f.At = true, o.Status, sc.idx.LastActivity[o.Project+"/"+o.ID]
		}
		out = append(out, f)
	}
	return out
}

// gateTokenRE matches a backticked token with no whitespace. A slug has
// no spaces, so a hard wrap can move a token whole but never split it.
var gateTokenRE = regexp.MustCompile("`([^`\\s]+)`")

// stageGateSection reads a stage's gate: whether any of its canvases is
// authored blocked, and the backticked tokens named between `## Gate` and
// the next `## ` heading. A directory-form stage reads every file.
func stageGateSection(root string, md *run.Metadata, stage string) (blocked bool, names map[string]bool) {
	var paths []string
	if files := stageCanvasFiles(md.Workflow, stage); files != nil {
		for _, f := range files {
			paths = append(paths, run.ContentFilePath(md.Project, md.ID, stage, f.Name))
		}
	} else {
		paths = []string{run.ContentPath(md.Project, md.ID, stage)}
	}
	names = map[string]bool{}
	for _, rel := range paths {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		if status, ok := stageGateStatus(string(body)); ok && status == "blocked" {
			blocked = true
		}
		for _, m := range gateTokenRE.FindAllStringSubmatch(gateSectionText(string(body)), -1) {
			names[m[1]] = true
		}
	}
	return blocked, names
}

// gateSectionText is the text from the first `## Gate` heading to the
// next `## ` heading, or "" when there is none.
func gateSectionText(body string) string {
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "## ") || strings.TrimSpace(strings.TrimPrefix(ln, "## ")) != "Gate" {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "## ") {
				end = j
				break
			}
		}
		return strings.Join(lines[i+1:end], "\n")
	}
	return ""
}

// namedBy reports whether a gate's tokens name f: its resolved id or the
// owner's, either bare or with the `<project>/` prefix. The agent writes
// its gate before harvest resolves the entry, so a token may be the slug
// it filed rather than the id the checked line now carries: a dated live
// owner (`fix` → `fix-2026-11-20`) matches through slugBaseMatches, and a
// collision-allocated idea (`fix` → `fix-2`) through gateReceipts. Only
// the run's own filings are ever candidates.
func (f filing) namedBy(names map[string]bool) bool {
	keys := []string{f.Ref}
	if f.Owner != nil {
		keys = append(keys, f.Owner.Project+"/"+f.Owner.ID)
	}
	for tok := range names {
		tp, tid, prefixed := strings.Cut(tok, "/")
		if !prefixed {
			tp, tid = "", tok
		}
		for _, k := range keys {
			kp, kid, _ := strings.Cut(k, "/")
			if (tp == "" || tp == kp) && slugBaseMatches([]string{kid}, tid) {
				return true
			}
		}
	}
	return false
}

// gateReceipts adds to names, for each token that is a slug md filed, the
// idea its extraction receipt says the entry landed at. Harvest rewrites
// the checked line to the allocated id, so the receipt is where the filed
// slug survives. One history read per token; callers reach for it only
// when a cheaper match has failed.
func gateReceipts(root string, md *run.Metadata, names map[string]bool) map[string]bool {
	out := make(map[string]bool, len(names))
	for tok := range names {
		out[tok] = true
		tp, tid, prefixed := strings.Cut(tok, "/")
		if !prefixed {
			tp, tid = md.Project, tok
		}
		p, id, err := run.FollowupDestination(root, md.Project+"/"+md.ID+"/"+tp+"/"+tid)
		if err == nil && id != "" {
			out[p+"/"+id] = true
		}
	}
	return out
}

// dueFiling returns the filing that makes md due at stage: the stage's
// gate is authored blocked, names the filing, and the filing settled
// after the stage's last work turn. Nil when there is none. The re-check
// moves the work turn past the settlement, so one settlement buys one
// resume.
func dueFiling(sc *pulseScan, md *run.Metadata, stage string) *filing {
	last, err := workTurnTime(sc.root, md.Project, md.ID, stage, sc.idx)
	if err != nil || last.IsZero() {
		return nil
	}
	blocked, names := stageGateSection(sc.root, md, stage)
	if !blocked || len(names) == 0 {
		return nil
	}
	var settled []filing
	for _, f := range runFilings(sc, md) {
		if f.Settled && f.At.After(last) {
			if f.namedBy(names) {
				return &f
			}
			settled = append(settled, f)
		}
	}
	if len(settled) == 0 {
		return nil
	}
	names = gateReceipts(sc.root, md, names)
	for _, f := range settled {
		if f.namedBy(names) {
			return &f
		}
	}
	return nil
}

// firstStageAdmission is what lets a run rootDesignSettled calls
// unsettled through the floor.
type firstStageAdmission int

const (
	// admitNone: the operator holds the trigger.
	admitNone firstStageAdmission = iota
	// admitOneStage: the next stage only — a design-only spawn's seed
	// turn, or one turn for an undelivered operator note.
	admitOneStage
	// admitResume: the prerequisite the blocked gate names has settled;
	// the ride is the ordinary dynamic one from the first stage.
	admitResume
)

// unsettledAdmission is the one answer the floor, the ride's bound and
// the heartbeat's wake share for an unsettled run, so they cannot
// disagree. turnClosed is rootDesignSettled's bit.
//
// The resume leg is the one way an operator-opened design proceeds
// without an advance: the operator ran it, it parked itself on a filing
// it names in its gate, and that filing settled. The design re-checks and
// stamps its own gate; the harness stamps nothing. A design-only spawn is
// left out — its brief was never the operator's design.
func unsettledAdmission(sc *pulseScan, md *run.Metadata, turnClosed bool) (firstStageAdmission, *filing) {
	if md.DesignOnly && !turnClosed {
		return admitOneStage, nil
	}
	if !md.DesignOnly && turnClosed {
		if f := firstStageDueFiling(sc, md); f != nil {
			return admitResume, f
		}
	}
	if pendingInputOnRun(sc.root, md) {
		return admitOneStage, nil
	}
	return admitNone, nil
}

// firstStageDueFiling is dueFiling at the workflow's first stage.
func firstStageDueFiling(sc *pulseScan, md *run.Metadata) *filing {
	w, err := LookupWorkflow(md.Workflow)
	if err != nil || len(w.Stages()) == 0 {
		return nil
	}
	return dueFiling(sc, md, w.Stages()[0])
}

// dueClause is the parked line's reason a run is due.
func (f filing) dueClause() string {
	return fmt.Sprintf("due: its gate names `%s`, %s since its last turn", f.Key(), f.How)
}

// prerequisiteResumeKickoff is the line a resumed first-stage turn's
// headless kickoff carries: what settled, and what the re-check owes.
// Empty when the stage is not being resumed by a settled filing.
func prerequisiteResumeKickoff(root string, md *run.Metadata, stage string) string {
	if md.DesignOnly {
		return ""
	}
	w, err := LookupWorkflow(md.Workflow)
	if err != nil || len(w.Stages()) == 0 || w.Stages()[0] != stage {
		return ""
	}
	sc, ok := newPulseScan(root)
	if !ok {
		return ""
	}
	f := dueFiling(sc, md, stage)
	if f == nil {
		return ""
	}
	return fmt.Sprintf("Resumed because `%s`, which your gate names, %s since your last turn. "+
		"Verify the evidence your gate says the re-check needs before stamping ready; "+
		"if it is not there, keep the gate blocked and say what is missing.", f.Key(), f.How)
}

// filingExitLines tells the operator, at a not-ready park, where each of
// the run's unsettled filings went and whether anything will bring the
// run back. Silent when the run has none.
func filingExitLines(root string, md *run.Metadata, stage string) string {
	sc, ok := newPulseScan(root)
	if !ok {
		return ""
	}
	var open []filing
	for _, f := range runFilings(sc, md) {
		if !f.Settled {
			open = append(open, f)
		}
	}
	if len(open) == 0 {
		return ""
	}
	blocked, names := stageGateSection(root, md, stage)
	if blocked && len(names) > 0 {
		names = gateReceipts(root, md, names)
	}
	var b strings.Builder
	named := false
	for _, f := range open {
		switch {
		case f.Owner != nil:
			next := "nothing pending"
			if w, err := LookupWorkflow(f.Owner.Workflow); err == nil {
				if ns, _, err := w.NextWithIndex(root, f.Owner, sc.idx); err == nil && ns != "" {
					next = "at " + ns
				}
			}
			fmt.Fprintf(&b, "filed: %s (%s run, %s, %s)\n", f.Ref, f.Owner.Workflow, f.Owner.Status, next)
			if f.Owner.Project+"/"+f.Owner.ID != f.Ref {
				fmt.Fprintf(&b, "  now: %s/%s\n", f.Owner.Project, f.Owner.ID)
			}
		case f.Tag != "":
			fmt.Fprintf(&b, "filed: %s (idea, tagged %s) — an armed serve promotes and rides it; now: moe %s new --from-idea %s\n",
				f.Ref, f.Tag, f.Tag, f.Ref)
		default:
			fmt.Fprintf(&b, "filed: %s (idea, untagged — yours to promote); now: moe sdlc new --from-idea %s\n", f.Ref, f.Ref)
		}
		if blocked && f.namedBy(names) {
			named = true
			b.WriteString("  this run resumes when it settles\n")
		}
	}
	if blocked && !named {
		b.WriteString("the gate names none of these, so nothing will resume this run on its own\n")
	}
	return b.String()
}
