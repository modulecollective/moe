// Package wiki is the shared engine that backs per-project wikis (kb
// today, twin tomorrow). A wiki is (engine, schema-config,
// content-directory, checkpoint): the engine and schema-config are
// shared infrastructure; the content directory and checkpoint are
// per-instance.
//
// The engine owns the on-disk shape (index.md, log.md, checkpoint.json
// and the invariants between them), the system-prompt section that
// frames an ingest session, and the session-end finalization that
// appends to log.md and writes checkpoint.json. Schema-config (open vs.
// closed, ingest prompt body, allowed primitives) is supplied per
// instance via Config.
//
// Phase 1 ships one instance — the kb open-schema config — and leaves
// room for the twin's closed-schema config plus operations like Lint
// and Reflect. The surface deliberately doesn't lock those in yet.
package wiki

// Mode picks between the two schema-evolution dispositions. Open lets
// the agent split / merge / rename / retire topic docs as warranted.
// Closed (twin) refuses doc-set changes that aren't explicitly
// authorized — AssertModeInvariants is where that gets enforced.
type Mode int

const (
	// Open — kb-style. Schema evolves under the operator's eye.
	Open Mode = iota
	// Closed — twin-style. Doc set is fixed; only authorized
	// schema changes are permitted.
	Closed
)

// String returns a stable, lowercase label for prompts and logs.
func (m Mode) String() string {
	switch m {
	case Open:
		return "open"
	case Closed:
		return "closed"
	default:
		return "unknown"
	}
}

// Config is the per-instance schema-config for a wiki. One instance
// today (kb); the twin will register a second when it lands.
//
// Paths are absolute. ContentDir points at the directory the agent
// edits (e.g. <root>/projects/<p>/kb). ProjectRepoPath points at the
// project's submodule checkout (<root>/projects/<p>/src) and may be
// "" if the project is registered without a submodule on disk — that
// just means checkpoint records project_repo_sha=null.
type Config struct {
	// Name is the short label used in prompts and log entries
	// (e.g. "kb").
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
	// Mode selects open- vs. closed-schema rules for the ingest
	// prompt and AssertModeInvariants.
	Mode Mode
	// IngestPrompt is the schema-config body that gets pasted into
	// the system prompt above the engine's mode rules. Carries the
	// "what is this wiki for, what's its tone" framing.
	IngestPrompt string
	// AllowedPrimitives lists the schema-evolution operations the
	// agent is allowed to use. Open-schema typically lists
	// {split, merge, rename, retire}; closed-schema is empty (or a
	// strict subset). Surfaced verbatim in the prompt.
	AllowedPrimitives []string
}
