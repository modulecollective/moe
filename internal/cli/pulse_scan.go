package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// pulseKickoff is the survey's first user turn (the whole `claude -p`
// prompt in headless mode). The steering lives in the stage fragment.
const pulseKickoff = "Run the pulse for this bureaucracy: a delta-first, read-only survey of every project below. " +
	"Survey what changed since the last pulse — the journal, twin-vs-code drift in the touched areas, the open backlog — " +
	"file followup entries for work worth doing, using the `<project>/` prefix to route each, and write the canvas report. " +
	"Follow the stage guidance. A quiet pulse — \"nothing new since the last pulse\" — is a valid, successful report; " +
	"never manufacture findings.\n\n" +
	"Close the canvas with the `## Gate` section (a ```json fence). Set \"status\" to a short word (e.g. \"ok\") once the " +
	"survey actually ran and concluded — that is what tells the harness this was a real sweep, not a crashed no-op. " +
	"The gate says two things: what to open and what is next.\n\n" +
	"A `\"loose\"` list holds runs to open. Each entry is an object: `{\"slug\": \"<project>/<slug>\", \"title\": ..., " +
	"\"why\": ..., \"design\": ...}`, defaulting to a `sdlc` fix run — the bar is mechanical, bounded, and verifiable, all " +
	"three, and the stage guidance holds it. An entry may instead set `\"design_only\": true`, which opens the run for one " +
	"headless design turn when it is picked — a lower bar for a shorter ride, and the one place a finding that needs " +
	"judgment rather than a fix may go.\n\n" +
	"`\"next\"` names the one run to ride after this sweep, anywhere in the bureaucracy: " +
	"`{\"run\": \"<project>/<slug>\", \"why\": \"<one line: why this before everything else>\"}`. It may be a parked run, " +
	"a tagged idea (promoted when picked), or a run this gate's `loose` opens. Read what each parked run's canvas says it " +
	"is waiting on, across projects, and pick the first thing that can actually move. Omit `next` when nothing should " +
	"start. For failed checks on an owned pushed PR, name its existing run with `\"repair\": \"ci\"` added. The harness " +
	"rechecks evidence and consent, claims a bounded attempt, then runs code, fresh test and review, and updates that PR. " +
	"See the stage guidance."

// pulseKickoffFromScan appends the harness-computed context to the static
// kickoff: where each project's source lives, then one section per project
// with a delta since the last pulse or live work. Every block reads the one
// scan, so they all describe the same moment. A gather that fails drops its
// own block rather than failing the sweep.
func pulseKickoffFromScan(sc *pulseScan, canonicalRoot, runID string, stderr io.Writer) string {
	blocks := []string{pulseKickoff}
	mds, _, err := project.List(sc.root)
	if err != nil {
		moePrintf(stderr, "pulse: list projects: %v\n", err)
	}
	last := lastPulseAt(sc, project.RootID, runID)
	var sources, paused, quiet []string
	var sections []string
	for _, p := range mds {
		if p.ID != project.RootID {
			sources = append(sources, fmt.Sprintf("- `%s`: %s", p.ID, filepath.Join(canonicalRoot, project.SubmoduleDir(p.ID))))
		}
		if clockInvoked && project.ModeOf(p) == project.ModePaused {
			paused = append(paused, p.ID)
			continue
		}
		var section []string
		if pulseProjectActive(sc, p.ID, last, runID) {
			section = projectKickoffBlocks(sc, p.ID, runID, stderr)
		} else if judged := judgedChoresBlock(sc, p.ID); judged != "" {
			section = []string{judged}
		}
		if len(section) == 0 {
			quiet = append(quiet, p.ID)
			continue
		}
		sections = append(sections, "# Project `"+p.ID+"`\n\n"+strings.Join(section, "\n\n"))
	}
	if len(sources) > 0 {
		blocks = append(blocks, "Project sources, the canonical checkouts. Read them by absolute path; they are read-only:\n"+
			strings.Join(sources, "\n"))
	}
	if len(paused) > 0 {
		blocks = append(blocks, "Paused, so left alone this sweep: "+strings.Join(paused, ", ")+".")
	}
	if len(quiet) > 0 {
		blocks = append(blocks, "No change since the last pulse and no live work: "+strings.Join(quiet, ", ")+".")
	}
	blocks = append(blocks, sections...)
	if ride := rideModeContextLine(); ride != "" {
		blocks = append(blocks, ride)
	}
	return strings.Join(blocks, "\n\n")
}

