// Package wiki is the engine behind the project digital twin: a
// closed-schema wiki over a fixed doc set. A wiki is (engine, config,
// content-directory, checkpoint) — the engine is shared infrastructure;
// the content directory and checkpoint are per-instance.
//
// The engine owns the on-disk shape (the managed docs, log.md,
// checkpoint.json and the invariants between them), the system-prompt
// section that frames an ingest session, and the session-end
// finalization that appends to log.md and writes checkpoint.json.
//
// The engine was once generic over two schema modes — the twin's fixed
// doc set and the kb workflow's freely-evolving one. kb is gone and the
// open-schema half with it; a project's `knowledge/` tree is now a plain
// doc tree that any sdlc stage may write, checked by internal/knowledge
// rather than driven by an engine.
package wiki

// Config is the per-instance config for a wiki. One instance today (the
// twin), one per project.
//
// Paths are absolute. ContentDir points at the directory the agent
// edits (<root>/projects/<p>/digital-twin). ProjectRepoPath points at
// the project's submodule checkout (<root>/projects/<p>/src) and may be
// "" if the project is registered without a submodule on disk — that
// just means checkpoint records project_repo_sha=null.
type Config struct {
	// Name is the short label used in prompts and log entries
	// (e.g. "twin").
	Name string
	// ContentDir is the absolute path to the wiki's on-disk dir.
	ContentDir string
	// ProjectRepoPath is the absolute path to the target repo's
	// working tree (the submodule checkout). May be "".
	ProjectRepoPath string
	// Project is the project id (e.g. "moe"). Recorded in the
	// checkpoint so project_repo_sha is unambiguous in isolation.
	Project string
	// BureaucracyPath is the absolute path to the bureaucracy repo
	// root. Used to capture bureaucracy_sha at finalize time.
	BureaucracyPath string
	// IngestPrompt is the per-instance body that gets pasted into
	// the system prompt above the engine's schema rules. Carries the
	// "what is this wiki for, what's its tone" framing.
	IngestPrompt string
	// ManagedDocs is the hard-fixed set of docs the engine knows
	// about. Required. Order is the order the docs are rendered in
	// preambles, kickoff prompts, and hygiene reports.
	ManagedDocs []ManagedDoc
}

// ManagedDoc names one of the wiki's hard-fixed docs. Twin's five
// (vision / architecture / patterns / operations / glossary) live in
// internal/cli/twin.go; the engine treats them as opaque
// (filename, title, purpose).
type ManagedDoc struct {
	// Filename is the path under ContentDir (e.g. "vision.md").
	// Flat — closed-schema has no topics/ subfolder.
	Filename string
	// Title is the human-readable heading rendered into the doc's
	// stub on bootstrap and into log entries / preambles.
	Title string
	// Purpose is a one-line "what this doc is for" the engine renders
	// in the closed-schema preamble so the agent knows what each
	// managed doc is supposed to hold without reading every file.
	Purpose string
	// SoftBudgetKB is the size past which the doc is rendered with an
	// over-budget nudge in the preamble and surfaced as an
	// OverBudgetDocs finding. Soft: it nudges the agent to compress,
	// it never gates a pass. Zero means "no budget declared" and
	// suppresses both the size annotation and the finding — an
	// open-schema or freshly-registered doc set opts in by setting it.
	//
	// Same disposition as loreSoftCap one file over: the only lever a
	// prose corpus responds to is telling the writer how big it has
	// gotten.
	SoftBudgetKB int
}
