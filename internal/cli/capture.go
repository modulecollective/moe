package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// captureKind is what tells the two capture workflows apart in the verbs
// they share — new, edit, list, the edit --chat session, scan and load.
// The plumbing is identical; only the workflow, the document and the
// operator-facing words differ. Verbs one kind has and the other doesn't
// (idea tag/untag/move/reopen) stay in that kind's file.
type captureKind struct {
	workflow string
	docID    string
	noun     string // "idea": usage lines, errors, list hints
	captured string // past tense `new` prints: "captured idea p/s"
	refine   string // the --chat flag's verb: "refine in an interactive …"
	refined  string // past tense `edit` prints: "refined idea p/s"
	kickoff  string // first user message of an edit --chat session
}

var ideaKind = captureKind{
	workflow: dash.IdeaWorkflow,
	docID:    dash.IdeaDocID,
	noun:     "idea",
	captured: "captured",
	refine:   "refine",
	refined:  "refined",
	// It has to do two jobs the fragment can't: say that *this* turn was
	// opened by an operator who wants to talk (so the agent asks before
	// editing rather than rewriting on sight), and re-assert the shelf
	// boundary at the point of action.
	kickoff: "The operator just opened this idea to refine it. Read the " +
		"canvas first, then ask what they want to sharpen — one question, and wait for " +
		"their answer before you edit. Keep it a shelf note: sharper framing, a concrete " +
		"example, a smaller scope. Not a design.",
}

var intentKind = captureKind{
	workflow: dash.IntentWorkflow,
	docID:    dash.IntentDocID,
	noun:     "intent",
	captured: "parked",
	refine:   "sharpen",
	refined:  "sharpened",
	// Where the idea kickoff guards against a design pass, this one guards
	// against authorship: the agent asks, then types what it heard.
	kickoff: "The operator just opened this intent to sharpen it. Read " +
		"the canvas first, then ask what they want to change — one question, and wait for " +
		"their answer before you edit. The direction is theirs: write what they say, ask " +
		"the question that tightens the bet, never invent direction the conversation " +
		"didn't state. An intent is a few lines, not an essay.",
}

// runNew is `moe <kind> new`: pop $EDITOR on a stub, then open the
// run with the edited body as its canvas. Capture never launches an agent.
func (k captureKind) runNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(k.noun+" new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s new <project>/<slug>\n", k.noun)
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
		moePrintf(stderr, "%s new: %v\n", k.noun, err)
		return 2
	}
	if canonical := run.Slugify(slug); canonical != slug {
		moePrintf(stderr, "%s new: slug must match [a-z0-9-]+ (lowercase kebab), got %q\n", k.noun, slug)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := ensureRootProject(root, projectID); err != nil {
		moePrintf(stderr, "%s new: %v\n", k.noun, err)
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
		moePrintf(stderr, "%s: set $EDITOR or $VISUAL — %s new needs an editor\n", k.noun, k.noun)
		return 1
	}

	// Pre-flight the slug before the editor pop. run.New checks again
	// inside the lock and is the authority on collisions; this gate
	// just refuses the obvious case before the operator types into a
	// tempfile we'd otherwise have to throw away (the original
	// late-bail bug). Match run.New's wording so the operator sees the
	// same error regardless of which gate caught it.
	if taken, err := run.SlugTaken(root, projectID, slug); err != nil {
		moePrintf(stderr, "%s new: %v\n", k.noun, err)
		return 1
	} else if taken {
		suggestion, serr := run.NextFreeID(root, projectID, slug)
		if serr != nil {
			moePrintf(stderr, "%s new: %v\n", k.noun, serr)
			return 1
		}
		moePrintf(stderr,
			"%s new: slug %q in project %s is already used (existing run or prior history); try %q or pick a different name\n",
			k.noun, slug, projectID, suggestion)
		return 1
	}

	// Pop $EDITOR on a stub written outside the bureaucracy tree, then
	// pass the edited body into run.New as seed content — run.New writes
	// the canvas at its canonical location and commits run.json + canvas
	// atomically. captureEditorBody returns tmpPath when there is edited
	// text worth preserving (the editor ran); the deferred cleanup below
	// removes it on success and keepTmp guards the post-editor failure
	// window.
	body, tmpPath, code := captureEditorBody("moe-"+k.noun+"-new-", fmt.Sprintf("# %s\n", slug), stderr)
	if code != 0 {
		if tmpPath != "" {
			moePrintf(stderr, "%s: your edited canvas is preserved at %s\n", k.noun, tmpPath)
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
		Workflow: k.workflow,
		SeedDocs: map[string]string{k.docID: body},
	}
	var md *run.Metadata
	err = repolock.With(root, repolock.Options{
		Purpose: k.noun + "-new",
		Run:     projectID + "/" + slug,
	}, func() error {
		m, err := run.New(root, projectID, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		keepTmp = true
		moePrintf(stderr, "%s: %v\n", k.noun, err)
		moePrintf(stderr, "%s: your edited canvas is preserved at %s\n", k.noun, tmpPath)
		return 1
	}
	moePrintf(stdout, "%s %s %s/%s\n", k.captured, k.noun, md.Project, md.ID)
	return 0
}

// runEdit is `moe <kind> edit`: $EDITOR on the canvas in place and a
// commit, or with --chat an agent session on the same document.
func (k captureKind) runEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(k.noun+" edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	chat := fs.Bool("chat", false, k.refine+" in an interactive agent session instead of $EDITOR")
	agentOverride := fs.String("agent", "", "with --chat, override the agent for this turn (claude/codex); does not persist")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s edit [--chat [--agent <name>]] <project>/<slug>\n", k.noun)
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if code := checkChatAgentFlags(k.noun+" edit", *chat, *agentOverride, stderr); code != 0 {
		return code
	}
	projectID, slug, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "%s edit: %v\n", k.noun, err)
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

	if _, err := k.loadRun(root, projectID, slug); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if *chat {
		return k.openChat(projectID, slug, *agentOverride, stdout, stderr)
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintf(stderr, "%s: set $EDITOR or $VISUAL (or pass --chat) — %s edit needs an editor\n", k.noun, k.noun)
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, k.docID))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "%s: canvas missing: %v\n", k.noun, err)
		return 1
	}

	if code := launchEditor(abs, stderr); code != 0 {
		return code
	}

	docDir := run.DocDir(projectID, slug, k.docID)
	msg := fmt.Sprintf("work: update %s\n\n", k.docID) +
		trailers.Block{
			Run:      slug,
			Project:  projectID,
			Workflow: k.workflow,
			Document: k.docID,
		}.String()
	err = repolock.With(root, repolock.Options{
		Purpose: k.noun + "-edit",
		Run:     projectID + "/" + slug,
	}, func() error {
		return run.StageAndCommit(root, msg, docDir)
	})
	switch {
	case errors.Is(err, run.ErrNothingToCommit):
		moePrintf(stdout, "%s %s/%s unchanged\n", k.noun, projectID, slug)
	case err != nil:
		moePrintf(stderr, "%s: commit: %v\n", k.noun, err)
		return 1
	default:
		moePrintf(stdout, "%s %s %s/%s\n", k.refined, k.noun, projectID, slug)
	}
	return 0
}

