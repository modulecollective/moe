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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/trailers"
)

// ErrRunNotFound is returned (wrapped) by Load when the run's run.json
// is missing. Callers use errors.Is to render a clean "run not found"
// message instead of leaking the per-turn worktree path through the
// raw filesystem error a typo would otherwise produce.
var ErrRunNotFound = errors.New("run not found")

// Document is the machine-readable slice of a single document's state.
// Documents themselves are just files on disk (content.md); this struct
// carries only the data that can't be derived from them — the Claude
// Code session id so stage sessions can resume the same conversation.
type Document struct {
	Session string `json:"session"`
}

// Run status values written to Metadata.Status. A run opens in
// StatusInProgress; `moe sdlc push` lands it in StatusMerged (the
// default FF-merge path) or StatusPushed (`--pr`, waiting on the human
// to merge or close on GitHub). `moe sync` then reconciles pushed runs
// into StatusMerged or StatusClosed. StatusPromoted is the terminal for
// an idea run handed off to another run — peer to StatusClosed but
// distinguishable without reading trailers, so dash can tell "moved on"
// from "dropped". Kept as a small closed set so readers can bucket
// without string-typo risk.
const (
	StatusInProgress = "in_progress"
	StatusPushed     = "pushed"
	StatusMerged     = "merged"
	StatusClosed     = "closed"
	StatusPromoted   = "promoted"
)

// Metadata is the on-disk shape of projects/<project>/runs/<id>/run.json.
type Metadata struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Status   string `json:"status"`
	Workflow string `json:"workflow"`
	Created  string `json:"created"`
	// Workspace, when non-empty, names the persistent named workspace
	// this run is attached to (under .moe/named/<project>/<name>/).
	// Empty means the run uses a per-run sandbox (.moe/clones/...).
	// Set at run-open time via `--workspace <name>` on the new verb;
	// every later verb (stage session, push, close, sync, shell)
	// routes off it. omitempty keeps run.json bodies of pre-workspace
	// runs unchanged so diffs and tests stay clean.
	Workspace string `json:"workspace,omitempty"`
	// Agent names the backend (claude / codex) that should drive
	// every stage turn on this run. Empty falls through to
	// $MOE_AGENT, then "claude". Persisted at run-open via
	// `--agent <name>` on `sdlc new` so cross-machine `sdlc resume`
	// picks up the same backend without having to re-pass the flag.
	// Per-stage overrides (`--agent codex` on a single stage) read
	// past this value but do not write back — the run-level default
	// is sticky.
	Agent string `json:"agent,omitempty"`
	// ReopenOf, when non-empty, names the prior run slug this run was
	// reopened from. Mirrors the MoE-Reopen-Of trailer on the open
	// commit, but lives in run.json so the stage prompt assembler can
	// surface prior-run lineage without walking git per stage turn.
	// Set at open time by `moe sdlc reopen` (via Options.ReopenOf);
	// empty on every other path. omitempty keeps pre-existing run.json
	// bodies unchanged.
	ReopenOf  string               `json:"reopen_of,omitempty"`
	Documents map[string]*Document `json:"documents"`
}

