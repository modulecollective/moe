// Package run creates and loads run state on the bureaucracy repo.
//
// A run is a unit of work against a registered project. New() writes
// projects/<project>/runs/<id>/run.json and commits it on main. The
// bureaucracy is branchless on purpose — it's an engineering journal, not a
// code repo. Per-run scoping comes from commit trailers (MoE-Run,
// MoE-Document, MoE-Session) attached by stage sessions and friends.
//
// Document conversations are layered on by the stage sessions (e.g.
// `moe sdlc design`) — run.New only opens the folder.
package run

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Document is the machine-readable slice of a single document's state.
// Documents themselves are just files on disk (content.md); this struct
// carries only the data that can't be derived from them — the Claude
// Code session id so stage sessions can resume the same conversation, and
// the Managed Agents session id when the document has a dispatched
// async run in flight or awaiting collection.
type Document struct {
	Session string `json:"session"`
	// Managed is the session id returned by POST /v1/sessions when the
	// document was last dispatched to Anthropic's Managed Agents API.
	// Empty when the document has never been dispatched, or after a
	// dispatched session has been collected and reconciled into the
	// bureaucracy. `moe tail` and `moe status` address the session by
	// this id, so the same (project, run, doc) grammar works for
	// both local and async runs.
	Managed string `json:"managed,omitempty"`
}

// Run status values written to Metadata.Status. A run opens in
// StatusInProgress and flips to StatusPushed when `moe sdlc push` opens
// the PR on the target repo. Kept as a small closed set so moe dash and
// related readers can bucket without string-typo risk.
const (
	StatusInProgress = "in_progress"
	StatusPushed     = "pushed"
)

// Metadata is the on-disk shape of projects/<project>/runs/<id>/run.json.
type Metadata struct {
	ID        string               `json:"id"`
	Project   string               `json:"project"`
	Title     string               `json:"title"`
	Status    string               `json:"status"`
	Workflow  string               `json:"workflow"`
	Created   string               `json:"created"`
	Documents map[string]*Document `json:"documents"`
}

