package cli

import (
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

	"github.com/modulecollective/moe/internal/bureaucracy"
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
	moePrintf(w, "  %-14s  %s\n", "add", "capture a new idea (writes a stub and opens $EDITOR)")
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
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea add [--id <slug>] <project> \"title\"\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
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
	// Open the editor first and commit whatever's on disk afterward, so the
	// operator's saved content lands in the capture commit. If they quit
	// without editing, the stub itself is still a valid capture.
	editorCode := launchEditor(abs, stdout, stderr)
	msg := fmt.Sprintf("Capture idea %s/%s: %s\n\nMoE-Idea: %s\nMoE-Project: %s\n",
		projectID, slug, title, slug, projectID)
	if err := run.StageAndCommit(root, msg, rel); err != nil {
		moePrintf(stderr, "idea: commit: %v\n", err)
		return 1
	}

	moePrintf(stdout, "captured idea %s/%s\n%s\n", projectID, slug, abs)
	return editorCode
}

func runIdeaRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("idea remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintf(stderr, "usage: moe idea remove <project> <slug>\n")
	}
	if err := fs.Parse(args); err != nil {
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
	if err := os.Remove(abs); err != nil {
		moePrintf(stderr, "idea: remove %s: %v\n", rel, err)
		return 1
	}
	msg := fmt.Sprintf("Remove idea %s/%s: %s\n\nMoE-Idea: %s\nMoE-Project: %s\n",
		projectID, slug, title, slug, projectID)
	if err := run.StageAndCommit(root, msg, rel); err != nil {
		moePrintf(stderr, "idea: commit: %v\n", err)
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
	if err := fs.Parse(args); err != nil {
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

// launchEditor opens path in $VISUAL or $EDITOR with stdio wired to the
// terminal, so the operator drops straight into editing the new stub.
// When neither variable is set, we just print a hint — the file is
// already on disk and the path was printed above.
func launchEditor(path string, stdout, stderr io.Writer) int {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		moePrintln(stdout, "(set $EDITOR or $VISUAL to open the file directly next time)")
		return 0
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
