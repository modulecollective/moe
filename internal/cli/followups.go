package cli

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// followups.md is the run-scoped scratch file harvested at close into
// idea runs. Format (matching designs/follow-up-ideas):
//
//	# Follow-ups
//
//	- [ ] `cleanup-foo-helper` — Clean up foo helper
//
//	  Why: bar/baz reach into foo's internals; foo.go:42 is the
//	  load-bearing assumption. Fix sketch: extract a tiny accessor.
//
//	- [x] `chase-zlib-upgrade` — Chase the zlib upgrade Q1 mentioned
//
// Each unchecked entry becomes one idea; the line is rewritten in
// place as `- [x]` carrying the resolved (possibly auto-disambiguated)
// slug. An optional indented body (two-space indent, blank lines
// between paragraphs) rides into the new idea's seed canvas under the
// title H1. Already-checked lines are pass-through, which makes a
// retry after a partial-failure idempotent.

// followupsHeader is the editor-pop banner auto-injected onto
// followups.md before $EDITOR opens. Companion to loreHeader in
// lore.go — both follow the convention described above
// injectEditorPopHeader: HTML comment (invisible in any markdown
// renderer), file-specific phrasing for what the gesture does, and a
// closing "remove freely" line so the operator can silence it.
const followupsHeader = `<!--
followups.md — out-of-scope items captured this run.
Save this file to spin each unchecked ` + "`- [ ]`" + ` entry into a new idea
run. Delete the line to skip. Lines marked ` + "`- [x]`" + ` are already
promoted; leave them alone.
This header is auto-injected on editor pop; remove it freely.
-->`

// followupOpenRE matches an unchecked entry. Captures the indent + box
// prefix (group 1), the slug (group 2), and the title (group 3 — may
// be empty after trim, in which case parseFollowups raises a more
// specific "title is empty" error). The em-dash separator (U+2014) is
// the design's required form; a hyphen or `--` would be ambiguous with
// the leading list marker.
//
// The slug group permits an optional `<project>/` prefix
// (`claudia/inherit-nginx`): one extra `/`-separated segment, each
// segment matching run.idPattern's shape exactly so a slug that parses
// here can't later be rejected by run.New. A bare slug (no `/`) keeps
// today's behaviour — the idea lands in the current project. This regex
// is SHARED with lore via parseChecklist; lore re-narrows it by
// rejecting any `/` (see parseLore) rather than forking a parser.
var followupOpenRE = regexp.MustCompile("^(\\s*-\\s+\\[\\s\\]\\s+)`([a-z0-9][a-z0-9-]*(?:/[a-z0-9][a-z0-9-]*)?)`\\s+—\\s*(.*?)\\s*$")

// followupCheckboxRE detects any list item that looks like a checkbox,
// open or done. Used to flag malformed unchecked entries that don't
// satisfy followupOpenRE — silently skipping those would lose the
// operator's intent.
var followupCheckboxRE = regexp.MustCompile(`^\s*-\s+\[[ xX]\]`)

// followupDoneRE matches an already-checked entry. We don't validate
// these (they are the audit trail of past close runs); we just need to
// recognise them so they don't get rejected as malformed.
var followupDoneRE = regexp.MustCompile(`^\s*-\s+\[[xX]\]`)

// followupUncheckedShapeRE detects any line whose shape suggests an
// unchecked follow-up, malformed or not. The pop-the-editor gate uses
// this as its trigger — anything that looks like an open item is worth
// the operator's review, even if parseFollowups would later reject it.
// Validation is still parseFollowups's job; this is intentionally a
// strictly looser predicate.
var followupUncheckedShapeRE = regexp.MustCompile(`^\s*-\s+\[\s\]`)

