package cli

import (
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
// don't know which stage just landed (resume hitting the merge gate).
//
// justFinished also drives the "back" targets offered at the prompt:
// a non-empty justFinished resolves to the list of stages strictly
// prior in the workflow ladder, each looked up against the paired
// command group. The prompt offers `b` to jump back to any of them —
// a single prior stage runs directly, multiple stages route through a
// sub-prompt that asks which to re-open. This lets the operator skip
// over intermediate stages (test → design) instead of stepping back
// one at a time.
func promptNextStage(root string, md *run.Metadata, justFinished string, stdout, stderr io.Writer) int {
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	var stage string
	if justFinished != "" {
		stage = wf.Successor(justFinished)
		if stage == "" {
			return 0
		}
	} else {
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
	hint := fmt.Sprintf("moe %s %s %s %s", wf.Name, next.Name, md.Project, md.ID)
	if !stdinIsTerminal() {
		moePrintf(stdout, "next: %s\n", hint)
		return 0
	}
	switch next.Name {
	case "push":
		// The ship gate prints immediately after test closes — synthesis
		// no longer fires at chain-prompt time. The operator's modal
		// answer here is `N` (decline), and on `m` the merge commit body
		// is bare anyway; paying a `claude -p` round-trip for either is
		// waste. Synthesis runs inside `push --pr` itself, where its
		// output actually lands on the PR.
		return promptPushNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
	}
	return promptStageNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
}

// backTargets returns the prior stages the chain prompt's `b` should
// route to: every stage strictly before justFinished in the workflow
// ladder, mapped through the paired command group. An empty
// justFinished (fresh-run callers) gets an empty slice — no prior
// stage to re-open. Stages whose command group has no matching verb
// (idea is the only such case today) are skipped silently so the
// operator only sees options that actually dispatch.
//
// Today every workflow's ladder is linear, so "prior" means "appears
// earlier in wf.Stages()". A fan-in or fan-out would need a richer
// answer; surface the design question if and when one shows up.
func backTargets(wf *Workflow, g *CommandGroup, justFinished string) []*Command {
	if justFinished == "" {
		return nil
	}
	var back []*Command
	for _, s := range wf.Stages() {
		if s == justFinished {
			break
		}
		if cmd := g.Lookup(s); cmd != nil {
			back = append(back, cmd)
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
		return back[0].Run([]string{md.Project, md.ID}, stdout, stderr)
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
	return cmd.Run([]string{md.Project, md.ID}, stdout, stderr)
}

// backHint formats the legend phrase for the `b` option. Single
// target reads "back to <stage>" so muscle memory still parses the
// option without typing it; multiple targets read "back to prior
// stage" — the operator picks the specific stage at the sub-prompt
// after typing `b`.
func backHint(back []*Command) string {
	if len(back) == 1 {
		return "back to " + back[0].Name
	}
	return "back to prior stage"
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
// label: two-space indent, "K=hint" pairs joined with " · ". Lowercase
// verbs read consistently across the three prompts.
func renderPromptLegend(opts []promptOption) string {
	var b strings.Builder
	b.WriteString("  ")
	for i, o := range opts {
		if i > 0 {
			b.WriteString(" · ")
		}
		fmt.Fprintf(&b, "%c=%s", o.key, o.hint)
	}
	return b.String()
}

// promptStageNextStage offers the non-push stage prompt: [Y/n] for most
// workflows, [Y/n/o] for sdlc non-push stages where headless one-shot
// is supported, [Y/n/o/s] when the next stage is sdlc's test (the skip
// shortcut jumps straight to the push prompt), and optional /x and /b
// suffixes when scuttle / back are non-nil. Y still defaults so a
// reflex Enter chains the next stage interactively, the same as before.
// `o` invokes the next stage with `--one-shot` prepended to its argv.
// `b` re-invokes the just-finished stage interactively. `x` dispatches
// the workflow's close command for the current run — the "abandon
// ship" path the operator forms at the same surface they decline from.
// `s` opens the push prompt early without satisfying the test stage:
// useful for doc-only diffs and trivial fixes where the anti-theater
// rule that test stage enforces would just produce a rubber-stamp
// canvas. Hardcoding the sdlc gate keeps the prompt honest — no other
// workflow has --one-shot or a test stage today, and we'd rather widen
// deliberately than offer affordances that don't exist.
//
// `x` is positioned adjacent to `n` (decline) because both read as "no":
// scuttle is "no, and also close this run." Grouping the two negatives
// reads better than appending `x` at the tail, and it leaves the
// forward-leaning `o` / `s` / `b` slots in their familiar positions.
//
// When the next stage is code, the just-finished design canvas is
// printed above the prompt — same shape as promptPushNextStage prints
// the code canvas above [N/m/p]. follow no longer surfaces the design
// canvas once the design session closes, so this is the canvas's one
// chance to land in front of the operator at the design→code gate.
// The [Y/n/o] default stays Y: design→code is reversible (re-design
// after a botched code run), so the canvas read is informative, not
// gating. Whitespace-only or missing canvas falls through to the bare
// prompt, no header or decoration.
func promptStageNextStage(next *Command, back []*Command, scuttle *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	// Surface the just-finished stage's canvas above the prompt so the
	// operator reads it before authorising the next stage. The pairing
	// is design → code: print design; code → test: print code. Same
	// shape promptPushNextStage uses for test → push.
	priorCanvas := ""
	switch next.Name {
	case "code":
		priorCanvas = "design"
	case "test":
		priorCanvas = "code"
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
	offerOneShot := md.Workflow == "sdlc"
	if offerOneShot {
		opts = append(opts, promptOption{key: 'o', hint: "run headless"})
	}
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
	label := renderPromptLabel(opts)
	moePrintf(stdout, "next: %s — run now? %s\n", hint, label)
	moePrintln(stdout, renderPromptLegend(opts))
	if md.Workflow == "sdlc" {
		moePrintln(stdout, "  !<stage>=cascade to gate · !!=cascade and ship")
	}
	sig, stopSig := installSigint()
	defer stopSig()
	line, interrupted, err := readLineWithSignal(stdinSharedReader(), sig)
	if interrupted {
		// Cooked-mode Ctrl-C used to trap the runtime. Treat it as
		// "decline" — safer default than guessing — and exit cleanly
		// so the queue walker (or a standalone chain) can unwind.
		moePrintln(stdout, "^C")
		return 0
	}
	if err != nil && err != io.EOF {
		moePrintf(stderr, "read stdin: %v\n", err)
		return 1
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if md.Workflow == "sdlc" && strings.HasPrefix(answer, "!") {
		return dispatchCascade(answer, next.Name, root, md, stdout, stderr)
	}
	if scuttle != nil && answer == "x" {
		return scuttle.Run([]string{md.Project, md.ID}, stdout, stderr)
	}
	if offerOneShot && answer == "o" {
		return next.Run([]string{"--one-shot", md.Project, md.ID}, stdout, stderr)
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
		// No synthesis here either — the `s` shortcut takes the same
		// path as the natural post-test cascade now: straight to the
		// ship gate, where the operator chooses (and synthesis only
		// runs inside `push --pr`).
		pushHint := fmt.Sprintf("moe %s %s %s %s", md.Workflow, pushCmd.Name, md.Project, md.ID)
		return promptPushNextStage(pushCmd, back, scuttle, root, md, pushHint, stdout, stderr)
	}
	if len(back) > 0 && answer == "b" {
		return dispatchBack(back, md, stdout, stderr)
	}
	accepted := answer == "" || strings.HasPrefix(answer, "y")
	if !accepted {
		return 0
	}
	return next.Run([]string{md.Project, md.ID}, stdout, stderr)
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
// thing the agent wrote. follow no longer surfaces stage canvases
// once their sessions close, so this is the canvas's one chance to
// land in front of the operator. By the time promptNextStage fires,
// session.Close has already rebased the session onto main, so root is
// the right base for the read. If both canvases are missing or
// whitespace-only, the prompt prints bare — no header or decoration.
// The canvas is markdown the agent wrote for the operator, printed
// as written.
//
// The push canvas is deliberately not in this fallback chain. Synthesis
// runs inside `push --pr` now, not at chain-prompt time, so by the time
// the operator reads this preamble the push canvas (if any) is left
// over from a prior `--pr` cycle — stale relative to whatever the
// operator's about to do. Test → code is the live story.
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
		moePrintln(stdout, "  !!=ship now (same as m)")
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
	case "m", "!!":
		// `!!` at the push gate ships the same way `m` does — the
		// cascade vocabulary is the same here, just with no stages
		// left to walk before the ship.
		return next.Run([]string{md.Project, md.ID}, stdout, stderr)
	case "p":
		return next.Run([]string{"--pr", md.Project, md.ID}, stdout, stderr)
	case "x":
		if scuttle != nil {
			return scuttle.Run([]string{md.Project, md.ID}, stdout, stderr)
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
				"cascade: `%s` is at or behind the push gate; type `!!` to ship or pick m/p/n/x/b. (stages: %s)\n",
				answer, strings.Join(wf.Stages(), ", "))
		}
		return 0
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
	return 0
}

// dispatchCascade parses the operator's `!<stage>` or `!!` answer at
// a non-push chain prompt, validates the destination against the
// workflow's stage ladder, walks the cascade, and either re-enters
// the chain at the destination gate or returns 0 if the cascade
// shipped. answer is already lowercased and trimmed. startStage is
// the next-to-run stage at the current gate (promptStageNextStage's
// next.Name) — the cascade's first dispatch.
//
// Returns the exit code to bubble up: the failing stage's code on a
// cascade failure, 0 on a successful park-at-gate or ship.
func dispatchCascade(answer, startStage, root string, md *run.Metadata, stdout, stderr io.Writer) int {
	var destination string
	if answer == "!!" {
		destination = ""
	} else {
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
	res, code := cascadeFromGate(startStage, destination, md, stdout, stderr)
	if summary := renderCascadeSummary(res); summary != "" {
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

// cascadeStepResult records one dispatched stage's outcome.
type cascadeStepResult struct {
	stage string
	code  int
}

// cascadeResult is what cascadeFromGate hands back: the ordered list
// of dispatched stages plus a flag set only when `!!` completed the
// ship. The summary line renders from this.
type cascadeResult struct {
	ran     []cascadeStepResult
	shipped bool
}

// cascadeFromGate dispatches stages headless from startStage up to,
// but not including, destination. An empty destination is the yolo
// variant (`!!`): it walks every remaining stage and ships at push.
//
// startStage is the next-to-run stage at the operator's current
// gate. destination is the stage the operator named in `!<stage>`;
// both have been validated by the caller against the workflow ladder.
// A destination at or behind startStage produces a no-op cascade and
// exit 0.
//
// Each headless dispatch goes through the typed Command's Run with
// `--one-shot` prepended — same shape the existing `o` keystroke
// uses — so stage-specific pre-flight (requireDesignCanvas,
// requireCodeCanvas, canvas skeleton seeding) still fires. The
// inCascade flag suppresses each stage's inner promptNextStage so
// the cascade owns routing.
//
// At push in yolo mode the dispatch is the merge path (no flags),
// not `--one-shot`: `!!` defaults to fast-forward merge, and the
// merge commit body is bare, so a synthesis pre-call would just
// write a canvas nothing reads.
func cascadeFromGate(startStage, destination string, md *run.Metadata, stdout, stderr io.Writer) (cascadeResult, int) {
	var res cascadeResult
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return res, 1
	}
	g, err := LookupGroup(md.Workflow)
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
	yolo := destination == ""
	if !yolo {
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
	prev := inCascade
	inCascade = true
	defer func() { inCascade = prev }()
	for i := startIdx; i < endIdx; i++ {
		stage := stages[i]
		cmd := g.Lookup(stage)
		if cmd == nil {
			moePrintf(stderr, "cascade: workflow %s has no command for stage %q\n", md.Workflow, stage)
			return res, 1
		}
		moePrintf(stdout, "cascade: %s (headless)\n", stage)
		if stage == "push" && yolo {
			// `!!` at push: ship via the merge path. No `--one-shot`
			// synthesis pre-call — the merge commit body is bare and
			// no PR body lands on a reviewer's screen, so the
			// curation would be writing a canvas nothing reads.
			ship := cmd.Run([]string{md.Project, md.ID}, stdout, stderr)
			res.ran = append(res.ran, cascadeStepResult{stage: stage, code: ship})
			if ship != 0 {
				return res, ship
			}
			res.shipped = true
			continue
		}
		code := cmd.Run([]string{"--one-shot", md.Project, md.ID}, stdout, stderr)
		res.ran = append(res.ran, cascadeStepResult{stage: stage, code: code})
		if code != 0 {
			return res, code
		}
	}
	return res, 0
}

// renderCascadeSummary formats the single-line summary printed after
// a cascade finishes (success or failure). Empty res (no-op cascade)
// returns "" so the caller skips the print — there's nothing to
// summarise.
//
// Examples:
//
//	cascade: code ok · test ok
//	cascade: code failed (exit 1) — stopped
//	cascade: code ok · test ok · push ok — shipped
//	cascade: code ok · test failed (exit 2) — stopped
func renderCascadeSummary(res cascadeResult) string {
	if len(res.ran) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res.ran))
	failed := false
	for _, r := range res.ran {
		if r.code != 0 {
			parts = append(parts, fmt.Sprintf("%s failed (exit %d)", r.stage, r.code))
			failed = true
		} else {
			parts = append(parts, fmt.Sprintf("%s ok", r.stage))
		}
	}
	s := "cascade: " + strings.Join(parts, " · ")
	if failed {
		s += " — stopped"
	} else if res.shipped {
		s += " — shipped"
	}
	return s
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
