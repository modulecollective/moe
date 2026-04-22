package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	moe "github.com/modulecollective/moe"
	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
)

// `moe idea` is a deliberately small top-level for capture: a markdown
// file in projects/<p>/ideas/, no workflow, no agent. Promoting an
// idea to a run is `moe sdlc new --from-idea=<slug>` (or kb's `new`),
// not a verb here — see designs/idea-capture.md.

func init() {
	Register(&Command{
		Name:    "idea",
		Summary: "lightweight backlog: capture an idea or list ideas (no workflow, no agent)",
		Run:     runIdea,
	})
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
	case "add":
		return runIdeaAdd(args[1:], stdout, stderr)
	case "edit":
		return runIdeaEdit(args[1:], stdout, stderr)
	case "remove":
		return runIdeaRemove(args[1:], stdout, stderr)
	case "list":
		return runIdeaList(args[1:], stdout, stderr)
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
	moePrintf(w, "  %-14s  %s\n", "add", "capture a new idea (opens $EDITOR, or --chat for Claude Code)")
	moePrintf(w, "  %-14s  %s\n", "edit", "refine a captured idea ($EDITOR, or --chat for Claude Code)")
	moePrintf(w, "  %-14s  %s\n", "remove", "delete a captured idea and commit the removal")
	moePrintf(w, "  %-14s  %s\n", "list", "list this project's captured ideas")
}

// ideaSlugPattern matches the same shape as run id slugs: lowercase
// letters/digits with internal dashes.
var ideaSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ideaDir is the on-disk directory (relative to the bureaucracy root)
// for a project's idea files. Centralized here so dash and runNew share
// the same convention without duplicating the path literal.
func ideaDir(projectID string) string {
	return filepath.Join("projects", projectID, "ideas")
}

// ideaPath is the on-disk path for a single idea (relative to the
// bureaucracy root).
func ideaPath(projectID, slug string) string {
	return filepath.Join(ideaDir(projectID), slug+".md")
}

func runIdeaAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idOverride := fs.String("id", "", "explicit slug (default: derived from title, with -N suffix on collision)")
	chat := fs.Bool("chat", false, "open a Claude Code session on the new idea instead of $EDITOR")
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea add [--id <slug>] [--chat] <project> \"title\"\n")
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

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		moePrintf(stderr, "idea: project %s not registered (%s missing)\n",
			projectID, filepath.Join("projects", projectID, "project.json"))
		return 1
	}

	dirty, err := run.WorkingTreeDirty(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if dirty {
		moePrintln(stderr, "idea: working tree has uncommitted changes; commit or stash first")
		return 1
	}

	slug, err := resolveIdeaSlug(root, projectID, *idOverride, title)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}

	if !*chat && os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL (or pass --chat) — idea add needs an editor")
		return 1
	}

	rel := ideaPath(projectID, slug)
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		moePrintf(stderr, "idea: mkdir: %v\n", err)
		return 1
	}
	body := fmt.Sprintf("# %s\n", title)
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		moePrintf(stderr, "idea: write %s: %v\n", rel, err)
		return 1
	}
	// Open the editor (or --chat session) first and commit whatever's on
	// disk afterward, so the operator's saved content lands in the capture
	// commit. If they quit without editing, the stub itself is still a
	// valid capture.
	var editorCode int
	if *chat {
		editorCode = runIdeaChat(root, abs, "capture", stdout, stderr)
	} else {
		editorCode = launchEditor(abs, stdout, stderr)
	}
	msg := fmt.Sprintf("Capture idea %s/%s: %s\n\nMoE-Idea: %s\nMoE-Project: %s\n",
		projectID, slug, title, slug, projectID)
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-add",
		Run:     projectID + "/" + slug,
	}, func() error {
		return run.StageAndCommit(root, msg, rel)
	})
	if err != nil {
		moePrintf(stderr, "idea: commit: %v\n", err)
		return 1
	}

	moePrintf(stdout, "captured idea %s/%s\n%s\n", projectID, slug, abs)
	return editorCode
}

