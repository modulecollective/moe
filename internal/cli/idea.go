package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/sync"
	"github.com/modulecollective/moe/internal/trailers"
)

// `moe idea` is the backlog surface: a shelf of thoughts-worth-capturing
// that sit between nothing and a full run. Ideas are just runs in a
// dedicated single-stage workflow (dash.IdeaWorkflow, dash.IdeaDocID) so
// the slug namespace, dash bucketing, and trailer conventions are the
// same as sdlc/kb. The distinguishing discipline: `moe idea` verbs
// never launch an agent — capture stays cheap.
//
// idea is reached one way — `moe idea <verb>` — same as every other
// workflow's top-level form. The Workflow registration is a separate
// concern (run.Load, dash lookup, `--from-idea` resolution all key off
// it); the operator-facing dispatch table is the top-level Command
// registered here.

func init() {
	g := NewCommandGroup("idea", "idea workflow")
	g.Register(&Command{
		Name:    "new",
		Summary: "capture a new idea in $EDITOR",
		Run:     runIdeaNew,
	})
	g.Register(&Command{
		Name:    "edit",
		Summary: "refine a captured idea in $EDITOR",
		Run:     runIdeaEdit,
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "close",
		Summary: "close a captured idea without promoting (status → closed)",
		Run:     runIdeaClose,
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "list",
		Summary: "list this project's open ideas",
		Run:     runIdeaList,
	})
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump an idea's canvas to stdout",
		Run:     runCat(dash.IdeaWorkflow, dash.IdeaDocID),
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render an idea's agent transcript",
		Run:     runLog(dash.IdeaWorkflow, dash.IdeaDocID),
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "move",
		Summary: "re-home an open idea under a different project",
		Run:     runIdeaMove,
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "reopen",
		Summary: "flip a promoted idea back to in_progress after its destination run was abandoned",
		Run:     runIdeaReopen,
		argKind: argIdea,
	})
	RegisterGroup(g)

	// Register the idea workflow so run.Load, dash lookup, and
	// --from-idea's wf.Stages() all resolve it. The single stage name
	// `idea` lives in the DAG without a matching `moe idea idea` verb
	// — operator-facing verbs (new/edit/close/list/cat) are group
	// subcommands above. wf.Next reporting "idea" is fine: no chain
	// prompt or resume path ever reaches the idea workflow today.
	w := NewWorkflow(dash.IdeaWorkflow)
	w.RegisterStage(dash.IdeaDocID)
	RegisterWorkflow(w)
}

func runIdeaNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea new <project>/<slug>\n")
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
		moePrintf(stderr, "idea new: %v\n", err)
		return 2
	}
	if canonical := run.Slugify(slug); canonical != slug {
		moePrintf(stderr, "idea new: slug must match [a-z0-9-]+ (lowercase kebab), got %q\n", slug)
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
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL — idea new needs an editor")
		return 1
	}

	// Pre-flight the slug before the editor pop. run.New checks again
	// inside the lock and is the authority on collisions; this gate
	// just refuses the obvious case before the operator types into a
	// tempfile we'd otherwise have to throw away (the original
	// late-bail bug). Match run.New's wording so the operator sees the
	// same error regardless of which gate caught it.
	if taken, err := run.SlugTaken(root, projectID, slug); err != nil {
		moePrintf(stderr, "idea new: %v\n", err)
		return 1
	} else if taken {
		suggestion, serr := run.NextFreeID(root, projectID, slug)
		if serr != nil {
			moePrintf(stderr, "idea new: %v\n", serr)
			return 1
		}
		moePrintf(stderr,
			"idea new: slug %q in project %s is already used (existing run or prior history); try %q or pick a different name\n",
			slug, projectID, suggestion)
		return 1
	}

	// Pop $EDITOR on a stub written outside the bureaucracy tree, then
	// pass the edited body into run.New as seed content — run.New writes
	// the canvas at its canonical location and commits run.json + canvas
	// atomically. captureEditorBody returns tmpPath when there is edited
	// text worth preserving (the editor ran); the deferred cleanup below
	// removes it on success and keepTmp guards the post-editor failure
	// window.
	body, tmpPath, code := captureEditorBody("moe-idea-new-", fmt.Sprintf("# %s\n", slug), stdout, stderr)
	if code != 0 {
		if tmpPath != "" {
			moePrintf(stderr, "idea: your edited canvas is preserved at %s\n", tmpPath)
		}
		return code
	}
	// Default-clean: cleanup happens unless a post-editor failure flips
	// keepTmp. The editor session is a multi-minute window, so anything
	// that fails after the operator may have written content keeps the
	// tempfile and names its absolute path on stderr — the pre-flight
	// above closes the common collision case, this is the safety net for
	// whatever races slip through (concurrent harvest, late-arriving
	// error from run.New).
	keepTmp := false
	defer func() {
		if !keepTmp {
			os.RemoveAll(filepath.Dir(tmpPath))
		}
	}()

	opts := run.Options{
		ID:       slug,
		Workflow: dash.IdeaWorkflow,
		SeedDocs: map[string]string{dash.IdeaDocID: body},
	}
	var md *run.Metadata
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "idea-new",
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
		moePrintf(stderr, "idea: %v\n", err)
		moePrintf(stderr, "idea: your edited canvas is preserved at %s\n", tmpPath)
		return 1
	}
	moePrintf(stdout, "captured idea %s/%s\n", md.Project, md.ID)
	return 0
}

