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
	hint := fmt.Sprintf("moe %s %s %s %s", wf.Name, next.Name, md.Project, md.ID)
	if !stdinIsTerminal() {
		moePrintf(stdout, "next: %s\n", hint)
		return 0
	}
	switch next.Name {
	case "push":
		return promptPushNextStage(next, back, root, md, hint, stdout, stderr)
	}
	return promptStageNextStage(next, back, root, md, hint, stdout, stderr)
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
// is supported, and an optional /b suffix when back is non-nil so the
// operator can re-open the just-finished stage. Y still defaults so a
// reflex Enter chains the next stage interactively, the same as before.
// `o` invokes the next stage with `--one-shot` prepended to its argv.
// `b` re-invokes the just-finished stage interactively. Hardcoding the
// sdlc gate keeps the prompt honest — no other workflow has --one-shot
// today, and we'd rather widen deliberately than offer a flag that
// doesn't exist.
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
func promptStageNextStage(next, back *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	if next.Name == "code" {
		canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "design"))
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
// (`moe <wf> push`), or PR (`moe <wf> push --pr`), plus an optional
// `b` to re-open the just-finished code stage when back is non-nil.
// Parsing is case-insensitive; the label capitalization just signals
// the default. N-as-default is load-bearing — a reflex Enter must
// never ship.
//
// The code canvas is printed above the prompt so the operator reads
// the agent's pre-push framing at the exact moment they're deciding
// whether to ship. follow no longer surfaces the code canvas during
// the code stage (it shows the sandbox diff instead), so this is the
// canvas's one chance to land in front of the operator. By the time
// promptNextStage fires, session.Close has already rebased the code
// session onto main, so root is the right base for the read.
// Whitespace-only or missing canvas falls through to the bare prompt:
// no header or decoration — the canvas is markdown the agent wrote
// for the operator, printed as written.
func promptPushNextStage(next, back *Command, root string, md *run.Metadata, hint string, stdout, stderr io.Writer) int {
	canvasPath := filepath.Join(root, run.ContentPath(md.Project, md.ID, "code"))
	if body, err := os.ReadFile(canvasPath); err == nil && strings.TrimSpace(string(body)) != "" {
		fmt.Fprint(stdout, string(body))
		if !strings.HasSuffix(string(body), "\n") {
			fmt.Fprintln(stdout)
		}
	}
	opts := []promptOption{
		{key: 'N', hint: "decline"},
		{key: 'm', hint: "fast-forward merge"},
		{key: 'p', hint: "open PR"},
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
	case "b":
		if back != nil {
			return back.Run([]string{md.Project, md.ID}, stdout, stderr)
		}
	}
	// Anything else — blank, "n", or a typo — declines. Safer than
	// guessing which ship path a garbled answer meant.
	return 0
}
