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
// justFinished also drives the "back" target offered at the prompt:
// a non-empty justFinished resolves to back := g.Lookup(justFinished),
// which the prompt offers as the `b` option so the operator can
// re-open the stage whose canvas is sitting above the cursor.
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
	var back *Command
	if justFinished != "" {
		back = g.Lookup(justFinished)
	}
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
		return promptPushNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
	}
	return promptStageNextStage(next, back, scuttle, root, md, hint, stdout, stderr)
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
// is supported, and optional /x and /b suffixes when scuttle / back are
// non-nil. Y still defaults so a reflex Enter chains the next stage
// interactively, the same as before. `o` invokes the next stage with
// `--one-shot` prepended to its argv. `b` re-invokes the just-finished
// stage interactively. `x` dispatches the workflow's close command for
// the current run — the "abandon ship" path the operator forms at the
// same surface they decline from. Hardcoding the sdlc gate keeps the
// prompt honest — no other workflow has --one-shot today, and we'd
// rather widen deliberately than offer a flag that doesn't exist.
//
// `x` is positioned adjacent to `n` (decline) because both read as "no":
// scuttle is "no, and also close this run." Grouping the two negatives
// reads better than appending `x` at the tail, and it leaves the
// forward-leaning `o` / `b` slots in their familiar positions.
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
func promptStageNextStage(next, back, scuttle *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
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
		canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, priorCanvas))
		if body, err := os.ReadFile(canvasPath); err == nil && strings.TrimSpace(string(body)) != "" {
			fmt.Fprint(stdout, string(body))
			if !strings.HasSuffix(string(body), "\n") {
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
	if back != nil {
		opts = append(opts, promptOption{key: 'b', hint: "back to " + back.Name})
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "next: %s — run now? %s\n", hint, label)
	moePrintln(stdout, renderPromptLegend(opts))
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
	if scuttle != nil && answer == "x" {
		return scuttle.Run([]string{md.Project, md.ID}, stdout, stderr)
	}
	if offerOneShot && answer == "o" {
		return next.Run([]string{"--one-shot", md.Project, md.ID}, stdout, stderr)
	}
	if back != nil && answer == "b" {
		return back.Run([]string{md.Project, md.ID}, stdout, stderr)
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
// body but is one stage back). follow no longer surfaces stage
// canvases once their sessions close, so this is the canvas's one
// chance to land in front of the operator. By the time
// promptNextStage fires, session.Close has already rebased the
// session onto main, so root is the right base for the read.
// Whitespace-only or missing canvas falls through to the bare prompt:
// no header or decoration — the canvas is markdown the agent wrote
// for the operator, printed as written.
func promptPushNextStage(next, back, scuttle *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "test"))
	if body, err := os.ReadFile(canvasPath); err == nil && strings.TrimSpace(string(body)) != "" {
		fmt.Fprint(stdout, string(body))
		if !strings.HasSuffix(string(body), "\n") {
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
	if back != nil {
		opts = append(opts, promptOption{key: 'b', hint: "back to " + back.Name})
	}
	label := renderPromptLabel(opts)
	moePrintf(stdout, "next: %s — run now? %s\n", hint, label)
	moePrintln(stdout, renderPromptLegend(opts))
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
		return next.Run([]string{md.Project, md.ID}, stdout, stderr)
	case "p":
		return next.Run([]string{"--pr", md.Project, md.ID}, stdout, stderr)
	case "x":
		if scuttle != nil {
			return scuttle.Run([]string{md.Project, md.ID}, stdout, stderr)
		}
	case "b":
		if back != nil {
			return back.Run([]string{md.Project, md.ID}, stdout, stderr)
		}
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
	return 0
}