// openChat is the Go-level seam behind `moe idea edit --chat` and `moe
// intent edit --chat`: an ordinary interactive stage session on the
// capture's own document. Riding runStageSession (rather than the
// run-less bespoke path this feature had before `ditch-idea-chat` removed
// it) is the whole point — the session id lands in run.json's
// `documents.<doc>.session` and resumes on the next `--chat`, which is
// what gives `moe idea log` / `moe intent log` a transcript to render.
//
// Three knobs beyond the defaults:
//
//   - NeedsSandbox stays false. A capture is a shelf note or a few lines
//     of direction; sharpening it doesn't verify claims about code.
//     That's the line against `moe chat`, which attaches a clone to
//     think *with* the project.
//   - SkipNextStage is always on. Both are single-stage workflows with
//     no successor, so the only post-turn prompt available is the close
//     nudge — wrong for an entry the operator is still deciding about.
//     Session exit drops them back to the shell.
//   - HarvestOnExit is always on. The session's prompt invites the agent
//     to file followups and lore like any other stage, but capture close
//     skips harvest, so without this the captures would be committed to
//     the journal and stranded there.
//
// For intents the agent is the pen, not the author: the fragment and the
// kickoff both cast it as scribe (see intent.go's package comment).
//
// Interactive-only: there is no headless parameter and no oneshot.md
// fragment, because nothing cascades into a capture.
func (k captureKind) openChat(projectID, slug, agentOverride string, stdout, stderr io.Writer) int {
	return runStageSession(projectID, slug, k.docID,
		stageSessionOpts{
			ValidateEntry: inProgressSessionEntry(k.workflow),
			InitialPrompt: k.kickoff,
			SkipNextStage: true,
			HarvestOnExit: true,
			Agent:         agentOverride,
		}, stdout, stderr)
}

func (k captureKind) runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(k.noun+" list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe %s list <project>\n", k.noun)
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

	entries, err := k.scanOpen(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	for _, slug := range entries {
		fmt.Fprintln(stdout, slug)
	}
	return 0
}

// scanOpen returns the slugs of projectID's in-progress runs of this
// kind, sorted. (The dash gatherers don't come through here — they
// project the caller's existing scan instead.)
func (k captureKind) scanOpen(root, projectID string) ([]string, error) {
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, md := range mds {
		if md.Workflow == k.workflow && md.Status == run.StatusInProgress && md.Project == projectID {
			out = append(out, md.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// loadRun loads a run and verifies it is of this kind, producing a
// recognisable error when the slug names a different workflow's run.
func (k captureKind) loadRun(root, projectID, slug string) (*run.Metadata, error) {
	md, err := run.Load(root, projectID, slug)
	if err != nil {
		if errors.Is(err, run.ErrRunNotFound) {
			return nil, fmt.Errorf("%s %s/%s does not exist; run `moe %s list %s` to see open %ss", k.noun, projectID, slug, k.noun, projectID, k.noun)
		}
		return nil, err
	}
	if md.Workflow != k.workflow {
		return nil, fmt.Errorf("run %s/%s is a %s run, not an %s", projectID, slug, md.Workflow, k.noun)
	}
	return md, nil
}
