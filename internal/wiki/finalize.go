package wiki

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// FinalizeContext carries the run-side facts the engine needs to
// finalize an ingest. The runtime supplies these when calling
// FinalizeIngest at session close.
type FinalizeContext struct {
	// RunID is the bureaucracy run that drove this ingest (e.g.
	// "wiki-engine"). Recorded in log.md and checkpoint.json.
	RunID string
	// RunTitle is the human-readable run title. Optional; included
	// in the log entry when non-empty so a reader skimming the
	// changelog sees what the run was about.
	RunTitle string
	// Now is the timestamp written to checkpoint.last_ingest_at and
	// to the log entry header. Injected for deterministic tests.
	// Defaults to time.Now if zero.
	Now time.Time
}

// FinalizeResult reports what FinalizeIngest did. Empty Changes plus
// nil error means the ingest produced no wiki edits — the engine
// short-circuits and writes nothing.
type FinalizeResult struct {
	// Changes is the list of paths (relative to ContentDir) that
	// changed during the ingest, with their disposition. Sorted by
	// path for deterministic log entries.
	Changes []Change
	// LogEntryWritten is true when a log.md entry was appended.
	LogEntryWritten bool
	// CheckpointWritten is true when checkpoint.json was written.
	CheckpointWritten bool
}

// Change is one entry in FinalizeResult — a single path's
// before/after disposition.
type Change struct {
	Path   string // relative to ContentDir
	Status ChangeStatus
}

// ChangeStatus is the disposition of a single path within the wiki dir.
type ChangeStatus int

const (
	// Added — path didn't exist at HEAD, exists in the working tree.
	Added ChangeStatus = iota
	// Modified — path's contents differ from HEAD.
	Modified
	// Removed — path existed at HEAD, missing in the working tree.
	Removed
)

func (s ChangeStatus) String() string {
	switch s {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Removed:
		return "removed"
	default:
		return "unknown"
	}
}

// FinalizeIngest is the session-end engine entry point. It diffs the
// wiki content directory against HEAD, appends a log.md entry
// summarising the change set, and writes checkpoint.json. The mutated
// files (log.md, checkpoint.json) are left in the working tree —
// the caller is expected to stage and commit them as part of the same
// per-turn commit that ships the wiki edits.
//
// FinalizeIngest is a no-op when nothing in ContentDir changed: a
// session where the operator opened the wiki, looked around, and
// exited shouldn't add a log entry or bump the checkpoint. Returns the
// result so the caller can report what (if anything) landed.
//
// Errors propagate. The engine logs unrecoverable problems to stderr
// (passed in via stderr) when the change is recoverable but worth
// surfacing — e.g. "project repo absent, recording project_repo_sha
// as null."
func FinalizeIngest(cfg Config, fctx FinalizeContext, stderr io.Writer) (FinalizeResult, error) {
	if cfg.ContentDir == "" {
		return FinalizeResult{}, errors.New("wiki: ContentDir is required")
	}
	if cfg.BureaucracyPath == "" {
		return FinalizeResult{}, errors.New("wiki: BureaucracyPath is required")
	}
	if fctx.Now.IsZero() {
		fctx.Now = time.Now()
	}
	now := fctx.Now.UTC()

	if err := assertModeInvariantsPreFinalize(cfg); err != nil {
		return FinalizeResult{}, err
	}

	changes, err := diffContentDir(cfg.BureaucracyPath, cfg.ContentDir)
	if err != nil {
		return FinalizeResult{}, err
	}
	// Filter out engine-managed files from the change set: appending
	// to log.md ourselves shouldn't generate a "modified log.md" line
	// in the same entry, and ditto for checkpoint.json and the
	// .wiki-ops stash.
	changes = excludeManaged(changes, cfg.ContentDir)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	// Harvest schema-evolution tags before deciding whether the change
	// set is empty: a session that only manipulates the stash without
	// touching any topic doc still degrades to no-op (the stash being
	// truncated isn't a wiki edit), but a session that produced both
	// content edits and tags lands them together in the log entry.
	ops := readAndTruncateOpsStash(cfg.ContentDir)

	if len(changes) == 0 {
		return FinalizeResult{Changes: nil}, nil
	}

	bureaucracySHA := capturedSHA(cfg.BureaucracyPath, false, stderr, "bureaucracy")
	var projectSHA *string
	if cfg.ProjectRepoPath != "" {
		projectSHA = capturedSHA(cfg.ProjectRepoPath, true, stderr, "project repo")
	}

	cp := Checkpoint{
		Version:        CheckpointVersion,
		LastIngestAt:   now.Format(time.RFC3339),
		LastIngestRun:  fctx.RunID,
		BureaucracySHA: bureaucracySHA,
		Project:        cfg.Project,
		ProjectRepoSHA: projectSHA,
	}
	if err := WriteCheckpoint(cfg.ContentDir, cp); err != nil {
		return FinalizeResult{}, err
	}

	if err := appendLogEntry(cfg.ContentDir, now, fctx, changes, ops); err != nil {
		return FinalizeResult{}, err
	}

	return FinalizeResult{
		Changes:           changes,
		LogEntryWritten:   true,
		CheckpointWritten: true,
	}, nil
}