// runIdeaEdit reopens a captured idea in $EDITOR and lands any saves
// as a single `Refine idea …` commit. No-op saves do not produce an
// empty commit. Kept separate from runIdeaAdd because add resolves a
// brand-new slug (collisions error) while edit requires an existing
// slug (miss errors) — opposite checks would only waste a mode flag
// if merged.
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

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		moePrintf(stderr, "idea: project %s not registered (%s missing)\n",
			projectID, filepath.Join("projects", projectID, "project.json"))
		return 1
	}

	dirty, err := run.WorkingTreeDirty(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if dirty {
		moePrintln(stderr, "idea: working tree has uncommitted changes; commit or stash first")
		return 1
	}

	rel := ideaPath(projectID, slug)
	abs := filepath.Join(root, rel)
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "idea: %s does not exist; run `moe idea list %s` to see captured ideas\n",
			rel, projectID)
		return 1
	}

	if !*chat && os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "idea: set $EDITOR or $VISUAL (or pass --chat) — idea edit needs an editor")
		return 1
	}

	var editorCode int
	if *chat {
		editorCode = runIdeaChat(root, abs, "refine", stdout, stderr)
	} else {
		editorCode = launchEditor(abs, stdout, stderr)
	}

	// Title for the commit subject tracks the current H1 (post-edit),
	// with the slug as the same fallback scanIdeas already uses. If the
	// operator blanked the H1, the slug keeps the history greppable.
	title, err := readIdeaTitle(abs)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if title == "" {
		title = slug
	}

	msg := fmt.Sprintf("Refine idea %s/%s: %s\n\nMoE-Idea: %s\nMoE-Project: %s\n",
		projectID, slug, title, slug, projectID)
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-edit",
		Run:     projectID + "/" + slug,
	}, func() error {
		return run.StageAndCommit(root, msg, rel)
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
	return editorCode
}

func runIdeaRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea remove <project> <slug>\n")
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

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		moePrintf(stderr, "idea: project %s not registered (%s missing)\n",
			projectID, filepath.Join("projects", projectID, "project.json"))
		return 1
	}

	dirty, err := run.WorkingTreeDirty(root)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if dirty {
		moePrintln(stderr, "idea: working tree has uncommitted changes; commit or stash first")
		return 1
	}

	rel := ideaPath(projectID, slug)
	abs := filepath.Join(root, rel)
	if _, err := os.Stat(abs); err != nil {
		moePrintf(stderr, "idea: %s does not exist; run `moe idea list %s` to see captured ideas\n",
			rel, projectID)
		return 1
	}
	title, err := readIdeaTitle(abs)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if title == "" {
		title = slug
	}
	msg := fmt.Sprintf("Remove idea %s/%s: %s\n\nMoE-Idea: %s\nMoE-Project: %s\n",
		projectID, slug, title, slug, projectID)
	err = withRepoLock(root, repolock.Options{
		Purpose: "idea-remove",
		Run:     projectID + "/" + slug,
	}, func() error {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("remove %s: %w", rel, err)
		}
		return run.StageAndCommit(root, msg, rel)
	})
	if err != nil {
		moePrintf(stderr, "idea: %v\n", err)
		return 1
	}
	moePrintf(stdout, "removed idea %s/%s\n", projectID, slug)
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

	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(root, "projects", projectID, "project.json")); err != nil {
		moePrintf(stderr, "idea: project %s not registered (%s missing)\n",
			projectID, filepath.Join("projects", projectID, "project.json"))
		return 1
	}

	entries, err := scanIdeas(root, projectID)
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

// ideaEntry is the minimal projection of an idea file used by both
// `moe idea list` and `moe dash`'s backlog bucket.
type ideaEntry struct {
	project string
	slug    string
	title   string
	path    string // absolute
}