// parsedFollowup is one harvest candidate plucked from followups.md.
type parsedFollowup struct {
	lineIdx int    // zero-based index into the raw line slice
	rawSlug string // exactly as on the line ("claudia/foo" or "foo") — mark-rewrite + dedup key
	project string // resolved target project: explicit prefix, else current project
	slug    string // bare base slug handed to createIdea ("foo", pre-disambiguation)
	title   string // title to embed in the new idea's H1
	body    string // optional dedented body markdown; "" means no body
}

// parseFollowups scans body and returns the lines (split on '\n') plus
// the unchecked entries to harvest. Validation is upfront and total:
// any malformed `- [ ]` line, any duplicated slug, and any missing
// title is reported with a 1-based line number, and harvest does NOT
// proceed. That keeps the partial-failure path bounded — once we start
// creating ideas, every remaining input line has already passed.
//
// Body capture: lines indented two-or-more spaces, plus blank lines
// inside an item, belong to the most recent open item's body. The
// body is dedented two spaces, leading/trailing blanks trimmed, and
// joined with '\n'. Bodies under checked (`[x]`) items are recognised
// (so they don't attach to a prior open item) but discarded — the
// idea has already been created on a past run.
//
// Slug routing: a slug may carry an optional `<project>/` prefix. The
// prefix names the target project the harvested idea lands in; a bare
// slug routes to currentProject (the run being closed). The raw slug
// (prefix and all) is kept verbatim for the audit-line rewrite and the
// dedup key, so `claudia/foo` and `westworld/foo` are distinct entries.
// Target-project existence is validated by the harvest caller, which
// has root in scope — see harvestFollowups.
func parseFollowups(body []byte, currentProject string) (lines []string, todo []parsedFollowup, err error) {
	lines, entries, err := parseChecklist(body, "follow-up", "follow-up")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		project, slug := currentProject, e.slug
		if i := strings.IndexByte(e.slug, '/'); i >= 0 {
			project, slug = e.slug[:i], e.slug[i+1:]
		}
		todo = append(todo, parsedFollowup{
			lineIdx: e.lineIdx,
			rawSlug: e.slug,
			project: project,
			slug:    slug,
			title:   e.title,
			body:    e.body,
		})
	}
	return lines, todo, nil
}

// isIndentedBody reports whether line qualifies as body content for the
// most recent item — two or more leading spaces. Tabs do not count;
// followups.md is editor-flavor markdown and the agent prompt teaches
// the two-space form explicitly.
func isIndentedBody(line string) bool {
	return len(line) >= 2 && line[0] == ' ' && line[1] == ' '
}

// trimAndDedentBody turns the raw body lines collected for one item
// into the dedented markdown that rides into the idea's seed canvas.
// Strips leading and trailing blank lines (the operator's blank-line
// gap between title and first paragraph isn't body content) and dedents
// every non-blank line by exactly two spaces (the bullet's content
// column). Returns "" when there's nothing to keep.
func trimAndDedentBody(body []string) string {
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}
	for len(body) > 0 && strings.TrimSpace(body[len(body)-1]) == "" {
		body = body[:len(body)-1]
	}
	if len(body) == 0 {
		return ""
	}
	out := make([]string, len(body))
	for i, line := range body {
		if line == "" {
			out[i] = ""
			continue
		}
		// isIndentedBody guaranteed ≥ 2 leading spaces for non-blanks
		// that reached this slice.
		out[i] = line[2:]
	}
	return strings.Join(out, "\n")
}