// diffContentDir lists paths under contentDir whose working-tree state
// differs from HEAD, both staged and unstaged. The bureaucracy repo is
// rooted at bureaucracyPath; contentDir must be inside it. Returns a
// flat list of Changes with ContentDir-relative paths.
//
// Implementation note: scoped to contentDir so we get the full set in
// one invocation regardless of whether the wiki dir is fresh (no HEAD
// entries yet) or pre-existing. status's two-character XY codes give
// us the staged + unstaged view in one shot; we collapse them to
// Added/Modified/Removed for the log.
func diffContentDir(bureaucracyPath, contentDir string) ([]Change, error) {
	rel, err := filepath.Rel(bureaucracyPath, contentDir)
	if err != nil {
		return nil, fmt.Errorf("wiki: contentDir %q not inside bureaucracy %q: %w",
			contentDir, bureaucracyPath, err)
	}
	if strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("wiki: contentDir %q escapes bureaucracy %q",
			contentDir, bureaucracyPath)
	}

	entries, err := git.Status(bureaucracyPath, rel)
	if err != nil {
		return nil, fmt.Errorf("wiki: git status %s: %w", rel, err)
	}

	var changes []Change
	for _, e := range entries {
		x := e.XY[0]
		y := e.XY[1]
		// Untracked files arrive as "?? <path>". Treat them as
		// added — they're new files the agent dropped that haven't
		// been staged yet.
		if x == '?' && y == '?' {
			rel, err := relPath(e.Path, contentDir, bureaucracyPath)
			if err != nil {
				return nil, err
			}
			changes = append(changes, Change{Path: rel, Status: Added})
			continue
		}
		// Renames split into a removal of old + addition of new.
		// Under -z, e.Path is the new path and e.From is the old.
		if x == 'R' || y == 'R' {
			oldRel, err := relPath(e.From, contentDir, bureaucracyPath)
			if err != nil {
				return nil, err
			}
			newRel, err := relPath(e.Path, contentDir, bureaucracyPath)
			if err != nil {
				return nil, err
			}
			changes = append(changes,
				Change{Path: oldRel, Status: Removed},
				Change{Path: newRel, Status: Added},
			)
			continue
		}
		status := Modified
		switch {
		case x == 'A' || y == 'A':
			status = Added
		case x == 'D' || y == 'D':
			status = Removed
		}
		rel, err := relPath(e.Path, contentDir, bureaucracyPath)
		if err != nil {
			return nil, err
		}
		changes = append(changes, Change{Path: rel, Status: status})
	}
	return changes, nil
}

