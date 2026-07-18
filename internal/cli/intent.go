package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// `moe intent` parks standing direction on a project: a short,
// operator-authored statement of where the project is going — a theme,
// a bet — kept open while it's relevant and closed when it stops being
// so. Intents are just runs in a dedicated single-stage workflow
// (dash.IntentWorkflow, dash.IntentDocID), the same shape as ideas, and
// they share the discipline that no `moe intent` verb launches an agent:
// agents *read* intents (via the stage-prompt catalog and the pulse),
// only the operator writes them.
//
// Deliberately narrower than idea: no move, no reopen, no log in v1 —
// add on first real need. An intent is never promoted, never executed,
// never handed to an agent to advance.

func init() {
	g := NewCommandGroup("intent", "intent workflow")
	g.Register(&Command{
		Name:    "new",
		Summary: "park a new intent in $EDITOR",
		Run:     runIntentNew,
	})
	g.Register(&Command{
		Name:    "edit",
		Summary: "sharpen a parked intent in $EDITOR",
		Run:     runIntentEdit,
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
		Run:     runIntentList,
	})
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump an intent's canvas to stdout",
		Run:     runCat(dash.IntentWorkflow, dash.IntentDocID),
		argKind: argIntent,
	})
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

func runIntentNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("intent new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe intent new <project>/<slug>\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "intent new: %v\n", err)
		return 2
	}
	if canonical := run.Slugify(slug); canonical != slug {
		moePrintf(stderr, "intent new: slug must match [a-z0-9-]+ (lowercase kebab), got %q\n", slug)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "intent: set $EDITOR or $VISUAL — intent new needs an editor")
		return 1
	}

	// Pre-flight the slug before the editor pop, matching idea new: refuse
	// the obvious collision before the operator types into a tempfile
	// we'd otherwise throw away. run.New re-checks inside the lock and is
	// the authority; this gate just closes the common case with the same
	// wording so the error reads the same regardless of which gate caught it.
	if taken, err := run.SlugTaken(root, projectID, slug); err != nil {
		moePrintf(stderr, "intent new: %v\n", err)
		return 1
	} else if taken {
		suggestion, serr := run.NextFreeID(root, projectID, slug)
		if serr != nil {
			moePrintf(stderr, "intent new: %v\n", serr)
			return 1
		}
		moePrintf(stderr,
			"intent new: slug %q in project %s is already used (existing run or prior history); try %q or pick a different name\n",
			slug, projectID, suggestion)
		return 1
	}

	body, tmpPath, code := captureEditorBody("moe-intent-new-", fmt.Sprintf("# %s\n", slug), stdout, stderr)
	if code != 0 {
		if tmpPath != "" {
			moePrintf(stderr, "intent: your edited canvas is preserved at %s\n", tmpPath)
		}
		return code
	}
	keepTmp := false
	defer func() {
		if !keepTmp {
			os.RemoveAll(filepath.Dir(tmpPath))
		}
	}()

	opts := run.Options{
		ID:       slug,
		Workflow: dash.IntentWorkflow,
		SeedDocs: map[string]string{dash.IntentDocID: body},
	}
	var md *run.Metadata
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "intent-new",
		Run:     projectID + "/" + slug,
	}, stdout, stderr, func() error {
		m, err := run.New(root, projectID, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		keepTmp = true
		moePrintf(stderr, "intent: %v\n", err)
		moePrintf(stderr, "intent: your edited canvas is preserved at %s\n", tmpPath)
		return 1
	}
	moePrintf(stdout, "parked intent %s/%s\n", md.Project, md.ID)
	return 0
}

func runIntentEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("intent edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe intent edit <project>/<slug>\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "intent edit: %v\n", err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if _, err := loadIntentRun(root, projectID, slug); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "intent: set $EDITOR or $VISUAL — intent edit needs an editor")
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, dash.IntentDocID))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "intent: canvas missing: %v\n", err)
		return 1
	}

	if code := launchEditor(abs, stdout, stderr); code != 0 {
		return code
	}

	docDir := run.DocDir(projectID, slug, dash.IntentDocID)
	msg := fmt.Sprintf("work: update %s\n\n", dash.IntentDocID) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: dash.IntentWorkflow,
			Document: dash.IntentDocID,
		}.String()
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "intent-edit",
		Run:     projectID + "/" + slug,
	}, stdout, stderr, func() error {
		return run.StageAndCommit(root, msg, docDir)
	})
	switch {
	case errors.Is(err, run.ErrNothingToCommit):
		moePrintf(stdout, "intent %s/%s unchanged\n", projectID, slug)
	case err != nil:
		moePrintf(stderr, "intent: commit: %v\n", err)
		return 1
	default:
		moePrintf(stdout, "sharpened intent %s/%s\n", projectID, slug)
	}
	return 0
}

// runIntentClose closes an intent (satisfied or abandoned). Delegates to
// the shared close handler in close.go; intents keep the short `Close
// intent <p>/<r>` subject and the capture-workflow close path (clean-tree
// gate, no followups harvest, canvas-empty exemption) — the same path
// idea close rides.
func runIntentClose(args []string, stdout, stderr io.Writer) int {
	return runClose(dash.IntentWorkflow, "Close intent %s/%s", nil, args, stdout, stderr)
}

func runIntentList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("intent list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe intent list <project>")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	entries, err := scanOpenIntents(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].slug < entries[j].slug })
	for _, e := range entries {
		fmt.Fprintln(stdout, e.slug)
	}
	return 0
}

// intentEntry is the minimal projection of an intent run used by `moe
// intent list` and the dash's INTENTS section.
type intentEntry struct {
	project string
	slug    string
}

// scanOpenIntents returns projectID's in-progress intent runs. (The dash
// gatherer doesn't come through here — gatherIntents projects the
// caller's existing scan instead.)
func scanOpenIntents(root, projectID string) ([]intentEntry, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	out := make([]intentEntry, 0, len(mds))
	for _, md := range mds {
		if md.Workflow != dash.IntentWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		if md.Project != projectID {
			continue
		}
		out = append(out, intentEntry{
			project: md.Project,
			slug:    md.ID,
		})
	}
	return out, nil
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

// loadIntentRun loads an intent run and verifies it is in fact an intent
// run (workflow=intent), producing a recognisable error when the slug
// names a different workflow's run.
func loadIntentRun(root, projectID, slug string) (*run.Metadata, error) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return nil, fmt.Errorf("intent %s/%s does not exist; run `moe intent list %s` to see open intents", projectID, slug, projectID)
		}
		return nil, err
	}
	if md.Workflow != dash.IntentWorkflow {
		return nil, fmt.Errorf("run %s/%s is a %s run, not an intent", projectID, slug, md.Workflow)
	}
	return md, nil
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
