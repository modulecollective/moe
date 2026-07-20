package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// feedback/lore.md is the run-scoped scratch file harvested at close
// into lore/<slug>.md at the bureaucracy root. Format mirrors
// followups.md exactly, by design — same parser shape, same operator
// gesture (delete-to-skip, leave-to-promote), same idempotency story
// — so the second close-time editor pop is legible without re-learning
// the shape. The only schema difference: each entry's indented body's
// first paragraph is conventionally an `applies-when:` line; that
// line is lifted into the promoted file's YAML frontmatter. An
// optional `supersedes:` paragraph names existing lore entries this
// promotion replaces; the remaining paragraphs become entry prose.
//
//	- [ ] `compose-tailscale-binds` — Reaching compose ports from the laptop
//
//	  applies-when: project uses docker-compose on a fly-box reached via
//	  tailscale, with no fly.toml services
//
//	  supersedes: compose-localhost-proxy, compose-localhost-proxy-2
//
//	  Under userspace tailscale on fly with no `fly.toml` services, …
//
// Promoted lore lands as lore/<resolved-slug>.md (ordinary collisions
// auto-disambiguate with -2, -3, …; supersedes may amend in place) and
// the checklist line is rewritten to `- [x] \`resolved-slug\`` — same
// audit-trail shape followups uses.

// loreHeader is the editor-pop banner auto-injected onto
// feedback/lore.md before $EDITOR opens. Constant lives next to its
// only caller so the message and the file stay in sync; injection is
// idempotent (see injectEditorPopHeader).
const loreHeader = `<!--
feedback/lore.md — portable, cross-project facts captured this run.
Save this file to promote each unchecked ` + "`- [ ]`" + ` entry into
lore/<slug>.md at the bureaucracy root. Delete the line to skip.
Use an indented ` + "`supersedes:`" + ` paragraph to replace or merge
existing lore entries; the replacement is written before they are deleted.
Lines marked ` + "`- [x]`" + ` are already promoted; leave them alone.
This header is auto-injected on editor pop; remove it freely.
-->`

// parsedLore is one harvest candidate plucked from feedback/lore.md.
type parsedLore struct {
	lineIdx     int    // zero-based index into the raw line slice
	slug        string // operator-supplied base slug (pre-disambiguation)
	title       string // title written into the promoted file's frontmatter + H1
	appliesWhen string // value lifted from the body's first `applies-when:` paragraph
	supersedes  []string
	body        string // dedented prose left over after the metadata paragraphs
}

var loreSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// lorePartialProgressError marks a promotion failure that happened
// after the replacement was written. The shared scratch harvester uses
// the type to checkpoint retry state without committing ordinary
// pre-write failures.
type lorePartialProgressError struct {
	err error
}

func (e *lorePartialProgressError) Error() string { return e.err.Error() }
func (e *lorePartialProgressError) Unwrap() error { return e.err }

func loreWriteErrorLeavesProgress(err error) bool {
	var partial *lorePartialProgressError
	return errors.As(err, &partial)
}

// parseLore scans body and returns the lines (split on '\n') plus the
// unchecked entries to harvest. Reuses the followups parser primitives
// (the checkbox regexes, the body dedent helper) so the only
// lore-specific work is splitting the dedented body into (applies-when,
// prose). Validation is upfront and total, same contract as
// parseFollowups: a malformed `- [ ]` line, a duplicate slug, or an
// empty title aborts the harvest with a 1-based line number.
func parseLore(body []byte) (lines []string, todo []parsedLore, err error) {
	lines, entries, err := parseChecklist(body, "lore entry", "lore")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.promoteTo != "" {
			return nil, nil, fmt.Errorf("line %d: lore entry must not carry a workflow tag", e.lineIdx+1)
		}
		// parseChecklist's shared regex permits a `<project>/` prefix for
		// followups routing; a lore slug is a bare filename
		// (lore/<slug>.md), so a `/` would write into a subdir. Reject it
		// here rather than fork a lore-specific parser.
		if strings.Contains(e.slug, "/") {
			return nil, nil, fmt.Errorf("line %d: lore slug must not contain '/' (lore is global; no project to route to)", e.lineIdx+1)
		}
		appliesWhen, rest := splitAppliesWhen(e.body)
		supersedes, prose, err := splitSupersedes(rest)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", e.lineIdx+1, err)
		}
		todo = append(todo, parsedLore{
			lineIdx:     e.lineIdx,
			slug:        e.slug,
			title:       e.title,
			appliesWhen: appliesWhen,
			supersedes:  supersedes,
			body:        prose,
		})
	}
	return lines, todo, nil
}

