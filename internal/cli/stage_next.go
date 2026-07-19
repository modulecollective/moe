package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// promptNextStage prints the next incomplete stage's exact invocation
// and, on an interactive terminal, offers to run it in-process. The
// push stage is special-cased: two ship paths (merge/pr) make the
// prompt three-way ([N/m/p]), and N-as-default preserves the rule that
// an external-side-effect stage can't ship on a reflex Enter. All
// other stages keep the Y-default yes/no prompt. Returns the exit
// code to bubble up from the current stage: 0 on skip/decline/successful
// chain, the inner command's exit code if the chained stage fails.
//
// justFinished, when non-empty, names the stage whose session just
// committed a work turn — typically the docID a runStageSession
// closure passes through. The chain prompt asks about that stage's
// successor in the workflow DAG (a pure data lookup), decoupled from
// Next()'s satisfaction walk: under the forward-walking rule, Next()
// reports the just-finished stage as still parked, which is the wrong
// answer for "want to advance?". Empty justFinished falls back to
// Next() — the right answer for fresh runs (where the workflow's
// first stage is the next thing to run) and for entry points that
// don't know which stage just landed (e.g. `moe sdlc reopen` landing
// on the design gate of the freshly seeded run).
//
// justFinished also drives the "back" targets offered at the prompt:
// a non-empty justFinished resolves to the list of stages up to and
// including that stage in the workflow ladder, each looked up against
// the paired command group. The prompt offers `b` to jump back to any
// of them — a single stage runs directly, multiple stages route through
// a sub-prompt that asks which to re-open. This lets the operator amend
// the stage they just read, or skip over intermediate stages (test →
// design) instead of stepping back one at a time.
func promptNextStage(root string, md *run.Metadata, justFinished string, stdout, stderr io.Writer) int {
	return promptNextStageOverride(root, md, justFinished, "", false, stdout, stderr)
}

// promptNextStageParked is the `--park` tail: print the next incomplete
// stage's invocation hint and return 0 without ever prompting to run it.
// It routes through promptNextStageOverride with park=true, which forces
// the same print-only branch non-TTY callers already take — so an
// interactive `moe sdlc new --park` and a scripted one both just emit
// `next: moe sdlc design p/s` and exit, leaving the promote-then-ride
// default untouched for the plain prompt.
func promptNextStageParked(root string, md *run.Metadata, stdout, stderr io.Writer) int {
	return promptNextStageOverride(root, md, "", "", true, stdout, stderr)
}

// promptNextStageOverride is promptNextStage with an optional override
// for the offered stage. When override is non-empty it replaces the
// stage the prompt offers, leaving back-targets keyed off justFinished
// untouched. The push-gate recovery session passes override="push": the
// recovery is a code turn, so justFinished is "code" (back offers
// code/design to re-fix the rebase), but the next step to offer is the
// push retry, not code's successor (test). Every other caller goes
// through promptNextStage with override="" — byte-for-byte the old path.
//
// park forces the print-only branch regardless of whether stdin is a
// terminal: it is the `--park` path (open the run and stop), which wants
// the same `next: …` hint a non-TTY caller gets, but must also suppress
// the chain prompt when a human is watching. Only promptNextStageParked
// passes true; every other caller passes false and keeps the tty-gated
// prompt.
func promptNextStageOverride(root string, md *run.Metadata, justFinished, override string, park bool, stdout, stderr io.Writer) int {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	var stage string
	switch {
	case override != "":
		stage = override
	case justFinished != "":
		stage = wf.Successor(justFinished)
		if stage == "" {
			// Terminal stage — no successor to offer. If the workflow
			// has a close command and the run is still in_progress,
			// offer the operator the same `[Y/n/x]` shape every other
			// chain prompt uses, with `Y` dispatching close. Non-TTY
			// callers (`moe twin reflect ... < /dev/null`) keep the
			// print-only nudge — anti-silent-close, same rule the
			// cascade's auto-close honours.
			if md.Status == run.StatusInProgress {
				if g, err := LookupGroup(md.Workflow); err == nil {
					if closeCmd := g.Lookup("close"); closeCmd != nil {
						if park || !stdinIsTerminal() {
							moePrintf(stdout,
								"%s sealed — run `moe %s close %s/%s` to mark the run terminal.\n",
								justFinished, md.Workflow, md.Project, md.ID)
							return 0
						}
						return promptCloseNextStage(closeCmd, justFinished, md, stdout, stderr)
					}
				}
			}
			return 0
		}
	default:
		n, kind, err := wf.Next(root, md)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		if kind != NextKindStage || n == "" {
			return 0
		}
		stage = n
	}
	g, err := LookupGroup(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	next := g.Lookup(stage)
	if next == nil {
		// Workflow tracks the stage but the group has no matching
		// command (idea is the case today). Treat the same as "no
		// runnable next" — print the stage hint and return.
		moePrintf(stdout, "next: %s %s (no runnable command)\n", wf.Name, stage)
		return 0
	}
	back := backTargets(wf, g, justFinished)
	// scuttle is the workflow's `close` command, offered at the prompt
	// as `x` so the operator can abandon the run from the same surface
	// they decline the next stage from. `x` reads as "exit/abandon" and
	// avoids overloading `s` (which the operator might mistake for
	// "skip" or "save"). Nil-safe: workflows that don't register close
	// (none today, but the prompt should stay honest) simply don't see
	// the option.
	scuttle := g.Lookup("close")
	// sdlc review/test closed `blocked` → kick back, don't walk forward.
	// The gate said the run isn't ready to advance, so offering its
	// successor on a reflex Enter would step past the objection. Reshape
	// into a kickback offer (interactive) or a back-pointing nudge
	// (non-TTY). Gated to override == "" so the kickback's own recovery
	// turn — which re-enters here with override set to the blocked stage —
	// re-offers that gate instead of recursing into another kickback.
	if override == "" && md.Workflow == "sdlc" &&
		(justFinished == "review" || justFinished == "test") {
		if canvas := readPrintableCanvas(root, md, justFinished); canvas != "" {
			if status, ok := stageGateStatus(canvas); ok && status == "blocked" {
				if park || !stdinIsTerminal() {
					// No operator to choose; point the nudge back at code
					// (where the fix lives), not the forward stage.
					moePrintf(stdout, "next: moe %s code %s/%s (%s blocked — kick back to fix)\n",
						wf.Name, md.Project, md.ID, justFinished)
					return 0
				}
				return promptKickback(g, scuttle, md, justFinished, canvas, stdout, stderr)
			}
		}
	}
	hint := fmt.Sprintf("moe %s %s %s/%s", wf.Name, next.Name, md.Project, md.ID)
	if park || !stdinIsTerminal() {
		moePrintf(stdout, "next: %s\n", hint)
		return 0
	}
	switch next.Name {
	case "push":
		// The ship gate prints immediately after test closes. Synthesis
		// does not fire at chain-prompt time because the operator may
		// decline; the chosen push command runs synthesis as part of its
		// shared preflight.
		return promptPushNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
	}
	return promptStageNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
}

// backTargets returns the stages the chain prompt's `b` should route
// to: every stage up to and including justFinished in the workflow
// ladder, mapped through the paired command group. An empty justFinished
// (fresh-run callers) gets an empty slice — no just-read stage to
// amend. Stages whose command group has no matching verb (idea is the
// only such case today) are skipped silently so the operator only sees
// options that actually dispatch.
//
// Today every workflow's ladder is linear, so "up to" means "appears
// no later than justFinished in wf.Stages()". A fan-in or fan-out would
// need a richer answer; surface the design question if and when one
// shows up.
func backTargets(wf *Workflow, g *CommandGroup, justFinished string) []*Command {
	if justFinished == "" {
		return nil
	}
	var back []*Command
	for _, s := range wf.Stages() {
		if cmd := g.Lookup(s); cmd != nil {
			back = append(back, cmd)
		}
		if s == justFinished {
			break
		}
	}
	return back
}

// dispatchBack invokes a back target. A single back target dispatches
// directly. Multiple targets fan out to a sub-prompt keyed by the
// stage name's first letter; a blank/unrecognized answer collapses to
// "declined" and returns 0, the same shape the top-level prompt uses
// for typos. Stage names with colliding first letters would break the
// keying — none today, and the workflow registry test would catch a
// future collision before it shipped.
func dispatchBack(back []*Command, md *run.Metadata, stdout, stderr io.Writer) int {
	if len(back) == 0 {
		return 0
	}
	if len(back) == 1 {
		return back[0].Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
	}
	keys := make(map[rune]*Command, len(back))
	parts := make([]string, 0, len(back))
	for _, cmd := range back {
		r := rune(cmd.Name[0])
		keys[r] = cmd
		parts = append(parts, fmt.Sprintf("%c=%s", r, cmd.Name))
	}
	moePrintf(stdout, "back to: %s ?\n", strings.Join(parts, " · "))
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" {
		return 0
	}
	cmd, ok := keys[rune(answer[0])]
	if !ok {
		return 0
	}
	return cmd.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
}

