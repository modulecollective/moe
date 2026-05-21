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
// don't know which stage just landed (resume hitting the merge gate).
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
	wf, err := LookupWorkflow(md.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	var stage string
	if justFinished != "" {
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
						if !stdinIsTerminal() {
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
	hint := fmt.Sprintf("moe %s %s %s/%s", wf.Name, next.Name, md.Project, md.ID)
	if !stdinIsTerminal() {
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
// before. Workflows with a registered headless dispatcher (sdlc, twin)
// also surface `!` as a peer in the bracket and main legend — single
// keystroke, dispatch one stage headless — while `!<stage>` and `!!`
// stay on a second cascade-extras line below. `b` re-invokes the
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
	// is the workflow's prereq edge — design → code, code → test,
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
			priorCanvas = "code"
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
	dispatcher := lookupHeadlessDispatcher(md.Workflow)
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
		moePrintln(stdout, "  !<stage> = cascade to gate · !! = cascade and ship")
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
	accepted := answer == "" || strings.HasPrefix(answer, "y")
	if !accepted {
		return 0
	}
	return next.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
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
		moePrintln(stdout, "  !! = ship now (same as m)")
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
		return next.Run([]string{md.Project + "/" + md.ID}, stdout, stderr)
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
				"cascade: `%s` is at or behind the push gate; type `!!` to ship or pick m/p/n/x/b. (stages: %s)\n",
				answer, strings.Join(wf.Stages(), ", "))
		}
		return 0
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
	return 0
}

// dispatchCascade parses the operator's `!`, `!<stage>`, or `!!` answer
// at a non-push chain prompt, validates the destination against the
// workflow's stage ladder, walks the cascade, and either re-enters
// the chain at the destination gate or returns 0 if the cascade
// shipped. answer is already lowercased and trimmed. startStage is
// the next-to-run stage at the current gate (promptStageNextStage's
// next.Name) — the cascade's first dispatch.
//
// Bare `!` dispatches exactly one stage (startStage) and re-prompts
// at the resulting gate. `!<stage>` walks up to but not including
// the named stage. `!!` walks every remaining stage and ships
// (or auto-closes, for workflows without push).
//
// Returns the exit code to bubble up: the failing stage's code on a
// cascade failure, 0 on a successful park-at-gate or ship.
func dispatchCascade(answer, startStage, root string, md *run.Metadata, stdout, stderr io.Writer) int {
	var destination string
	oneStep := false
	switch {
	case answer == "!!":
		destination = ""
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
	res, code := cascadeFromGate(startStage, destination, oneStep, md, stdout, stderr)
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

// pushFromCascade is the typed push entry the cascade's `!!` step
// calls into. Wired to runPushTyped in init() rather than referenced
// directly so the var-init dependency analyser doesn't trace through
// cascadeFromGate → runPushTyped → openCodeSessionFor… (var) →
// runStageSession (var) → promptNextStage → … → cascadeFromGate and
// flag the chain as an initialization cycle. Tests override this
// var to stub the cascade's push step without touching the standalone
// `moe sdlc push` path (which still goes through pushCmd.Run →
// runPushTyped → discard-error).
var pushFromCascade func(workflow string, args []string, stdout, stderr io.Writer) (int, error)

func init() {
	pushFromCascade = runPushTyped
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
// of dispatched stages plus a flag set only when `!!` completed the
// ship. The summary line renders from this.
type cascadeResult struct {
	ran     []cascadeStepResult
	shipped bool
}

// cascadeFromGate dispatches stages headless from startStage up to,
// but not including, destination. An empty destination is the yolo
// variant (`!!`): it walks every remaining stage and ships at push.
// When oneStep is true, destination is ignored and the cascade
// dispatches exactly one stage (startStage) — the bare-`!` form.
// oneStep at the terminal stage still dispatches that stage without
// shipping or auto-closing; that's what distinguishes it from `!!`.
//
// startStage is the next-to-run stage at the operator's current
// gate. destination is the stage the operator named in `!<stage>`;
// both have been validated by the caller against the workflow ladder.
// A destination at or behind startStage produces a no-op cascade and
// exit 0.
//
// Each headless dispatch goes through the workflow's registered
// dispatcher — the Go-level seam the cascade driver (`!` / `!<stage>`
// / `!!`) reaches into — so stage-specific pre-flight
// (requireDesignCanvas, requireCodeCanvas, canvas skeleton seeding)
// still fires. The suppressNextStage flag
// suppresses each stage's inner promptNextStage so the cascade owns
// routing.
//
// At push in yolo mode the dispatch is the merge path (pushCmd.Run with
// no flags). `!!` defaults to fast-forward merge; runPushTyped writes the merge-path push note after deterministic hooks and
// shipping.
func cascadeFromGate(startStage, destination string, oneStep bool, md *run.Metadata, stdout, stderr io.Writer) (cascadeResult, int) {
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
	dispatcher := lookupHeadlessDispatcher(md.Workflow)
	if dispatcher == nil {
		moePrintf(stderr, "cascade: workflow %q has no headless dispatcher\n", md.Workflow)
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
			// `!!` at push ships via the merge path. runPushTyped
			// owns synthesis before the shared ship gate, so the
			// cascade only needs to call the typed push entry once.
			//
			// Call runPushTyped via pushFromCascade (bypassing
			// g.Lookup("push")) so the deferred-to-recovery signal —
			// when push's pre-push gate hands off to a fresh code
			// session — comes back as a typed *PushDeferredError. The
			// command-group indirection used by !<stage> cascades
			// discards that error to preserve the Command.Run
			// contract; only this path needs it. Without the typed
			// channel, a recovery session that exits 0 would look
			// identical to a real ship and the cascade summary would
			// claim "shipped" when nothing was pushed.
			ship, err := pushFromCascade(md.Workflow, []string{md.Project + "/" + md.ID}, stdout, stderr)
			var deferred *PushDeferredError
			if errors.As(err, &deferred) {
				res.ran = append(res.ran, cascadeStepResult{
					stage:    stage,
					code:     ship,
					deferred: deferred.Recovery,
				})
				// Propagate the recovery session's exit verbatim:
				// 0 if the agent resolved cleanly (operator's next
				// move is to re-run push), non-zero if the agent
				// gave up. Either way the deferred marker keeps
				// the summary honest — this was not a ship.
				return res, ship
			}
			res.ran = append(res.ran, cascadeStepResult{stage: stage, code: ship})
			if ship != 0 {
				return res, ship
			}
			res.shipped = true
			continue
		}
		code := dispatcher(stage, md.Project, md.ID, true, stdout, stderr)
		res.ran = append(res.ran, cascadeStepResult{stage: stage, code: code})
		if code != 0 {
			return res, code
		}
	}
	// `!!` for a workflow without push (twin today) auto-closes the run
	// after the last stage commits — same operator intent as the sdlc
	// push branch above ("cascade and terminate"), just routed through
	// close instead of push. sdlc set res.shipped=true in the push branch
	// already, so the gate skips it there. --no-edit keeps the close
	// non-interactive (followups.md harvests as-is); a hands-off cascade
	// should never block on an editor.
	if yolo && !res.shipped {
		if closeCmd := g.Lookup("close"); closeCmd != nil {
			moePrintf(stdout, "cascade: close (headless)\n")
			code := closeCmd.Run([]string{"--no-edit", md.Project + "/" + md.ID}, stdout, stderr)
			res.ran = append(res.ran, cascadeStepResult{stage: "close", code: code})
			if code != 0 {
				return res, code
			}
			res.shipped = true
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
//	cascade: code ok · test ok · push deferred to recovery (rebase conflict) — stopped
//	cascade: code ok · test ok · push deferred to recovery (pre-push hook) — stopped
func renderCascadeSummary(res cascadeResult) string {
	if len(res.ran) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res.ran))
	stopped := false
	for _, r := range res.ran {
		switch {
		case r.deferred != "":
			parts = append(parts, fmt.Sprintf("%s deferred to recovery (%s)", r.stage, deferredLabel(r.deferred)))
			stopped = true
		case r.code != 0:
			parts = append(parts, fmt.Sprintf("%s failed (exit %d)", r.stage, r.code))
			stopped = true
		default:
			parts = append(parts, fmt.Sprintf("%s ok", r.stage))
		}
	}
	s := "cascade: " + strings.Join(parts, " · ")
	if stopped {
		s += " — stopped"
	} else if res.shipped {
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

// headlessDispatcher is the Go-level seam a workflow's per-stage init
// registers so the chain prompt's cascade driver (`!` / `!<stage>` /
// `!!`) can drive a stage headless without a hardcoded switch on
// workflow name. The contract matches openSdlcStage / openTwinStage
// exactly: take (stage, projectID, runID, suppressNextStage, stdout,
// stderr), invoke the right per-stage helper headless, return its
// exit code.
type headlessDispatcher func(stage, projectID, runID string, suppressNextStage bool, stdout, stderr io.Writer) int

var headlessDispatchers = map[string]headlessDispatcher{}

// registerHeadlessDispatcher wires a workflow's headless dispatcher
// into the registry. Called from each workflow's init() so the
// chain-prompt and cascade machinery can stay workflow-agnostic.
// Panics on duplicate names — same fail-loud contract as
// RegisterWorkflow.
func registerHeadlessDispatcher(workflow string, d headlessDispatcher) {
	if _, dup := headlessDispatchers[workflow]; dup {
		panic("cli: duplicate headless dispatcher for workflow " + workflow)
	}
	headlessDispatchers[workflow] = d
}

// lookupHeadlessDispatcher returns the registered dispatcher for
// workflow, or nil if none. nil means "this workflow has no headless
// dispatch wired" — the chain prompt suppresses the cascade legend
// and the cascade refuses to walk.
func lookupHeadlessDispatcher(workflow string) headlessDispatcher {
	return headlessDispatchers[workflow]
}
