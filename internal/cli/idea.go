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
	"time"

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
// The idea workflow itself is registered in the workflow registry (for
// LookupWorkflow / dash), but not as a top-level dispatcher — `moe idea`
// is its own top-level so muscle memory for `add/edit/remove/list`
// maps directly onto the new verbs.

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
	// --from-idea's wf.Stages() all resolve it. The stage command
	// itself is unreachable — the dispatcher above handles idea verbs,
	// not `moe idea idea` — so we give it a usage stub just in case.
	wf := NewWorkflow(ideaWorkflow, "single-stage idea-capture workflow (driven by `moe idea`)")
	wf.Register(&Command{
		Name:    ideaDocID,
		Summary: "idea canvas (use `moe idea edit` instead of invoking directly)",
		Run: func(args []string, stdout, stderr io.Writer) int {
			moePrintln(stderr, "idea runs are driven via `moe idea` — try `moe idea edit <project> <slug>`")
			return 2
		},
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
	case "migrate":
		return runIdeaMigrate(args[1:], stdout, stderr)
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
	moePrintf(w, "  %-14s  %s\n", "migrate", "convert pre-existing projects/<p>/ideas/*.md files into idea runs (one-shot)")
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

	opts := run.Options{
		ID:       *idOverride,
		Workflow: ideaWorkflow,
		SeedDocs: map[string]string{ideaDocID: string(body)},
	}
	runRef := projectID
	if *idOverride != "" {
		runRef = projectID + "/" + *idOverride
	}
	var md *run.Metadata
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-new",
		Run:     runRef,
	}, func() error {
		m, err := run.New(root, projectID, title, opts)
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		moePrintf(stderr, "idea: %v\n", err)
		return 1
	}
	moePrintf(stdout, "captured idea %s/%s\n", md.Project, md.ID)
	return 0
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

func runIdeaClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea close <project> <slug>\n")
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

	md, err := loadIdeaRun(root, projectID, slug)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if md.Status != run.StatusInProgress {
		moePrintf(stderr, "idea %s/%s already %s\n", projectID, slug, md.Status)
		return 1
	}

	md.Status = run.StatusClosed
	runJSONRel := filepath.Join(run.Dir(projectID, slug), "run.json")
	msg := fmt.Sprintf(`Close idea %s/%s

MoE-Run: %s
MoE-Project: %s
MoE-Workflow: %s
`, projectID, slug, slug, projectID, ideaWorkflow)
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-close",
		Run:     projectID + "/" + slug,
	}, func() error {
		if err := run.Save(root, md); err != nil {
			return err
		}
		return run.StageAndCommit(root, msg, runJSONRel)
	})
	if err != nil {
		moePrintf(stderr, "idea: close: %v\n", err)
		return 1
	}
	moePrintf(stdout, "closed idea %s/%s\n", projectID, slug)
	return 0
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
// selects which stages/_idea fragment seeds the system prompt. Unlike
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
// an idea chat session: soul → stages/_idea/<mode>.md → a minimal
// operational core naming the canvas file. Deliberately narrower than
// buildSystemPrompt (used by stage sessions), which is tied to
// run.Metadata and per-document thread files that ideas don't carry
// a live Claude session for.
func buildIdeaChatPrompt(abs, mode string) string {
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if frag := moe.Stage("_idea", mode); frag != "" {
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

// runIdeaMigrate is the one-shot housekeeping verb that converts the
// pre-run-workflow idea layout (projects/<p>/ideas/<slug>.md) into
// idea runs (projects/<p>/runs/<slug>/{run.json,documents/idea/content.md}).
// It's deliberately a single commit per migration pass — the whole
// fleet flips at once so there's no dual-read period to maintain. If a
// slug collides with an existing run, the idea migrates under a
// -N-suffixed slug (run.New's usual rule); the source file is removed
// regardless. Refuses to run on a dirty tree.
func runIdeaMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "print what would happen and exit without touching the repo")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe idea migrate [--dry-run]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if err := requireCleanTree(root); err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "ideas", "*.md"))
	if err != nil {
		moePrintf(stderr, "idea migrate: glob: %v\n", err)
		return 1
	}
	if len(matches) == 0 {
		moePrintln(stdout, "no legacy idea files found — nothing to migrate")
		return 0
	}

	// Sort for deterministic commit order across re-runs of a dry run.
	sort.Strings(matches)

	type plan struct {
		project string
		oldSlug string
		newSlug string
		title   string
		body    string
		oldRel  string
	}
	plans := make([]plan, 0, len(matches))
	for _, abs := range matches {
		// projects/<project>/ideas/<slug>.md
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			moePrintf(stderr, "idea migrate: rel %s: %v\n", abs, err)
			return 1
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 4 || parts[0] != "projects" || parts[2] != "ideas" {
			moePrintf(stderr, "idea migrate: unexpected path shape %s\n", rel)
			return 1
		}
		projectID := parts[1]
		slug := strings.TrimSuffix(parts[3], ".md")
		body, err := os.ReadFile(abs)
		if err != nil {
			moePrintf(stderr, "idea migrate: read %s: %v\n", rel, err)
			return 1
		}
		title := firstH1(string(body))
		if title == "" {
			title = slug
		}
		plans = append(plans, plan{
			project: projectID,
			oldSlug: slug,
			newSlug: slug,
			title:   title,
			body:    string(body),
			oldRel:  rel,
		})
	}

	// Preview (and finalize) slugs, auto-suffixing on collision. We
	// reuse run.New's collision suffix via a pre-flight check; the
	// actual writes happen after so the whole fleet can share a
	// single commit.
	for i := range plans {
		p := &plans[i]
		// Check against existing runs AND earlier-in-this-batch
		// planned slugs: migrating two ideas with the same slug
		// across two projects is fine (namespace is per-project),
		// but within a project we need to advance past earlier
		// picks too.
		collisions := 0
		for {
			conflict, err := slugConflict(root, p.project, p.newSlug)
			if err != nil {
				moePrintf(stderr, "idea migrate: %v\n", err)
				return 1
			}
			for j := 0; j < i && !conflict; j++ {
				if plans[j].project == p.project && plans[j].newSlug == p.newSlug {
					conflict = true
				}
			}
			if !conflict {
				break
			}
			collisions++
			p.newSlug = fmt.Sprintf("%s-%d", trimNumericSuffix(p.oldSlug), collisions+1)
		}
	}

	if *dryRun {
		for _, p := range plans {
			if p.newSlug == p.oldSlug {
				moePrintf(stdout, "would migrate %s/%s → runs/%s\n", p.project, p.oldSlug, p.newSlug)
			} else {
				moePrintf(stdout, "would migrate %s/%s → runs/%s (suffixed for collision)\n", p.project, p.oldSlug, p.newSlug)
			}
		}
		return 0
	}

	return withRepoLockCode(root, repolock.Options{
		Purpose: "idea-migrate",
	}, func() int {
		addPaths := make([]string, 0, 2*len(plans))
		for _, p := range plans {
			md := &run.Metadata{
				ID:        p.newSlug,
				Project:   p.project,
				Title:     p.title,
				Status:    run.StatusInProgress,
				Workflow:  ideaWorkflow,
				Created:   migrateTimestamp(),
				Documents: map[string]*run.Document{},
			}
			if err := run.Save(root, md); err != nil {
				moePrintf(stderr, "idea migrate: save %s/%s run.json: %v\n", p.project, p.newSlug, err)
				return 1
			}
			canvasRel := run.ContentPath(p.project, p.newSlug, ideaDocID)
			canvasAbs := filepath.Join(root, canvasRel)
			if err := os.MkdirAll(filepath.Dir(canvasAbs), 0o755); err != nil {
				moePrintf(stderr, "idea migrate: mkdir %s: %v\n", filepath.Dir(canvasAbs), err)
				return 1
			}
			if err := os.WriteFile(canvasAbs, []byte(p.body), 0o644); err != nil {
				moePrintf(stderr, "idea migrate: write %s: %v\n", canvasRel, err)
				return 1
			}
			addPaths = append(addPaths,
				filepath.Join(run.Dir(p.project, p.newSlug), "run.json"),
				canvasRel,
			)
		}
		if err := run.Stage(root, addPaths...); err != nil {
			moePrintf(stderr, "idea migrate: git add: %v\n", err)
			return 1
		}
		for _, p := range plans {
			if err := gitRM(root, p.oldRel); err != nil {
				moePrintf(stderr, "idea migrate: git rm %s: %v\n", p.oldRel, err)
				return 1
			}
		}
		msg := fmt.Sprintf("migrate: convert %d idea file(s) to idea runs\n\nMoE-Workflow: %s\n", len(plans), ideaWorkflow)
		if err := gitCommit(root, msg); err != nil {
			moePrintf(stderr, "idea migrate: commit: %v\n", err)
			return 1
		}
		for _, p := range plans {
			moePrintf(stdout, "migrated %s/%s → runs/%s\n", p.project, p.oldSlug, p.newSlug)
		}
		return 0
	})
}