// backHint formats the legend phrase for the `b` option. Single target
// reads "back to <stage>" so muscle memory still parses the option
// without typing it; multiple targets read "back to stage" — the
// operator picks the specific stage at the sub-prompt after typing `b`.
func backHint(back []*Command) string {
	if len(back) == 1 {
		return "back to " + back[0].Name
	}
	return "back to stage"
}

// readPrintableCanvas returns the named stage's content.md body if it
// exists and contains non-whitespace, or the empty string otherwise.
// Read errors collapse to empty — the canvas is operator-facing context,
// not load-bearing state, so a missing file falls through silently.
func readPrintableCanvas(root string, md *run.Metadata, stage string) string {
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, stage))
	body, err := os.ReadFile(canvasPath)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(string(body)) == "" {
		return ""
	}
	return string(body)
}

// promptOption is one entry in a chain prompt's label/legend pair. key
// is the single rune the operator types (uppercase for the default);
// hint is the short verb shown in the legend. Two helpers turn a
// []promptOption into the bracketed label and the indented legend
// below it, so adding or reordering options stays a one-line edit at
// the call site.
type promptOption struct {
	key  rune
	hint string
}

// renderPromptLabel returns "[K1/K2/K3]" — bracketed, slash-separated
// runes in slice order. The first option's case determines the default
// (Y vs N today); the helper does not enforce it.
func renderPromptLabel(opts []promptOption) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, o := range opts {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteRune(o.key)
	}
	b.WriteByte(']')
	return b.String()
}

// renderPromptLegend returns the one-line legend printed below the
// label: two-space indent, "K = hint" pairs joined with " · ". The space
// padding around `=` keeps the bare `!` entry from rendering as `≠`
// under programming ligatures (Fira Code, JetBrains Mono, Cascadia
// Code, Iosevka, …); padding every entry costs nothing and reads
// better across the board. Lowercase verbs read consistently across
// the three prompts.
func renderPromptLegend(opts []promptOption) string {
	var b strings.Builder
	b.WriteString("  ")
	for i, o := range opts {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%c = %s", o.key, o.hint)
	}
	return b.String()
}

