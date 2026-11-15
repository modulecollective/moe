package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
)

// `moe intent` parks standing direction on a project: a short,
// operator-authored statement of where the project is going — a theme,
// a bet — kept open while it's relevant and closed when it stops being
// so. Intents are just runs in a dedicated single-stage workflow
// (dash.IntentWorkflow, dash.IntentDocID), the same shape as ideas.
//
// Authorship stays the operator's. Agents *read* intents (via the
// stage-prompt catalog and the pulse) and never originate direction —
// an intent is never promoted, never executed, never handed to an agent
// to advance. `moe intent edit --chat` is not an exception to that: it's
// a session the operator opens and drives, where the agent is the pen
// and the sharpener, typing what the conversation converges on. The ban
// is on autonomous agents writing direction, and it stands.
//
// Deliberately narrower than idea: no move, no reopen. An intent is a
// few lines the operator keeps or closes.

func init() {
	g := NewCommandGroup("intent", "intent workflow")
	g.Register(&Command{
		Name:    "new",
		Summary: "park a new intent in $EDITOR",
		Run:     intentKind.runNew,
	})
	g.Register(&Command{
		Name:    "edit",
		Summary: "sharpen a parked intent ($EDITOR, or --chat for an agent session)",
		Run:     intentKind.runEdit,
		argKind: argIntent,
	})
	g.Register(&Command{
		Name:    "close",
		Summary: "close an intent that's satisfied or gone stale (status → closed)",
		Run:     runIntentClose,
		argKind: argIntent,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "list this project's open intents",
		Run:     intentKind.runList,
	})
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump an intent's canvas to stdout",
		Run:     runCat(dash.IntentWorkflow, dash.IntentDocID),
		argKind: argIntent,
	})
	// `edit --chat` mints a durable session on the intent document, so
	// the transcript needs a reader — without one the sessions this
	// feature creates would be unreadable, the dead surface the removed
	// idea-chat path left behind.
	g.Register(&Command{
		Name:    "log",
		Summary: "render an intent's agent transcript",
		Run:     runLog(dash.IntentWorkflow, dash.IntentDocID),
		argKind: argIntent,
	})
	// Manual recovery for captures a session couldn't fan out — a run
	// that stranded entries before HarvestOnExit existed, or a
	// session-end harvest that failed partway. Intent close never
	// harvests (capture close), so without this verb there is no
	// operator-driven path at all.
	g.Register(harvestCommand(dash.IntentWorkflow))
	RegisterGroup(g)

	// Register the intent workflow so run.Load, dash lookup, and the cat
	// resolver's wf.Stages() all resolve it. The single stage name
	// `intent` lives in the DAG without a matching `moe intent intent`
	// verb — operator-facing verbs (new/edit/close/list/cat) are group
	// subcommands above, same shape as idea.
	w := NewWorkflow(dash.IntentWorkflow)
	w.RegisterStage(dash.IntentDocID)
	RegisterWorkflow(w)
}

// runIntentClose closes an intent (satisfied or abandoned). Delegates to
// the shared close handler in close.go; intents keep the short `Close
// intent <p>/<r>` subject and the capture-workflow close path (clean-tree
// gate, no followups harvest, canvas-empty exemption) — the same path
// idea close rides.
func runIntentClose(args []string, stdout, stderr io.Writer) int {
	return runClose(dash.IntentWorkflow, "Close intent %s/%s", nil, args, stdout, stderr)
}

// gatherIntents projects the open intent runs in mds into the
// dash.IntentInput set the INTENTS section renders, reading each title
// off its canvas's first heading. All projects are gathered; BuildRows
// applies the dash's ProjectFilter. mds is the caller's existing scan,
// so this adds only the per-intent title read (intents are few and
// operator-pruned).
func gatherIntents(root string, mds []*run.Metadata) []dash.IntentInput {
	var out []dash.IntentInput
	for _, md := range mds {
		if md.Workflow != dash.IntentWorkflow || md.Status != run.StatusInProgress {
			continue
		}
		out = append(out, dash.IntentInput{
			Project: md.Project,
			Slug:    md.ID,
			Title:   intentTitle(root, md.Project, md.ID),
		})
	}
	return out
}

// intentTitle reads an intent canvas's first ATX `# ` heading, the title
// the catalog and dash render. Falls back to the slug when the canvas is
// missing, unreadable, or headless — a half-written intent must never
// break the prompt or the dash. The first heading wins.
func intentTitle(root, projectID, slug string) string {
	path := filepath.Join(root, run.ContentPath(projectID, slug, dash.IntentDocID))
	content, err := os.ReadFile(path)
	if err != nil {
		return slug
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			if title := strings.TrimSpace(rest); title != "" {
				return title
			}
		}
	}
	return slug
}