// slugConflict reports whether (project, slug) already names a run dir
// or a history entry. Thin wrapper so migrate can reuse run.New's
// collision discipline without opening a run.
func slugConflict(root, projectID, slug string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, run.Dir(projectID, slug))); err == nil {
		return true, nil
	}
	// Fall back to git-log grep by shelling out via run's existing
	// command surface would be ideal; but slugTaken is unexported.
	// For migration, a history-only collision (no on-disk dir) is
	// unlikely — the bureaucracy repos that predate this change have
	// ideas under ideas/, not runs/. An on-disk check is sufficient
	// here, and run.New's own suffix rule catches any history
	// collision if the operator re-runs.
	return false, nil
}

// trimNumericSuffix strips a trailing "-N" (N integer) from slug so
// collisions on foo-2 advance to foo-3 rather than foo-2-2. Mirrors
// run.nextFreeID's rule.
func trimNumericSuffix(slug string) string {
	slug = strings.TrimRight(slug, "-")
	if i := strings.LastIndex(slug, "-"); i >= 0 {
		for _, r := range slug[i+1:] {
			if r < '0' || r > '9' {
				return slug
			}
		}
		return slug[:i]
	}
	return slug
}

// migrateTimestamp returns today's date in the format run.New writes
// to Metadata.Created.
func migrateTimestamp() string {
	return time.Now().UTC().Format("2006-01-02")
}

// withRepoLockCode is a thin wrapper that lets a repolock block
// return an exit code directly (the same int convention runIdea* verbs
// use) rather than funnelling through an error.
func withRepoLockCode(root string, opts repolock.Options, fn func() int) int {
	var code int
	err := withRepoLock(root, opts, func() error {
		code = fn()
		return nil
	})
	if err != nil {
		return 1
	}
	return code
}

// gitRM runs `git rm -- path` under root. Thin helper because the
// public run.Stage only does `git add`.
func gitRM(root, path string) error {
	cmd := exec.Command("git", "rm", "--", path)
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// gitCommit runs `git commit -m msg` under root.
func gitCommit(root, msg string) error {
	cmd := exec.Command("git", "commit", "-m", msg)
	cmd.Dir = root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// firstH1 returns the trimmed text after the first `# ` line in body,
// or "" if none appears in the head of the file. Only the head is
// scanned so a long body doesn't slow the migrator.
func firstH1(body string) string {
	for _, line := range strings.SplitN(body, "\n", 16) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		}
	}
	return ""
}