// Options carries user-supplied fields for New. Workflow is required;
// the rest are optional.
type Options struct {
	// ID overrides the auto-derived slug. Must match idPattern if set.
	ID string
	// Workflow names the workflow this run belongs to. Required —
	// fragment lookup in buildSystemPrompt keys on this, so there is no
	// safe default. Callers that want to validate against a registry
	// should do so before invoking New.
	Workflow string
	// Now is injected for deterministic tests. Defaults to time.Now.
	Now func() time.Time

	// SeedDocs, when non-empty, writes an initial content.md for each
	// listed document alongside the run's creation. Keys are document
	// ids (e.g. "design"); values are the file bodies. The files land
	// under DocDir(project, id, docID) and ride along on the open
	// commit, so a promoted-from-idea run starts with its first-stage
	// canvas already populated. Seeded files intentionally do NOT
	// carry a MoE-Document trailer — the stage is not yet satisfied,
	// its agent still owes a work turn.
	SeedDocs map[string]string

	// RemovePaths, when non-empty, lists paths (relative to root)
	// whose deletion should land in the same commit as the run's
	// creation. Used by --from-idea to atomically drop the source
	// idea file so the idea-to-run transition is a single commit in
	// git history.
	RemovePaths []string

	// SubjectFrom, when non-empty, inserts ` from <SubjectFrom>` into
	// the open commit's subject after the run id, before the colon —
	// so a promoted idea commits as `Open run p/r from idea slug: T`
	// rather than the default `Open run p/r: T`.
	SubjectFrom string

	// ExtraTrailers, when non-empty, appends additional trailers to
	// the open commit's message body (one per entry, e.g.
	// `MoE-Idea: <slug>`). Standard MoE-Run / MoE-Project trailers
	// are always written first.
	ExtraTrailers []string
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Slugify turns a free-form title into an id-shaped slug: lowercase,
// non-alphanumerics collapsed to single dashes, trimmed. Returns "" if
// nothing usable remains (e.g. an emoji-only title).
func Slugify(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	prevDash := true // leading dashes get trimmed
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// Dir returns the path (relative to the bureaucracy root) where a
// run's state lives.
func Dir(projectID, id string) string {
	return filepath.Join("projects", projectID, "runs", id)
}

// New opens a fresh run: writes projects/<project>/runs/<id>/run.json
// and commits it on main.
//
// The id is derived from the title (Slugify) unless opts.ID is set. On
// collision the slug gets a -2, -3, … suffix. An explicit opts.ID is
// never auto-suffixed; collisions there are an error so the caller notices.
//
// Refuses if the project is not registered, the explicit id collides, or
// the working tree is dirty (a stray edit shouldn't ride along on the
// "open run" commit).
func New(root, projectID, title string, opts Options) (*Metadata, error) {
	if !idPattern.MatchString(projectID) {
		return nil, fmt.Errorf("run: project id %q must match %s", projectID, idPattern)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("run: title is required")
	}
	if opts.Workflow == "" {
		return nil, fmt.Errorf("run: workflow is required")
	}

	projectJSON := filepath.Join(root, "projects", projectID, "project.json")
	if _, err := os.Stat(projectJSON); err != nil {
		return nil, fmt.Errorf("run: project %s not registered (%s missing)", projectID, filepath.Join("projects", projectID, "project.json"))
	}

	var id string
	var autoSuffix bool
	if opts.ID != "" {
		id = opts.ID
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("run: id %q must match %s", id, idPattern)
		}
	} else {
		base := Slugify(title)
		if base == "" {
			return nil, fmt.Errorf("run: cannot derive slug from title %q; pass --id to set one explicitly", title)
		}
		id = base
		autoSuffix = true
	}

	runDirRel := Dir(projectID, id)
	if _, err := os.Stat(filepath.Join(root, runDirRel)); err == nil {
		if !autoSuffix {
			return nil, fmt.Errorf("run: %s already exists", runDirRel)
		}
		id = nextFreeID(root, projectID, id)
		runDirRel = Dir(projectID, id)
	}

	dirty, err := workingTreeDirty(root)
	if err != nil {
		return nil, err
	}
	if dirty {
		return nil, fmt.Errorf("run: working tree has uncommitted changes; commit or stash first")
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	md := &Metadata{
		ID:        id,
		Project:   projectID,
		Title:     title,
		Status:    StatusInProgress,
		Workflow:  opts.Workflow,
		Created:   now().UTC().Format("2006-01-02"),
		Documents: map[string]*Document{},
	}

	runJSONRel := filepath.Join(runDirRel, "run.json")
	if err := writeJSON(filepath.Join(root, runJSONRel), md); err != nil {
		return nil, err
	}
	addPaths := []string{runJSONRel}
	for docID, body := range opts.SeedDocs {
		if !idPattern.MatchString(docID) {
			return nil, fmt.Errorf("run: seed document id %q must match %s", docID, idPattern)
		}
		seedRel := ContentPath(projectID, id, docID)
		if err := os.MkdirAll(filepath.Join(root, DocDir(projectID, id, docID)), 0o755); err != nil {
			return nil, fmt.Errorf("run: mkdir seed doc dir: %w", err)
		}
		if err := os.WriteFile(filepath.Join(root, seedRel), []byte(body), 0o644); err != nil {
			return nil, fmt.Errorf("run: write seed doc: %w", err)
		}
		addPaths = append(addPaths, seedRel)
	}
	addArgs := append([]string{"add", "--"}, addPaths...)
	if err := runGit(root, addArgs...); err != nil {
		return nil, fmt.Errorf("run: git add: %w", err)
	}
	for _, p := range opts.RemovePaths {
		if err := runGit(root, "rm", "--", p); err != nil {
			return nil, fmt.Errorf("run: git rm %s: %w", p, err)
		}
	}

	subject := fmt.Sprintf("Open run %s/%s", projectID, id)
	if opts.SubjectFrom != "" {
		subject += " from " + opts.SubjectFrom
	}
	subject += ": " + title

	var trailers strings.Builder
	fmt.Fprintf(&trailers, "MoE-Run: %s\nMoE-Project: %s\n", id, projectID)
	for _, t := range opts.ExtraTrailers {
		trailers.WriteString(t)
		trailers.WriteString("\n")
	}
	msg := subject + "\n\n" + trailers.String()
	if err := runGit(root, "commit", "-m", msg); err != nil {
		return nil, fmt.Errorf("run: git commit: %w", err)
	}
	return md, nil
}

// Save persists md to projects/<project>/runs/<id>/run.json, creating
// the directory if needed. The caller is responsible for staging and
// committing afterward.
func Save(root string, md *Metadata) error {
	path := filepath.Join(root, Dir(md.Project, md.ID), "run.json")
	return writeJSON(path, md)
}

// DocDir returns the path (relative to the bureaucracy root) where a
// document's files live: documents/<doc>/ under the run dir.
func DocDir(projectID, id, docID string) string {
	return filepath.Join(Dir(projectID, id), "documents", docID)
}

// ContentPath returns the canonical content file for a document, relative
// to the bureaucracy root. This is the file agents edit.
func ContentPath(projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), "content.md")
}