// createIdea opens a new idea run with slug auto-disambiguated from
// slugBase: if slugBase is taken, tries slugBase-2, slugBase-3, … until
// one is free. Used by the close-time followups harvester (idea new
// goes through run.New directly with the operator-typed slug). Caller
// holds the bureaucracy lock — createIdea does NOT take its own, so it
// can run inside an existing repolock acquisition (e.g. the harvest
// loop inside runClose).
//
// body is the seed canvas body; an empty body falls back to "# slug\n"
// so the canvas isn't blank. extra carries optional trailers riding
// along on the open commit (e.g. MoE-From-Run for harvested ideas).
// Returns the opened run's metadata so callers can see the resolved
// slug.
func createIdea(root, projectID, slugBase, body string, extra trailers.Block) (*run.Metadata, error) {
	if slugBase == "" {
		return nil, fmt.Errorf("idea: empty slug")
	}
	candidate := slugBase
	for n := 2; ; n++ {
		if body == "" {
			body = fmt.Sprintf("# %s\n", candidate)
		}
		opts := run.Options{
			ID:       candidate,
			Workflow: dash.IdeaWorkflow,
			SeedDocs: map[string]string{dash.IdeaDocID: body},
			Trailers: extra,
			// Callers (idea new, harvest) gate on dirty state above.
			// The harvester in particular runs while followups.md is
			// dirty by design — let those modifications stand and
			// rely on each call's explicit addPaths to keep the new
			// run's open commit clean.
			AllowDirty: true,
		}
		md, err := run.New(root, projectID, opts)
		if err == nil {
			return md, nil
		}
		if !errors.Is(err, run.ErrSlugTaken) {
			return nil, err
		}
		candidate = fmt.Sprintf("%s-%d", slugBase, n)
	}
}

func runIdeaEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea edit <project>/<slug>\n")
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
		moePrintf(stderr, "idea edit: %v\n", err)
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

	if _, err := loadIdeaRun(root, projectID, slug); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL — idea edit needs an editor")
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, dash.IdeaDocID))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "idea: canvas missing: %v\n", err)
		return 1
	}

	if code := launchEditor(abs, stdout, stderr); code != 0 {
		return code
	}

	docDir := run.DocDir(projectID, slug, dash.IdeaDocID)
	msg := fmt.Sprintf("work: update %s\n\n", dash.IdeaDocID) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: dash.IdeaWorkflow,
			Document: dash.IdeaDocID,
		}.String()
	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "idea-edit",
		Run:     projectID + "/" + slug,
	}, stdout, stderr, func() error {
		return run.StageAndCommit(root, msg, docDir)
	})
	switch {
	case errors.Is(err, run.ErrNothingToCommit):
		moePrintf(stdout, "idea %s/%s unchanged\n", projectID, slug)
	case err != nil:
		moePrintf(stderr, "idea: commit: %v\n", err)
		return 1
	default:
		moePrintf(stdout, "refined idea %s/%s\n", projectID, slug)
	}
	return 0
}