// splitAppliesWhen consumes an `applies-when:` paragraph at the head
// of body and returns (value, rest). The paragraph may span multiple
// lines — they're joined with single spaces — and ends at the first
// blank line or end-of-body. If body doesn't start with
// `applies-when:` (after optional leading blanks), returns ("", body)
// and the harvester falls back to "(missing)" in the promoted file's
// frontmatter. Fail-soft is deliberate: a forgetful agent shouldn't
// abort the entire close, and the in-prompt catalog renders
// "(missing)" the same way so the operator sees and fixes it.
func splitAppliesWhen(body string) (appliesWhen, rest string) {
	appliesWhen, rest, ok := splitLoreParagraph(body, "applies-when:")
	if !ok {
		return "", body
	}
	return appliesWhen, rest
}

// splitSupersedes consumes a leading `supersedes:` paragraph and
// validates its comma-separated lore slugs. Wrapped lines are joined
// with spaces exactly like applies-when. A missing paragraph is a
// no-op; a present malformed one aborts harvest before any files move.
func splitSupersedes(body string) (supersedes []string, rest string, err error) {
	value, rest, ok := splitLoreParagraph(body, "supersedes:")
	if !ok {
		return nil, body, nil
	}
	if value == "" {
		return nil, "", fmt.Errorf("supersedes list is empty")
	}
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		slug := strings.TrimSpace(raw)
		if slug == "" || !loreSlugRE.MatchString(slug) {
			return nil, "", fmt.Errorf("invalid supersedes slug %q", slug)
		}
		if seen[slug] {
			return nil, "", fmt.Errorf("duplicate supersedes slug %q", slug)
		}
		seen[slug] = true
		supersedes = append(supersedes, slug)
	}
	return supersedes, rest, nil
}

func splitLoreParagraph(body, prefix string) (value, rest string, ok bool) {
	if body == "" {
		return "", "", false
	}
	bodyLines := strings.Split(body, "\n")
	i := 0
	for i < len(bodyLines) && bodyLines[i] == "" {
		i++
	}
	if i >= len(bodyLines) {
		return "", "", false
	}
	if !strings.HasPrefix(strings.TrimSpace(bodyLines[i]), prefix) {
		return "", body, false
	}
	end := i + 1
	for end < len(bodyLines) && bodyLines[end] != "" {
		end++
	}
	joined := strings.Join(bodyLines[i:end], " ")
	joined = strings.TrimSpace(joined)
	joined = strings.TrimPrefix(joined, prefix)
	value = strings.TrimSpace(joined)

	tail := bodyLines[end:]
	for len(tail) > 0 && tail[0] == "" {
		tail = tail[1:]
	}
	for len(tail) > 0 && tail[len(tail)-1] == "" {
		tail = tail[:len(tail)-1]
	}
	rest = strings.Join(tail, "\n")
	return value, rest, true
}

