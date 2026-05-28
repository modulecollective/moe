package wiki

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// DetectionResult is the outcome of DetectUnrecordedEdits — the list
// of managed docs touched outside a twin session, plus a display-only
// timestamp framing when the comparison was anchored.
type DetectionResult struct {
	// UnrecordedDocs are the managed-doc filenames (relative to
	// ContentDir) whose most recent commit in the bureaucracy repo
	// lacks the `MoE-Workflow: twin` trailer (and isn't a net-noop
	// revert to checkpoint state). Sorted.
	UnrecordedDocs []string
	// Since is checkpoint.last_ingest_at parsed — display-only,
	// surfaced in operator-facing notes ("last reflected …"). Zero
	// when the wiki has no checkpoint yet. No longer feeds detection.
	Since time.Time
}

// DetectUnrecordedEdits flags managed docs whose latest commit wasn't
// produced by a twin session (reflect / claim). Closed-schema only —
// open-schema's "decided edits" idea isn't load-bearing today, so the
// open path returns an empty result.
//
// Implementation: for each managed doc, read the body of its most
// recent commit and look for a `MoE-Workflow: twin` trailer. Commits
// with the trailer are recorded by definition. Commits without it are
// unrecorded unless the doc's tree state at HEAD matches the
// checkpoint's bureaucracy SHA — a revert that returns the doc to its
// checkpoint state is a net no-op and shouldn't block the operator.
// Cheap (one git log + at most one git diff per doc); runs lazily at
// the start of twin-touching commands.
//
// The `cp.LastIngestAt == ""` short-circuit stays: it's the only
// signal we have that the twin has been initialised. Without it, a
// fresh project's first non-engine commit would be flagged before any
// baseline exists.
func DetectUnrecordedEdits(cfg Config) (DetectionResult, error) {
	if cfg.Mode != Closed {
		return DetectionResult{}, nil
	}
	if cfg.BureaucracyPath == "" {
		return DetectionResult{}, fmt.Errorf("wiki: detect requires BureaucracyPath")
	}
	cp, ok, err := ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return DetectionResult{}, err
	}
	if !ok || cp.LastIngestAt == "" {
		// No checkpoint → no baseline. An unbootstrapped twin has
		// no edits to claim; first reflect will set the baseline.
		return DetectionResult{}, nil
	}
	since, err := time.Parse(time.RFC3339, cp.LastIngestAt)
	if err != nil {
		return DetectionResult{}, fmt.Errorf("wiki: parse last_ingest_at %q: %w", cp.LastIngestAt, err)
	}

	var checkpointSHA string
	if cp.BureaucracySHA != nil {
		checkpointSHA = *cp.BureaucracySHA
	}

	var unrecorded []string
	for _, d := range cfg.ManagedDocs {
		recorded, ok, err := lastCommitIsTwin(cfg, d.Filename)
		if err != nil {
			return DetectionResult{Since: since}, err
		}
		if !ok {
			// No history for this doc → nothing to flag.
			continue
		}
		if recorded {
			continue
		}
		if checkpointSHA != "" && docUnchangedSinceSHA(cfg, d.Filename, checkpointSHA) {
			continue
		}
		unrecorded = append(unrecorded, d.Filename)
	}
	sort.Strings(unrecorded)
	return DetectionResult{UnrecordedDocs: unrecorded, Since: since}, nil
}

// docUnchangedSinceSHA reports whether the managed doc's tree state at
// HEAD matches its tree state at sha — i.e. all post-sha commits on
// this path net to a no-op (typical case: the operator reverted a
// plan/reflect pass that had touched the doc). Returns false on any
// git error so the caller degrades to "treat as changed" rather than
// silently swallow real edits.
func docUnchangedSinceSHA(cfg Config, filename, sha string) bool {
	rel := managedDocRelToBureaucracy(cfg, filename)
	if rel == "" {
		return false
	}
	return git.Probe(cfg.BureaucracyPath, "diff", "--quiet", sha, "HEAD", "--", rel)
}