// relPath turns a bureaucracy-root-relative path (from git status) into
// a ContentDir-relative one. Errors if the path escapes ContentDir —
// scoped git status should never return such a path, but treat it as
// a programming error rather than silently accept it.
func relPath(bureaucracyRel, contentDir, bureaucracyPath string) (string, error) {
	abs := filepath.Join(bureaucracyPath, bureaucracyRel)
	rel, err := filepath.Rel(contentDir, abs)
	if err != nil {
		return "", fmt.Errorf("wiki: rel %q under %q: %w", bureaucracyRel, contentDir, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("wiki: path %q is outside ContentDir %q", bureaucracyRel, contentDir)
	}
	return rel, nil
}

// excludeManaged drops engine-owned files (log.md, checkpoint.json,
// .wiki-ops) from the change set so finalize doesn't list its own
// writes as part of the ingest's diff. Anything else under ContentDir
// is fair game.
func excludeManaged(changes []Change, contentDir string) []Change {
	managed := map[string]bool{
		"log.md":          true,
		"checkpoint.json": true,
		opsStashName:      true,
	}
	out := changes[:0]
	for _, c := range changes {
		if managed[c.Path] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// appendLogEntry writes a markdown section to <ContentDir>/log.md
// describing the ingest. The format is two stacked groups: the
// operations the agent named via `[wiki-op]` tags (split / merge /
// rename / retire), then the deterministic content-edit list grouped
// by status. Either group may be empty — a session with no tags
// renders content edits only; a session with tags but no content
// edits doesn't reach this function (FinalizeIngest short-circuits on
// an empty change set).
func appendLogEntry(contentDir string, now time.Time, fctx FinalizeContext, changes []Change, ops []wikiOp) error {
	path := logPath(contentDir)
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("wiki: read %s: %w", path, err)
	}
	body := string(existing)
	if body == "" {
		body = "# Changelog\n\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if !strings.HasSuffix(body, "\n\n") {
		body += "\n"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s — %s\n\n", now.Format("2006-01-02"), fctx.RunID)
	if title := strings.TrimSpace(fctx.RunTitle); title != "" {
		fmt.Fprintf(&b, "_%s_\n\n", title)
	}

	// Operations group sits above content edits — the agent's
	// labelled view of what they did, then the deterministic diff.
	for _, op := range ops {
		line := formatOpLine(op)
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", line)
	}

	groups := map[ChangeStatus][]string{}
	for _, c := range changes {
		groups[c.Status] = append(groups[c.Status], c.Path)
	}
	for _, status := range []ChangeStatus{Added, Modified, Removed} {
		paths := groups[status]
		if len(paths) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", status, strings.Join(paths, ", "))
	}
	b.WriteString("\n")

	body += b.String()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("wiki: write %s: %w", path, err)
	}
	return nil
}

// capturedSHA returns the HEAD SHA at repoPath, or nil if the path is
// absent or (when checkDirty is true) has uncommitted changes. label
// is used in the warning message. Best-effort: any git failure becomes
// a stderr warning and a nil pointer rather than a hard error — the
// checkpoint's nullable SHA fields are designed for exactly this case.
func capturedSHA(repoPath string, checkDirty bool, stderr io.Writer, label string) *string {
	if _, err := os.Stat(repoPath); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "wiki: %s at %s absent; recording sha=null\n", label, repoPath)
		}
		return nil
	}
	if checkDirty {
		dirty, err := repoDirty(repoPath)
		if err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "wiki: %s status check failed (%v); recording sha=null\n", label, err)
			}
			return nil
		}
		if dirty {
			if stderr != nil {
				fmt.Fprintf(stderr, "wiki: %s at %s is dirty; recording sha=null\n", label, repoPath)
			}
			return nil
		}
	}
	sha, err := git.HEAD(repoPath)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "wiki: %s rev-parse failed (%v); recording sha=null\n", label, err)
		}
		return nil
	}
	if sha == "" {
		return nil
	}
	return &sha
}

func repoDirty(repoPath string) (bool, error) {
	entries, err := git.Status(repoPath)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}