// Options carries user-supplied fields for New. Workflow is required;
// the rest are optional.
type Options struct {
	// ID is the user-typed slug for the run. Must match idPattern. The
	// caller has already validated it (canonical-slug check at the verb
	// boundary); New refuses non-matching values defensively and refuses
	// collisions loudly with a free-suggestion in the error message.
	ID string
	// IDBase, when non-empty and ID is empty, supplies the slug base
	// for derived-slug paths (`--from-idea`, `sdlc reopen`). On collision
	// the suffix is the current date (YYYY-MM-DD), falling back to
	// -YYYY-MM-DD-N if two derivations land on the same day. Derived
	// slugs disambiguate silently because the operator didn't pick them;
	// user-typed slugs fail loud (see ID above).
	IDBase string
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

	// SubjectFrom, when non-empty, appends ` from <SubjectFrom>` to the
	// open commit's subject so a promoted idea commits as
	// `Open run p/r from idea slug` rather than the default `Open run p/r`.
	SubjectFrom string

	// Trailers carries optional MoE-* trailers to attach to the open
	// commit alongside the always-emitted MoE-Run / MoE-Project. The
	// caller leaves the Run/Project fields zero — New fills them in
	// canonical order. Today only MoE-Idea (--from-idea) and
	// MoE-From-Run (followups harvest) are populated by callers.
	Trailers trailers.Block

	// Workspace, when non-empty, attaches this run to a named
	// per-project workspace instead of giving it a fresh per-run
	// sandbox. Persisted to Metadata.Workspace so every later verb
	// routes off the same flag. The shared `runNew` validates the
	// name (lower-kebab, see workspace.ValidateName) and refuses to
	// open a second run against an already-claimed name; the
	// workspace itself is materialised lazily on first stage attach
	// (sdlc design under the sdlc workflow) or shell drop-in.
	Workspace string

	// Agent, when non-empty, names the agent backend that should
	// drive stage turns on this run. Persisted to Metadata.Agent.
	// Stage callers thread this through stageSessionOpts.Agent so
	// resolveAgentName picks it up over $MOE_AGENT / the "claude"
	// hard default. Empty leaves Metadata.Agent unset; the same
	// precedence then runs unchanged.
	Agent string

	// ReopenOf, when non-empty, names the prior run slug this run was
	// reopened from. Persisted to Metadata.ReopenOf so the stage
	// prompt assembler can name prior-run artifacts without walking
	// git per stage turn. Set by `moe sdlc reopen` alongside the
	// Trailers.ReopenOf trailer; the trailer remains the canonical
	// signal for dash / journal index, the metadata field is the
	// cheap read path. Empty on every other path.
	ReopenOf string

	// AllowDirty bypasses the working-tree-clean precondition. The
	// guardrail is there so a stray edit doesn't ride along on the
	// open-run commit; callers that already vetted the tree (the
	// followups harvester is the only current example — it allows a
	// modified followups.md to ride along to the close commit, while
	// each per-idea open-run commit only stages its own paths) opt out
	// here. The opt-out is per-caller because run.New still stages
	// only its addPaths, so dirt elsewhere is not silently committed.
	AllowDirty bool
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
// The id comes from opts.ID (user-typed slug — collisions fail loud
// with a free-suggestion in the error) or opts.IDBase (derived slug
// from --from-idea / sdlc reopen — collisions silently get a
// -YYYY-MM-DD date suffix, falling back to -YYYY-MM-DD-N if two
// derivations land on the same day). One of the two must be set.
//
// Refuses if the project is not registered, the explicit id collides, or
// the working tree is dirty (a stray edit shouldn't ride along on the
// "open run" commit).
func New(root, projectID string, opts Options) (*Metadata, error) {
	if !idPattern.MatchString(projectID) {
		return nil, fmt.Errorf("run: project id %q must match %s", projectID, idPattern)
	}
	if opts.Workflow == "" {
		return nil, fmt.Errorf("run: workflow is required")
	}

	projectJSON := filepath.Join(root, "projects", projectID, "project.json")
	if _, err := os.Stat(projectJSON); err != nil {
		return nil, fmt.Errorf("run: project %s not registered (%s missing)", projectID, filepath.Join("projects", projectID, "project.json"))
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	var id string
	var dateSuffix bool
	switch {
	case opts.ID != "":
		id = opts.ID
		if !idPattern.MatchString(id) {
			return nil, fmt.Errorf("run: id %q must match %s", id, idPattern)
		}
	case opts.IDBase != "":
		if !idPattern.MatchString(opts.IDBase) {
			return nil, fmt.Errorf("run: id base %q must match %s", opts.IDBase, idPattern)
		}
		id = opts.IDBase
		dateSuffix = true
	default:
		return nil, fmt.Errorf("run: one of Options.ID or Options.IDBase is required")
	}

	taken, err := SlugTaken(root, projectID, id)
	if err != nil {
		return nil, err
	}
	if taken {
		if !dateSuffix {
			suggestion, serr := NextFreeID(root, projectID, id)
			if serr != nil {
				return nil, serr
			}
			return nil, fmt.Errorf("%w: slug %q in project %s (existing run or prior history); try %q or pick a different name", ErrSlugTaken, id, projectID, suggestion)
		}
		id, err = nextFreeDatedID(root, projectID, opts.IDBase, now())
		if err != nil {
			return nil, err
		}
	}
	runDirRel := Dir(projectID, id)

	if !opts.AllowDirty {
		dirty, err := workingTreeDirty(root)
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("run: working tree has uncommitted changes; commit or stash first")
		}
	}

	md := &Metadata{
		ID:        id,
		Project:   projectID,
		Status:    StatusInProgress,
		Workflow:  opts.Workflow,
		Agent:     opts.Agent,
		Created:   now().Local().Format("2006-01-02"),
		Workspace: opts.Workspace,
		ReopenOf:  opts.ReopenOf,
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
	if err := git.Run(root, addArgs...); err != nil {
		return nil, fmt.Errorf("run: git add: %w", err)
	}
	for _, p := range opts.RemovePaths {
		if err := git.Run(root, "rm", "--", p); err != nil {
			return nil, fmt.Errorf("run: git rm %s: %w", p, err)
		}
	}

	subject := fmt.Sprintf("Open run %s/%s", projectID, id)
	if opts.SubjectFrom != "" {
		subject += " from " + opts.SubjectFrom
	}

	tr := opts.Trailers
	tr.Run = id
	tr.Project = projectID
	msg := subject + "\n\n" + tr.String()
	if err := git.Run(root, "commit", "-m", msg); err != nil {
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

// ThreadPathFor returns the per-agent conversation transcript path
// (`thread-<agent>.jsonl`) relative to the bureaucracy root. Every
// stage turn mirrors its agent's per-session JSONL here. A document
// touched by two agents accumulates two files in its directory
// (`thread-claude.jsonl`, `thread-codex.jsonl`) — the operator's
// `cat thread-*.jsonl` reads the agent-tagged forensic history.
func ThreadPathFor(agent, projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), "thread-"+agent+".jsonl")
}

// PromptPathFor returns the per-agent assembled-prompt snapshot path
// (`prompt-<agent>.md`) relative to the bureaucracy root. Stage
// sessions overwrite this file each turn with the full
// `--append-system-prompt` payload (soul, stage fragment, operational
// core, project guidance, banners, …) so the operator can see what
// the agent actually received. The per-turn diff lands in git history
// via commitTurn's docDir staging.
func PromptPathFor(agent, projectID, id, docID string) string {
	return filepath.Join(DocDir(projectID, id, docID), "prompt-"+agent+".md")
}

// FollowupsPath returns the path (relative to the bureaucracy root) of
// a run's follow-ups scratch file: a markdown checklist sibling of
// run.json that grows during stages and is harvested into ideas at
// close. The file is optional — a run without follow-ups never has one
// on disk.
func FollowupsPath(projectID, id string) string {
	return filepath.Join(Dir(projectID, id), "followups.md")
}

// FeedbackDir returns the path (relative to the bureaucracy root) of a
// run's feedback/ directory: sibling of run.json that holds free-form
// notes workflow agents leave for downstream recipients (twin reflect,
// …). The directory is optional — a run that
// never produces feedback never has one on disk.
func FeedbackDir(projectID, id string) string {
	return filepath.Join(Dir(projectID, id), "feedback")
}

// FeedbackPath returns the path (relative to the bureaucracy root) of a
// per-recipient feedback file under a run's feedback/ directory. v1
// callers pass "twin" so reflect picks the notes up on its next pass;
// the recipient axis is the wedge that lets a future "moe" recipient
// slot in without restructuring.
func FeedbackPath(projectID, id, recipient string) string {
	return filepath.Join(FeedbackDir(projectID, id), recipient+".md")
}

// uuidV4Pattern matches the canonical 8-4-4-4-12 hex form Claude Code
// requires for --session-id. Kept here so EnsureDocument can detect and
// heal entries that predate the UUID requirement.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewSessionID returns a fresh random UUIDv4 for use as a Claude Code
// --session-id. Claude Code rejects non-UUID session ids, so we mint
// one per document and store it in run.json. Exported so run-less
// sessions (e.g. wiki lint) can mint their own without duplicating
// the generator.
func NewSessionID() (string, error) {
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
		sid, err := NewSessionID()
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
	return git.Run(root, "commit", "-m", msg)
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
	return git.Run(root, addArgs...)
}

// HasStagedChanges reports whether the index has anything staged
// relative to HEAD.
func HasStagedChanges(root string) bool {
	return hasStagedChanges(root)
}

// ErrNothingToCommit is returned by StageAndCommit when git has no staged
// changes — signals "turn produced no document edits" to the caller.
var ErrNothingToCommit = errors.New("run: nothing to commit")

// ErrSlugTaken is returned by New when an explicit Options.ID collides
// with an existing run dir or a slug that already appears in main's
// commit history. Wrapped (not the bare error) so callers can detect
// the condition with errors.Is and retry under a different slug — the
// followups harvester uses this for auto-disambiguation across a batch.
var ErrSlugTaken = errors.New("run: slug already used")

func hasStagedChanges(root string) bool {
	// `diff --cached --quiet` exits 0 if nothing is staged, 1 if there
	// are staged changes — Probe returns true on exit 0, so a Probe of
	// true means "no staged changes" and we negate it.
	return !git.Probe(root, "diff", "--cached", "--quiet")
}

// LatestWorkTurnSHA returns the SHA and committer time of the most recent
// `work: update <docID>` commit for the run. Slugs are unique per
// project across all workflows (the runs/<slug> directory namespace is
// flat), so (project, run, document) is enough to key a run's history;
// the workflow trailer is written but not filtered on. An anchored
// subject grep keeps session-start, merge, and push commits from
// slipping past. Returns ("", time.Time{}, nil) when there has been no
// work turn yet — the caller treats that as "first turn, nothing to
// diff against."
func LatestWorkTurnSHA(root, projectID, runID, docID string) (sha string, when time.Time, err error) {
	// Doc IDs are [a-z0-9-]+ today, so QuoteMeta is belt-and-suspenders:
	// nothing in that class is a BRE metacharacter, but escape anyway so
	// a future looser validator can't turn a doc ID into a regex foot-gun.
	out, err := git.Output(root,
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("^work: update %s$", regexp.QuoteMeta(docID)),
		"--grep", fmt.Sprintf("MoE-Project: %s", projectID),
		"--grep", fmt.Sprintf("MoE-Run: %s", runID),
		"--grep", fmt.Sprintf("MoE-Document: %s", docID),
		"--format=%H %ct",
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(out)
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

// LatestAdvanceSHA returns the SHA and committer time of the most recent
// `advance: <docID>` marker commit for the run — the click-forward marker
// the chain prompt writes when the operator declines the next stage but
// records the current one as done (commitAdvance). Mirrors
// LatestWorkTurnSHA's scoping (anchored subject grep + the same
// project/run/document trailer greps) but matches the advance subject
// instead of the work-turn subject. Returns ("", time.Time{}, nil) when no
// advance marker exists yet — the stage was never click-advanced.
func LatestAdvanceSHA(root, projectID, runID, docID string) (sha string, when time.Time, err error) {
	out, err := git.Output(root,
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("^advance: %s$", regexp.QuoteMeta(docID)),
		"--grep", fmt.Sprintf("MoE-Project: %s", projectID),
		"--grep", fmt.Sprintf("MoE-Run: %s", runID),
		"--grep", fmt.Sprintf("MoE-Document: %s", docID),
		"--format=%H %ct",
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(out)
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
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s/%s: %w", projectID, id, ErrRunNotFound)
		}
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
// buckets and render each run's age ("60d ago").
func LastActivity(root, runID string) (time.Time, error) {
	out, err := git.Output(root,
		"log", "-1",
		"--grep", fmt.Sprintf("MoE-Run: %s", runID),
		"--format=%ct",
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return time.Time{}, nil
	}
	epoch, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("run: parse %%ct %q: %w", line, err)
	}
	return time.Unix(epoch, 0).UTC(), nil
}

// JournalIndex is the precomputed in-memory view of MoE-trailer
// signals dash reads from the bureaucracy journal. One batched
// `git log` builds it (BuildJournalIndex); downstream callers do map
// lookups instead of forking git per run, dropping dash's hot-path
// invocation count from ~99 to ~3.
//
// LastActivity / PromotedTo / PRURL are keyed by run slug (MoE-Run
// trailer value) and follow the same "first commit encountered wins"
// rule LastActivity uses — HEAD-side topo order, not strictly newest
// committer date. Missing slugs read as the zero value (zero time,
// "") so callers don't need to branch on presence. WorkTurnTime is
// keyed by (project, run, doc) — every consumer (Workflow.NextWithIndex,
// stage-satisfaction walks) already knows the project, and the seam
// has to be cross-project safe because slugs are per-project unique,
// not per-bureaucracy.
type JournalIndex struct {
	// LastActivity maps run slug → committer time of the latest
	// reachable commit carrying MoE-Run: <slug>. Same contract as
	// LastActivity for any single slug.
	LastActivity map[string]time.Time
	// PromotedTo maps run slug → MoE-Promoted-To trailer value
	// (`<project>/<runID>`) recorded on the most recent commit
	// scoped to the slug. Replaces a per-row trailerValue fork.
	PromotedTo map[string]string
	// PRURL maps run slug → MoE-PR trailer value recorded on the
	// most recent commit scoped to the slug. Replaces a per-row
	// trailerValue fork.
	PRURL map[string]string
	// WorkTurnTime maps (project, run, doc) → committer time of the
	// most recent `work: update <doc>` commit scoped to that run.
	// Same contract as LatestWorkTurnSHA's `when` return — zero
	// time means "no work turn on record yet."
	WorkTurnTime map[WorkTurnKey]time.Time
	// AdvanceTime maps (project, run, doc) → committer time of the
	// most recent `advance: <doc>` marker commit scoped to that run.
	// Same key and contract as WorkTurnTime, populated in the same
	// scan and read by stageSatisfied's advance check, so dash's
	// index path agrees with the per-call LatestAdvanceSHA fork.
	AdvanceTime map[WorkTurnKey]time.Time
	// ReopenedFrom maps new run slug → prior run slug, populated from
	// the MoE-Reopen-Of trailer carried on a reopened run's open
	// commit. Parallel in shape to PromotedTo, but read in the
	// opposite direction: PromotedTo is keyed by the source run
	// (whose commit carries the trailer at status-bump time);
	// ReopenedFrom is keyed by the destination run (whose open
	// commit carries the trailer). Dash uses the value set to
	// recognise prior runs that have *not* been reopened yet — those
	// are the candidates the closed bucket marks.
	ReopenedFrom map[string]string
	// ChainedChild maps "<project>/<slug>" of a parent run to
	// "<project>/<slug>" of its currently-live chained child, or
	// to "" if the most recent chain-related commit cleared the
	// parent's edge. Absent keys mean the parent has never had a
	// chain trailer. Three-valued by design so a newer chain-clear
	// commit blocks an older chain-edit from re-asserting an edge —
	// the entry pins the verdict either way.
	//
	// Read-side rule for "is this parent currently chained?":
	// `v, ok := ChainedChild[parent]; ok && v != ""`. Built from
	// MoE-Chained-To / MoE-Chained-To-Removed trailers, walked HEAD-
	// first; first commit touching a parent decides its live state.
	// Within one commit, MoE-Chained-To beats MoE-Chained-To-Removed
	// for the same parent (a `chain edit` save naturally pairs a
	// remove of the prior edge with the add of the new one, and the
	// add is the survivor).
	ChainedChild map[string]string
	// ChoreByRun maps "<project>/<slug>" to "<project>/<chore>" for
	// runs opened by `moe chore open`.
	ChoreByRun map[string]string
	// ChoreTouched maps "<project>/<chore>" to the most recent
	// reachable commit time carrying MoE-Chore-Touched for that chore.
	ChoreTouched map[string]time.Time
	// ChoreSkipped maps "<project>/<chore>" to the most recent
	// reachable commit time carrying MoE-Chore-Skipped for that chore,
	// written by `moe chore skip`. Evaluate folds it into the value the
	// due reasons compare against, as if a run had completed then.
	ChoreSkipped map[string]time.Time
	// DailyRunCount maps a UTC date ("2006-01-02") to the number of
	// distinct runs that had at least one MoE-Run commit that day — the
	// "runs active per day" tempo the dash histogram charts. A run is
	// counted by its full (project, slug) identity, not bare slug: slugs
	// are only per-project unique, so two projects can carry the same
	// slug and a bare-slug key would collapse them into one tally. Built
	// by counting distinct (project, slug) pairs per day during the same
	// walk (a run that commits five times in a day counts once); only days
	// with activity are present, so a missing key reads as zero.
	DailyRunCount map[string]int
	// DailyRunCountByProject maps a project id to that project's own
	// DailyRunCount (UTC date → distinct run slugs active that day). The
	// per-project slice the dash filters to under `moe dash --project`.
	// Commits with no MoE-Project trailer bucket under "" — nobody reads
	// that key by name (the empty filter takes the global DailyRunCount
	// path), but it keeps the global total whole. Summing every project's
	// per-day count reproduces DailyRunCount.
	DailyRunCountByProject map[string]map[string]int
}

// WorkTurnKey scopes a work-turn lookup to (project, run, doc). The
// project leg is load-bearing: runs/<slug> is project-scoped, so two
// projects can carry the same slug (TestWorkflowNextIgnoresOtherProject
// SameSlug pins this), and a slug-only key would cross-satisfy them.
type WorkTurnKey struct {
	Project string
	Run     string
	Doc     string
}

// ChainChildLive reports whether childKey names a run that's on disk
// and not terminal — the read-side of the chain-edge rule (terminal
// children are filtered, so an edge to one wouldn't fire on the ride).
// Empty or missing-from-byKey counts as not live. The dash render and
// the chain-edit annotations share this one definition.
func ChainChildLive(childKey string, byKey map[string]*Metadata) bool {
	if childKey == "" {
		return false
	}
	child, ok := byKey[childKey]
	if !ok {
		return false
	}
	switch child.Status {
	case StatusClosed, StatusMerged, StatusPromoted, StatusPushed:
		return false
	}
	return true
}

// ChainOrderItem is one active run handed to OrderChainUnits: its
// qualified "<project>/<slug>" key and the recency time used to place
// it. Callers pass items in their own recency order (newest first);
// OrderChainUnits is stable over that order, so equal-time ties resolve
// however the caller already sorted them.
type ChainOrderItem struct {
	Key  string
	When time.Time
}

// OrderChainUnits groups the given active runs into chain units and
// returns them most-recent-first, each unit ordered head→tail. It is
// the shared ordering behind the dash's ACTIVE section and the
// `chain edit` editor file, so both read the same way.
//
// A unit is either a single run (an orphan, or any run with no live
// active child) or a head run followed transitively by its live
// chained children — a contiguous head→tail block. Edges are read from
// idx.ChainedChild, kept only when both endpoints appear in items and
// the child is live (ChainChildLive). Each unit floats by its
// most-recent member; units sort by that time, descending, stably over
// the caller's item order.
//
// A parentless cycle (no head) is caught by the safety net: any run
// left unplaced after the head walks is emitted as its own one-key unit
// in its recency slot, so no run is ever dropped.
//
// items must be in recency order (newest first); see ChainOrderItem.
func OrderChainUnits(items []ChainOrderItem, idx *JournalIndex, byKey map[string]*Metadata) [][]string {
	inActive := make(map[string]bool, len(items))
	whenOf := make(map[string]time.Time, len(items))
	for _, it := range items {
		inActive[it.Key] = true
		whenOf[it.Key] = it.When
	}

	// Live edges with both endpoints active. childOf is ≤1 per parent
	// (ChainedChild maps a parent to one child); parentOf records the
	// active incoming edge — only its presence is read, so fan-in's
	// last-writer-wins is harmless.
	childOf := make(map[string]string)
	parentOf := make(map[string]string)
	for parent, child := range idx.ChainedChild {
		if inActive[parent] && inActive[child] && ChainChildLive(child, byKey) {
			childOf[parent] = child
			parentOf[child] = parent
		}
	}

	type unit struct {
		keys []string
		rep  time.Time // representative time = most-recent member
	}
	consumed := make(map[string]bool, len(items))
	var units []unit
	for _, it := range items { // recency order
		k := it.Key
		if consumed[k] {
			continue
		}
		if _, hasParent := parentOf[k]; hasParent {
			continue // a member; emitted within its head's unit
		}
		if _, hasChild := childOf[k]; !hasChild {
			consumed[k] = true
			units = append(units, unit{keys: []string{k}, rep: it.When})
			continue
		}
		// Head: walk childOf transitively, cycle-guarded by consumed.
		var u unit
		for cur := k; cur != "" && !consumed[cur]; cur = childOf[cur] {
			consumed[cur] = true
			u.keys = append(u.keys, cur)
			if w := whenOf[cur]; w.After(u.rep) {
				u.rep = w
			}
		}
		units = append(units, u)
	}
	// Safety net for a parentless cycle (no head): keep any unplaced run
	// in its recency slot rather than dropping it.
	for _, it := range items {
		if !consumed[it.Key] {
			consumed[it.Key] = true
			units = append(units, unit{keys: []string{it.Key}, rep: it.When})
		}
	}
	sort.SliceStable(units, func(i, j int) bool {
		return units[i].rep.After(units[j].rep)
	})
	out := make([][]string, len(units))
	for i, u := range units {
		out[i] = u.keys
	}
	return out
}

// BuildJournalIndex walks `git log` once and indexes every MoE-Run
// trailer, plus the MoE-Promoted-To and MoE-PR trailers carried on
// the same run-scoped commits. One fork+exec replaces the per-run
// trailerValue + LastActivity + LastActivityMap calls dash used to
// make on the hot path.
//
// HEAD-only walk: a run only reaches the dash via run.json on disk,
// and run.json lands on main as part of the opening commit, so any
// MoE-Run-tagged commit dash cares about is reachable from HEAD.
// Mirrors the scope LastActivityMap walked.
func BuildJournalIndex(root string) (*JournalIndex, error) {
	// Two --grep patterns are OR'd by default. The widening pulls in
	// chain-edit / chain-clear commits, which carry no MoE-Run trailer
	// (one edit touches several parents — no single canonical run to
	// scope it to) and would otherwise be invisible to the existing
	// MoE-Run-only filter.
	out, err := git.Output(root,
		"log",
		"--grep", "^MoE-Run: ",
		"--grep", "^MoE-Chore",
		"--grep", "^MoE-Chained-To",
		"--format=%ct%x00%B%x1e",
	)
	if err != nil {
		return nil, fmt.Errorf("run: git log: %w", err)
	}
	idx := &JournalIndex{
		LastActivity: make(map[string]time.Time),
		PromotedTo:   make(map[string]string),
		PRURL:        make(map[string]string),
		WorkTurnTime: make(map[WorkTurnKey]time.Time),
		AdvanceTime:  make(map[WorkTurnKey]time.Time),
		ReopenedFrom: make(map[string]string),
		ChainedChild: make(map[string]string),
		ChoreByRun:   make(map[string]string),
		ChoreTouched: make(map[string]time.Time),
		ChoreSkipped: make(map[string]time.Time),
	}
	// dailyProjSlugs accumulates, per project, the distinct slug set active
	// on each UTC day (project → day → set<slug>). Nesting by project
	// encodes the qualified (project, slug) identity a run is counted by —
	// within one project a bare slug is unique. Kept as sets during the
	// walk (so repeat commits from one run on one day collapse to a single
	// tally) and folded to the two DailyRunCount maps at the end — storing
	// the counts, not the sets, keeps the index small.
	dailyProjSlugs := make(map[string]map[string]map[string]struct{})
	for _, record := range strings.Split(out, "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		nul := strings.IndexByte(record, 0)
		if nul < 0 {
			continue
		}
		epoch, err := strconv.ParseInt(record[:nul], 10, 64)
		if err != nil {
			continue
		}
		body := record[nul+1:]
		subject := body
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			subject = body[:nl]
		}
		slug := ""
		var promotedTo, prURL, projectID, docID, reopenOf, chore, choreSkipped string
		var choreTouched []string
		// Per-commit chain verdicts. addByParent wins over
		// removeByParent for the same parent within one commit (an edit
		// save pairs a remove of the prior edge with an add of the new
		// one; the new edge is the live state). First add wins on
		// duplicate Chained-To lines for the same parent — linear-chain
		// invariant says there should only be one, but pick a
		// deterministic survivor regardless.
		var addByParent, removeByParent map[string]string
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "MoE-Run:"); ok {
				if slug == "" {
					slug = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Project:"); ok {
				if projectID == "" {
					projectID = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Document:"); ok {
				if docID == "" {
					docID = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Promoted-To:"); ok {
				if promotedTo == "" {
					promotedTo = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-PR:"); ok {
				if prURL == "" {
					prURL = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Reopen-Of:"); ok {
				if reopenOf == "" {
					reopenOf = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Chore-Touched:"); ok {
				if touched := strings.TrimSpace(v); touched != "" {
					choreTouched = append(choreTouched, touched)
				}
				continue
			}
			// MoE-Chore-Skipped must be matched before MoE-Chore — the
			// latter is a prefix of the former. (CutPrefix("MoE-Chore:")
			// can't match "MoE-Chore-Skipped:" since the next char is '-',
			// not ':', but match it first to mirror the Touched arm.)
			if v, ok := strings.CutPrefix(line, "MoE-Chore-Skipped:"); ok {
				if choreSkipped == "" {
					choreSkipped = strings.TrimSpace(v)
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Chore:"); ok {
				if chore == "" {
					chore = strings.TrimSpace(v)
				}
				continue
			}
			// MoE-Chained-To-Removed must be matched before
			// MoE-Chained-To — the latter is a prefix of the former.
			if v, ok := strings.CutPrefix(line, "MoE-Chained-To-Removed:"); ok {
				parent, _, ok := splitChainPair(v)
				if !ok {
					continue
				}
				if removeByParent == nil {
					removeByParent = make(map[string]string)
				}
				if _, dup := removeByParent[parent]; !dup {
					removeByParent[parent] = ""
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "MoE-Chained-To:"); ok {
				parent, child, ok := splitChainPair(v)
				if !ok {
					continue
				}
				if addByParent == nil {
					addByParent = make(map[string]string)
				}
				if _, dup := addByParent[parent]; !dup {
					addByParent[parent] = child
				}
				continue
			}
		}
		// Apply per-commit chain verdicts before the slug gate — chain
		// commits carry no MoE-Run trailer (one edit touches several
		// parents) and would otherwise be discarded.
		for parent, child := range addByParent {
			if _, decided := idx.ChainedChild[parent]; decided {
				continue
			}
			idx.ChainedChild[parent] = child
		}
		for parent := range removeByParent {
			if _, decided := idx.ChainedChild[parent]; decided {
				continue
			}
			idx.ChainedChild[parent] = ""
		}
		for _, touched := range choreTouched {
			if _, ok := idx.ChoreTouched[touched]; !ok {
				idx.ChoreTouched[touched] = time.Unix(epoch, 0).UTC()
			}
		}
		if choreSkipped != "" {
			if _, ok := idx.ChoreSkipped[choreSkipped]; !ok {
				idx.ChoreSkipped[choreSkipped] = time.Unix(epoch, 0).UTC()
			}
		}
		if slug == "" {
			continue
		}
		// Tally the slug under its commit's project and UTC day for the
		// activity histogram. Unlike LastActivity (first-commit-wins),
		// every commit contributes — the set membership dedups within a
		// day. A commit with no MoE-Project trailer buckets under "".
		day := time.Unix(epoch, 0).UTC().Format("2006-01-02")
		byDay := dailyProjSlugs[projectID]
		if byDay == nil {
			byDay = make(map[string]map[string]struct{})
			dailyProjSlugs[projectID] = byDay
		}
		set := byDay[day]
		if set == nil {
			set = make(map[string]struct{})
			byDay[day] = set
		}
		set[slug] = struct{}{}
		if _, ok := idx.LastActivity[slug]; !ok {
			idx.LastActivity[slug] = time.Unix(epoch, 0).UTC()
		}
		if promotedTo != "" {
			if _, ok := idx.PromotedTo[slug]; !ok {
				idx.PromotedTo[slug] = promotedTo
			}
		}
		if prURL != "" {
			if _, ok := idx.PRURL[slug]; !ok {
				idx.PRURL[slug] = prURL
			}
		}
		if reopenOf != "" {
			if _, ok := idx.ReopenedFrom[slug]; !ok {
				idx.ReopenedFrom[slug] = reopenOf
			}
		}
		if chore != "" && projectID != "" {
			k := projectID + "/" + slug
			if _, ok := idx.ChoreByRun[k]; !ok {
				idx.ChoreByRun[k] = chore
			}
		}
		// Work-turn keying is project-scoped, and the subject pin
		// keeps session-start / merge / push commits out — same
		// filter LatestWorkTurnSHA uses.
		if projectID != "" && docID != "" && subject == "work: update "+docID {
			k := WorkTurnKey{Project: projectID, Run: slug, Doc: docID}
			if _, ok := idx.WorkTurnTime[k]; !ok {
				idx.WorkTurnTime[k] = time.Unix(epoch, 0).UTC()
			}
		}
		// Advance markers share the work-turn keying; the distinct
		// subject pin keeps them in their own map, mirroring
		// LatestAdvanceSHA's grep.
		if projectID != "" && docID != "" && subject == "advance: "+docID {
			k := WorkTurnKey{Project: projectID, Run: slug, Doc: docID}
			if _, ok := idx.AdvanceTime[k]; !ok {
				idx.AdvanceTime[k] = time.Unix(epoch, 0).UTC()
			}
		}
	}
	// Collapse the per-(project,day) slug sets to distinct-run counts:
	// the per-project map keeps each project's own window, and the global
	// map sums them — so two projects sharing a slug on one day count as
	// two distinct runs globally, one in each project.
	idx.DailyRunCount = make(map[string]int)
	idx.DailyRunCountByProject = make(map[string]map[string]int, len(dailyProjSlugs))
	for proj, byDay := range dailyProjSlugs {
		perProject := make(map[string]int, len(byDay))
		for day, set := range byDay {
			perProject[day] = len(set)
			idx.DailyRunCount[day] += len(set)
		}
		idx.DailyRunCountByProject[proj] = perProject
	}
	return idx, nil
}

// splitChainPair splits a MoE-Chained-To / -Removed trailer value into
// its parent and child halves. The wire format is two whitespace-
// separated "<project>/<slug>" tokens (project leg required since
// slugs are per-project unique, not bureaucracy-unique). Returns
// ok=false on malformed input — caller should ignore the line rather
// than fail the whole index build, the same posture other trailer
// parsing here takes.
func splitChainPair(v string) (parent, child string, ok bool) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return "", "", false
	}
	if !strings.Contains(f[0], "/") || !strings.Contains(f[1], "/") {
		return "", "", false
	}
	return f[0], f[1], true
}

// LastFileActivity returns the committer time of the most recent commit
// that touched relPath (relative to root), or the zero time if the
// path has no git history. Scoped by path rather than by MoE-Run
// trailer, but otherwise mirrors LastActivity.
func LastFileActivity(root, relPath string) (time.Time, error) {
	out, err := git.Output(root,
		"log", "-1",
		"--format=%ct",
		"--", relPath,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("run: git log: %w", err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return time.Time{}, nil
	}
	epoch, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("run: parse %%ct %q: %w", line, err)
	}
	return time.Unix(epoch, 0).UTC(), nil
}

// nextFreeDatedID resolves an IDBase collision to base-YYYY-MM-DD. If
// that's already taken (two promotes of the same idea-slug in one day),
// it walks base-YYYY-MM-DD-2, base-YYYY-MM-DD-3, … Dates use the
// operator's local zone so the slug flips around the calendar the
// operator reads. Tests that pin a fixed instant near UTC midnight must
// construct it in time.Local (or the desired zone), not time.UTC, or
// the assertion will drift in CI runners whose zone flips the date.
func nextFreeDatedID(root, projectID, base string, now time.Time) (string, error) {
	dated := base + "-" + now.Local().Format("2006-01-02")
	taken, err := SlugTaken(root, projectID, dated)
	if err != nil {
		return "", err
	}
	if !taken {
		return dated, nil
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", dated, n)
		taken, err := SlugTaken(root, projectID, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
}

// NextFreeID walks base, base-2, base-3, … until it finds a slug that
// isn't taken — see SlugTaken for what "taken" means. The base itself
// is never returned; the caller has already checked it. A trailing -N
// is stripped before counting so a collision on fix-timeout-2 continues
// to -3 rather than producing fix-timeout-2-2.
//
// Exported alongside SlugTaken so pre-flighting verbs can suggest the
// same disambiguated slug New itself would suggest on collision.
func NextFreeID(root, projectID, base string) (string, error) {
	base = strings.TrimRight(base, "-")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		tail := base[i+1:]
		if _, err := strconv.Atoi(tail); err == nil {
			base = base[:i]
		}
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		taken, err := SlugTaken(root, projectID, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
}

// SlugTaken reports whether (projectID, slug) is usable for a new run.
// "Taken" means either the run dir already exists on disk OR main
// carries a commit with `MoE-Project: <p>` and `MoE-Run: <slug>`
// trailers. The history check is load-bearing: runs/<slug> is a flat
// namespace, so reusing a deleted run's slug reintroduces its old work
// turns into a fresh run's stage-satisfaction check.
//
// Exported so verbs that pop an editor before run.New (idea new's
// $EDITOR session is a multi-minute window) can pre-flight the slug
// and refuse before any operator effort goes into the tempfile.
func SlugTaken(root, projectID, slug string) (bool, error) {
	if _, err := os.Stat(filepath.Join(root, Dir(projectID, slug))); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("run: stat %s: %w", Dir(projectID, slug), err)
	}
	out, err := git.Output(root,
		"log", "-1",
		"--all-match",
		"--grep", fmt.Sprintf("MoE-Project: %s", projectID),
		"--grep", fmt.Sprintf("MoE-Run: %s", slug),
		"--format=%H",
	)
	if err != nil {
		return false, fmt.Errorf("run: git log: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

func workingTreeDirty(root string) (bool, error) {
	entries, err := git.Status(root)
	if err != nil {
		return false, fmt.Errorf("run: git status: %w", err)
	}
	return len(entries) > 0, nil
}

// WorkingTreeDirty exposes the same precondition New uses internally so
// other commit-on-create entry points (e.g. `moe idea add`) can refuse
// to ride a stray edit on their commit.
func WorkingTreeDirty(root string) (bool, error) {
	return workingTreeDirty(root)
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