// lastCommitIsTwin reads the body of the most recent commit touching
// filename and reports whether it carries `MoE-Workflow: twin`. The
// second return is false when the doc has no history (untracked /
// orphan path / mis-rooted config), in which case there's nothing to
// flag and the caller should skip it. Parse shape mirrors
// recentTwinSessions in dash.go: split on lines, look for the trailer
// prefix.
func lastCommitIsTwin(cfg Config, filename string) (bool, bool, error) {
	rel := managedDocRelToBureaucracy(cfg, filename)
	if rel == "" {
		return false, false, nil
	}
	body, err := git.Output(cfg.BureaucracyPath, "log", "-1", "--format=%B", "--", rel)
	if err != nil {
		// Untracked / no history → degrade silently. The doc just
		// looks unchanged from the bureaucracy's point of view.
		return false, false, nil
	}
	if strings.TrimSpace(body) == "" {
		return false, false, nil
	}
	for _, line := range strings.Split(body, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "MoE-Workflow:")
		if !ok {
			continue
		}
		if strings.TrimSpace(v) == "twin" {
			return true, true, nil
		}
	}
	return false, true, nil
}

// managedDocRelToBureaucracy returns the doc's path relative to the
// bureaucracy root, suitable for `git log -- <rel>`. Returns "" if
// the bureaucracy root isn't a prefix of ContentDir (mis-rooted
// config), in which case the caller treats it as "no history."
func managedDocRelToBureaucracy(cfg Config, filename string) string {
	if cfg.BureaucracyPath == "" {
		return ""
	}
	dir := strings.TrimPrefix(cfg.ContentDir, cfg.BureaucracyPath)
	dir = strings.TrimPrefix(dir, "/")
	if dir == "" {
		return filename
	}
	return dir + "/" + filename
}

// ClaimPromptSection is the wiki-specific block for a twin claim
// session. Sibling of IngestPromptSection / LintPromptSection /
// reflect: same preamble, different framing — record
// what the operator changed and why, without editing managed docs.
//
// Closed-schema only.
func ClaimPromptSection(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", fmt.Errorf("wiki: claim is closed-schema only (got %s)", cfg.Mode)
	}
	var b strings.Builder
	b.WriteString(wikiPreamble(cfg))
	b.WriteString(`Claim pass (closed-schema):

The operator edited one or more managed docs outside a reflect pass.
Walk through what changed with them and synthesise a log entry
recording:

- **What changed** — derived from the diff (the engine seeds a diff
  block in your kickoff prompt).
- **Why** — the operator's words, plus motivation pulled from any
  recent run docs the engine seeded for context.
- **Which run** — the bureaucracy run id whose context fed the
  decision, if any. Render this as a ` + "`_For: <run-id>_`" + ` line below
  the run-title line.

Claim is bookkeeping. Do **not** edit managed docs in this session.
The operator's edits already exist on disk; claim only records
context. If reflect-style content fixes are also warranted, the
operator can run ` + "`moe twin reflect`" + ` after claiming.

Engine-managed files: the engine writes checkpoint.json on its own.
For claim, ` + "`log.md`" + ` is the journal entry — append a new entry at
the bottom in the shape:

    ## YYYY-MM-DD — claim-<timestamp>
    _<short title>_
    _For: <run-id>_   (if a bureaucracy run drove this; omit otherwise)

    <one-paragraph synthesis: what changed, why, naming the docs>

The per-pass durable record lives at
` + "`projects/<project>/runs/<run-slug>/documents/claim/content.md`" + ` —
the engine threads the exact path into your kickoff. Treat it as a
short PR description for the operator's edits: Trigger / Decision /
Diff in prose. log.md is the journal line; the canvas is the long
record. The session refuses to seal until the canvas is non-empty.

The engine bumps the checkpoint at session close so the next reflect
sees a clean state.

Schema-evolution rules (closed-schema): the doc set is fixed.
Do not create, rename, or delete managed docs.

`)
	if len(cfg.AllowedPrimitives) > 0 {
		fmt.Fprintf(&b, "Allowed primitives: %s.\n", strings.Join(cfg.AllowedPrimitives, ", "))
	} else {
		b.WriteString("Allowed primitives: (none — content edits only).\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// UnrecordedDiff returns a `git diff` of the managed docs from
// checkpoint.bureaucracy_sha to HEAD, scoped to ContentDir. Returns
// "" when there's no checkpoint to diff from or no diff to surface.
func UnrecordedDiff(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", nil
	}
	cp, ok, err := ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return "", err
	}
	if !ok || cp.BureaucracySHA == nil || *cp.BureaucracySHA == "" {
		return "", nil
	}
	rel := managedDocRelToBureaucracy(cfg, "")
	rel = strings.TrimSuffix(rel, "/")
	if rel == "" {
		return "", nil
	}
	out, err := git.Output(cfg.BureaucracyPath, "diff", *cp.BureaucracySHA+"..HEAD", "--", rel)
	if err != nil {
		return "", nil
	}
	return out, nil
}
