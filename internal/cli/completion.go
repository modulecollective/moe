package cli

// Shell tab completion. Two commands make up the whole surface:
//
//   - `moe __complete <words...>` is the hidden callback the shell shims
//     invoke on every TAB. It walks the in-memory command registry plus
//     a handful of value sources (runs, ideas, workspaces) and prints
//     newline-separated candidates. All the intelligence lives here in
//     Go where it's testable in-process — the shell stays dumb.
//   - `moe completion [bash|zsh|fish]` prints the static shim the
//     operator evals from their rc file. The shims never change as
//     commands are added; only the registry and the value sources do.
//
// The design lives in projects/moe/runs/tab-complete-everywhere. Key
// invariants: __complete never errors loudly (a callback that writes to
// stderr or exits nonzero corrupts the shell line), and a command
// without an argKind annotation degrades to tree-only completion rather
// than a wrong value suggestion.

import (
	"io"
	"os"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/workspace"
)

// argKind tags a command's first positional argument with the value
// source completion should draw from. argNone (the zero value) means
// "no value completion": the command still gets static tree completion
// (its own name, its group's subcommands), it just offers no candidates
// for its positional. A missing or wrong annotation therefore degrades
// to tree-only completion, never to a wrong suggestion — the safe
// failure mode that earns the per-command annotation its churn.
type argKind int

const (
	argNone argKind = iota
	argProjectRun
	argWorkspace
	argIdea
	argIntent
)

// flagArgKind maps a value-taking flag to the candidate source for its
// value. Kept tiny and name-keyed on purpose: it lets the single
// highest-traffic flag (`--from-idea`) complete its idea slug without
// exposing every command's FlagSet — the framework-shaped refactor the
// design ruled out. `--workspace` → argWorkspace is the obvious next
// entry; it's a one-line add when wanted.
var flagArgKind = map[string]argKind{
	"--from-idea": argIdea,
}

// valueFlags lists the flags whose *next token* is a value, not a
// positional. __complete consults it for two reasons: to keep a flag's
// value slot from falling through to positional run candidates (e.g.
// after `--agent`), and to skip a flag's value when deciding whether
// the partial is the command's first positional. A flag absent here is
// treated as a bare switch — its successor token is a fresh positional.
var valueFlags = map[string]bool{
	"--from-idea": true,
	"--agent":     true,
	"--workspace": true,
	"--to":        true,
}

func init() {
	Register(&Command{
		Name:   "__complete",
		Hidden: true,
		Run:    runComplete,
	})
	Register(&Command{
		Name:    "completion",
		Summary: "print a shell completion script: moe completion [bash|zsh|fish]",
		Run:     runCompletionSnippet,
	})
}

// runComplete is the shell-facing completion callback. The shims hand it
// the words typed after `moe`, the last of which is the partial under
// the cursor (possibly empty). It prints newline-separated candidates to
// stdout and ALWAYS exits 0: a callback that writes to stderr or exits
// nonzero corrupts the user's shell line, so every lookup failure
// (outside a bureaucracy, an unreadable runs tree) degrades to the
// static candidates and a clean exit.
func runComplete(args []string, stdout, stderr io.Writer) int {
	// args is exactly the words after `moe`. A no-arg invocation means
	// the cursor sits on the first word with an empty partial.
	if len(args) == 0 {
		args = []string{""}
	}
	for _, c := range completeWords(quietRoot(), args) {
		moePrintln(stdout, c)
	}
	return 0
}

// quietRoot resolves the bureaucracy root without writing to stderr —
// findRoot's error-printing sibling would corrupt the completion line.
// Returns "" when not inside a bureaucracy; callers fall back to static
// (registry-only) candidates.
func quietRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		return ""
	}
	return root
}

