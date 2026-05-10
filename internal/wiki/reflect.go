package wiki

import (
	"fmt"
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
const firstReflectCommitCap = 500

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

Vision is asymmetric — flag drift between project state and the
stated vision, but don't rewrite vision yourself. Vision changes
are the operator's call (decided edits — see ` + "`moe twin claim`" + `).

Roadmap convention: roadmap.md uses four ` + "`##`" + ` sections — Near
term, Mid term, Long term, Parked. On a fresh roadmap.md (just
` + "`# Roadmap`" + ` and nothing else), establish the four headings at
this pass. On subsequent passes, walk the prior content with the
operator and promote / demote / retire entries against the idea
backlog and recent activity.

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
		// Best-effort — a missing project repo just means no commits to list.
		return nil, 0, nil
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
		// Git can fail if the SHA is unreachable (history rewrite,
		// shallow clone). Degrade silently rather than block reflect.
		return nil, 0, nil
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
		// Degrade: render the capped slice with no footer rather than
		// block reflect on a count failure.
		return commits[:firstReflectCommitCap], 0, nil
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
		if t, err := time.Parse(time.RFC3339, cp.LastIngestAt); err == nil {
			threshold = t
		}
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
