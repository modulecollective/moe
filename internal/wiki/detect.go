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
// produced by a twin session (reflect). Closed-schema only —
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