// markHarvested rewrites the `- [ ] `slug“ prefix of a harvested line
// into `- [x] `resolvedSlug“, canonicalising the box to the single-space
// form. It re-runs followupOpenRE — the same regex parseChecklist used
// to accept the line — and splices, so any spacing the parser tolerated
// (extra spaces around the dash or box, a tab inside the box) is marked
// by construction rather than left visibly unchecked. Parse and mark
// share one grammar. The leading indent and everything after the slug's
// closing backtick (the ` — Title` tail) survive verbatim.
//
// ok is false when the line doesn't match, or matches a *different* slug
// than baseSlug (an index-mapping guard). With this shared-regex rewrite
// in place, !ok is unreachable in normal operation — the identical line
// parsed moments earlier and only *other* indices get mutated in
// between — so the caller treats it as a loud invariant failure, not a
// recovery path.
func markHarvested(line, baseSlug, resolvedSlug string) (string, bool) {
	loc := followupOpenRE.FindStringSubmatchIndex(line)
	if loc == nil {
		return "", false
	}
	if line[loc[4]:loc[5]] != baseSlug {
		return "", false
	}
	indentEnd := 0
	for indentEnd < len(line) && (line[indentEnd] == ' ' || line[indentEnd] == '\t') {
		indentEnd++
	}
	indent := line[:indentEnd]
	tail := line[loc[5]+1:] // everything after the slug's closing backtick
	return indent + "- [x] `" + resolvedSlug + "`" + tail, true
}

// harvestFollowups runs the harvest loop for one run, called from
// runClose for non-idea workflows. Caller holds the bureaucracy lock.
//
// Steps, on the happy path:
//  1. If !skipEdit and the on-disk file has at least one unchecked
//     entry, open followups.md in $EDITOR for operator review. An
//     absent / header-only / all-`[x]` file skips the pop entirely —
//     there's nothing to review and nothing to fan out.
//  2. Read and parse the file. Empty/absent file is a no-op.
//  3. For each unchecked entry, call createIdea (which writes its own
//     open-run commit). Track the resolved slug so the line can be
//     rewritten as `- [x]`.
//  4. After every entry succeeds, write the updated followups.md to
//     disk. The caller stages it alongside run.json on the close commit.
//
// Partial failure (a createIdea call mid-batch returns non-nil): the
// harvest commits a "harvest progress" record of the lines that did
// succeed, then returns the original error. The run stays in_progress;
// requireCleanTree on the next attempt is satisfied because the
// progress commit took the dirty file with it.
func harvestFollowups(root, projectID, runID, workflow string, skipEdit bool) error {
	relPath := run.FollowupsPath(projectID, runID)
	openTrailers := trailers.Block{FromRun: projectID + "/" + runID}

	spec := scratchHarvestSpec[parsedFollowup]{
		relPath:         relPath,
		header:          followupsHeader,
		progressSubject: fmt.Sprintf("harvest: capture follow-ups for %s/%s", projectID, runID),
		writeErrPrefix:  "create idea",
		parse: func(body []byte) ([]string, []scratchItem[parsedFollowup], error) {
			lines, todo, err := parseFollowups(body, projectID)
			if err != nil {
				return nil, nil, err
			}
			// Validation is upfront and total (the harvest's contract):
			// reject an unknown target project before any idea is
			// created, so a typo'd prefix (`claduia/…`) costs the
			// operator one edit, not a half-finished batch. Each distinct
			// project is checked once, in line order, so the error names
			// the first offending line.
			checked := map[string]bool{}
			for _, fu := range todo {
				if checked[fu.project] {
					continue
				}
				if err := requireProject(root, fu.project); err != nil {
					return nil, nil, fmt.Errorf("line %d: %w", fu.lineIdx+1, err)
				}
				checked[fu.project] = true
			}
			items := make([]scratchItem[parsedFollowup], 0, len(todo))
			for _, fu := range todo {
				// slug carries the raw (prefixed) form so markHarvested
				// matches the `- [ ] ` + "`claudia/foo`" line to rewrite.
				items = append(items, scratchItem[parsedFollowup]{lineIdx: fu.lineIdx, slug: fu.rawSlug, entry: fu})
			}
			return lines, items, nil
		},
		write: func(fu parsedFollowup) (string, error) {
			// followups.md preserves the prose title — render it as the H1
			// so the harvested idea reads as the operator wrote it, even
			// though the slug is what the namespace keys off.
			canvasBody := fmt.Sprintf("# %s\n", fu.title)
			if fu.body != "" {
				canvasBody = fmt.Sprintf("# %s\n\n%s\n", fu.title, fu.body)
			}
			// Route to the entry's target project (its prefix, else the
			// current project). openTrailers' MoE-From-Run stays pointed
			// at the source run — that's where the followup was captured.
			md, err := createIdea(root, fu.project, fu.slug, canvasBody, openTrailers)
			if err != nil {
				return "", err
			}
			// Preserve the prefix shape in the audit line so it records
			// where the idea actually landed (post-disambiguation): a
			// prefixed entry rewrites to `project/resolved-id`, a bare
			// one to just the resolved id.
			if strings.Contains(fu.rawSlug, "/") {
				return md.Project + "/" + md.ID, nil
			}
			return md.ID, nil
		},
	}
	return harvestScratchTyped(root, projectID, runID, workflow, skipEdit, spec)
}

