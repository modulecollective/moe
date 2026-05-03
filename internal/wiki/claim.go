package wiki

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DetectionResult is the outcome of DetectUnrecordedEdits — the list
// of managed docs touched after the last log.md entry, plus the
// timestamp the comparison was made against.
type DetectionResult struct {
	// UnrecordedDocs are the managed-doc filenames (relative to
	// ContentDir) whose most recent commit in the bureaucracy repo
	// is newer than the wiki's last_ingest_at. Sorted.
	UnrecordedDocs []string
	// Since is the timestamp of the last_ingest_at comparison was
	// made against. Zero when the wiki has no checkpoint yet.
	Since time.Time
}

// DetectUnrecordedEdits flags managed docs that have been edited in
// the bureaucracy repo since the wiki's last log entry. Closed-schema
// only — open-schema's "decided edits" idea isn't load-bearing today,
// so the open path returns an empty result.
//
// Implementation: for each managed doc, ask git for the timestamp of
// its most recent commit; compare against checkpoint.last_ingest_at.
// When a post-checkpoint commit is found and the checkpoint records a
// bureaucracy SHA, also confirm the doc's tree state at HEAD differs
// from its tree state at that SHA — a revert that returns the doc to
// its checkpoint state is a net no-op and shouldn't block the
// operator. Cheap (one git log + at most one git diff per doc); runs
// lazily at the start of twin-touching commands.
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
		// No checkpoint → no comparison point. An unbootstrapped twin
		// has no edits to claim; first reflect will set the baseline.
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
		when, err := lastCommitTime(cfg, d.Filename)
		if err != nil {
			return DetectionResult{Since: since}, err
		}
		if when.IsZero() {
			continue
		}
		if !when.After(since) {
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
	cmd := exec.Command("git", "diff", "--quiet", sha, "HEAD", "--", rel)
	cmd.Dir = cfg.BureaucracyPath
	return cmd.Run() == nil
}

func lastCommitTime(cfg Config, filename string) (time.Time, error) {
	rel := managedDocRelToBureaucracy(cfg, filename)
	if rel == "" {
		return time.Time{}, nil
	}
	cmd := exec.Command("git", "log", "-1", "--format=%cI", "--", rel)
	cmd.Dir = cfg.BureaucracyPath
	out, err := cmd.Output()
	if err != nil {
		// Untracked / no history → degrade silently. The doc just
		// looks unchanged from the bureaucracy's point of view.
		return time.Time{}, nil
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, line)
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
// ReflectPromptSection: same preamble, different framing — record
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
For claim, ` + "`log.md`" + ` is your output — append a new entry at the
bottom in the shape:

    ## YYYY-MM-DD — claim-<timestamp>
    _<short title>_
    _For: <run-id>_   (if a bureaucracy run drove this; omit otherwise)

    <one-paragraph synthesis: what changed, why, naming the docs>

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
	args := []string{"diff", *cp.BureaucracySHA + "..HEAD", "--", rel}
	cmd := exec.Command("git", args...)
	cmd.Dir = cfg.BureaucracyPath
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return string(out), nil
}