// renderLoreFile assembles the lore/<slug>.md body from one parsed
// entry. Output shape is fixed by design: a YAML frontmatter carrying
// title / applies-when / discovered-in, then a markdown H1, then
// (optionally) the prose body. Missing applies-when renders as
// "(missing)" so the wiki index reads it the same way it reads
// half-written hand-authored entries.
func renderLoreFile(title, appliesWhen, discoveredIn, prose string) string {
	if appliesWhen == "" {
		appliesWhen = "(missing)"
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "applies-when: %s\n", appliesWhen)
	fmt.Fprintf(&b, "discovered-in: %s\n", discoveredIn)
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n", title)
	if prose != "" {
		b.WriteString("\n")
		b.WriteString(prose)
		if !strings.HasSuffix(prose, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// promoteLoreEntry writes one parsed entry to lore/<resolved-slug>.md.
// On an ordinary slug collision walks -2, -3, … until a free slot is
// found. A supersede may reuse an existing destination when it names
// that slug (an in-place amendment), or when an identical replacement
// is already present from a partial prior attempt. The replacement is
// always written before superseded files are deleted, leaving visible
// junk rather than invisible absence if the operation is interrupted.
func promoteLoreEntry(root, projectID, runID string, p parsedLore) (string, error) {
	loreDir := wiki.LoreDir(root)
	if err := os.MkdirAll(loreDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", loreDir, err)
	}
	discoveredIn := fmt.Sprintf("%s/runs/%s", projectID, runID)
	body := renderLoreFile(p.title, p.appliesWhen, discoveredIn, p.body)
	slug := p.slug
	for n := 2; ; n++ {
		abs := filepath.Join(loreDir, slug+".md")
		_, statErr := os.Stat(abs)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", fmt.Errorf("stat %s: %w", abs, statErr)
		}
		if stringSliceContains(p.supersedes, slug) {
			break
		}
		if len(p.supersedes) > 0 {
			existing, readErr := os.ReadFile(abs)
			if readErr != nil {
				return "", fmt.Errorf("read %s: %w", abs, readErr)
			}
			if string(existing) == body {
				break
			}
		}
		slug = fmt.Sprintf("%s-%d", p.slug, n)
	}
	abs := filepath.Join(loreDir, slug+".md")
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", abs, err)
	}
	for _, oldSlug := range p.supersedes {
		if oldSlug == slug {
			continue
		}
		oldPath := filepath.Join(loreDir, oldSlug+".md")
		if err := os.Remove(oldPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", &lorePartialProgressError{
				err: fmt.Errorf("delete superseded lore %s: %w", oldPath, err),
			}
		}
	}
	return slug, nil
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// harvestLore is the lore counterpart of harvestFollowups. Same flow,
// same idempotency story — the only differences are the file written
// per entry (lore/<slug>.md instead of an idea run) and the slug
// disambiguation living inline (lore is just files on disk, no
// project to route through). Caller holds the bureaucracy lock.
//
// Steps, on the happy path:
//  1. If !skipEdit and the on-disk file has at least one unchecked
//     entry, inject the editor-pop header and open feedback/lore.md
//     in $EDITOR for operator review. An absent / header-only /
//     all-`[x]` file skips the pop — nothing to review.
//  2. Read and parse the file. Empty/absent file is a no-op.
//  3. For each unchecked entry, write lore/<resolved-slug>.md and
//     rewrite the line to `- [x]`.
//  4. After every entry succeeds, write the updated feedback/lore.md
//     to disk. The caller stages it alongside run.json on the close
//     commit.
//
// Partial failure (a promoteLoreEntry call mid-batch returns
// non-nil): commit a "lore harvest progress" record of the lines that
// did succeed, then return the original error. The run stays
// in_progress; requireCleanTree on the next attempt is satisfied
// because the progress commit took the dirty file with it.
func harvestLore(root, projectID, runID, workflow string, skipEdit bool) error {
	relPath := run.FeedbackPath(projectID, runID, "lore")
	spec := scratchHarvestSpec[parsedLore]{
		relPath:                  relPath,
		header:                   loreHeader,
		progressSubject:          fmt.Sprintf("harvest: capture lore for %s/%s", projectID, runID),
		progressPaths:            []string{wiki.LoreDirRel},
		writeErrorLeavesProgress: loreWriteErrorLeavesProgress,
		writeErrPrefix:           "promote lore",
		parse: func(body []byte) ([]string, []scratchItem[parsedLore], error) {
			lines, todo, err := parseLore(body)
			if err != nil {
				return nil, nil, err
			}
			items := make([]scratchItem[parsedLore], 0, len(todo))
			for _, p := range todo {
				items = append(items, scratchItem[parsedLore]{lineIdx: p.lineIdx, slug: p.slug, entry: p})
			}
			return lines, items, nil
		},
		write: func(p parsedLore) (string, error) {
			return promoteLoreEntry(root, projectID, runID, p)
		},
	}
	return harvestScratchTyped(root, projectID, runID, workflow, skipEdit, spec)
}
