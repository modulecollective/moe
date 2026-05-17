package wiki

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
)

// firstReflectCommitCap caps the verbatim commit list rendered into
// the events block when there's no prior checkpoint SHA. Large
// projects can have thousands of commits at first reflect; the cap
// keeps the kickoff prompt bounded. Beyond the cap, an
// "(N earlier commits omitted)" footer makes the truncation visible
// to the agent so the seeded history-summary.md can call out that
// older history is in git, not in this prompt. The SHA..HEAD branch
// stays uncapped — its window is already bounded by reflect cadence.
//
// Var rather than const so the cap test can shrink it; production
// value is 500. Same pattern as git.go's writeRetryCap.
var firstReflectCommitCap = 500

// ReflectPromptSection is the wiki-specific block appended to the
// system prompt for a closed-schema reflect session. Sibling of
// IngestPromptSection / ClaimPromptSection: same preamble, different
// framing — walk each managed doc against recent events and propose
// updates, fold the roadmap forward, and clean up structural findings
// before sealing the pass.
//
// Closed-schema only. Open-schema reflect (whether kb wants one) is
// undecided; the seam exists, the implementation doesn't, so this
// errors loudly on Open rather than silently inheriting kb framing.
func ReflectPromptSection(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", fmt.Errorf("wiki: reflect is closed-schema only (got %s)", cfg.Mode)
	}
	var b strings.Builder
	b.WriteString(wikiPreamble(cfg))
	b.WriteString(`Reflect pass (closed-schema):

Walk each managed doc against the events block. For each, decide:
did anything happen that should change this doc? Propose updates
with the operator before writing them. Apply fixes inline once
agreed. Skim docs that look untouched and don't manufacture work —
a quiet section is fine.

Twin docs are written for a reader who doesn't already know the
project. Primer-plus-reference, not changelog. Durable rules,
terse prose, examples used in service of the rule. When a feature
was tried and removed, the rule it taught stays (YAGNI in this
corner of the system, surface area earned scrutiny, this kind of
speculation didn't pay off); the section about the removed
feature does not. Extract the rule, drop the example. Compress
over preserve. ` + "`history-summary.md`" + ` keeps the chronological
narrative; finalize folds events in there.

Single-home discipline: each rule, principle, or named shape has
one home in the twin. If a rule already lives in another managed
doc, point there (by section heading) instead of restating it.
Architecture owns shape and boundaries; patterns owns named
recurring or refused shapes; operations owns rituals and tools;
roadmap owns intent over time. A line that could plausibly live
in two docs lives in one — pick by which doc the reader would
search first.

Vision is asymmetric — but split into two registers.
**Reference drift** (terminology, examples, names of tools or
people, broken pointers, stale lists of "currently we use X")
can be fixed in place when drift has ≥2 sightings in recent
events and the edit *tightens* an existing statement rather
than reversing one. **Intent drift** (stated bets, non-goals,
problem statement, scope) stays surface-only — flag it; the
operator runs ` + "`moe twin claim`" + ` if they agree. If you can't tell
which register a drift belongs to, flag for the operator
instead of editing.

Roadmap convention: roadmap.md uses five ` + "`##`" + ` sections — Near
term, Mid term, Long term, Directions, Parked. Near/Mid/Long are
committed intent across horizons; Directions holds uncommitted
"places the project could plausibly grow" (recorded so reflect
conversations can promote them as appetite shifts); Parked is
items the project deliberately is not doing. On a fresh
roadmap.md (just ` + "`# Roadmap`" + ` and nothing else), establish the
five headings at this pass. On subsequent passes, walk the prior
content with the operator and promote / demote / retire entries
against the idea backlog and recent activity.

Glossary convention: glossary.md is a single alphabetical list of
project-specific terms. Each entry is a ` + "`### Term`" + ` heading
followed by 1–3 sentences that compress the definition and point back
to the home doc by section heading (never line number). The
definition lives in the home doc; the glossary is the index.

Inclusion bar — a term earns a glossary entry when **either** it
appears load-bearing in 2+ twin docs, **or** it names a code seam the
twin discusses (a package, command, struct, or commit trailer). A
term that appears once in one doc stays in that doc. Generic nouns
("the agent", "the operator") do not earn entries. Acronyms follow
the same rule: project-specific short forms (` + "`MoE`" + `, ` + "`kb`" + `,
` + "`sdlc`" + `) belong; general programming acronyms (` + "`PR`" + `,
` + "`SHA`" + `, ` + "`CLI`" + `) do not. When synonyms drift across docs,
normalize the prose to the glossary form during the pass.

Hygiene findings (orphans, broken cross-links, empty docs) are
pre-scanned and surfaced in your kickoff. Walk them before the
doc-by-doc pass so structural issues inform the synthesis. Apply
fixes inline as you and the operator agree on them. Anything you
can't auto-fix gets walked with the operator; if a finding genuinely
can't be resolved this pass, capture it as a followup and remove
the structural cause (e.g. retire the orphaned doc) before the pass
closes — the engine re-scans at session-end and refuses to seal a
reflect with leftover findings.

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

// EventsSinceCheckpoint summarises the project-side commits and
// closed bureaucracy runs that landed since the last reflect pass.
// Closed-schema only — open-schema wikis have a different rhythm and
// no decided-update concept worth surfacing.
//
// Returns a markdown block suitable for splicing into the kickoff
// prompt under a "## Events since last reflect" heading. The block
// is empty (returns "") when there's nothing since the checkpoint —
// a freshly-reflected wiki has no events to walk against.
//
// No truncation: the rolling history-summary.md absorbs old history,
// and this block is the verbatim tail since SHA-prev. On first reflect
// (no checkpoint) the tail is the full project history.
func EventsSinceCheckpoint(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", fmt.Errorf("wiki: events block is closed-schema only (got %s)", cfg.Mode)
	}

	cp, ok, err := ReadCheckpoint(cfg.ContentDir)
	if err != nil {
		return "", err
	}

	commits, omitted, err := projectCommitsSince(cfg, cp, ok)
	if err != nil {
		return "", err
	}
	runs, err := closedRunsSince(cfg, cp, ok)
	if err != nil {
		return "", err
	}

	if len(commits) == 0 && len(runs) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("## Events since last reflect\n\n")
	if !ok {
		b.WriteString("No prior checkpoint — this is the twin's first reflect pass. " +
			"Listing the project commit history and every closed run; on very " +
			"large projects the commit list is capped — see the footer if any. " +
			"The agent will seed history-summary.md from this pass.\n\n")
	}

	if len(commits) > 0 {
		b.WriteString("**Project commits**")
		if cp.ProjectRepoSHA != nil && *cp.ProjectRepoSHA != "" {
			fmt.Fprintf(&b, " since %s", git.ShortSHA(*cp.ProjectRepoSHA))
		}
		b.WriteString(":\n")
		for _, c := range commits {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		if omitted > 0 {
			fmt.Fprintf(&b, "- _(%d earlier commits omitted)_\n", omitted)
		}
		b.WriteString("\n")
	}

	if len(runs) > 0 {
		b.WriteString("**Closed bureaucracy runs**")
		if ok && cp.LastIngestAt != "" {
			fmt.Fprintf(&b, " since %s", cp.LastIngestAt)
		}
		b.WriteString(":\n")
		for _, r := range runs {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// projectCommitsSince returns the project-repo commit lines since the
// reflect checkpoint, plus an omitted-count for the first-reflect cap.
// The omitted count is always 0 on the SHA..HEAD branch and on the
// no-cap-fired first-reflect case; it's positive only when the cap
// fired and we successfully read the total commit count.
func projectCommitsSince(cfg Config, cp Checkpoint, hasCheckpoint bool) ([]string, int, error) {
	if cfg.ProjectRepoPath == "" {
		return nil, 0, nil
	}
	if _, err := os.Stat(cfg.ProjectRepoPath); err != nil {
		// NotExist is legitimate — a twin can run before its project
		// repo exists. Any other stat error (permission, I/O, broken
		// symlink) is real and should surface.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("wiki: stat project repo %s: %w", cfg.ProjectRepoPath, err)
	}
	incremental := hasCheckpoint && cp.ProjectRepoSHA != nil && *cp.ProjectRepoSHA != ""
	args := []string{"log", "--no-merges", "--format=%h %s"}
	if incremental {
		args = append(args, fmt.Sprintf("%s..HEAD", *cp.ProjectRepoSHA))
	} else {
		// First reflect: read at most cap+1 rows so we can detect
		// whether the cap fired without paying for a full traversal.
		args = append(args, fmt.Sprintf("-n%d", firstReflectCommitCap+1))
	}
	out, err := git.Output(cfg.ProjectRepoPath, args...)
	if err != nil {
		// Both branches propagate. On incremental, a failure usually
		// means the checkpoint SHA is unreachable (history rewrite,
		// shallow clone) — surfacing the git error lets the operator
		// reset the checkpoint or deepen the clone rather than walk
		// docs against an empty events block. On first reflect, a
		// failure on a healthy repo with no SHA in args is git itself
		// breaking and likewise needs to surface.
		return nil, 0, fmt.Errorf("wiki: project commit log: %w", err)
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		commits = append(commits, line)
	}
	if incremental || len(commits) <= firstReflectCommitCap {
		return commits, 0, nil
	}
	total, err := projectCommitTotal(cfg.ProjectRepoPath)
	if err != nil {
		// `git log` succeeded a few lines above; if `rev-list --count`
		// fails right after, something is genuinely wrong on this repo.
		// Surfacing it beats rendering a capped slice with no footer,
		// which would read as a complete list to the agent.
		return nil, 0, fmt.Errorf("wiki: project commit count: %w", err)
	}
	return commits[:firstReflectCommitCap], total - firstReflectCommitCap, nil
}

func projectCommitTotal(repoPath string) (int, error) {
	out, err := git.Output(repoPath, "rev-list", "--count", "--no-merges", "HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// closedRunsSince lists the project's terminal runs (closed, merged,
// or promoted) whose last MoE-Run-trailered commit post-dates the
// reflect checkpoint. Mirrors cli/dash.go:closedRunsSinceCount —
// `run.Scan` for metadata and `run.BuildJournalIndex` for the
// activity time, both rooted in git history rather than filesystem
// mtime.
//
// On first reflect (no checkpoint) the threshold is zero and every
// terminal run lands; the agent folds them into history-summary.md
// at pass-end and subsequent reflects only walk the tail.
func closedRunsSince(cfg Config, cp Checkpoint, hasCheckpoint bool) ([]string, error) {
	root := cfg.BureaucracyPath
	mds, err := run.Scan(root)
	if err != nil {
		return nil, fmt.Errorf("wiki: scan runs: %w", err)
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, fmt.Errorf("wiki: journal index: %w", err)
	}
	var threshold time.Time
	if hasCheckpoint && cp.LastIngestAt != "" {
		t, err := time.Parse(time.RFC3339, cp.LastIngestAt)
		if err != nil {
			return nil, fmt.Errorf("wiki: parse checkpoint LastIngestAt %q: %w", cp.LastIngestAt, err)
		}
		threshold = t
	}
	type entry struct {
		id, title string
		when      time.Time
	}
	var rows []entry
	for _, md := range mds {
		if md.Project != cfg.Project {
			continue
		}
		switch md.Status {
		case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		default:
			continue
		}
		when := idx.LastActivity[md.ID]
		if when.IsZero() {
			continue
		}
		if !threshold.IsZero() && !when.After(threshold) {
			continue
		}
		rows = append(rows, entry{id: md.ID, title: md.Title, when: when})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].when.After(rows[j].when) })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		title := r.title
		if title == "" {
			title = r.id
		}
		out = append(out, fmt.Sprintf("%s — %s (%s)", r.id, title, r.when.Format("2006-01-02")))
	}
	return out, nil
}

// ReadHistorySummary reads <ContentDir>/history-summary.md if present.
// Returns ("", nil) when the file is absent or empty — both are normal
// states (a fresh wiki has no summary, and the agent seeds it at the
// end of the first reflect pass). Closed-schema only, like the rest of
// reflect.
func ReadHistorySummary(cfg Config) (string, error) {
	if cfg.Mode != Closed {
		return "", fmt.Errorf("wiki: history summary is closed-schema only (got %s)", cfg.Mode)
	}
	body, err := os.ReadFile(HistorySummaryPath(cfg.ContentDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("wiki: read history summary: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}