// completeWords resolves the candidate list for a partial completion.
// words is the slice typed after `moe`; words[len-1] is the partial
// under the cursor. root is the bureaucracy root, or "" when outside
// one (in which case value sources yield nothing and only the static
// tree completes). The returned slice is sorted and prefix-filtered.
func completeWords(root string, words []string) []string {
	partial := words[len(words)-1]
	prior := words[:len(words)-1]

	// 1. Flag-value completion runs ahead of positional resolution: the
	//    value of `--from-idea` is an idea slug regardless of which
	//    command carries the flag.
	if cands, handled := completeFlagValue(root, prior, partial); handled {
		return cands
	}

	// 2. A partial that is itself a flag token wants flag-NAME
	//    completion, which v1 doesn't offer — emit nothing rather than
	//    fall through and suggest positionals for a `--`-prefixed word.
	if isFlagToken(partial) {
		return nil
	}

	// 3. Static tree + positional value resolution.
	if len(prior) == 0 {
		return filterPrefix(topLevelNames(), partial)
	}
	first := prior[0]
	if g, ok := groups[first]; ok {
		if len(prior) == 1 {
			return filterPrefix(groupSubNames(g), partial)
		}
		leaf := g.Lookup(prior[1])
		if leaf == nil {
			return nil
		}
		if firstPositional(prior[2:]) {
			return filterPrefix(valueCandidates(root, leaf.argKind), partial)
		}
		return nil
	}
	leaf, ok := commands[first]
	if !ok {
		return nil
	}
	if firstPositional(prior[1:]) {
		return filterPrefix(valueCandidates(root, leaf.argKind), partial)
	}
	return nil
}

// completeFlagValue handles the three token shapes a flag value can
// arrive in across the supported shells:
//
//	--from-idea <partial>     space form: prior ends with the flag
//	--from-idea=<partial>     single token (zsh, fish)
//	--from-idea = <partial>   bash splits on '=' into three tokens
//
// handled is true when the partial is a flag's value, in which case
// cands is the (possibly empty) candidate list and the caller must not
// also offer positionals. A value-taking flag with no completion source
// (e.g. --agent) returns handled=true with no candidates, so its value
// slot doesn't fall through to positional runs.
func completeFlagValue(root string, prior []string, partial string) (cands []string, handled bool) {
	// Single-token `--flag=val` (shells that don't break on '=').
	if strings.HasPrefix(partial, "--") && strings.Contains(partial, "=") {
		name, val, _ := strings.Cut(partial, "=")
		if k, ok := flagArgKind[name]; ok {
			out := filterPrefix(valueCandidates(root, k), val)
			for i, c := range out {
				out[i] = name + "=" + c
			}
			return out, true
		}
		return nil, true
	}
	if len(prior) == 0 {
		return nil, false
	}
	last := prior[len(prior)-1]
	// bash wordbreak form: `--flag` `=` `<partial>`.
	if last == "=" && len(prior) >= 2 {
		name := prior[len(prior)-2]
		if k, ok := flagArgKind[name]; ok {
			return filterPrefix(valueCandidates(root, k), partial), true
		}
		return nil, valueFlags[name]
	}
	// Space form: the previous token is the flag itself.
	if k, ok := flagArgKind[last]; ok {
		return filterPrefix(valueCandidates(root, k), partial), true
	}
	return nil, valueFlags[last]
}

// firstPositional reports whether the partial under the cursor would be
// the command's first positional argument — i.e. no positional has been
// typed yet in toks (the tokens between the command name and the
// partial). Flags and their values are skipped. This is what stops the
// completer from offering run candidates for a command's SECOND
// positional (e.g. the stage name after `cat <project>/<run>`), which
// v1 doesn't complete.
func firstPositional(toks []string) bool {
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, "-") {
			if valueFlags[t] && !strings.Contains(t, "=") {
				i++ // the next token is this flag's value, not a positional
			}
			continue
		}
		return false
	}
	return true
}

func valueCandidates(root string, k argKind) []string {
	if root == "" {
		return nil
	}
	switch k {
	case argProjectRun:
		return projectRunCandidates(root)
	case argWorkspace:
		return workspaceCandidates(root)
	case argIdea:
		return ideaCandidates(root)
	case argIntent:
		return intentCandidates(root)
	default:
		return nil
	}
}

// projectRunCandidates lists every non-idea run as `project/run`. Idea
// runs are excluded — they share the slug shape but are a different
// token kind (completed via argIdea), and an sdlc/kb verb can't act on
// one. No status filter: cat, log, and reopen legitimately target
// closed and merged runs, so hiding them would cost more than the noise
// of listing them.
func projectRunCandidates(root string) []string {
	mds, err := run.Scan(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(mds))
	for _, md := range mds {
		// Idea and intent runs share the slug shape but are their own
		// token kinds (argIdea / argIntent); an sdlc/kb verb can't act on
		// one, so keep them out of the generic project/run set.
		if md.Workflow == dash.IdeaWorkflow || md.Workflow == dash.IntentWorkflow {
			continue
		}
		out = append(out, md.Project+"/"+md.ID)
	}
	return out
}

