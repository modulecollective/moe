package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//	- [x] `chase-zlib-upgrade` — Chase the zlib upgrade Q1 mentioned
//
// Each unchecked entry becomes one idea; the line is rewritten in
// place as `- [x]` carrying the resolved (possibly auto-disambiguated)
// slug. Already-checked lines are pass-through, which makes a retry
// after a partial-failure idempotent.

// followupHeader is the stub a fresh editor session lands on when no
// followups.md exists yet. Just a header — the operator (or agent) adds
// list items below it.
const followupHeader = "# Follow-ups\n\n"

// followupOpenRE matches an unchecked entry. Captures the indent + box
// prefix (group 1), the slug (group 2), and the title (group 3 — may
// be empty after trim, in which case parseFollowups raises a more
// specific "title is empty" error). The em-dash separator (U+2014) is
// the design's required form; a hyphen or `--` would be ambiguous with
// the leading list marker.
var followupOpenRE = regexp.MustCompile("^(\\s*-\\s+\\[\\s\\]\\s+)`([a-z0-9][a-z0-9-]*)`\\s+—\\s*(.*?)\\s*$")

// followupCheckboxRE detects any list item that looks like a checkbox,
// open or done. Used to flag malformed unchecked entries that don't
// satisfy followupOpenRE — silently skipping those would lose the
// operator's intent.
var followupCheckboxRE = regexp.MustCompile(`^\s*-\s+\[[ xX]\]`)

// followupDoneRE matches an already-checked entry. We don't validate
// these (they are the audit trail of past close runs); we just need to
// recognise them so they don't get rejected as malformed.
var followupDoneRE = regexp.MustCompile(`^\s*-\s+\[[xX]\]`)

// parsedFollowup is one harvest candidate plucked from followups.md.
type parsedFollowup struct {
	lineIdx int    // zero-based index into the raw line slice
	slug    string // operator-supplied base slug (pre-disambiguation)
	title   string // title to embed in the new idea's H1
}

// parseFollowups scans body and returns the lines (split on '\n') plus
// the unchecked entries to harvest. Validation is upfront and total:
// any malformed `- [ ]` line, any duplicated slug, and any missing
// title is reported with a 1-based line number, and harvest does NOT
// proceed. That keeps the partial-failure path bounded — once we start
// creating ideas, every remaining input line has already passed.
func parseFollowups(body []byte) (lines []string, todo []parsedFollowup, err error) {
	lines = strings.Split(string(body), "\n")
	seen := map[string]int{}
	for i, line := range lines {
		if !followupCheckboxRE.MatchString(line) {
			continue
		}
		if followupDoneRE.MatchString(line) {
			continue
		}
		m := followupOpenRE.FindStringSubmatch(line)
		if m == nil {
			return nil, nil, fmt.Errorf("line %d: malformed follow-up %q (expected: - [ ] `slug` — Title)", i+1, line)
		}
		slug := m[2]
		title := strings.TrimSpace(m[3])
		if title == "" {
			return nil, nil, fmt.Errorf("line %d: follow-up title is empty", i+1)
		}
		if prev, dup := seen[slug]; dup {
			return nil, nil, fmt.Errorf("line %d: follow-up slug %q duplicates line %d", i+1, slug, prev+1)
		}
		seen[slug] = i
		todo = append(todo, parsedFollowup{lineIdx: i, slug: slug, title: title})
	}
	return lines, todo, nil
}

// markHarvested rewrites a `- [ ] `slug“ prefix into `- [x] `resolved“
// in place. Preserves indentation and the rest of the line so the title
// survives unchanged.
func markHarvested(line, baseSlug, resolvedSlug string) string {
	old := "- [ ] `" + baseSlug + "`"
	new := "- [x] `" + resolvedSlug + "`"
	return strings.Replace(line, old, new, 1)
}

// harvestFollowups runs the harvest loop for one run, called from
// runClose for non-idea workflows. Caller holds the bureaucracy lock.
//
// Steps, on the happy path:
//  1. If !skipEdit, scaffold and open followups.md in $EDITOR.
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
	absPath := filepath.Join(root, relPath)

	if !skipEdit {
		if err := scaffoldFollowups(absPath); err != nil {
			return fmt.Errorf("scaffold followups.md: %w", err)
		}
		if err := launchEditorOrFail(absPath); err != nil {
			return err
		}
	}

	body, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", relPath, err)
	}

	lines, todo, err := parseFollowups(body)
	if err != nil {
		return fmt.Errorf("%s: %w", relPath, err)
	}
	if len(todo) == 0 {
		return nil
	}

	openTrailers := trailers.Block{FromRun: projectID + "/" + runID}

	for hi, fu := range todo {
		md, ierr := createIdea(root, projectID, fu.slug, fu.title, "", openTrailers)
		if ierr != nil {
			// If we already harvested some entries, persist their
			// `- [x]` rewrites as a standalone bookkeeping commit so
			// the retry-after-fix skips them. With zero progress the
			// followups.md is unchanged on disk and there is nothing
			// to record beyond the failure itself.
			if hi > 0 {
				if perr := commitHarvestProgress(root, projectID, runID, workflow, relPath, lines); perr != nil {
					return fmt.Errorf("create idea %s (then progress commit failed: %v): %w", fu.slug, perr, ierr)
				}
			}
			return fmt.Errorf("create idea %s (after harvesting %d): %w", fu.slug, hi, ierr)
		}
		lines[fu.lineIdx] = markHarvested(lines[fu.lineIdx], fu.slug, md.ID)
	}

	if err := os.WriteFile(absPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	return nil
}

// scaffoldFollowups creates absPath with the minimal `# Follow-ups\n\n`
// header if no file exists yet, so the operator drops into a known
// shape rather than an empty buffer. A no-op when the file is already
// there — the agent (or a previous close attempt) owns its content.
func scaffoldFollowups(absPath string) error {
	if _, err := os.Stat(absPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absPath, []byte(followupHeader), 0o644)
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

// commitHarvestProgress lands lines (the partially-harvested file) as
// a standalone bookkeeping commit, so the working tree is clean again
// for the operator's retry. Subject and trailers mirror the close
// commit but call out the abort: the run is still in_progress, so the
// `Close ... ` shape would be misleading.
func commitHarvestProgress(root, projectID, runID, workflow, relPath string, lines []string) error {
	if err := os.WriteFile(filepath.Join(root, relPath), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return fmt.Errorf("write progress %s: %w", relPath, err)
	}
	subject := fmt.Sprintf("harvest: capture follow-ups for %s/%s", projectID, runID)
	msg := subject + "\n\n" +
		trailers.Block{
			Run:      runID,
			Project:  projectID,
			Workflow: workflow,
		}.String()
	return run.StageAndCommit(root, msg, relPath)
}

// dirtyOutsidePath returns true if the working tree has uncommitted
// changes anywhere except exceptRel (relative to root). The harvester
// allows an operator's local edits to followups.md to ride along on
// the close commit, but anything else dirty in the tree is still a
// guardrail violation.
func dirtyOutsidePath(root, exceptRel string) (bool, error) {
	entries, err := git.Status(root)
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	for _, e := range entries {
		if e.Path == exceptRel {
			continue
		}
		return true, nil
	}
	return false, nil
}
