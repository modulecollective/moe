package wiki

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

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

	commits, err := projectCommitsSince(cfg, cp, ok)
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
			"Listing the full project commit history and every closed run; the " +
			"agent will seed history-summary.md from this pass.\n\n")
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

func projectCommitsSince(cfg Config, cp Checkpoint, hasCheckpoint bool) ([]string, error) {
	if cfg.ProjectRepoPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(cfg.ProjectRepoPath); err != nil {
		// Best-effort — a missing project repo just means no commits to list.
		return nil, nil
	}
	args := []string{"log", "--no-merges", "--format=%h %s"}
	if hasCheckpoint && cp.ProjectRepoSHA != nil && *cp.ProjectRepoSHA != "" {
		args = append(args, fmt.Sprintf("%s..HEAD", *cp.ProjectRepoSHA))
	}
	// First reflect (no checkpoint SHA): unbounded `git log`. The
	// agent folds the full history into history-summary.md at the end
	// of the pass, so subsequent reflects only walk the tail.
	cmd := exec.Command("git", args...)
	cmd.Dir = cfg.ProjectRepoPath
	out, err := cmd.Output()
	if err != nil {
		// Git can fail if the SHA is unreachable (history rewrite,
		// shallow clone). Degrade silently rather than block reflect.
		return nil, nil
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		commits = append(commits, line)
	}
	return commits, nil
}

func closedRunsSince(cfg Config, cp Checkpoint, hasCheckpoint bool) ([]string, error) {
	projectsRoot := filepath.Join(cfg.BureaucracyPath, "projects", cfg.Project, "runs")
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wiki: read %s: %w", projectsRoot, err)
	}
	var threshold time.Time
	if hasCheckpoint && cp.LastIngestAt != "" {
		t, err := time.Parse(time.RFC3339, cp.LastIngestAt)
		if err == nil {
			threshold = t
		}
	}

	type closedRun struct {
		id    string
		title string
		when  time.Time
	}
	var runs []closedRun
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runJSON := filepath.Join(projectsRoot, e.Name(), "run.json")
		body, err := os.ReadFile(runJSON)
		if err != nil {
			continue
		}
		var md struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Status  string `json:"status"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(body, &md); err != nil {
			continue
		}
		// "Closed" here means terminal — merged, closed (not pushed),
		// or promoted. Ideas in flight, in-progress runs, and pushed
		// runs aren't yet load-bearing for reflect.
		switch md.Status {
		case "closed", "merged", "promoted":
		default:
			continue
		}
		// LastFileActivity stand-in: stat the run dir.
		info, err := os.Stat(filepath.Join(projectsRoot, e.Name()))
		if err != nil {
			continue
		}
		when := info.ModTime().UTC()
		if !threshold.IsZero() && !when.After(threshold) {
			continue
		}
		runs = append(runs, closedRun{id: md.ID, title: md.Title, when: when})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].when.After(runs[j].when) })
	out := make([]string, 0, len(runs))
	for _, r := range runs {
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