// promptStageNextStage offers the non-push stage prompt: [Y/n] for
// most workflows, with `s` appended when the next stage is sdlc's test
// (the skip shortcut jumps straight to the push prompt), and optional
// /x and /b suffixes when scuttle / back are non-nil. Y still defaults
// so a reflex Enter chains the next stage interactively, the same as
// before. Workflows with a registered cascade dispatcher (sdlc, twin)
// also surface `!` as a peer in the bracket and main legend — single
// keystroke, dispatch one stage headless — while `!<stage>` / `!!` /
// `!!!` stay on a second cascade-extras line below. `b` re-invokes the
// just-finished stage interactively. `x` dispatches the workflow's
// close command for the current run — the "abandon ship" path the
// operator forms at the same surface they decline from. `s` opens
// the push prompt early without satisfying the test stage: useful
// for doc-only diffs and trivial fixes where the anti-theater rule
// that test stage enforces would just produce a rubber-stamp canvas.
// Hardcoding the sdlc gate keeps the prompt honest — no other
// workflow has a test stage today, and we'd rather widen deliberately
// than offer affordances that don't exist.
//
// `x` is positioned adjacent to `n` (decline) because both read as "no":
// scuttle is "no, and also close this run." Grouping the two negatives
// reads better than appending `x` at the tail, and it leaves the
// forward-leaning `s` / `b` slots in their familiar positions.
//
// When the next stage is code, the just-finished design canvas is
// printed above the prompt — same shape as promptPushNextStage prints
// the code canvas above [N/m/p]. This is the operator's one chance to
// read the canvas at the design→code gate before authorising the next
// stage. The [Y/n] default stays Y: design→code is reversible (re-design
// after a botched code run), so the canvas read is informative, not
// gating. Whitespace-only or missing canvas falls through to the bare
// prompt, no header or decoration.
func promptStageNextStage(next *Command, back []*Command, scuttle *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	// Surface the just-finished stage's canvas above the prompt so the
	// operator reads it before authorising the next stage. The pairing
	// is the workflow's prereq edge — design → code, code → review,
	// vision → architecture, …, glossary → finalize. Same shape
	// promptPushNextStage uses for test → push. Falls back to the
	// hardcoded design/code mapping when the workflow registry isn't
	// reachable (tests against a throwaway workflow).
	priorCanvas := ""
	if wf, err := LookupWorkflow(md.Workflow); err == nil {
		if prereqs := wf.Prereqs(next.Name); len(prereqs) > 0 {
			priorCanvas = prereqs[0]
		}
	}
	if priorCanvas == "" {
		switch next.Name {
		case "code":
			priorCanvas = "design"
		case "test":
			priorCanvas = "review"
		}
	}
	if priorCanvas != "" {
		if body := readPrintableCanvas(root, md, priorCanvas); body != "" {
			fmt.Fprint(stdout, body)
			if !strings.HasSuffix(body, "\n") {
				fmt.Fprintln(stdout)
			}
		}
	}
	opts := []promptOption{
		{key: 'Y', hint: "run"},
		{key: 'n', hint: "decline"},
	}
	if scuttle != nil {
		opts = append(opts, promptOption{key: 'x', hint: "scuttle (close)"})
	}
	// `a` is "decline running the next stage, but record the just-finished
	// one as done" — the click-forward key. Without it, declining parks the
	// run at the just-finished stage and the next pickup re-opens and
	// re-runs its agent (Workflow.Next reports the parked stage). The
	// advance marker satisfies that stage so the next pickup starts at the
	// successor instead. Gated to sdlc's gates (design→code, code→review,
	// review→test), where priorCanvas names the stage to mark; other gates
	// (the twin ladder, idea) keep the plain decline.
	offerAdvance := md.Workflow == "sdlc" && priorCanvas != ""
	if offerAdvance {
		opts = append(opts, promptOption{key: 'a', hint: "decline, advance to " + next.Name})
	}
	dispatcher := lookupCascadeDispatcher(md.Workflow)
	// `s` is the cascade-only shortcut to jump from post-code straight
	// to the push prompt, skipping test. Gated to sdlc + next.Name ==
	// "test" so the option only shows up at the exact gate it makes
	// sense: post-code, where the next thing the chain would do is open
	// test. Other prompts (post-design, non-sdlc workflows) leave this
	// off — they have no test stage to skip over.
	offerSkipToPush := md.Workflow == "sdlc" && next.Name == "test"
	if offerSkipToPush {
		opts = append(opts, promptOption{key: 's', hint: "skip to push"})
	}
	if len(back) > 0 {
		opts = append(opts, promptOption{key: 'b', hint: backHint(back)})
	}
	if dispatcher != nil {
		// `!` is a single-keystroke peer to Y/n/x/s/b — it dispatches
		// exactly one stage headless. Living in the main legend (and
		// the bracket) is what keeps it visible; the cascade-extras
		// line below only covers the genuinely multi-character forms.
		opts = append(opts, promptOption{key: '!', hint: "cascade one stage"})
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "next: %s — run now? %s\n", hint, label)
	moePrintln(stdout, renderPromptLegend(opts))
	if dispatcher != nil {
		moePrintln(stdout, "  !<stage> = cascade to gate · !! = ship this run · !!! = ship + ride the chain · !!!! = ride dynamic")
	}
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		// Cooked-mode Ctrl-C used to trap the runtime. Treat it as
		// "decline" — safer default than guessing — and exit cleanly
		// so the chain can unwind.
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if dispatcher != nil && strings.HasPrefix(answer, "!") {
		return dispatchCascade(answer, next.Name, root, md, stdout, stderr)
	}
	if scuttle != nil && answer == "x" {
		return scuttle.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
	}
	if offerSkipToPush && answer == "s" {
		// Skip-to-push opens the push prompt directly without
		// satisfying test. The push command lives in the same group as
		// the just-decline-able test stage; look it up the same way
		// promptNextStage does for the natural cascade. `back` is the
		// same prior-stage list this prompt is offering (justFinished ==
		// "code" upstream, so back contains design+code as appropriate),
		// which is the right back target for the push prompt: the
		// operator's mental "just finished" is code; test was the one
		// they elected to skip. A workflow that registers test but no
		// push wouldn't make sense, but we stay nil-safe just in case.
		g, err := LookupGroup(md.Workflow)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		pushCmd := g.Lookup("push")
		if pushCmd == nil {
			moePrintf(stderr, "workflow %q has no push command\n", md.Workflow)
			return 1
		}
		// No prompt-time synthesis here either — the `s` shortcut takes
		// the same path as the natural post-test cascade: straight to the
		// ship gate, where the operator chooses and the push command runs
		// synthesis if they actually ship.
		pushHint := fmt.Sprintf("moe %s %s %s/%s", md.Workflow, pushCmd.Name, md.Project, md.ID)
		return promptPushNextStage(pushCmd, back, scuttle, root, md, pushHint, stdout, stderr)
	}
	if len(back) > 0 && answer == "b" {
		return dispatchBack(back, md, stdout, stderr)
	}
	if offerAdvance && answer == "a" {
		// Mark the just-finished stage (priorCanvas) done, then stop —
		// `a` is a decline, so the next stage is not dispatched. The
		// marker makes Workflow.Next return the successor on the next
		// pickup instead of re-opening priorCanvas.
		if err := commitAdvance(root, md, priorCanvas); err != nil {
			moePrintf(stderr, "advance: %v\n", err)
			return 1
		}
		moePrintf(stdout, "advanced past %s — next pickup of %s/%s starts at %s\n",
			priorCanvas, md.Project, md.ID, next.Name)
		return 0
	}
	accepted := answer == "" || strings.HasPrefix(answer, "y")
	if !accepted {
		return 0
	}
	return next.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
}

// promptKickback is the chain prompt shown after an interactive sdlc
// review or test session closes `blocked`. Instead of the forward
// [Y/n/…] offer (which would walk past the objection on a reflex
// Enter), it offers `[Y/n/d/x]`: Y kicks back to code with the findings
// as the re-opened stage's kickoff, d kicks back to design instead, n
// parks the run, x scuttles it. The default Y makes the common path —
// "the reviewer/tester is right, go fix the code" — a single Enter.
//
// The blocked canvas is printed above the menu so the operator reads
// the findings before choosing — same shape promptStageNextStage /
// promptPushNextStage print the prior canvas above their prompts. The
// re-opened stage carries NextStageOverride set to the blocked stage,
// so the post-fix chain prompt re-offers review/test (the gate that
// blocked), not its successor.
//
// Caller responsibility: gate on stdinIsTerminal() before invoking
// (promptNextStageOverride does, falling back to a back-pointing nudge
// for non-TTY callers) — same contract as promptCloseNextStage.
func promptKickback(g *CommandGroup, scuttle *Command, md *run.Metadata, blockedStage, canvas string, stdout, stderr io.Writer) int {
	fmt.Fprint(stdout, canvas)
	if !strings.HasSuffix(canvas, "\n") {
		fmt.Fprintln(stdout)
	}
	hasDesign := g.Lookup("design") != nil
	opts := []promptOption{
		{key: 'Y', hint: "kick back to code"},
		{key: 'n', hint: "decline (park)"},
	}
	if hasDesign {
		opts = append(opts, promptOption{key: 'd', hint: "kick back to design"})
	}
	if scuttle != nil {
		opts = append(opts, promptOption{key: 'x', hint: "scuttle (close)"})
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "%s blocked — kick back to fix? %s\n", blockedStage, label)
	moePrintln(stdout, renderPromptLegend(opts))
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch {
	case scuttle != nil && answer == "x":
		return scuttle.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
	case hasDesign && answer == "d":
		return openKickbackSession(md, "design", blockedStage, canvas, false, stdout, stderr)
	case answer == "n":
		return 0
	case answer == "" || strings.HasPrefix(answer, "y"):
		return openKickbackSession(md, "code", blockedStage, canvas, false, stdout, stderr)
	}
	// Anything else — a typo — declines, same as the other chain prompts.
	return 0
}

// openKickbackSession opens the chosen back-target document ("code" or
// "design") as a recovery turn carrying the blocking findings, with the
// chain set to re-offer the stage that blocked. headless distinguishes
// the two callers: the interactive kickback (promptKickback) passes
// false and keeps the post-turn chain prompt; the headless ship cascade
// (cascadeStageGate) passes true for a bounded one-shot recovery whose
// gate the cascade re-checks itself.
//
// Wired to runKickbackSession in init() rather than referenced directly
// for the same reason pushFromCascade is: the var's initializer would
// otherwise trace openRecoveryStageSession → runStageSession (var) →
// promptNextStageOverride → promptKickback → openKickbackSession and the
// var-init dependency analyser would flag the chain as a cycle. Tests
// override this var to stub the dispatch.
var openKickbackSession func(md *run.Metadata, document, blockedStage, canvas string, headless bool, stdout, stderr io.Writer) int

func init() {
	openKickbackSession = runKickbackSession
}

