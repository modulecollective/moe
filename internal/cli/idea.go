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
	"strings"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// `moe idea` is the backlog surface: a shelf of thoughts-worth-capturing
// that sit between nothing and a full run. Ideas are just runs in a
// dedicated single-stage workflow (ideaWorkflow, ideaDocID) so the slug
// namespace, dash bucketing, and trailer conventions are the same as
// sdlc/kb/quick. The distinguishing discipline: `moe idea` verbs never
// launch Claude unless --chat is passed — capture stays cheap.
//
// The idea workflow is reachable two ways: the short `moe idea <verb>`
// top-level command keeps capture cheap, and the same verbs also hang
// off `moe workflow idea <verb>` so the workflow namespace is complete.
// Both doors land on the same runIdeaNew/Edit/Close/List handlers —
// the short form stays canonical; the workflow form is there so
// `moe workflow` listings don't have a hole.

// ideaWorkflow is the workflow name written to run.json's `workflow`
// field for idea runs. Kept as a constant so the few places that
// special-case it (dash, from-idea promotion) can key off one symbol.
const ideaWorkflow = "idea"

// ideaDocID is the document id for the idea's sole stage. Canvas lives
// at projects/<p>/runs/<slug>/documents/idea/content.md.
const ideaDocID = "idea"

func init() {
	Register(&Command{
		Name:    "idea",
		Summary: "lightweight backlog: capture an idea or list ideas (no agent unless --chat)",
		Run:     runIdea,
	})

	// Register the idea workflow so run.Load, dash lookup, and
	// --from-idea's wf.Stages() all resolve it — and expose its verbs
	// under `moe workflow idea` as facades pointing at the same
	// handlers the top-level `moe idea` uses. The single stage is kept
	// as a defensive stub; the real operator-facing verbs are the
	// facades below.
	wf := NewWorkflow(ideaWorkflow, "lightweight backlog: capture and list ideas")
	wf.Register(&Command{
		Name:    ideaDocID,
		Summary: "idea canvas (use `moe idea edit` instead of invoking directly)",
		Hidden:  true, // kept in stageOrder for Workflow.Next; hidden from usage
		Run: func(args []string, stdout, stderr io.Writer) int {
			moePrintln(stderr, "idea runs are driven via `moe idea` — try `moe idea edit <project> <slug>`")
			return 2
		},
	})
	wf.RegisterFacade(&Command{
		Name:    "new",
		Summary: "capture a new idea (opens $EDITOR, or --chat for Claude Code)",
		Run:     runIdeaNew,
	})
	wf.RegisterFacade(&Command{
		Name:    "edit",
		Summary: "refine a captured idea ($EDITOR, or --chat for Claude Code)",
		Run:     runIdeaEdit,
	})
	wf.RegisterFacade(&Command{
		Name:    "close",
		Summary: "close a captured idea without promoting (status → closed)",
		Run:     runIdeaClose,
	})
	wf.RegisterFacade(&Command{
		Name:    "list",
		Summary: "list this project's open ideas",
		Run:     runIdeaList,
	})
	wf.RegisterFacade(&Command{
		Name:    "cat",
		Summary: "dump an idea's canvas to stdout",
		Run:     runIdeaCat,
	})
	RegisterWorkflow(wf)
}

func runIdea(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printIdeaUsage(stdout)
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		printIdeaUsage(stdout)
		return 0
	case "new":
		return runIdeaNew(args[1:], stdout, stderr)
	case "edit":
		return runIdeaEdit(args[1:], stdout, stderr)
	case "close":
		return runIdeaClose(args[1:], stdout, stderr)
	case "list":
		return runIdeaList(args[1:], stdout, stderr)
	case "cat":
		return runIdeaCat(args[1:], stdout, stderr)
	default:
		moePrintf(stderr, "unknown idea subcommand %q\n", args[0])
		printIdeaUsage(stderr)
		return 1
	}
}

func printIdeaUsage(w io.Writer) {
	moePrintln(w, "usage: moe idea <subcommand> [args...]")
	moePrintln(w, "")
	moePrintln(w, "subcommands:")
	moePrintf(w, "  %-14s  %s\n", "new", "capture a new idea (opens $EDITOR, or --chat for Claude Code)")
	moePrintf(w, "  %-14s  %s\n", "edit", "refine a captured idea ($EDITOR, or --chat for Claude Code)")
	moePrintf(w, "  %-14s  %s\n", "close", "close a captured idea without promoting (status → closed)")
	moePrintf(w, "  %-14s  %s\n", "list", "list this project's open ideas")
	moePrintf(w, "  %-14s  %s\n", "cat", "dump an idea's canvas to stdout")
}

func runIdeaNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	chat := fs.Bool("chat", false, "open a Claude Code session on the new idea instead of $EDITOR")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea new [--id <slug>] [--chat] <project> \"title\"\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)
	title := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if title == "" {
		moePrintln(stderr, "title is required")
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
	if !*chat && os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL (or pass --chat) — idea new needs an editor")
		return 1
	}

	// Write the stub to a tempfile outside the bureaucracy tree so the
	// editor/chat flow doesn't dirty it. We pass the edited body into
	// run.New as seed content — run.New writes the canvas at its
	// canonical location and commits run.json + canvas atomically.
	tmpDir, err := os.MkdirTemp("", "moe-idea-new-")
	if err != nil {
		moePrintf(stderr, "idea: tempdir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, "content.md")
	if err := os.WriteFile(tmpPath, []byte(fmt.Sprintf("# %s\n", title)), 0o644); err != nil {
		moePrintf(stderr, "idea: write stub: %v\n", err)
		return 1
	}

	if *chat {
		if code := runIdeaChat(root, tmpPath, "capture", stdout, stderr); code != 0 {
			return code
		}
	} else {
		if code := launchEditor(tmpPath, stdout, stderr); code != 0 {
			return code
		}
	}

	body, err := os.ReadFile(tmpPath)
	if err != nil {
		moePrintf(stderr, "idea: read edited canvas: %v\n", err)
		return 1
	}

	// --id is a hard override: collisions are an error so the operator
	// notices a typo. Without --id, fall through to createIdea which
	// auto-disambiguates from Slugify(title) — same path the close-time
	// followups harvester takes.
	var md *run.Metadata
	if *idOverride != "" {
		opts := run.Options{
			ID:       *idOverride,
			Workflow: ideaWorkflow,
			SeedDocs: map[string]string{ideaDocID: string(body)},
		}
		err = withRepoLock(root, repolock.Options{
			Purpose: "idea-new",
			Run:     projectID + "/" + *idOverride,
		}, func() error {
			m, err := run.New(root, projectID, title, opts)
			if err != nil {
				return err
			}
			md = m
			return nil
		})
	} else {
		err = withRepoLock(root, repolock.Options{
			Purpose: "idea-new",
			Run:     projectID,
		}, func() error {
			m, err := createIdea(root, projectID, run.Slugify(title), title, string(body), nil)
			if err != nil {
				return err
			}
			md = m
			return nil
		})
	}
	if err != nil {
		moePrintf(stderr, "idea: %v\n", err)
		return 1
	}
	moePrintf(stdout, "captured idea %s/%s\n", md.Project, md.ID)
	return 0
}

// createIdea opens a new idea run with slug auto-disambiguated from
// slugBase: if slugBase is taken, tries slugBase-2, slugBase-3, … until
// one is free. Used by `moe idea new` (without --id) and by the
// close-time followups harvester. Caller holds the bureaucracy lock —
// createIdea does NOT take its own, so it can run inside an existing
// repolock acquisition (e.g. the harvest loop inside runClose).
//
// body is the seed canvas body ("# Title\n" is fine for bare follow-ups;
// idea new threads the operator's edited body in instead). trailers are
// extra MoE-* lines appended to the open commit (e.g. MoE-From-Run for
// harvested ideas). Returns the opened run's metadata so callers can
// see the resolved slug.
func createIdea(root, projectID, slugBase, title, body string, trailers []string) (*run.Metadata, error) {
	if slugBase == "" {
		return nil, fmt.Errorf("idea: cannot derive slug from title %q", title)
	}
	if body == "" {
		body = fmt.Sprintf("# %s\n", title)
	}
	candidate := slugBase
	for n := 2; ; n++ {
		opts := run.Options{
			ID:            candidate,
			Workflow:      ideaWorkflow,
			SeedDocs:      map[string]string{ideaDocID: body},
			ExtraTrailers: trailers,
			// Callers (idea new, harvest) gate on dirty state above.
			// The harvester in particular runs while followups.md is
			// dirty by design — let those modifications stand and
			// rely on each call's explicit addPaths to keep the new
			// run's open commit clean.
			AllowDirty: true,
		}
		md, err := run.New(root, projectID, title, opts)
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
	chat := fs.Bool("chat", false, "open a Claude Code session on the idea instead of $EDITOR")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea edit [--chat] <project> <slug>\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID := fs.Arg(0)
	slug := fs.Arg(1)

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
	if !*chat && os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL (or pass --chat) — idea edit needs an editor")
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, ideaDocID))
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "idea: canvas missing: %v\n", err)
		return 1
	}

	if *chat {
		if code := runIdeaChat(root, abs, "refine", stdout, stderr); code != 0 {
			return code
		}
	} else {
		if code := launchEditor(abs, stdout, stderr); code != 0 {
			return code
		}
	}

	docDir := run.DocDir(projectID, slug, ideaDocID)
	msg := fmt.Sprintf(`work: update %s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
MoE-Document: %s
`, ideaDocID, slug, projectID, ideaWorkflow, ideaDocID)
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-edit",
		Run:     projectID + "/" + slug,
	}, func() error {
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
	return runClose(ideaWorkflow, "Close idea %s/%s", nil, args, stdout, stderr)
}

