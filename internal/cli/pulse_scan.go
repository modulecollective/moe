package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/run"
)

// pulseKickoff is the survey's first user turn (the whole `claude -p`
// prompt in headless mode). The steering lives in the stage fragment;
// this just points the agent at the job.
const pulseKickoff = "Run the pulse for this project: a delta-first, read-only sweep. " +
	"Survey what changed since the last pulse — the journal, twin-vs-code drift in the touched areas, the open backlog — " +
	"file followup entries for work worth doing, and write the canvas report. Follow the stage guidance. A quiet pulse — " +
	"\"nothing new since the last pulse\" — is a valid, successful report; never manufacture findings.\n\n" +
	"Close the canvas with the `## Gate` section (a ```json fence). Set \"status\" to a short word (e.g. \"ok\") once the " +
	"survey actually ran and concluded — that is what tells the harness this was a real sweep, not a crashed no-op. " +
	"The gate opens runs and orders them in one grammar: you write each run where it goes.\n\n" +
	"A `\"loose\"` list holds runs to open with no ordering opinion — they park standalone. Each entry is an object: " +
	"`{\"slug\": ..., \"title\": ..., \"why\": ..., \"design\": ...}`, defaulting to a `sdlc` fix run — the bar is mechanical, " +
	"bounded, and verifiable, all three, and the stage guidance holds it. A `loose` entry may instead set " +
	"`\"design_only\": true`, which opens the run, rides it one headless design turn and parks it for the operator — a " +
	"lower bar for a shorter ride, and the one place a finding that needs judgment rather than a fix may go; the stage " +
	"guidance holds that bar too. For an existing tagged idea, judge whether it has a clear objective, a bounded method, " +
	"and a credible verification source. Routine fact collection belongs in the destination workflow's design stage and " +
	"need not be in the seed. Withhold when the objective or acceptance criterion depends on an operator choice, or the evidence points " +
	"to a redesign. Either way, `why` is the one line the operator reads next to the verdict.\n\n" +
	"A `\"threads\"` list holds runs in execution order, each thread attached after an existing run (`\"onto\"`), under a " +
	"freshly named head (`\"head\"`), or self-rooted as its own thread (neither key). A thread's `\"runs\"` entry is either a **string** " +
	"naming any parked run in the project — naming one chained elsewhere moves it — or an **object** in the same shape as a " +
	"`loose` entry, which opens that run right at that position. This is where your ordering judgment goes; there is no prose " +
	"ranking section. The bar is the spawn bar plus ordering conviction: would the operator kick these, in this order, " +
	"unchanged? If the order is a guess, put the runs in `loose`. Under a dynamic sweep every kickable parked thread starts " +
	"when the sweep finishes — the ones you groom and the ones already in order — so the field you write is `\"park\"`: one " +
	"line naming why the operator should look at this thread first (an ordering you wouldn't defend, a speculative member, " +
	"an irreversible or outward-facing surface). No park means it runs, and that includes a run you wrote to `loose`, which " +
	"is a thread of one. `loose` says you have no ordering opinion; it does not say the work waits.\n\n" +
	"Omitting both lists is the normal outcome; a followup is the default channel for everything that doesn't clear the bar. " +
	"For failed checks on an owned pushed PR, nominate its existing run with " +
	"`{\"run\":\"<exact-slug>\",\"repair\":\"ci\"}` alone in a thread group, without onto or head. " +
	"The harness rechecks evidence and consent, claims a bounded attempt, then runs code, fresh test and review, and updates that PR. " +
	"See the stage guidance."

// pulseKickoffFromScan appends the harness-computed context to the
// static kickoff: six blocks (GitHub, recently-settled runs, chain
// state, advanced-and-left runs, openable judged chores, the input
// channel) and two trailing lines (what this invocation licensed, and
// what the project's standing mode does with that licence). The
// survey's InitialPromptBuilder calls it with a scan of the session
// worktree runStageSession hands the builder; every block reads that
// one scan, so the blocks describe one consistent moment rather than
// six successive ones. Best-effort throughout: a gather that fails
// drops its own block rather than failing the sweep.
func pulseKickoffFromScan(sc *pulseScan, projectID, runID string, stderr io.Writer) (string, map[string]string) {
	blocks := []string{pulseKickoff}
	if gh := pulseGitHubContext(sc, projectID, runID, stderr); gh != "" {
		blocks = append(blocks, gh)
	}
	if checks := renderOwnedPRChecks(sc, sc.prChecks(projectID, stderr)); checks != "" {
		blocks = append(blocks, checks)
	}
	if settled := settledRunsBlock(sc, projectID); settled != "" {
		blocks = append(blocks, settled)
	}
	if chains := chainStateBlock(sc, projectID); chains != "" {
		blocks = append(blocks, chains)
	}
	if advanced := advancedRunsBlock(sc, projectID); advanced != "" {
		blocks = append(blocks, advanced)
	}
	if judged := judgedChoresBlock(sc, projectID); judged != "" {
		blocks = append(blocks, judged)
	}
	// The input record: what the operator has pushed at the board, what
	// the board has asked them, and the bar for asking. Beside the chore
	// block rather than after the ride line — both are "standing operator
	// state this sweep has to account for", and neither depends on what
	// this invocation licensed.
	if inputs := pendingInputBlock(sc, projectID); inputs != "" {
		blocks = append(blocks, inputs)
	}
	// Its own block, not a tail on the chain-state one: the line is about
	// what this sweep may start, which is true whether or not the board
	// happens to hold an active chain of two or more. Nested under the
	// chain-state block it reached the agent only by coincidence.
	if ride := rideModeContextLine(); ride != "" {
		blocks = append(blocks, ride)
	}
	// Beside it rather than folded into it: the ride line says what this
	// invocation licensed, and this says what the project's standing mode
	// does with that licence.
	if mode := projectModeContextLine(sc.root, projectID); mode != "" {
		blocks = append(blocks, mode)
	}
	// The chain edge set as the agent saw it. The sweep's ordering
	// opinion is formed against exactly this picture, so it is what the
	// apply step checks itself against before restructuring anything.
	return strings.Join(blocks, "\n\n"), sc.graph.Edges()
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
// and the journal index, plus the by-key map and chain graph derived
// from them.
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
	graph *run.ChainGraph

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
	return &pulseScan{root: root, mds: mds, idx: idx, byKey: byKey, graph: run.NewChainGraph(idx, byKey)}, true
}

func (sc *pulseScan) prChecks(projectID string, stderr io.Writer) []ownedPRChecks {
	if sc.ownedPRs == nil {
		sc.ownedPRs = make(map[string][]ownedPRChecks)
	}
	if prs, ok := sc.ownedPRs[projectID]; ok {
		return prs
	}
	prs := heartbeatPRChecks
	if prs == nil {
		prs = gatherOwnedPRChecks(sc, projectID, stderr)
	}
	sc.ownedPRs[projectID] = prs
	return prs
}