func runKickbackSession(md *run.Metadata, document, blockedStage, canvas string, headless bool, stdout, stderr io.Writer) int {
	if headless {
		moePrintf(stderr, "       kicking back to %s for %s-blocked (headless); the cascade re-checks %s after the fix lands\n",
			document, blockedStage, blockedStage)
	} else {
		moePrintf(stderr, "       kicking back to %s for %s-blocked; the chain prompt will re-offer %s after the fix lands\n",
			document, blockedStage, blockedStage)
	}
	kickoff := buildKickbackKickoff(md.Workflow, blockedStage, canvas)
	return openRecoveryStageSession(md, document, blockedStage, headless, kickoff, stdout, stderr)
}

// promptCloseNextStage is the terminal-stage analogue of
// promptStageNextStage / promptPushNextStage: the last committed stage
// left the run `done` but `in_progress`, and the workflow has a `close`
// command, so the chain prompt asks `[Y/n/x]` instead of printing a
// copy-paste hint. Y dispatches close interactively (no `--no-edit` —
// the operator just typed Y, so opening the followups editor is the
// right behaviour); n leaves the run at done · close? for the operator
// to come back to; x is an alias for Y (muscle-memory consistency with
// the other prompts, where `x` is "scuttle (close)" — at this gate
// close IS the next step, so the alias collapses to the same dispatch).
//
// Caller responsibility: gate on stdinIsTerminal() before invoking. The
// non-TTY branch retains the print-only nudge so headless callers
// (`moe twin reflect ... < /dev/null`) never close silently.
func promptCloseNextStage(closeCmd *Command, justFinished string, md *run.Metadata, stdout, stderr io.Writer) int {
	opts := []promptOption{
		{key: 'Y', hint: "close"},
		{key: 'n', hint: "decline"},
		{key: 'x', hint: "close (alias)"},
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "%s sealed — close run now? %s\n", justFinished, label)
	moePrintln(stdout, renderPromptLegend(opts))
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		// SIGINT at this prompt collapses to decline, same shape the
		// other chain prompts use — the operator can always re-run
		// `moe <wf> close` later.
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	accepted := answer == "" || strings.HasPrefix(answer, "y") || answer == "x"
	if !accepted {
		return 0
	}
	return closeCmd.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
}