func runIdeaList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe idea list <project>")
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
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
		fmt.Fprintf(stdout, "%s\t%s\n", e.slug, e.title)
	}
	return 0
}

// runIdeaCat dumps an idea's canvas to stdout. Read-only by definition:
// no editor, no chat, no flags, no commit, no clean-tree gate. Slug
// resolution still goes through loadIdeaRun so a typo or wrong-workflow
// slug fails loud with the same message idea edit/close use.
func runIdeaCat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea cat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea cat <project> <slug>\n")
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	projectID, slug := fs.Arg(0), fs.Arg(1)

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := loadIdeaRun(root, projectID, slug); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	abs := filepath.Join(root, run.ContentPath(projectID, slug, ideaDocID))
	f, err := os.Open(abs)
	if err != nil {
		moePrintf(stderr, "idea: %v\n", err)
		return 1
	}
	defer f.Close()
	if _, err := io.Copy(stdout, f); err != nil {
		moePrintf(stderr, "idea: %v\n", err)
		return 1
	}
	return 0
}

// ideaEntry is the minimal projection of an idea run used by `moe idea
// list` and `moe dash`'s backlog bucket.
type ideaEntry struct {
	project string
	slug    string
	title   string
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
		if md.Workflow != ideaWorkflow {
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
			title:   md.Title,
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
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("idea %s/%s does not exist; run `moe idea list %s` to see open ideas", projectID, slug, projectID)
		}
		return nil, err
	}
	if md.Workflow != ideaWorkflow {
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

// runIdeaChat launches an interactive Claude Code session on the idea
// canvas. mode is "capture" (new idea) or "refine" (existing idea) and
// selects which stages/idea fragment seeds the system prompt. Unlike
// stage-session chats this one is one-shot: no --session-id, no
// thread persistence, no per-turn commits. When the operator exits
// claude, the caller stages & commits whatever landed on disk.
func runIdeaChat(root, abs, mode string, stdout, stderr io.Writer) int {
	bin, err := exec.LookPath("claude")
	if err != nil {
		moePrintf(stderr, "idea: claude CLI not found on PATH: %v\n", err)
		return 1
	}

	prompt := buildIdeaChatPrompt(abs, mode)

	var kickoff string
	switch mode {
	case "capture":
		kickoff = "The operator just captured a new idea. Read the canvas " +
			"file first. If the title is ambiguous, ask one clarifying " +
			"question; otherwise write a terse body (one to ten bullets) " +
			"directly to the file and stop."
	case "refine":
		kickoff = "The operator just opened an existing idea to refine. " +
			"Read the canvas file first, then ask what they'd like to " +
			"sharpen. Wait for their reply before editing."
	}

	args := []string{
		"--add-dir", root,
		"--append-system-prompt", prompt,
	}
	// In `new` flow the canvas is a tempfile outside the repo; give
	// claude explicit access to its parent so the edit permission
	// sandbox doesn't block the write. For `edit` flow the canvas
	// lives under root and this is a harmless duplicate add.
	if canvasDir := filepath.Dir(abs); canvasDir != "" && canvasDir != root {
		args = append(args, "--add-dir", canvasDir)
	}
	if kickoff != "" {
		args = append(args, kickoff)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		moePrintf(stderr, "idea: chat session exited: %v\n", err)
		return 1
	}
	return 0
}

// buildIdeaChatPrompt assembles the --append-system-prompt payload for
// an idea chat session: soul → stages/idea/<mode>.md → a minimal
// operational core naming the canvas file. Deliberately narrower than
// buildSystemPrompt (used by stage sessions), which is tied to
// run.Metadata and per-document thread files that ideas don't carry
// a live Claude session for.
func buildIdeaChatPrompt(abs, mode string) string {
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if frag := moe.Stage(ideaWorkflow, mode); frag != "" {
		sections = append(sections, frag)
	}
	sections = append(sections, fmt.Sprintf(
		`You are helping the operator capture or refine an *idea* in a
Ministry of Everything bureaucracy repo. Ideas are a pre-design shelf:
terse, unstructured, meant to be cheap to record.

Your canvas is the single file:
  %s

Edit the file directly — do not propose a diff. When the idea is
captured (or the operator says they're done refining), stop. Do not
design, plan, or open follow-ups.`, abs))
	return strings.Join(sections, "\n---\n\n")
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
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		moePrintf(stderr, "idea: editor exited: %v\n", err)
		return 1
	}
	return 0
}
