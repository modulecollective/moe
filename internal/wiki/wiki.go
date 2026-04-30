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
	// ManagedDocs is the hard-fixed set of docs the engine knows
	// about under closed-schema. Empty for open-schema (kb), required
	// for closed-schema (twin). Order is the order the docs are
	// rendered in preambles, kickoff prompts, and lint reports.
	ManagedDocs []ManagedDoc
}

// ManagedDoc names one of a closed-schema wiki's hard-fixed docs.
// Twin's four (vision / architecture / patterns / operations) live in
// internal/cli/twin.go; the engine treats them as opaque
// (filename, title, purpose, per-doc reflect framing).
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
	// ReflectPrompt is the per-doc framing the reflect kickoff lays
	// down under a doc-named subhead. Lifted from TWIN-REFACTOR's
	// twin schema and tightened into prompt form.
	ReflectPrompt string
}