// promptPushNextStage offers three choices: decline (default), merge
// (`moe <wf> push`), or PR (`moe <wf> push --pr`), plus optional `x`
// (scuttle: dispatch the workflow's close) and `b` (re-open the
// just-finished code stage) suffixes when the respective commands are
// non-nil. Parsing is case-insensitive; the label capitalization just
// signals the default. N-as-default is load-bearing — a reflex Enter
// must never ship, and must never terminate the run either; scuttle is
// explicit-only.
//
// `x` sits adjacent to N (decline): both are "no" answers; scuttle is
// "no, and close." The forward-leaning m/p/b slots keep their familiar
// positions for muscle memory.
//
// The just-finished stage's canvas is printed above the prompt so the
// operator reads the agent's pre-push framing at the exact moment
// they're deciding whether to ship. With test stage in place, that's
// the test canvas — the verification narrative is the more direct
// "should we ship?" lens than the code canvas (which holds the PR
// body but is one stage back). When the test canvas is missing or
// whitespace-only (the operator skipped test via the post-code `s`
// shortcut, or invoked `moe sdlc push` directly without test having
// landed), fall back to the code canvas: the operator's last reading
// material before the ship decision should still be the most recent
// thing the agent wrote. This is the operator's one chance to read
// the canvas at this gate. By the time promptNextStage fires,
// session.Close has already rebased the session onto main, so root is
// the right base for the read. If both canvases are missing or
// whitespace-only, the prompt prints bare — no header or decoration.
// The canvas is markdown the agent wrote for the operator, printed
// as written.
//
// The push canvas is deliberately not in this fallback chain. Synthesis
// runs inside the chosen push command, not at chain-prompt time, so by
// the time the operator reads this preamble the push canvas (if any) may
// be left over from a prior push attempt — stale relative to whatever
// the operator's about to do. Test → code is the live story.
func promptPushNextStage(next *Command, back []*Command, scuttle *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	body := readPrintableCanvas(root, md, "test")
	if body == "" {
		body = readPrintableCanvas(root, md, "code")
	}
	if body != "" {
		fmt.Fprint(stdout, body)
		if !strings.HasSuffix(body, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	opts := []promptOption{
		{key: 'N', hint: "decline"},
	}
	if scuttle != nil {
		opts = append(opts, promptOption{key: 'x', hint: "scuttle (close)"})
	}
	opts = append(opts,
		promptOption{key: 'm', hint: "fast-forward merge"},
		promptOption{key: 'p', hint: "open PR"},
	)
	if len(back) > 0 {
		opts = append(opts, promptOption{key: 'b', hint: backHint(back)})
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "next: %s — run now? %s\n", hint, label)
	moePrintln(stdout, renderPromptLegend(opts))
	if md.Workflow == "sdlc" {
		moePrintln(stdout, "  !! = ship this run · !!! = ship + ride the chain · !!!! = ride dynamic (the machine may extend it)")
	}
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		// N is already the default here; SIGINT collapses to the same
		// safe sentinel and avoids the runtime trap.
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch answer {
	case "m":
		return next.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
	case "!!":
		// `!!` at the push gate uses the typed cascade push path, not
		// Command.Run: cascade harvest is --no-edit even though manual
		// `m` keeps the editor pop.
		return promptPushCascadeShip(md, false, rideNone, stdout, stderr)
	case "!!!":
		// Same typed cascade push path as `!!`, plus the chain ride.
		return promptPushCascadeShip(md, true, rideStatic, stdout, stderr)
	case "!!!!":
		// Same ride, with the dynamic license: the tail pulse may groom
		// onto the ridden unit's tail and — on an unchained run, where
		// this is the whole point — kick a thread it just groomed.
		return promptPushCascadeShip(md, true, rideDynamic, stdout, stderr)
	case "p":
		return next.Run([]string{"--pr", md.Project + "/" + md.ID}, stdout, stderr)
	case "x":
		if scuttle != nil {
			return scuttle.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
		}
	case "b":
		if len(back) > 0 {
			return dispatchBack(back, md, stdout, stderr)
		}
	}
	if md.Workflow == "sdlc" && strings.HasPrefix(answer, "!") {
		// `!<stage>` at the push gate is a no-op: every named stage
		// is at or behind the current gate. Print a hint so the
		// operator sees why nothing happened — then decline, same as
		// any other typo at this prompt.
		wf, werr := LookupWorkflow(md.Workflow)
		if werr == nil {
			moePrintf(stderr,
				"cascade: `%s` is at or behind the push gate; type `!!` (ship this run) / `!!!` (ship + ride the chain) / `!!!!` (ride dynamic) or pick m/p/n/x/b. (stages: %s)\n",
				answer, strings.Join(wf.Stages(), ", "))
		}
		return 0
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
	return 0
}

func promptPushCascadeShip(md *run.Metadata, rideChain bool, mode rideMode, stdout, stderr io.Writer) int {
	defer withRideMode(mode)()
	steps, shipped, code := cascadeShipStep(md.Workflow, md, rideChain, stdout, stderr)
	res := cascadeResult{ran: steps, shipped: shipped}
	if summary := renderCascadeSummary(md.Project+"/"+md.ID, res); summary != "" {
		moePrintln(stdout, summary)
	}
	return code
}

// dispatchCascade parses the operator's `!`, `!<stage>`, `!!`, or `!!!` answer
// at a non-push chain prompt, validates the destination against the
// workflow's stage ladder, walks the cascade, and either re-enters
// the chain at the destination gate or returns 0 if the cascade
// shipped. answer is already lowercased and trimmed. startStage is
// the next-to-run stage at the current gate (promptStageNextStage's
// next.Name) — the cascade's first dispatch.
//
// Bare `!` dispatches exactly one stage (startStage) headless and
// re-prompts at the resulting gate. `!<stage>` walks headless up to
// but not including the named stage. `!!` walks every remaining stage
// headless and ships **this run** (or auto-closes, for workflows
// without push), then stops. `!!!` is the same walk as `!!` but rides
// the whole chain — after this run ships it cascades into the next
// live chained child.
//
// Returns the exit code to bubble up: the failing stage's code on a
// cascade failure, 0 on a successful park-at-gate or ship.
func dispatchCascade(answer, startStage, root string, md *run.Metadata, stdout, stderr io.Writer) int {
	// The consent level the operator just typed, held for the whole
	// dispatch so every tail pulse under it — this run's and every run
	// the ride reaches — reads the same mode. See ridemode.go.
	defer withRideMode(rideModeForAnswer(answer))()

	var destination string
	oneStep := false
	rideChain := false
	switch {
	case answer == "!!":
		// Ship this run and stop — no chain ride.
	case answer == "!!!", answer == "!!!!":
		// Both ride; the fourth bang differs only in what the tail
		// pulse may do, which travels as the ride mode, not here.
		rideChain = true
	case answer == "!":
		oneStep = true
	default:
		destination = strings.TrimPrefix(answer, "!")
		wf, err := LookupWorkflow(md.Workflow)
		if err != nil {
			moePrintf(stderr, "%v\n", err)
			return 1
		}
		stages := wf.Stages()
		if indexOfString(stages, destination) < 0 {
			moePrintf(stderr,
				"cascade: unknown stage %q for %s; try: %s\n",
				destination, md.Workflow, strings.Join(stages, ", "))
			return 0
		}
	}
	res, code := cascadeFromGate(startStage, destination, oneStep, rideChain, md, stdout, stderr)
	if summary := renderCascadeSummary(md.Project+"/"+md.ID, res); summary != "" {
		moePrintln(stdout, summary)
	}
	if code != 0 {
		return code
	}
	if res.shipped {
		return 0
	}
	// Re-enter the chain at the destination gate. The last cascaded
	// stage is the `justFinished` anchor for promptNextStage's
	// successor lookup. An empty res (no-op cascade for a destination
	// at or behind the start) lands back at the same gate via the
	// Next() fallback inside promptNextStage.
	var lastStage string
	if len(res.ran) > 0 {
		lastStage = res.ran[len(res.ran)-1].stage
	}
	return promptNextStage(root, md, lastStage, stdout, stderr)
}

// pushFromCascade is the typed push entry the cascade's `!!` / `!!!` step
// calls into. Wired to runPushTyped in init() rather than referenced
// directly so the var-init dependency analyser doesn't trace through
// cascadeFromGate → runPushTyped → openCodeSessionFor… (var) →
// runStageSession (var) → promptNextStage → … → cascadeFromGate and
// flag the chain as an initialization cycle. Tests override this
// var to stub the cascade's push step without touching the standalone
// `moe sdlc push` path (which still goes through pushCmd.Run →
// runPushTyped → discard-error).
var pushFromCascade func(workflow string, args []string, opts pushRunOptions, stdout, stderr io.Writer) (int, bool, error)

func init() {
	pushFromCascade = runPushTypedWithOptions
}

// cascadeStepResult records one dispatched stage's outcome. deferred
// is non-empty only when push's pre-push gate handed off to a
// recovery code session — its value ("rebase-conflict" or
// "hook-failure") is what the summary renders inside
// `push deferred to recovery (...)`. A deferred step is a stop, not
// a ship, regardless of the recovery session's own exit code.
type cascadeStepResult struct {
	stage    string
	code     int
	deferred string
}

// cascadeResult is what cascadeFromGate hands back: the ordered list
// of dispatched stages plus a flag set only when `!!` / `!!!` completed the
// ship. The summary line renders from this.
type cascadeResult struct {
	ran     []cascadeStepResult
	shipped bool
}

// cascadeFromGate dispatches stages from startStage up to, but not
// including, destination. An empty destination is the cascade-to-ship
// variant (`!!` and `!!!`): it walks every remaining stage headless and
// ships at push. rideChain distinguishes the two — false for `!!` (ship
// this run, then stop), true for `!!!` (ship, then ride into the next
// live chained child). Every cascaded stage runs headless regardless;
// rideChain only governs the chain-ride hook after a successful ship.
// When oneStep is true, destination is ignored and the cascade
// dispatches exactly one stage (startStage) — the bare-`!` form.
// oneStep at the terminal stage still dispatches that stage without
// shipping or auto-closing; that's what distinguishes it from `!!` / `!!!`.
//
// startStage is the next-to-run stage at the operator's current
// gate. destination is the stage the operator named in `!<stage>`;
// both have been validated by the caller against the workflow ladder.
// A destination at or behind startStage produces a no-op cascade and
// exit 0.
//
// Each stage dispatch goes through the workflow's registered
// dispatcher — the Go-level seam the cascade driver (`!` / `!<stage>`
// / `!!` / `!!!`) reaches into — so stage-specific pre-flight
// (requireDesignCanvas, requireCodeCanvas, canvas skeleton seeding)
// still fires. Every dispatch is headless, and a headless turn skips
// its own inner promptNextStage (runStageSession's tail guards on
// opts.Headless), so the cascade owns routing.
//
// At push in cascade-to-ship mode the dispatch is the merge path
// (pushCmd.Run with no flags). `!!` and `!!!` default to fast-forward
// merge; runPushTyped writes the merge-path push note after deterministic
// hooks and shipping.
func cascadeFromGate(startStage, destination string, oneStep bool, rideChain bool, md *run.Metadata, stdout, stderr io.Writer) (cascadeResult, int) {
	var res cascadeResult
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return res, 1
	}
	stages := wf.Stages()
	startIdx := indexOfString(stages, startStage)
	if startIdx < 0 {
		moePrintf(stderr, "cascade: unknown start stage %q for %s\n", startStage, md.Workflow)
		return res, 1
	}
	endIdx := len(stages)
	yolo := !oneStep && destination == ""
	switch {
	case oneStep:
		// Dispatch exactly one stage. The terminal-stage case (twin's
		// finalize from post-glossary) falls through naturally: endIdx
		// = startIdx + 1 still walks one stage and the !yolo gate
		// below skips the post-loop auto-close.
		endIdx = startIdx + 1
	case !yolo:
		destIdx := indexOfString(stages, destination)
		if destIdx < 0 {
			moePrintf(stderr, "cascade: unknown destination stage %q for %s\n", destination, md.Workflow)
			return res, 1
		}
		if destIdx <= startIdx {
			// Destination is the current gate (re-prompt) or behind
			// it (no-op). Either way nothing to dispatch.
			return res, 0
		}
		endIdx = destIdx
	}
	dispatcher := lookupCascadeDispatcher(md.Workflow)
	if dispatcher == nil {
		moePrintf(stderr, "cascade: workflow %q has no cascade dispatcher\n", md.Workflow)
		return res, 1
	}
	g, err := LookupGroup(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return res, 1
	}
	for i := startIdx; i < endIdx; i++ {
		stage := stages[i]
		moePrintf(stdout, "cascade: %s (headless)\n", stage)
		if stage == "push" && yolo {
			// The sdlc terminal ship — merge-path push, its bounded
			// retry-after-recovery loop, and (for `!!!`) the chain ride —
			// lives in cascadeShipStep so this loop body reads uniformly.
			// shipped is false for any deferred-stop or push failure;
			// code carries the exit to propagate. Return early unless the
			// ship completed cleanly.
			steps, shipped, code := cascadeShipStep(md.Workflow, md, rideChain, stdout, stderr)
			res.ran = append(res.ran, steps...)
			res.shipped = res.shipped || shipped
			if code != 0 || !shipped {
				return res, code
			}
			continue
		}
		code := dispatcher(stage, md.Project, md.ID, true, stdout, stderr)
		res.ran = append(res.ran, cascadeStepResult{stage: stage, code: code})
		if code != 0 {
			return res, code
		}
		steps, parked, gateCode := cascadeStageGate(wf, dispatcher, md, stage, yolo, stdout, stderr)
		res.ran = append(res.ran, steps...)
		if parked {
			// Blocked review/test gate on a non-yolo cascade: stop the walk
			// and let dispatchCascade's tail anchor promptNextStage at this
			// stage (the last res.ran entry), where the kickback offer / nudge
			// fires. Exit 0 — a park at a gate, not a failure.
			return res, 0
		}
		if gateCode != 0 {
			return res, gateCode
		}
	}
	// `!!` / `!!!` for a workflow without push (twin today) auto-closes the run
	// after the last stage commits — same operator intent as the sdlc
	// push branch above ("cascade and terminate"), just routed through
	// close instead of push. sdlc set res.shipped=true in the push branch
	// already, so the gate skips it there. --no-edit keeps the close
	// non-interactive (followups.md harvests as-is); a hands-off cascade
	// should never block on an editor.
	if yolo && !res.shipped {
		// Chain ride (`!!!` only) fires before the synthetic auto-close
		// — per design, the ride sits between "all real stages
		// committed" and the terminal action. For sdlc the analogous
		// slot is "after push success" in the loop above; this branch is
		// the non-sdlc analogue. `!!` stops here — rideChain is the gate.
		// A Ctrl-C inside the ride halts the chain before the
		// auto-close, same as the sdlc push branch.
		//
		// An ordinary ride failure does not: this parent's own walk
		// succeeded, and skipping its close would park a dead run on the
		// dash to punish a child's failure. So remember the code, close,
		// and propagate below.
		var rideCode int
		if rideChain {
			rideCode = maybeRideChain(md, rideChain, stdout, stderr)
			if rideCode == exitInterrupted {
				return res, rideCode
			}
		}
		if closeCmd := g.Lookup("close"); closeCmd != nil {
			moePrintf(stdout, "cascade: close (headless)\n")
			// The tail pulse's interrupt is deliberately NOT mapped to
			// exitInterrupted here: this branch is non-sdlc only (sdlc ships
			// via the push branch above), and the chain ride already ran at
			// line ~1000, before this close. There is no ride left to skip,
			// so a Ctrl-C'd tail pulse is exactly the bare-close case — the
			// cascade's own work (every stage committed, run closed)
			// succeeded, so it exits on the close's own code, same rule as
			// `moe <wf> close`.
			code := closeCmd.Run([]string{"--no-edit", md.Project + "/" + md.ID}, stdout, stderr)
			res.ran = append(res.ran, cascadeStepResult{stage: "close", code: code})
			if code != 0 {
				return res, code
			}
			res.shipped = true
		}
		// The close's own code already returned above if non-zero — it is
		// this run's own failure and the nearer of the two. Otherwise the
		// stalled ride is what's left to report.
		return res, rideCode
	}
	return res, 0
}

var checkCascadeStageGate = func(wf *Workflow, md *run.Metadata, stage string, stderr io.Writer) (bool, int) {
	if !wf.HasStageGate(stage) {
		return true, 0
	}
	root, err := findRoot(stderr)
	if err != nil {
		return false, 1
	}
	ok, err := wf.CheckStageGate(root, md, stage)
	if err != nil {
		moePrintf(stderr, "cascade: %s gate: %v\n", stage, err)
		return false, 1
	}
	if !ok {
		moePrintf(stderr, "cascade: %s gate not satisfied; parked at %s\n", stage, stage)
		return false, 1
	}
	return true, 0
}

// cascadeStageGate evaluates a just-dispatched stage's gate and, on a
// headless ship cascade (`!!` / `!!!`) that hit a *blocked* sdlc
// review/test gate, makes one bounded headless kickback to code before
// re-dispatching the stage and re-checking once. It is the judgement-
// recovery analogue of cascadeShipStep's mechanical push retry: a real
// review finding no longer dies silently in a headless chain, but the
// loop stays bounded at a single attempt — if the fix doesn't stick the
// run parks exactly as before. retries mirrors cascadeShipStep's
// `retries >= 1` shape: one try per gate, then stop.
//
// Recovery is gated to the ship cascade (yolo). A non-yolo cascade
// (`!` / `!<stage>` / `--once` / `--to=`) that hits a *blocked* sdlc
// review/test gate does not dead-end: it returns parked=true (code 0,
// no steps), and cascadeFromGate stops the walk so dispatchCascade's
// tail re-enters the chain prompt at the blocked stage. There the
// kickback offer fires on a TTY, or the `moe sdlc code <run> (review
// blocked — kick back to fix)` nudge prints when headless — the "offer
// when a human is present, launch when headless" line the design draws.
// Any other non-yolo gate failure (test gate refusing an unfilled
// skeleton, gate read errors) keeps the hard "parked at <stage>" +
// exit 1 below: a stage refusal stops the chain, and there is no
// kickback affordance to offer for it.
//
// The blocked check reads the canvas's gate status directly (mirroring
// the interactive headless nudge in promptNextStageOverride) rather than
// leaning on checkCascadeStageGate's pass/fail alone: only a literal
// "blocked" status earns a kickback (yolo) or a park-to-prompt
// (non-yolo). A test gate that fails on an unfilled skeleton (status
// "ready", empty sections) is not a finding a code turn can resolve, so
// it parks without burning a recovery turn — and reading the status
// first keeps checkCascadeStageGate's "parked" line from printing on a
// recovery that ultimately succeeds.
//
// Returns the recovery/re-dispatch steps to append (empty when no
// recovery ran), a parked flag (set only for a non-yolo blocked
// review/test gate that should fall through to the chain prompt), and
// the exit code to propagate (0 = gate satisfied, proceed; non-zero =
// park).
func cascadeStageGate(wf *Workflow, dispatcher cascadeDispatcher, md *run.Metadata, stage string, yolo bool, stdout, stderr io.Writer) (steps []cascadeStepResult, parked bool, code int) {
	if !yolo && md.Workflow == "sdlc" && (stage == "review" || stage == "test") {
		if _, blocked := cascadeStageBlocked(md, stage, stderr); blocked {
			moePrintf(stderr, "cascade: %s closed blocked; parking at its gate\n", stage)
			return nil, true, 0
		}
	}
	retries := 0
	for {
		if yolo && retries < 1 && md.Workflow == "sdlc" && (stage == "review" || stage == "test") {
			if canvas, blocked := cascadeStageBlocked(md, stage, stderr); blocked {
				retries++
				// One headless kickback to code, carrying the blocking
				// canvas as kickoff — the same seam the interactive
				// kickback uses, run headless and re-checked by the loop.
				recCode := openKickbackSession(md, "code", stage, canvas, true, stdout, stderr)
				steps = append(steps, cascadeStepResult{stage: "code", code: recCode})
				if recCode != 0 {
					return steps, false, recCode
				}
				// Re-dispatch the blocked stage; the loop re-checks its gate.
				moePrintf(stdout, "cascade: %s (headless)\n", stage)
				dCode := dispatcher(stage, md.Project, md.ID, true, stdout, stderr)
				steps = append(steps, cascadeStepResult{stage: stage, code: dCode})
				if dCode != 0 {
					return steps, false, dCode
				}
				continue
			}
		}
		gateOK, gateCode := checkCascadeStageGate(wf, md, stage, stderr)
		if gateOK && gateCode == 0 {
			return steps, false, 0
		}
		return steps, false, gateCode
	}
}

// cascadeStageBlocked reports whether stage's canvas closed with a
// literal `blocked` gate, returning the canvas body so the caller can
// inline it into the kickback kickoff without a second read. A missing
// root, missing/empty canvas, or unparseable gate all read as
// not-blocked — the caller falls through to the canonical gate check.
func cascadeStageBlocked(md *run.Metadata, stage string, stderr io.Writer) (canvas string, blocked bool) {
	root, err := findRoot(stderr)
	if err != nil {
		return "", false
	}
	canvas = readPrintableCanvas(root, md, stage)
	if canvas == "" {
		return "", false
	}
	status, ok := stageGateStatus(canvas)
	return canvas, ok && status == "blocked"
}

// cascadeShipStep runs the sdlc terminal ship for a `!!` / `!!!` cascade:
// the merge-path push, its bounded retry-after-recovery loop, and (for
// `!!!`) the chain ride into the next live child. It owns the slice of
// cascadeFromGate the recent headless bugs lived in, so the stage loop
// above reads uniformly — dispatch each stage; the terminal ship is one
// call.
//
// `!!` / `!!!` at push ship via the merge path. runPushTyped owns
// synthesis before the shared ship gate, so the cascade drives the ship
// through the one typed push entry — no separate synthesis call. The push
// goes through pushFromCascade (bypassing g.Lookup("push")) so the
// deferred-to-recovery signal — when push's pre-push gate hands off to a
// fresh code session — comes back as a typed *PushDeferredError. The
// command-group indirection used by !<stage> cascades discards that error
// to preserve the Command.Run contract; only this path needs it. Without
// the typed channel, a recovery session that exits 0 would look identical
// to a real ship and the cascade summary would claim "shipped" when
// nothing was pushed.
//
// Headless (`!!!`) push earns one push retry after a clean recovery: when
// the pre-push gate hands off to a headless one-shot code session and
// that session resolves cleanly (exit 0, commit made), re-run the gate
// against the new commit. A resolved rebase fast-forwards on the retry; a
// fixed hook passes. Driven (`!!`) recovery is interactive — the operator
// picks up at the chain prompt, so retrying would race the human. retries
// bounds the loop at one: if the retry still defers, the fix didn't stick
// — stop with the deferred marker and let the operator look.
//
// Returns the dispatched step(s), whether the ship completed (false for
// any deferred-stop or push failure, true only for a real ship), and the
// exit code the cascade should propagate. The caller appends the steps,
// ORs shipped into its result, and returns early unless shipped is true
// and code is 0.
func cascadeShipStep(workflow string, md *run.Metadata, rideChain bool, stdout, stderr io.Writer) (steps []cascadeStepResult, shipped bool, code int) {
	retries := 0
	for {
		ship, interrupted, err := pushFromCascade(workflow, []string{md.Project + "/" + md.ID}, pushRunOptions{
			HeadlessRecovery: true,
			SkipTerminalEdit: true,
		}, stdout, stderr)
		var deferred *PushDeferredError
		if errors.As(err, &deferred) {
			// Record the deferred step even when a retry will follow: a
			// recover-then-ship reads honestly as two steps (`push
			// deferred to recovery (…) · push ok`).
			steps = append(steps, cascadeStepResult{
				stage:    "push",
				code:     ship,
				deferred: deferred.Recovery,
			})
			// Stop when: the recovery session gave up (non-zero exit), or
			// the one retry is already spent. The deferred marker keeps
			// the summary honest — this was not a ship.
			if ship != 0 || retries >= 1 {
				return steps, false, ship
			}
			// Headless clean recovery: the agent committed a fix. Re-run
			// the pre-push gate once against it.
			retries++
			continue
		}
		if ship != 0 {
			steps = append(steps, cascadeStepResult{stage: "push", code: ship})
			return steps, false, ship
		}
		if interrupted {
			// The ff-merge shipped, but the operator Ctrl-C'd the tail
			// pulse. Halt the chain before the ride — record the push step
			// as interrupted (not "ok") so the summary reads "push
			// interrupted — stopped", and propagate exitInterrupted so
			// everything above stops rather than riding on to the next run.
			steps = append(steps, cascadeStepResult{stage: "push", code: exitInterrupted})
			return steps, true, exitInterrupted
		}
		steps = append(steps, cascadeStepResult{stage: "push", code: ship})
		// Chain ride (`!!!` only): after the parent's terminal stage
		// ships, if a live chain edge points at an unresolved child,
		// cascade into it. `!!` stops here — rideChain is the gate.
		// Recursive by construction — the child's cascadeFromGate reaches
		// its own push (sdlc) or auto-close (non-sdlc) and re-fires this
		// hook on its own outgoing edge. A ride that stalls — Ctrl-C or
		// an ordinary stage failure — propagates back as this cascade's
		// exit code: the parent has already shipped, and shipped=true
		// records that honestly, but the invocation as a whole did not
		// come out clean and its caller deserves to know.
		if rideChain {
			if rideCode := maybeRideChain(md, rideChain, stdout, stderr); rideCode != 0 {
				return steps, true, rideCode
			}
		}
		return steps, true, 0
	}
}

// maybeRideChain dispatches the parent's chained child at the end of a
// `!!!` cascade, if a live unresolved edge exists. The caller gates the
// call on rideChain (only `!!!` rides); rideChain is threaded through to
// the child cascade so the ride propagates. The child opens at its
// first pending stage (Workflow.Next) and runs headless. Recursive by
// construction — the child's cascadeFromGate reaches its own terminal
// stage and re-fires this hook on its own outgoing edge, so `!!!` rides
// the whole chain in one shot.
//
// Returns the child cascade's exit code verbatim, for the caller to
// propagate. A ride that stalls is a failed invocation: `chain kick` is
// the programmatic entry point (cron, scripts), and a caller that can
// only tell a clean ride from a stalled one by scraping stderr has no
// usable signal. The parent's ship stays authoritative where it is
// actually recorded — the run still ships and its summary still reads
// "— shipped"; the process exit code describes the whole invocation,
// and the invocation included a ride that stalled. exitInterrupted
// keeps its existing meaning by falling out of the same rule.
//
// Recursion falls out: a grandchild failure surfaces as the child
// cascade's non-zero exit, which this propagates in turn, and each
// level prints its own "chain ride into … exited N" so the trail names
// the stalled run at every depth.
//
// Best-effort otherwise: every no-op mode (missing index, malformed
// child key, terminal child, child missing from disk, nothing pending)
// returns 0 — no ride happened, which is not a failure — silently
// except where it's worth a stderr line (the index can't be built, or
// a non-interrupt child cascade exits non-zero).
//
// findRoot is re-derived here so the helper's signature stays a
// drop-in at any cascade seam; the cost is one extra git rev-parse
// per terminal stage in a chain, which is rounding error next to the
// stage dispatch itself.
func maybeRideChain(parentMD *run.Metadata, rideChain bool, stdout, stderr io.Writer) int {
	root, err := findRoot(stderr)
	if err != nil {
		return 0
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "chain ride: build index: %v\n", err)
		return 0
	}
	parentKey := parentMD.Project + "/" + parentMD.ID
	childKey := idx.ChainedChild[parentKey]
	if childKey == "" {
		return 0
	}
	childProj, childID, err := splitProjectRun(childKey)
	if err != nil {
		moePrintf(stderr, "chain ride: malformed child key %q: %v\n", childKey, err)
		return 0
	}
	childMD, err := run.Load(root, childProj, childID)
	if err != nil {
		// Trailer references a child that doesn't exist on disk
		// (race with delete, or a typo from a hand-edited file).
		// Quiet skip — surfacing it on every ride would spam.
		return 0
	}
	switch childMD.Status {
	case run.StatusClosed, run.StatusMerged, run.StatusPromoted, run.StatusPushed:
		// Decision 1: terminal children skipped at the chain prompt.
		// StatusPushed too — there's no stage left for the cascade
		// to drive, only a human-owed merge click.
		return 0
	}
	wf, err := LookupWorkflow(childMD.Workflow)
	if err != nil {
		moePrintf(stderr, "chain ride: %v\n", err)
		return 0
	}
	nextStage, kind, err := wf.Next(root, childMD)
	if err != nil {
		moePrintf(stderr, "chain ride: next stage: %v\n", err)
		return 0
	}
	if kind != NextKindStage || nextStage == "" {
		// Nothing pending — done-but-not-closed runs surface a
		// `· close?` hint in the dash already; chain ride does not
		// auto-close someone else's parked run.
		return 0
	}
	moePrintf(stdout, "chain: riding into %s at %s (headless)\n", childKey, nextStage)
	childRes, childCode := cascadeFromGate(nextStage, "", false, rideChain, childMD, stdout, stderr)
	if summary := renderCascadeSummary(childKey, childRes); summary != "" {
		moePrintln(stdout, summary)
	}
	if childCode != 0 && childCode != exitInterrupted {
		// Interrupt needs no line — the child summary already read
		// "… interrupted — stopped". Every other failure gets the
		// pointer at *which* run stalled, since the code alone doesn't
		// say and a deep chain may print several.
		moePrintf(stderr, "chain ride into %s exited %d\n", childKey, childCode)
	}
	return childCode
}

// renderCascadeSummary formats the single-line summary printed after
// a cascade finishes (success or failure). runKey is the project/run
// the cascade ran, rendered on the line so stacked summaries (a `!!`
// chain riding from one run into the next) are told apart. Empty res
// (no-op cascade) returns "" so the caller skips the print — there's
// nothing to summarise.
//
// Examples:
//
//	cascade moe/run: code ok · test ok
//	cascade moe/run: code failed (exit 1) — stopped
//	cascade moe/run: code ok · test ok · push ok — shipped
//	cascade moe/run: code ok · test failed (exit 2) — stopped
//	cascade moe/run: code ok · test interrupted — stopped
//	cascade moe/run: code ok · test ok · push deferred to recovery (rebase conflict) — stopped
//	cascade moe/run: code ok · test ok · push deferred to recovery (pre-push hook) — stopped
func renderCascadeSummary(runKey string, res cascadeResult) string {
	if len(res.ran) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res.ran))
	for _, r := range res.ran {
		switch {
		case r.deferred != "":
			parts = append(parts, fmt.Sprintf("%s deferred to recovery (%s)", r.stage, deferredLabel(r.deferred)))
		case r.code == exitInterrupted:
			// An operator Ctrl-C, not a stage failure — read it as
			// "interrupted" so the summary doesn't libel a clean,
			// cut-short turn as a barf. The trailing "— stopped" clause
			// still fires below (code != 0), which is correct: the
			// cascade did stop here.
			parts = append(parts, fmt.Sprintf("%s interrupted", r.stage))
		case r.code != 0:
			parts = append(parts, fmt.Sprintf("%s failed (exit %d)", r.stage, r.code))
		default:
			parts = append(parts, fmt.Sprintf("%s ok", r.stage))
		}
	}
	s := "cascade " + runKey + ": " + strings.Join(parts, " · ")
	// The last recorded step decides the trailing clause. A deferred
	// step that was followed by a clean push retry must not force
	// "stopped" — the cascade recovered and shipped, and the final
	// `push ok` step says so. Only a trailing deferred or failed step
	// (the cascade really stopped there) renders "— stopped".
	last := res.ran[len(res.ran)-1]
	switch {
	case last.deferred != "" || last.code != 0:
		s += " — stopped"
	case res.shipped:
		s += " — shipped"
	}
	return s
}

