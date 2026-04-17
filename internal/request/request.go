// Package request creates and loads request state on the bureaucracy repo.
//
// A request is a unit of work against a registered project. New() writes
// requests/<project>/runs/<id>/request.json and commits it on main. The
// bureaucracy is branchless on purpose — it's an engineering journal, not a
// code repo. Per-request scoping comes from commit trailers (MoE-Request,
// MoE-Document, MoE-Session) attached by `moe work` and friends.
//
// Document conversations are layered on later by `moe work` — request.New
// only opens the folder.
package request

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
// carries only the data that can't be derived from them — the Claude Code
// session id so `moe work` can resume the same conversation.
type Document struct {
	Session string `json:"session"`
}

// Metadata is the on-disk shape of requests/<project>/runs/<id>/request.json.
type Metadata struct {
	ID        string               `json:"id"`
	Project   string               `json:"project"`
	Title     string               `json:"title"`
	Status    string               `json:"status"`
	Created   string               `json:"created"`
	Documents map[string]*Document `json:"documents"`
}

// Options carries optional user-supplied fields for New.
type Options struct {
	// ID overrides the auto-derived slug. Must match idPattern if set.
	ID string
	// Now is injected for deterministic tests. Defaults to time.Now.
	Now func() time.Time
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

// RunDir returns the path (relative to the bureaucracy root) where a
// request's state lives.
func RunDir(projectID, id string) string {
	return filepath.Join("requests", projectID, "runs", id)
}

// New opens a fresh request: writes requests/<project>/runs/<id>/request.json
// and commits it on main.
//
// The id is derived from the title (Slugify) unless opts.ID is set. On
// collision the slug gets a -2, -3, … suffix. An explicit opts.ID is
// never auto-suffixed; collisions there are an error so the caller notices.
//
// Refuses if the project is not registered, the explicit id collides, or
// the working tree is dirty (a stray edit shouldn't ride along on the
// "open request" commit).
func New(root, projectID, title string, opts Options) (*Metadata, error) {
	if !idPattern.MatchString(projectID) {
		return nil, fmt.Errorf("request: project id %q must match %s", projectID, idPattern)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("request: title is required")
	}

	projectJSON := filepath.Join(root, "requests", projectID, "project.json")
	if _, err := os.Stat(projectJSON); err != nil {
		return nil, fmt.Errorf("request: project %s not registered (%s missing)", projectID, filepath.Join("requests", projectID, "project.json"))
	}

	var id string
	var autoSuffix bool
	if opts.ID != "" {
		id = opts.ID
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("request: id %q must match %s", id, idPattern)
		}
	} else {
		base := Slugify(title)
		if base == "" {
			return nil, fmt.Errorf("request: cannot derive slug from title %q; pass --id to set one explicitly", title)
		}
		id = base
		autoSuffix = true
	}

	runDirRel := RunDir(projectID, id)
	if _, err := os.Stat(filepath.Join(root, runDirRel)); err == nil {
		if !autoSuffix {
			return nil, fmt.Errorf("request: %s already exists", runDirRel)
		}
		id = nextFreeID(root, projectID, id)
		runDirRel = RunDir(projectID, id)
	}

	dirty, err := workingTreeDirty(root)
	if err != nil {
		return nil, err
	}
	if dirty {
		return nil, fmt.Errorf("request: working tree has uncommitted changes; commit or stash first")
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	md := &Metadata{
		ID:        id,
		Project:   projectID,
		Title:     title,
		Status:    "in_progress",
		Created:   now().UTC().Format("2006-01-02"),
		Documents: map[string]*Document{},
	}

	reqJSONRel := filepath.Join(runDirRel, "request.json")
	if err := writeJSON(filepath.Join(root, reqJSONRel), md); err != nil {
		return nil, err
	}
	if err := runGit(root, "add", reqJSONRel); err != nil {
		return nil, fmt.Errorf("request: git add: %w", err)
	}
	msg := fmt.Sprintf(`Open request %s/%s: %s

MoE-Request: %s
MoE-Project: %s
`, projectID, id, title, id, projectID)
	if err := runGit(root, "commit", "-m", msg); err != nil {
		return nil, fmt.Errorf("request: git commit: %w", err)
	}
	return md, nil
}

// Save persists md to requests/<project>/runs/<id>/request.json, creating
// the directory if needed. The caller is responsible for staging and
// committing afterward.
func Save(root string, md *Metadata) error {
	path := filepath.Join(root, RunDir(md.Project, md.ID), "request.json")
	return writeJSON(path, md)
}

// DocDir returns the path (relative to the bureaucracy root) where a
// document's files live: documents/<doc>/ under the run dir.
func DocDir(projectID, id, docID string) string {
	return filepath.Join(RunDir(projectID, id), "documents", docID)
}

// ContentPath returns the canonical content file for a document, relative
// to the bureaucracy root. This is the file agents edit.
func ContentPath(projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), "content.md")
}

// uuidV4Pattern matches the canonical 8-4-4-4-12 hex form Claude Code
// requires for --session-id. Kept here so EnsureDocument can detect and
// heal entries that predate the UUID requirement.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newSessionID returns a fresh random UUIDv4 for use as a Claude Code
// --session-id. Claude Code rejects non-UUID session ids, so we mint one
// per document and store it in request.json.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("request: generate session id: %w", err)
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
		return nil, false, fmt.Errorf("request: document id %q must match %s", docID, idPattern)
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
		return nil, false, fmt.Errorf("request: mkdir document dir: %w", err)
	}
	return doc, mutated, nil
}

// StageAndCommit stages pathspecs and commits with msg. Returns ErrNothingToCommit
// if there's nothing staged after the add — common for a `moe work` turn where
// the operator exited Claude without having it write anything.
func StageAndCommit(root, msg string, pathspecs ...string) error {
	addArgs := append([]string{"add", "--"}, pathspecs...)
	if err := runGit(root, addArgs...); err != nil {
		return err
	}
	if !hasStagedChanges(root) {
		return ErrNothingToCommit
	}
	return runGit(root, "commit", "-m", msg)
}

// ErrNothingToCommit is returned by StageAndCommit when git has no staged
// changes — signals "turn produced no document edits" to the caller.
var ErrNothingToCommit = errors.New("request: nothing to commit")

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
// `work: update <docID>` commit for the request, identified by the
// MoE-Request and MoE-Document trailers commitTurn writes. Returns
// ("", time.Time{}, nil) when there has been no work turn yet — the caller
// treats that as "first turn, nothing to diff against."
func LatestWorkTurnSHA(root, requestID, docID string) (sha string, when time.Time, err error) {
	cmd := exec.Command("git",
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("MoE-Request: %s", requestID),
		"--grep", fmt.Sprintf("MoE-Document: %s", docID),
		"--format=%H %ct",
	)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request: git log: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", time.Time{}, nil
	}
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return "", time.Time{}, fmt.Errorf("request: unexpected git log output %q", line)
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request: parse %%ct %q: %w", parts[1], err)
	}
	return parts[0], time.Unix(epoch, 0).UTC(), nil
}

// Load reads requests/<project>/runs/<id>/request.json.
func Load(root, projectID, id string) (*Metadata, error) {
	path := filepath.Join(root, RunDir(projectID, id), "request.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("request: read %s: %w", path, err)
	}
	md := &Metadata{}
	if err := json.Unmarshal(b, md); err != nil {
		return nil, fmt.Errorf("request: parse %s: %w", path, err)
	}
	return md, nil
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
		if _, err := os.Stat(filepath.Join(root, RunDir(projectID, candidate))); err == nil {
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
		return false, fmt.Errorf("request: git status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
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