// hasUncheckedEntry reports whether absPath contains any line whose
// shape matches an open follow-up (`- [ ] ...`). It's a deliberately
// loose check: malformed open lines (missing slug, wrong dash) also
// trigger a true so the operator gets the chance to fix them in
// $EDITOR rather than hit a parse error after the pop was skipped.
//
// Absent file (the common case at close time when the agent appended
// nothing) and read errors below the IsNotExist line both return
// false: there's no review to do, so the harvest can fall through to
// the parser's empty-file no-op path.
func hasUncheckedEntry(absPath string) bool {
	body, err := os.ReadFile(absPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if followupUncheckedShapeRE.MatchString(line) {
			return true
		}
	}
	return false
}

// injectEditorPopHeader prepends headerText to absPath if the file
// doesn't already start with an HTML comment. The header explains the
// editor pop's gesture (save-to-promote, delete-to-skip) in-place so
// the operator doesn't have to remember context across stage outputs.
//
// Idempotent on the no-op-when-present check: if the operator removed
// the comment mid-edit, the next pop on the next close will re-inject
// — useful when months go by between encounters with a rarely-touched
// bucket. Absent file is also a no-op; the caller's
// hasUncheckedEntry gate keeps us off the inject path when there's
// nothing to review.
func injectEditorPopHeader(absPath, headerText string) error {
	body, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", absPath, err)
	}
	// Skip leading blank lines when looking for an existing comment —
	// the operator might have added a blank line above a previous
	// header without realising.
	trimmed := strings.TrimLeft(string(body), "\n")
	if strings.HasPrefix(trimmed, "<!--") {
		return nil
	}
	out := headerText
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if len(body) > 0 {
		out += "\n"
		out += string(body)
	}
	return os.WriteFile(absPath, []byte(out), 0o644)
}

// launchEditorOrFail mirrors launchEditor but returns an error rather
// than printing-and-exiting, so the caller can wrap context. Editor
// gating ($EDITOR/$VISUAL) is enforced upstream by the close handler.
func launchEditorOrFail(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return fmt.Errorf("no $EDITOR or $VISUAL set; pass --no-edit to skip the editor step")
	}
	// $1 (not string interp) keeps paths with spaces/quotes/`;` shell-safe — don't collapse.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor exited: %w", err)
	}
	return nil
}

// dirtyOutsidePaths returns true if the working tree has uncommitted
// changes anywhere except the named paths (relative to root). The
// close handler uses this to tolerate local edits to the harvest
// scratch files — followups.md and feedback/lore.md — while still
// refusing on anything else dirty.
func dirtyOutsidePaths(root string, exceptRels ...string) (bool, error) {
	entries, err := git.Status(root)
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	allowed := make(map[string]struct{}, len(exceptRels))
	for _, p := range exceptRels {
		allowed[p] = struct{}{}
	}
	for _, e := range entries {
		if _, ok := allowed[e.Path]; ok {
			continue
		}
		return true, nil
	}
	return false, nil
}