// deferredLabel turns the *PushDeferredError.Recovery tag into the
// human phrase the summary renders. "rebase-conflict" stays
// "rebase conflict" (drop the dash); "hook-failure" reads as
// "pre-push hook" — the only event that defers today, and the phrase
// the operator already saw in the recovery session's kickoff.
// Unknown tags fall through to the raw value so a future recovery
// flavour is at least legible; the test wires it into the canonical
// renderings.
func deferredLabel(recovery string) string {
	switch recovery {
	case "rebase-conflict":
		return "rebase conflict"
	case "hook-failure":
		return "pre-push hook"
	default:
		return recovery
	}
}

// indexOfString returns the index of s in xs, or -1 if absent.
func indexOfString(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// cascadeDispatcher is the Go-level seam a workflow's per-stage init
// registers so the chain prompt's cascade driver (`!` / `!<stage>` /
// `!!` / `!!!`) can drive a stage without a hardcoded switch on
// workflow name. The contract matches openSdlcStage / openTwinStage
// exactly: take (stage, projectID, runID, headless, stdout, stderr),
// invoke the right per-stage helper, return its exit code. Every
// cascade dispatch is headless, and a headless turn skips the post-turn
// prompt structurally (runStageSession's tail guards on opts.Headless),
// so there is no separate "suppress next stage" flag to thread.
type cascadeDispatcher func(stage, projectID, runID string, headless bool, stdout, stderr io.Writer) int

var cascadeDispatchers = map[string]cascadeDispatcher{}

// registerCascadeDispatcher wires a workflow's cascade dispatcher into
// the registry. Called from each workflow's init() so the chain-prompt
// and cascade machinery can stay workflow-agnostic.
// Panics on duplicate names — same fail-loud contract as
// RegisterWorkflow.
func registerCascadeDispatcher(workflow string, d cascadeDispatcher) {
	if _, dup := cascadeDispatchers[workflow]; dup {
		panic("cli: duplicate cascade dispatcher for workflow " + workflow)
	}
	cascadeDispatchers[workflow] = d
}

// lookupCascadeDispatcher returns the registered dispatcher for
// workflow, or nil if none. nil means "this workflow has no cascade
// dispatch wired" — the chain prompt suppresses the cascade legend and
// the cascade refuses to walk.
func lookupCascadeDispatcher(workflow string) cascadeDispatcher {
	return cascadeDispatchers[workflow]
}