// projectKickoffBlocks gathers one project's context blocks.
func projectKickoffBlocks(sc *pulseScan, projectID, runID string, stderr io.Writer) []string {
	var blocks []string
	add := func(b string) {
		if b != "" {
			blocks = append(blocks, b)
		}
	}
	if projectID != project.RootID {
		add(pulseGitHubContext(sc, projectID, runID, stderr))
		add(renderOwnedPRChecks(sc, sc.prChecks(projectID, stderr)))
	}
	add(settledRunsBlock(sc, projectID))
	add(parkedRunsBlock(sc, projectID))
	add(advancedRunsBlock(sc, projectID))
	add(judgedChoresBlock(sc, projectID))
	add(pendingInputBlock(sc, projectID))
	add(projectModeContextLine(sc.root, projectID))
	return blocks
}

// pulseProjectActive reports whether a project earns its blocks: something
// landed since the last pulse, or it holds live work a pick could name.
func pulseProjectActive(sc *pulseScan, projectID string, last time.Time, runID string) bool {
	if last.IsZero() {
		return true
	}
	for _, md := range sc.mds {
		if md.Project != projectID || md.Workflow == pulseWorkflow || md.ID == runID {
			continue
		}
		if sc.idx.LastActivity[md.Project+"/"+md.ID].After(last) {
			return true
		}
		switch {
		case md.Status == run.StatusPushed:
			return true
		case md.Status != run.StatusInProgress:
		case operatorCascades(md.Workflow):
			return true
		case md.Workflow == dash.IdeaWorkflow && md.PromoteTo != "":
			return true
		}
	}
	return false
}

