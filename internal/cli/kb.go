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
	kb := NewWorkflow("kb", "Knowledge base workflow: new, research, summarize")
	kb.RegisterFacade(newRunCommand("kb"))
	kb.Register(&Command{
		Name:    "research",
		Summary: "open a Claude Code session on the run's research bibliography",
		Run:     runResearch,
	})
	kb.Register(&Command{
		Name:    "summarize",
		Summary: "open a Claude Code ingest session on the project's wiki",
		Run:     runSummarize,
	}, "research")
	kb.RegisterFacade(closeCommand("kb", "Close kb run %s/%s", nil))
	// Lint is out-of-band relative to runs (no stage, no canvas,
	// no run.json), so it lives alongside `new` and `close` as a
	// workflow facade. Reuses kbWikiBuilder so the lint and
	// summarize sessions agree on the wiki's identity and on-disk
	// shape.
	kb.RegisterFacade(lintCommand("kb", kbLintWikiBuilder))
	RegisterWorkflow(kb)
	Register(kb.Command())
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
		moePrintln(stderr, "usage: moe kb research <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code session on the research bibliography.")
		moePrintln(stderr, "The agent extends the source list with web searches rather than replacing it.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	const kickoff = "The operator just opened this research session. " +
		"Read the canvas file before replying, so your acknowledgement reflects " +
		"what's actually on it. In one or two sentences, acknowledge where the " +
		"source list stands (fresh start vs. resumed) and ask what topic or " +
		"angle they'd like you to search for. Then wait for their reply."
	return runStageSession(fs.Arg(0), fs.Arg(1), "research",
		stageSessionOpts{InitialPrompt: kickoff}, stdout, stderr)
}

func runSummarize(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kb summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe kb summarize <project> <run>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens an interactive Claude Code ingest session on the project's wiki.")
		moePrintln(stderr, "The agent works the run's research bibliography into projects/<project>/knowledge/")
		moePrintln(stderr, "— editing existing topic docs, creating new ones, and maintaining index.md.")
		moePrintln(stderr, "Per-run canvas is a scratchpad; the wiki diff is the artifact.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	const kickoff = "The operator just opened this kb ingest session. " +
		"Read the run's research bibliography and the project's wiki under " +
		"projects/<project>/knowledge/ before replying. In one or two sentences, " +
		"acknowledge where the wiki stands (fresh vs. populated) and propose " +
		"how you'd work the new sources in — which existing topic docs you'd " +
		"extend, where you might add new ones — then wait for the operator's go-ahead."
	return runStageSession(fs.Arg(0), fs.Arg(1), "summarize",
		stageSessionOpts{
			InitialPrompt: kickoff,
			WikiBuilder:   kbWikiBuilder,
		}, stdout, stderr)
}