// ideaCandidates lists open ideas as `project/slug`. Only in-progress
// ideas are offered: a promoted or closed idea is a dead target for
// `--from-idea` and `idea edit`, and the live set is what either verb
// wants.
func ideaCandidates(root string) []string {
	mds, err := run.Scan(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, md := range mds {
		if md.Workflow != dash.IdeaWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		out = append(out, md.Project+"/"+md.ID)
	}
	return out
}

// intentCandidates lists open intents as `project/slug`. Only in-progress
// intents are offered: edit/close/cat legitimately target a parked
// intent, and a closed one is a dead target for the write verbs — the
// live set is what the operator reaches for. (cat also reads closed ones,
// but the open set is the useful completion default, same call idea's
// argIdea makes.)
func intentCandidates(root string) []string {
	mds, err := run.Scan(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, md := range mds {
		if md.Workflow != dash.IntentWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		out = append(out, md.Project+"/"+md.ID)
	}
	return out
}

// workspaceCandidates lists every named workspace as `project/name`.
// workspace.List probes git per row for its table view; completion only
// needs Project and Name, but the existing List is cheap enough at
// current scale that a leaner scan isn't worth a second code path.
func workspaceCandidates(root string) []string {
	infos, err := workspace.List(root, "")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Project+"/"+info.Name)
	}
	return out
}

func topLevelNames() []string {
	out := make([]string, 0, len(commands))
	for n, c := range commands {
		if c.Hidden {
			continue
		}
		out = append(out, n)
	}
	return out
}

func groupSubNames(g *CommandGroup) []string {
	out := make([]string, 0, len(g.commands))
	for n, c := range g.commands {
		if c.Hidden {
			continue
		}
		out = append(out, n)
	}
	return out
}

// filterPrefix keeps the candidates that start with partial and sorts
// them, so completion output is deterministic (tests assert on it) and
// the shell shows a stable order.
func filterPrefix(cands []string, partial string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if strings.HasPrefix(c, partial) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// runCompletionSnippet prints the shell shim the operator evals from
// their rc file. One snippet per shell; none of them change as commands
// are added.
func runCompletionSnippet(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		completionUsage(stderr)
		return 2
	}
	snippet, ok := completionSnippets[args[0]]
	if !ok {
		moePrintf(stderr, "completion: unsupported shell %q\n", args[0])
		completionUsage(stderr)
		return 2
	}
	moePrintln(stdout, snippet)
	return 0
}

func completionUsage(w io.Writer) {
	moePrintln(w, "usage: moe completion [bash|zsh|fish]")
	moePrintln(w, "")
	moePrintln(w, "Print a shell completion script. Install it by evaluating the")
	moePrintln(w, "output from your shell's startup file, e.g.:")
	moePrintln(w, "")
	moePrintln(w, `  bash:  eval "$(moe completion bash)"   # in ~/.bashrc`)
	moePrintln(w, `  zsh:   eval "$(moe completion zsh)"    # in ~/.zshrc, after compinit`)
	moePrintln(w, "  fish:  moe completion fish | source    # in ~/.config/fish/config.fish")
}

var completionSnippets = map[string]string{
	"bash": bashCompletion,
	"zsh":  zshCompletion,
	"fish": fishCompletion,
}

// The shims all share one contract: pass the words after `moe` to
// `moe __complete`, with the word under the cursor (possibly empty) as
// the final argument, and feed stdout back as candidates. The only
// per-shell work is normalizing each shell's current-line state into
// that word list — which is why these strings never need to change.

const bashCompletion = `_moe_complete() {
    local IFS=$'\n'
    local words=("${COMP_WORDS[@]:1:COMP_CWORD-1}" "${COMP_WORDS[COMP_CWORD]}")
    COMPREPLY=($(moe __complete "${words[@]}" 2>/dev/null))
}
complete -F _moe_complete moe`

const zshCompletion = `#compdef moe
_moe() {
    local -a completions
    completions=(${(f)"$(moe __complete "${(@)words[2,CURRENT]}" 2>/dev/null)"})
    compadd -- ${completions:#}
}
compdef _moe moe`

const fishCompletion = `function __moe_complete
    set -l tokens (commandline -opc)
    set -l cur (commandline -ct)
    moe __complete $tokens[2..-1] "$cur" 2>/dev/null
end
complete -c moe -f -a '(__moe_complete)'`