// ThreadPath returns the path (relative to the bureaucracy root) of a
// document's conversation transcript. Stage sessions mirror Claude
// Code's per-session JSONL here every turn, so the full human/agent
// exchange is stored in-repo alongside the compressed content.md.
func ThreadPath(projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), "thread.jsonl")
}

// uuidV4Pattern matches the canonical 8-4-4-4-12 hex form Claude Code
// requires for --session-id. Kept here so EnsureDocument can detect and
// heal entries that predate the UUID requirement.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newSessionID returns a fresh random UUIDv4 for use as a Claude Code
// --session-id. Claude Code rejects non-UUID session ids, so we mint one
// per document and store it in run.json.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("run: generate session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

// EnsureDocument adds a pending entry for docID to md if one doesn't
// already exist, and makes sure the document's directory exists on disk.
// If the existing entry has no session id (or an invalid one — e.g. from
// before Claude Code required UUIDs), a fresh one is generated.
//
// Returns the entry and whether md was mutated; the caller decides
// whether to persist (Save + commit).
func EnsureDocument(root string, md *Metadata, docID string) (*Document, bool, error) {
	if !idPattern.MatchString(docID) {
		return nil, false, fmt.Errorf("run: document id %q must match %s", docID, idPattern)
	}
	if md.Documents == nil {
		md.Documents = map[string]*Document{}
	}
	doc, existed := md.Documents[docID]
	mutated := false
	if !existed {
		doc = &Document{}
		md.Documents[docID] = doc
		mutated = true
	}
	if !uuidV4Pattern.MatchString(doc.Session) {
		sid, err := newSessionID()
		if err != nil {
			return nil, false, err
		}
		doc.Session = sid
		mutated = true
	}
	if err := os.MkdirAll(filepath.Join(root, DocDir(md.Project, md.ID, docID)), 0o755); err != nil {
		return nil, false, fmt.Errorf("run: mkdir document dir: %w", err)
	}
	return doc, mutated, nil
}

// StageAndCommit stages pathspecs and commits with msg. Returns ErrNothingToCommit
// if there's nothing staged after the add — common for a stage turn where
// the operator exited Claude without having it write anything.
func StageAndCommit(root, msg string, pathspecs ...string) error {
	if err := Stage(root, pathspecs...); err != nil {
		return err
	}
	if !HasStagedChanges(root) {
		return ErrNothingToCommit
	}
	return runGit(root, "commit", "-m", msg)
}

// Stage runs `git add -- pathspecs...` under root. A split primitive so
// callers that need to introspect staging state before committing (e.g.,
// run a pre-commit hook only when the doc actually changed) can do so
// without reimplementing the exec.
func Stage(root string, pathspecs ...string) error {
	if len(pathspecs) == 0 {
		return nil
	}
	addArgs := append([]string{"add", "--"}, pathspecs...)
	return runGit(root, addArgs...)
}

// HasStagedChanges reports whether the index has anything staged
// relative to HEAD.
func HasStagedChanges(root string) bool {
	return hasStagedChanges(root)
}

// ErrNothingToCommit is returned by StageAndCommit when git has no staged
// changes — signals "turn produced no document edits" to the caller.
var ErrNothingToCommit = errors.New("run: nothing to commit")

// CommitAllowEmpty stages pathspecs (if any) and commits with msg, passing
// --allow-empty so the commit lands even when nothing is staged. Used for
// stage sign-offs: the trailer in the commit message is itself the payload,
// so an empty tree is a legitimate commit.
func CommitAllowEmpty(root, msg string, pathspecs ...string) error {
	if len(pathspecs) > 0 {
		addArgs := append([]string{"add", "--"}, pathspecs...)
		if err := runGit(root, addArgs...); err != nil {
			return err
		}
	}
	return runGit(root, "commit", "--allow-empty", "-m", msg)
}