// scanIdeas reads projects/<projectID>/ideas/*.md, parses each title
// from its first H1 line, and returns one entry per file. Order is
// unspecified; callers sort to taste.
func scanIdeas(root, projectID string) ([]ideaEntry, error) {
	dir := filepath.Join(root, ideaDir(projectID))
	matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("idea: glob: %w", err)
	}
	out := make([]ideaEntry, 0, len(matches))
	for _, p := range matches {
		slug := strings.TrimSuffix(filepath.Base(p), ".md")
		title, err := readIdeaTitle(p)
		if err != nil {
			return nil, err
		}
		if title == "" {
			title = slug
		}
		out = append(out, ideaEntry{project: projectID, slug: slug, title: title, path: p})
	}
	return out, nil
}

// scanAllIdeas walks every project's ideas dir under root.
func scanAllIdeas(root string) ([]ideaEntry, error) {
	matches, err := filepath.Glob(filepath.Join(root, "projects", "*", "ideas", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("idea: glob all: %w", err)
	}
	out := make([]ideaEntry, 0, len(matches))
	for _, p := range matches {
		// projects/<project>/ideas/<slug>.md → project = parent-of-parent
		ideasDir := filepath.Dir(p)
		projectID := filepath.Base(filepath.Dir(ideasDir))
		slug := strings.TrimSuffix(filepath.Base(p), ".md")
		title, err := readIdeaTitle(p)
		if err != nil {
			return nil, err
		}
		if title == "" {
			title = slug
		}
		out = append(out, ideaEntry{project: projectID, slug: slug, title: title, path: p})
	}
	return out, nil
}

// readIdeaTitle returns the trimmed text of the file's first H1 line,
// or "" if no `# ` line appears in the head of the file. Only the head
// is scanned so a long body doesn't slow `moe dash`.
func readIdeaTitle(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("idea: read %s: %w", path, err)
	}
	return firstH1(string(b)), nil
}

// firstH1 returns the trimmed text after the first `# ` line in body,
// or "" if none appears in the head of the file.
func firstH1(body string) string {
	for _, line := range strings.SplitN(body, "\n", 16) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		}
	}
	return ""
}

// resolveIdeaSlug picks the slug for a new idea, mirroring run.New's
// rule: explicit --id is taken as-is and collisions error; an
// auto-derived slug from the title falls back to base-2, base-3, … on
// collision.
func resolveIdeaSlug(root, projectID, override, title string) (string, error) {
	if override != "" {
		if !ideaSlugPattern.MatchString(override) {
			return "", fmt.Errorf("idea: id %q must match %s", override, ideaSlugPattern)
		}
		if _, err := os.Stat(filepath.Join(root, ideaPath(projectID, override))); err == nil {
			return "", fmt.Errorf("idea: %s already exists", ideaPath(projectID, override))
		}
		return override, nil
	}
	base := run.Slugify(title)
	if base == "" {
		return "", fmt.Errorf("idea: cannot derive slug from title %q; pass --id to set one explicitly", title)
	}
	if _, err := os.Stat(filepath.Join(root, ideaPath(projectID, base))); err != nil {
		return base, nil
	}
	// Strip an existing numeric -N suffix so collisions on foo-2 continue
	// to foo-3 rather than producing foo-2-2 — same shape as run.nextFreeID.
	root2 := base
	if i := strings.LastIndex(root2, "-"); i >= 0 {
		if _, err := strconv.Atoi(root2[i+1:]); err == nil {
			root2 = root2[:i]
		}
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s-%d", root2, n)
		if _, err := os.Stat(filepath.Join(root, ideaPath(projectID, c))); err != nil {
			return c, nil
		}
	}
}

// runIdeaChat launches an interactive Claude Code session on the idea
// file. mode is "capture" (new idea) or "refine" (existing idea) and
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
// run.Metadata and per-document thread files that ideas don't have.
func buildIdeaChatPrompt(abs, mode string) string {
	var sections []string
	if soul := moe.Soul(); soul != "" {
		sections = append(sections, soul)
	}
	if frag := moe.Stage("idea", mode); frag != "" {
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