// runIdeaClose is the entry point for `moe idea close`. Delegates to
// the shared close handler in close.go; ideas keep the short `Close
// idea <p>/<r>` subject shape that predates the shared helper (sdlc/kb
// use `Close <wf> run <p>/<r>` — see design).
func runIdeaClose(args []string, stdout, stderr io.Writer) int {
	return runClose(dash.IdeaWorkflow, "Close idea %s/%s", nil, args, stdout, stderr)
}

func runIdeaList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe idea list <project>")
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

	entries, err := scanOpenIdeas(root, projectID)
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

// runIdeaMove re-homes an open idea run from <project>/<slug> to
// <to-project>/<slug>. Slug is unchanged — slugs are project-scoped on
// disk and keeping it stable means any stored reference (followups
// notes, prior canvases) doesn't silently break. Refuses on wrong
// workflow, non-open status, missing destination project, slug
// collision at destination, or same-project no-op. See design doc
// move-ideas-between-projects-or-at-capture for rationale.
func runIdeaMove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea move", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea move <project>/<slug> <to-project>\n")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	fromProject, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "idea move: %v\n", err)
		return 2
	}
	toProject := fs.Arg(1)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, fromProject); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if err := requireProject(root, toProject); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if fromProject == toProject {
		moePrintf(stderr, "idea: source and destination project are the same (%s) — nothing to move\n", fromProject)
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	md, err := loadIdeaRun(root, fromProject, slug)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if md.Status != run.StatusInProgress {
		moePrintf(stderr, "idea %s/%s is %s, not open — refusing to move\n", fromProject, slug, md.Status)
		return 1
	}

	fromRel := run.Dir(fromProject, slug)
	destRel := run.Dir(toProject, slug)
	if _, err := os.Stat(filepath.Join(root, destRel)); err == nil {
		moePrintf(stderr,
			"idea: %s already exists; close or rename it before moving %s here\n",
			destRel, slug)
		return 1
	}

	msg := fmt.Sprintf("Move idea %s/%s to %s\n\n", fromProject, slug, toProject) +
		trailers.Block{
			Run:           slug,
			Project:       toProject,
			Workflow:      dash.IdeaWorkflow,
			IdeaMovedFrom: fromProject + "/" + slug,
		}.String()

	err = sync.WithJournalPush(root, repolock.Options{
		Purpose: "idea-move",
		Run:     toProject + "/" + slug,
	}, stdout, stderr, func() error {
		// git mv refuses if the destination's parent dir doesn't exist,
		// and a project that has never opened a run has no runs/ yet.
		if err := os.MkdirAll(filepath.Join(root, "projects", toProject, "runs"), 0o755); err != nil {
			return fmt.Errorf("mkdir destination runs/: %w", err)
		}
		if err := git.Run(root, "mv", fromRel, destRel); err != nil {
			return fmt.Errorf("git mv: %w", err)
		}
		md.Project = toProject
		if err := run.Save(root, md); err != nil {
			return fmt.Errorf("save run.json: %w", err)
		}
		runJSONRel := filepath.Join(destRel, "run.json")
		if err := git.Run(root, "add", "--", runJSONRel); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
		return git.Run(root, "commit", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "idea: move: %v\n", err)
		return 1
	}
	moePrintf(stdout, "moved idea %s/%s to %s/%s\n", fromProject, slug, toProject, slug)
	return 0
}

// runIdeaReopen flips a closed idea back to in_progress. For promoted
// ideas, runopen.ReopenIdea preserves the destination-closed guard so
// reopening cannot create two live owners of the same intent.
func runIdeaReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea reopen <project>/<slug>\n")
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
		moePrintf(stderr, "idea reopen: %v\n", err)
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

	if err := runopen.ReopenIdea(root, projectID, slug, stdout, stderr); err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			moePrintf(stderr, "idea %s/%s does not exist; run `moe idea list %s` to see open ideas\n", projectID, slug, projectID)
		} else {
			moePrintf(stderr, "idea: reopen: %v\n", err)
		}
		return 1
	}
	moePrintf(stdout, "reopened idea %s/%s\n", projectID, slug)
	return 0
}