func hasStagedChanges(root string) bool {
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = root
	// --quiet: exit 1 if there are staged changes, 0 if not.
	err := cmd.Run()
	return err != nil
}

// LatestWorkTurnSHA returns the SHA and committer time of the most recent
// `work: update <docID>` commit for the run, identified by the
// MoE-Run and MoE-Document trailers commitTurn writes. Returns
// ("", time.Time{}, nil) when there has been no work turn yet — the caller
// treats that as "first turn, nothing to diff against."
func LatestWorkTurnSHA(root, runID, docID string) (sha string, when time.Time, err error) {
	cmd := exec.Command("git",
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("MoE-Run: %s", runID),
		"--grep", fmt.Sprintf("MoE-Document: %s", docID),
		"--format=%H %ct",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", time.Time{}, nil
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("run: unexpected git log output %q", line)
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("run: parse %%ct %q: %w", parts[1], err)
	}
	return parts[0], time.Unix(epoch, 0).UTC(), nil
}

// Load reads projects/<project>/runs/<id>/run.json.
func Load(root, projectID, id string) (*Metadata, error) {
	path := filepath.Join(root, Dir(projectID, id), "run.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("run: read %s: %w", path, err)
	}
	md := &Metadata{}
	if err := json.Unmarshal(b, md); err != nil {
		return nil, fmt.Errorf("run: parse %s: %w", path, err)
	}
	if md.Workflow == "" {
		return nil, fmt.Errorf("run: %s: workflow is required", path)
	}
	return md, nil
}

// Scan walks projects/*/runs/*/run.json under root and returns every
// run's metadata, in unspecified order. The caller does the sorting
// and bucketing (moe dash, moe history). A missing or empty projects/
// directory returns (nil, nil) — a freshly-initialized bureaucracy is a
// valid state, not an error.
func Scan(root string) ([]*Metadata, error) {
	pattern := filepath.Join(root, "projects", "*", "runs", "*", "run.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("run: glob: %w", err)
	}
	out := make([]*Metadata, 0, len(matches))
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("run: read %s: %w", path, err)
		}
		md := &Metadata{}
		if err := json.Unmarshal(b, md); err != nil {
			return nil, fmt.Errorf("run: parse %s: %w", path, err)
		}
		if md.Workflow == "" {
			return nil, fmt.Errorf("run: %s: workflow is required", path)
		}
		out = append(out, md)
	}
	return out, nil
}

// LastActivity returns the committer time of the most recent commit
// carrying MoE-Run: <runID>, or the zero time if no such commit
// exists (a run dir can exist without its opening commit being
// reachable from HEAD, though that's unusual). Used by moe dash to sort
// buckets and to distinguish dormant runs from live ones.
func LastActivity(root, runID string) (time.Time, error) {
	cmd := exec.Command("git",
		"log", "-1",
		"--grep", fmt.Sprintf("MoE-Run: %s", runID),
		"--format=%ct",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return time.Time{}, nil
	}
	epoch, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("run: parse %%ct %q: %w", line, err)
	}
	return time.Unix(epoch, 0).UTC(), nil
}

// nextFreeID walks base, base-2, base-3, … until it finds a slug whose run
// dir doesn't already exist. The base itself is never returned — the caller
// has already checked it. We strip any trailing -N from base before counting
// so a collision on fix-timeout-2 continues to -3 rather than producing
// fix-timeout-2-2.
func nextFreeID(root, projectID, base string) string {
	base = strings.TrimRight(base, "-")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		tail := base[i+1:]
		if _, err := strconv.Atoi(tail); err == nil {
			base = base[:i]
		}
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if _, err := os.Stat(filepath.Join(root, Dir(projectID, candidate))); err == nil {
			continue
		}
		return candidate
	}
}

func workingTreeDirty(root string) (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("run: git status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// WorkingTreeDirty exposes the same precondition New uses internally so
// other commit-on-create entry points (e.g. `moe idea new`) can refuse
// to ride a stray edit on their commit.
func WorkingTreeDirty(root string) (bool, error) {
	return workingTreeDirty(root)
}

// runGit invokes git with stdio wired to the user's terminal so credential
// helpers and SSH prompts can complete. Capturing stderr would hide those
// prompts and make the command appear to hang.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
