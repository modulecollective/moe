package cli

import (
	"flag"
	"io"
	"path/filepath"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// The kb workflow owns the research→summarize lifecycle for knowledge-
// base runs. There is no push — the artifact is markdown inside the
// bureaucracy: research builds the bibliography; summarize is the
// ingest stage, where the operator and agent work the new sources into
// the project's wiki under projects/<project>/knowledge/. The wiki
// engine (internal/wiki) owns the on-disk shape, finalization, and
// per-turn staging; kb is one shipped config of that engine.

// kbWikiIngestPrompt is the kb-instance framing the engine pastes
// above its mode rules. Stage tone (process, voice, what to avoid)
// lives in workflows/kb/summarize.md and is loaded via moe.Stage; this
// string carries only the wiki-identity framing — what this wiki is
// *for* — so the same engine can host a closed-schema twin config
// later by swapping this body and the Mode.
const kbWikiIngestPrompt = `This is the project's open-schema knowledge base.
The job is to work the run's research bibliography into the wiki:
place new content into existing topic docs where it fits, create new
topic docs when nothing does, and maintain index.md as the catalog of
what's where. Topic identity is decoupled from run identity — a single
ingest may update zero, one, or many topic docs.`

// kbWikiBuilder is the WikiBuilder hook the kb summarize stage hands
// to runStageSession. It resolves the per-project wiki content
// directory, the project repo's submodule path (best-effort —
// FinalizeIngest tolerates a missing or dirty repo), and the
// open-schema config the engine consumes.
func kbWikiBuilder(root string, md *run.Metadata) (*wiki.Config, error) {
	contentDir := filepath.Join(root, "projects", md.Project, "knowledge")
	cfg := &wiki.Config{
		Name:              "kb",
		ContentDir:        contentDir,
		ProjectRepoPath:   filepath.Join(root, project.SubmoduleDir(md.Project)),
		Project:           md.Project,
		BureaucracyPath:   root,
		Mode:              wiki.Open,
		IngestPrompt:      kbWikiIngestPrompt,
		AllowedPrimitives: []string{"split", "merge", "rename", "retire"},
	}
	return cfg, nil
}

func init() {
	g := NewCommandGroup("kb", "knowledge-base workflow: new, research, summarize")
	g.Register(newRunCommand("kb"))
	g.Register(&Command{
		Name:    "research",
		Summary: "open a Claude Code session on the run's research bibliography",
		Run:     runResearch,
	})
	g.Register(&Command{
		Name:    "summarize",
		Summary: "open a Claude Code ingest session on the project's wiki",
		Run:     runSummarize,
	})
	g.Register(closeCommand("kb", "Close kb run %s/%s", nil))
	g.Register(&Command{
		Name:    "cat",
		Summary: "dump a stage canvas to stdout (kb cat <project>/<run> <stage>)",
		Run:     runCat("kb", ""),
	})
	g.Register(&Command{
		Name:    "log",
		Summary: "render a stage's agent transcript (kb log <project>/<run> <stage>)",
		Run:     runLog("kb", ""),
	})
	// Lint is out-of-band relative to runs (no stage, no canvas,
	// no run.json), so it lives alongside `new` and `close` as a
	// non-stage group subcommand. Reuses kbWikiBuilder so the lint
	// and summarize sessions agree on the wiki's identity and
	// on-disk shape.
	g.Register(lintCommand("kb", kbLintWikiBuilder))
	RegisterGroup(g)

	w := NewWorkflow("kb")
	w.RegisterStage("research")
	w.RegisterStage("summarize", "research")
	RegisterWorkflow(w)
}

// kbLintWikiBuilder is the (root, projectID) → *wiki.Config adapter
// the lint facade calls. kbWikiBuilder takes a *run.Metadata, but
// lint has no run; this thin wrapper synthesises the minimum
// metadata kbWikiBuilder needs (just md.Project) so the wiki cfg
// resolves to the right per-project content directory.
func kbLintWikiBuilder(root, projectID string) (*wiki.Config, error) {
	return kbWikiBuilder(root, &run.Metadata{Project: projectID})
}

func runResearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb research", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe kb research <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the research bibliography.")
		moePrintln(stderr, "The agent extends the source list with web searches rather than replacing it.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "kb research: %v\n", err)
		return 2
	}
	return openKbResearch(projectID, runID, false, false, stdout, stderr)
}

// openKbResearch is the Go-level seam behind `moe kb research`. The
// typed `Command.Run` parses args and hands to this helper; the chain
// prompt's cascade driver (`!` / `!<stage>` / `!!` / `!!!`) reaches it directly
// via openKbStage. headless=true selects the bounded one-turn variant
// (`claude -p` plus the workflow's oneshot.md fragment), the same path
// the equivalent twin / sdlc seams take.
func openKbResearch(projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
	const kickoff = "The operator just opened this research session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"source list stands (fresh start vs. resumed) and ask what topic or " +
		"angle they'd like you to search for. Then wait for their reply."
	return runStageSession(projectID, runID, "research",
		stageSessionOpts{
			InitialPrompt: kickoff,
			Headless:      headless,
			SkipNextStage: suppressNextStage,
		}, stdout, stderr)
}

func runSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe kb summarize <project>/<run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code ingest session on the project's wiki.")
		moePrintln(stderr, "The agent works the run's research bibliography into projects/<project>/knowledge/")
		moePrintln(stderr, "— editing existing topic docs, creating new ones, and maintaining index.md.")
		moePrintln(stderr, "Per-run canvas is a scratchpad; the wiki diff is the artifact.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, runID, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "kb summarize: %v\n", err)
		return 2
	}
	return openKbSummarize(projectID, runID, false, false, stdout, stderr)
}

// openKbSummarize is the Go-level seam behind `moe kb summarize`. Same
// contract as openKbResearch one stage downstream — adds the wiki
// engine wiring kbWikiBuilder produces.
func openKbSummarize(projectID, runID string, headless, suppressNextStage bool, stdout, stderr io.Writer) int {
	const kickoff = "The operator just opened this kb ingest session. " +
		"Read the run's research bibliography and the project's wiki under " +
		"projects/<project>/knowledge/ before replying. In one or two sentences, " +
		"acknowledge where the wiki stands (fresh vs. populated) and propose " +
		"how you'd work the new sources in — which existing topic docs you'd " +
		"extend, where you might add new ones — then wait for the operator's go-ahead."
	return runStageSession(projectID, runID, "summarize",
		stageSessionOpts{
			InitialPrompt: kickoff,
			Headless:      headless,
			SkipNextStage: suppressNextStage,
			WikiBuilder:   kbWikiBuilder,
		}, stdout, stderr)
}
