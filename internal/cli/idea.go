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

	"github.com/modulecollective/moe/internal/agent"
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
// same as sdlc. Capture stays cheap: `moe idea new` never launches an
// agent. Refinement is the one door an agent may hold — `moe idea edit
// --chat` opens an ordinary stage session on the idea's own document
// (see openIdeaChat).
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
		Summary: "refine a captured idea ($EDITOR, or --chat for an agent session)",
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
		Name:    "tag",
		Summary: "license the machine to promote an idea (workflow tag, default sdlc)",
		Run:     runIdeaTag,
		argKind: argIdea,
	})
	g.Register(&Command{
		Name:    "untag",
		Summary: "clear an idea's workflow tag — the per-idea pause",
		Run:     runIdeaUntag,
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
	// Manual recovery for captures a session couldn't fan out — same
	// reasoning as the intent group's registration: idea close is a
	// capture close and never harvests, so this verb is the only
	// operator-driven path to what an `edit --chat` session filed.
	g.Register(harvestCommand(dash.IdeaWorkflow))
	RegisterGroup(g)

	// Register the idea workflow so run.Load, dash lookup, and
	// --from-idea's wf.Stages() all resolve it. The single stage name
	// `idea` lives in the DAG without a matching `moe idea idea` verb
	// — operator-facing verbs (new/edit/close/list/cat) are group
	// subcommands above. wf.Next reporting "idea" is fine: `edit --chat`
	// opens the stage session directly and suppresses the chain prompt
	// (a single-stage workflow has nowhere to chain to).
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
	body, tmpPath, code := captureEditorBody("moe-idea-new-", fmt.Sprintf("# %s\n", slug), stderr)
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
// so the canvas isn't blank. promoteTo is the optional harvested
// follow-up workflow tag persisted on the idea. extra carries optional
// trailers riding along on the open commit (e.g. MoE-From-Run for
// harvested ideas). Returns the opened run's metadata so callers can
// see the resolved slug.
func createIdea(root, projectID, slugBase, body, promoteTo string, extra trailers.Block) (*run.Metadata, error) {
	if slugBase == "" {
		return nil, fmt.Errorf("idea: empty slug")
	}
	candidate := slugBase
	for n := 2; ; n++ {
		if body == "" {
			body = fmt.Sprintf("# %s\n", candidate)
		}
		opts := run.Options{
			ID:        candidate,
			Workflow:  dash.IdeaWorkflow,
			SeedDocs:  map[string]string{dash.IdeaDocID: body},
			PromoteTo: promoteTo,
			Trailers:  extra,
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
	chat := fs.Bool("chat", false, "refine in an interactive agent session instead of $EDITOR")
	agentOverride := fs.String("agent", "", "with --chat, override the agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea edit [--chat] [--agent <name>] <project>/<slug>\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if code := checkChatAgentFlags("idea edit", *chat, *agentOverride, stderr); code != 0 {
		return code
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
	if *chat {
		return openIdeaChat(projectID, slug, *agentOverride, stdout, stderr)
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL (or pass --chat) — idea edit needs an editor")
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, dash.IdeaDocID))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "idea: canvas missing: %v\n", err)
		return 1
	}

	if code := launchEditor(abs, stderr); code != 0 {
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

// ideaChatKickoff is the first user message of a refinement session.
// It has to do two jobs the fragment can't: say that *this* turn was
// opened by an operator who wants to talk (so the agent asks before
// editing rather than rewriting on sight), and re-assert the shelf
// boundary at the point of action.
const ideaChatKickoff = "The operator just opened this idea to refine it. Read the " +
	"canvas first, then ask what they want to sharpen — one question, and wait for " +
	"their answer before you edit. Keep it a shelf note: sharper framing, a concrete " +
	"example, a smaller scope. Not a design."

// openIdeaChat is the Go-level seam behind `moe idea edit --chat`: an
// ordinary interactive stage session on the idea's own document. Riding
// runStageSession (rather than the run-less bespoke path this feature
// had before `ditch-idea-chat` removed it) is the whole point — the
// session id lands in run.json's `documents.idea.session` and resumes on
// the next `--chat`, which is what gives `moe idea log` a transcript to
// render.
//
// Three knobs beyond the defaults:
//
//   - NeedsSandbox stays false. An idea is a shelf note; refining it
//     sharpens the operator's framing of a problem, it doesn't verify
//     claims about code. That's the line against `moe chat`, which
//     attaches a clone to think *with* the project.
//   - SkipNextStage is always on. idea is a single-stage workflow with
//     no successor, so the only post-turn prompt available is the close
//     nudge — wrong for a shelf entry the operator is still deciding
//     about. Session exit drops them back to the shell.
//   - HarvestOnExit is always on. The session's prompt invites the agent
//     to file followups and lore like any other stage, but idea close is
//     a capture close and skips harvest, so without this the captures
//     would be committed to the journal and stranded there.
//
// Interactive-only: there is no headless parameter and no oneshot.md
// fragment, because nothing cascades into an idea.
func openIdeaChat(projectID, slug, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, slug, dash.IdeaDocID,
		stageSessionOpts{
			InitialPrompt: ideaChatKickoff,
			SkipNextStage: true,
			HarvestOnExit: true,
			Agent:         agentOverride,
		}, stdout, stderr)
}

// checkChatAgentFlags validates the `--chat` / `--agent` pair the two
// edit verbs share. `--agent` without `--chat` is a usage error rather
// than a silent no-op: the editor path spawns no agent, so accepting the
// flag would be the dead-flag smell the removed idea-chat path had.
// verb names the caller for the error prefix. Returns 0 to proceed.
func checkChatAgentFlags(verb string, chat bool, agentOverride string, stderr io.Writer) int {
	if agentOverride == "" {
		return 0
	}
	if !chat {
		moePrintf(stderr, "%s: --agent needs --chat; the $EDITOR path launches no agent\n", verb)
		return 2
	}
	if _, err := agent.Get(agentOverride); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 2
	}
	return 0
}

// runIdeaClose is the entry point for `moe idea close`. Delegates to
// the shared close handler in close.go; ideas keep the short `Close
// idea <p>/<r>` subject shape that predates the shared helper (sdlc
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

// defaultPromoteTag is the workflow `moe idea tag` stamps when the
// operator names none. sdlc is the overwhelming case — the tag says
// "an agent could just execute this" and sdlc is how work executes.
const defaultPromoteTag = "sdlc"

// runIdeaTag stamps a workflow tag onto a parked idea. The tag is the
// machine's license: the pulse survey proposes only tagged ideas, so
// tagging is how the operator says "you may start this" without the
// promote ritual. It is a license, not a schedule — the survey still
// decides whether and where the idea rides.
//
// The workflow argument is optional and defaults to sdlc; it is
// validated against the same bar the followups grammar applies to a
// filer's `(sdlc)` tag, so the two mint identical state.
func runIdeaTag(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea tag <project>/<slug> [workflow]\n")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 && fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	workflow := defaultPromoteTag
	if fs.NArg() == 2 {
		workflow = fs.Arg(1)
	}
	if err := validatePromoteTag(workflow); err != nil {
		moePrintf(stderr, "idea tag: %v\n", err)
		return 2
	}
	return setIdeaTag(fs.Arg(0), workflow, "idea tag", stdout, stderr)
}

// runIdeaUntag clears an idea's workflow tag — the per-idea pause. An
// untagged idea is operator-fenced: no pulse will propose it, whoever
// filed it and whatever ran before.
func runIdeaUntag(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea untag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea untag <project>/<slug>\n")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	return setIdeaTag(fs.Arg(0), "", "idea untag", stdout, stderr)
}

// setIdeaTag is the body both verbs share: resolve the ref, apply the
// idea gates, and write the tag through the same seam the dash chips
// use. An empty workflow untags; verb names the caller for error
// prefixes.
func setIdeaTag(ref, workflow, verb string, stdout, stderr io.Writer) int {
	projectID, slug, err := splitProjectRun(ref)
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
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

	md, err := loadIdeaRun(root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if md.Status != run.StatusInProgress {
		moePrintf(stderr, "idea %s/%s is %s, not open — refusing to change its tag\n", projectID, slug, md.Status)
		return 1
	}

	err = runopen.TagIdea(root, projectID, slug, workflow, stdout, stderr)
	switch {
	case errors.Is(err, run.ErrNothingToCommit):
		// Already in the requested state — say so and exit clean, so a
		// double-tap (or a re-run of the same one-liner) is a no-op
		// rather than a failure.
		if workflow == "" {
			moePrintf(stdout, "idea %s/%s is already untagged\n", projectID, slug)
		} else {
			moePrintf(stdout, "idea %s/%s is already tagged → %s\n", projectID, slug, workflow)
		}
	case err != nil:
		moePrintf(stderr, "%s: %v\n", verb, err)
		return 1
	case workflow == "":
		moePrintf(stdout, "untagged idea %s/%s\n", projectID, slug)
	default:
		moePrintf(stdout, "tagged idea %s/%s → %s\n", projectID, slug, workflow)
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
func captureEditorBody(prefix, stub string, stderr io.Writer) (body, tmpPath string, code int) {
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
	if c := launchEditor(path, stderr); c != 0 {
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
func launchEditor(path string, stderr io.Writer) int {
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