// parkedRunsBlock lists the runs a pick could name, each with what its own
// canvas opens on. A dependency is prose on the blocked run's canvas, so
// this is where the survey meets it.
func parkedRunsBlock(sc *pulseScan, projectID string) string {
	var lines []string
	for _, key := range kickableRuns(sc, projectID) {
		md := sc.byKey[key]
		note := ""
		if hold := kickFloorHold(sc.root, key, sc); hold != "" {
			note = " — held: " + hold
		}
		note += operatorNoteMark(sc.root, md)
		lines = append(lines, fmt.Sprintf("- `%s` (%s)%s — %s", key, md.Workflow, note, settledRunTitle(sc.root, md)))
	}
	for _, md := range sc.mds {
		if md.Project == projectID && md.Workflow == dash.IdeaWorkflow && md.Status == run.StatusInProgress && md.PromoteTo != "" {
			lines = append(lines, fmt.Sprintf("- `%s/%s` (idea tagged %s)%s — %s", md.Project, md.ID, md.PromoteTo, operatorNoteMark(sc.root, md), settledRunTitle(sc.root, md)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Parked work a `next` could name. Before picking a run, read its canvas for anything it says it is waiting on:\n" +
		strings.Join(lines, "\n")
}

// operatorNoteMark flags a parked run the operator has pushed a note at, so
// the pick can put it first.
func operatorNoteMark(root string, md *run.Metadata) string {
	if pendingInputOnRun(root, md) {
		return " — operator note waiting"
	}
	return ""
}

// judgedChoresBlock lists the project's judged chores the survey could
// act on this turn — the operator's `when` criterion, and when the chore
// was last completed. This is the whole judged-chore seam on the read
// side: the criterion is prose the agent evaluates against the delta,
// and the gate's `chore` spec is where the verdict goes.
//
// Chores that are cooling down or already have an open run are omitted:
// the survey can't act on them, so naming them is noise. No openable
// judged chores → no block, same as the sibling blocks. A chore-load
// failure drops the block rather than failing the sweep.
func judgedChoresBlock(sc *pulseScan, projectID string) string {
	defs, err := chore.LoadAll(sc.root)
	if err != nil {
		return ""
	}
	var lines []string
	for _, st := range chore.EvaluateAll(defs, sc.mds, sc.idx, time.Now()) {
		d := st.Definition
		if d.Project != projectID || !d.Judged() || st.OpenRun != "" || st.CooldownBlocking {
			continue
		}
		last := "never"
		if !st.LastCompleted.IsZero() {
			last = st.LastCompleted.UTC().Format("2006-01-02 15:04Z")
		}
		lines = append(lines, fmt.Sprintf("- `%s` — due when: %s (last completed: %s)", d.Name, d.When, last))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Judged chores: registrations the operator authored whose due-ness is a judgment, not a glob or a clock. " +
		"For each, ask only whether what landed since the last pulse meets the condition as written — the work itself is " +
		"already authored, so this is not the spawn bar. When it does, nominate it in the gate with " +
		"`{\"chore\": \"<name>\", \"why\": \"<the landed change that met it>\"}`, in `loose` or at a thread position. " +
		"Quiet is normal.\n" + strings.Join(lines, "\n")
}

// pulseScan is the one disk read a sweep's kickoff makes: the run scan
// and the journal index, plus the by-key map derived from them.
//
// Every context block wants some of this, and each used to take its own
// copy — a scan and an index build per block per sweep, each describing
// a slightly different moment. One read is cheaper and, more to the
// point, coherent: the blocks the agent reads all describe the same
// instant.
type pulseScan struct {
	root  string
	mds   []*run.Metadata
	idx   *run.JournalIndex
	byKey map[string]*run.Metadata

	// chores memoizes the whole register's chore states, evaluated
	// against this scan's runs and index. Loaded on the first ask rather
	// than in newPulseScan: loading forks git once per definition, and
	// most scans never ask.
	chores       []chore.State
	choresLoaded bool
	ownedPRs     map[string][]ownedPRChecks
}

// choreStates evaluates every registered chore against this scan's runs
// and journal index — once per scan, shared by every caller. The
// heartbeat gate asks once per project per tick, so the memo is what
// keeps the chore legs inside the gate's cheap-read budget.
//
// Not safe for concurrent use, like the rest of pulseScan: a scan
// belongs to one sweep or one tick, and both walk their projects in
// sequence.
//
// A chore-load failure yields no states, which reads as "nothing is
// due". Same warn-by-silence posture the sweep's own chore blocks take
// — a broken definition must not decide a tick.
func (sc *pulseScan) choreStates(now time.Time) []chore.State {
	if sc.choresLoaded {
		return sc.chores
	}
	sc.choresLoaded = true
	defs, err := chore.LoadAll(sc.root)
	if err != nil {
		return nil
	}
	sc.chores = chore.EvaluateAll(defs, sc.mds, sc.idx, now)
	return sc.chores
}

// newPulseScan reads the runs and the journal. ok is false when either
// read fails; the caller drops its blocks rather than failing the sweep,
// which is what each block did on its own.
func newPulseScan(root string) (*pulseScan, bool) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, false
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, false
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}
	return &pulseScan{root: root, mds: mds, idx: idx, byKey: byKey}, true
}

func (sc *pulseScan) prChecks(projectID string, stderr io.Writer) []ownedPRChecks {
	if sc.ownedPRs == nil {
		sc.ownedPRs = make(map[string][]ownedPRChecks)
	}
	if prs, ok := sc.ownedPRs[projectID]; ok {
		return prs
	}
	var prs []ownedPRChecks
	if heartbeatPRChecks != nil {
		// The clock's observation covers every project; keep this one's.
		for _, p := range heartbeatPRChecks {
			if strings.HasPrefix(p.Run, projectID+"/") {
				prs = append(prs, p)
			}
		}
	} else {
		prs = gatherOwnedPRChecks(sc, projectID, stderr)
	}
	sc.ownedPRs[projectID] = prs
	return prs
}

// allPRChecks is every project's PR evidence, the set a `next` repair is
// checked against.
func (sc *pulseScan) allPRChecks(stderr io.Writer) []ownedPRChecks {
	seen := map[string]bool{}
	var out []ownedPRChecks
	for _, md := range sc.mds {
		if seen[md.Project] || md.Project == project.RootID {
			continue
		}
		seen[md.Project] = true
		out = append(out, sc.prChecks(md.Project, stderr)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
	return out
}