// ideaEntry is the minimal projection of an idea run used by `moe idea
// list` and `moe dash`'s backlog bucket.
type ideaEntry struct {
	project string
	slug    string
}

// scanOpenIdeas returns all in-progress idea runs for projectID. If
// projectID is "", all projects are scanned — used by dash.
func scanOpenIdeas(root, projectID string) ([]ideaEntry, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	out := make([]ideaEntry, 0, len(mds))
	for _, md := range mds {
		if md.Workflow != dash.IdeaWorkflow {
			continue
		}
		if md.Status != run.StatusInProgress {
			continue
		}
		if projectID != "" && md.Project != projectID {
			continue
		}
		out = append(out, ideaEntry{
			project: md.Project,
			slug:    md.ID,
		})
	}
	return out, nil
}

// loadIdeaRun loads an idea run and verifies it is in fact an idea run
// (workflow=idea), producing a recognisable error when the slug names
// a different workflow's run.
func loadIdeaRun(root, projectID, slug string) (*run.Metadata, error) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return nil, fmt.Errorf("idea %s/%s does not exist; run `moe idea list %s` to see open ideas", projectID, slug, projectID)
		}
		return nil, err
	}
	if md.Workflow != dash.IdeaWorkflow {
		return nil, fmt.Errorf("run %s/%s is a %s run, not an idea", projectID, slug, md.Workflow)
	}
	return md, nil
}

// requireProject errors if projectID has no project.json on disk.
func requireProject(root, projectID string) error {
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		return fmt.Errorf("project %s not registered (%s missing)",
			projectID, filepath.Join("projects", projectID, "project.json"))
	}
	return nil
}

// requireCleanTree errors if the working tree has uncommitted changes.
func requireCleanTree(root string) error {
	dirty, err := run.WorkingTreeDirty(root)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("working tree has uncommitted changes; commit or stash first")
	}
	return nil
}

// captureEditorBody seeds a fresh tempfile (content.md under a
// prefix-named tempdir outside the bureaucracy tree) with stub, launches
// $EDITOR/$VISUAL on it, and returns the edited body. Callers gate on an
// editor being configured before invoking.
//
// tmpPath is returned non-empty only when the editor actually ran, i.e.
// when there may be operator-typed text worth preserving: a failure
// before the editor pops (tempdir/stub write) cleans up its own scratch
// dir and returns an empty path, while an editor-launch or read failure
// returns the live path so the caller can preserve it. code is 0 on
// success. On success the caller owns tmpPath — it should delete
// filepath.Dir(tmpPath) once the body is committed, and keep it (naming
// the path on stderr) if the commit fails, since the multi-minute editor
// window makes the typed body the recoverable asset.
func captureEditorBody(prefix, stub string, stdout, stderr io.Writer) (body, tmpPath string, code int) {
	tmpDir, err := os.MkdirTemp("", prefix)
	if err != nil {
		moePrintf(stderr, "tempdir: %v\n", err)
		return "", "", 1
	}
	path := filepath.Join(tmpDir, "content.md")
	if err := os.WriteFile(path, []byte(stub), 0o644); err != nil {
		// Nothing typed yet — drop the scratch dir and return no path so
		// the caller never advertises a "preserved" file holding only the
		// stub.
		os.RemoveAll(tmpDir)
		moePrintf(stderr, "write stub: %v\n", err)
		return "", "", 1
	}
	// Past this point the operator may type into the file, so failures
	// hand the path back for the caller to preserve.
	if c := launchEditor(path, stdout, stderr); c != 0 {
		return "", path, c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		moePrintf(stderr, "read edited canvas: %v\n", err)
		return "", path, 1
	}
	return string(b), path, 0
}

// launchEditor opens path in $VISUAL or $EDITOR with stdio wired to
// the terminal, so the operator drops straight into editing the file.
// Callers are expected to have gated on an editor being available —
// running with neither var set is a programmer error.
func launchEditor(path string, stdout, stderr io.Writer) int {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	// $1 (not string interp) keeps paths with spaces/quotes/`;` shell-safe — don't collapse.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		moePrintf(stderr, "editor exited: %v\n", err)
		return 1
	}
	return 0
}
