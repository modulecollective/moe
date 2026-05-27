package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// metaMoeScanResult is the output of a project-scoped pre-scan. The
// report stage seeds these findings into the kickoff so the agent
// starts grounded in deterministic signal — slug collisions and
// unchecked followups — instead of re-deriving them turn-by-turn.
type metaMoeScanResult struct {
	// Project is the project the scan was run against.
	Project string
	// SlugCollisions groups run slugs that share a base after the
	// auto-suffix walker (`-N`, `-YYYY-MM-DD`, `-YYYY-MM-DD-N`) is
	// undone. Keys are base slugs, values are the full slugs (sorted)
	// that share that base. Only groups with two or more members are
	// returned — a lone slug isn't a collision.
	SlugCollisions map[string][]string
	// FollowupCounts maps each run slug with one or more unchecked
	// followups to that count. Runs with zero unchecked entries (or no
	// followups.md at all) are absent from the map.
	FollowupCounts map[string]int
}

// metaMoeAutoSuffixDate matches the trailing -YYYY-MM-DD or
// -YYYY-MM-DD-N pattern that run.New's IDBase collision walker
// produces (see nextFreeDatedID). Anchored at end so it only strips
// once and won't chew an inner date out of a slug like
// "metrics-2026-01-01-redo".
var metaMoeAutoSuffixDate = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}(-\d+)?$`)

// metaMoeAutoSuffixNum matches the trailing -N pattern
// run.NextFreeID emits on a title-derived collision. N is >= 2 in
// practice; we accept any positive int and trust the slug grammar
// (idPattern in run/run.go) to keep noise out.
var metaMoeAutoSuffixNum = regexp.MustCompile(`-\d+$`)

// metaMoeBaseSlug strips the deterministic auto-suffixes
// run.NextFreeID and nextFreeDatedID emit and returns the underlying
// base slug. A slug with no recognized suffix returns unchanged. The
// dated form is checked before the numeric form so
// "foo-2026-01-02-3" reduces to "foo" in one pass rather than
// "foo-2026-01-02".
func metaMoeBaseSlug(slug string) string {
	if loc := metaMoeAutoSuffixDate.FindStringIndex(slug); loc != nil {
		return slug[:loc[0]]
	}
	if loc := metaMoeAutoSuffixNum.FindStringIndex(slug); loc != nil {
		return slug[:loc[0]]
	}
	return slug
}

// metaMoeScanProject runs the deterministic pre-scan over one
// project's run directory and returns the findings. A missing
// project dir (no runs yet) returns an empty result, not an error —
// running meta-moe against a fresh project is legitimate and the
// agent should see "nothing repeated, nothing left over."
func metaMoeScanProject(root, projectID string) (metaMoeScanResult, error) {
	out := metaMoeScanResult{
		Project:        projectID,
		SlugCollisions: map[string][]string{},
		FollowupCounts: map[string]int{},
	}
	runsDir := filepath.Join(root, "projects", projectID, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("metamoe: read %s: %w", runsDir, err)
	}

	groups := map[string][]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		base := metaMoeBaseSlug(slug)
		groups[base] = append(groups[base], slug)

		// Followups.md sits next to run.json. Count once per slug.
		count, err := metaMoeCountUncheckedFollowups(filepath.Join(runsDir, slug, "followups.md"))
		if err != nil {
			return out, err
		}
		if count > 0 {
			out.FollowupCounts[slug] = count
		}
	}
	for base, slugs := range groups {
		if len(slugs) < 2 {
			continue
		}
		sort.Strings(slugs)
		out.SlugCollisions[base] = slugs
	}
	return out, nil
}

// metaMoeCountUncheckedFollowups counts `- [ ]` lines in a
// followups.md file. A missing file returns 0 cleanly — the file is
// optional. The match is line-leading and tolerant of leading
// whitespace; it deliberately does not match `- [x]` (lowercase x)
// or `- [X]` since those are harvested-or-resolved.
func metaMoeCountUncheckedFollowups(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("metamoe: open %s: %w", path, err)
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimLeft(scanner.Text(), " \t")
		if strings.HasPrefix(line, "- [ ]") {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("metamoe: scan %s: %w", path, err)
	}
	return count, nil
}

// metaMoeRenderKickoff turns the scan result into the kickoff prose
// the report stage hands the agent on session start. Empty sections
// collapse to a one-liner — the agent should see "no signals on this
// axis" rather than a missing heading. The pre-scan section is only
// part of the kickoff; the prompt fragment names the rest of the
// shape.
func metaMoeRenderKickoff(scan metaMoeScanResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The operator just opened a meta-moe report session for project %q. "+
		"Read the project's run history before replying — the canvas under your "+
		"system prompt is empty until you draft it. Below is a deterministic "+
		"pre-scan of the runs directory; treat it as a starting list of "+
		"candidate signals, not the report.\n\n", scan.Project)

	b.WriteString("## Repeated work (slug collisions)\n\n")
	if len(scan.SlugCollisions) == 0 {
		b.WriteString("(no auto-suffixed run-slug groups under this project)\n\n")
	} else {
		bases := make([]string, 0, len(scan.SlugCollisions))
		for k := range scan.SlugCollisions {
			bases = append(bases, k)
		}
		sort.Strings(bases)
		b.WriteString("Each base below has more than one run slug attached " +
			"after auto-suffix stripping (-N or -YYYY-MM-DD). That is the " +
			"deterministic shape of repeated work; it does not catch " +
			"semantic re-runs (`foo-redux`, `foo-take2`) — read those off " +
			"the run titles and canvases yourself.\n\n")
		for _, base := range bases {
			fmt.Fprintf(&b, "- `%s`: %s\n", base, strings.Join(scan.SlugCollisions[base], ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Unchecked followups\n\n")
	if len(scan.FollowupCounts) == 0 {
		b.WriteString("(no runs with unchecked followups under this project)\n\n")
	} else {
		slugs := make([]string, 0, len(scan.FollowupCounts))
		for k := range scan.FollowupCounts {
			slugs = append(slugs, k)
		}
		sort.Strings(slugs)
		b.WriteString("Counts are leftover `- [ ]` entries in each run's " +
			"followups.md. A high count means stage-time captures the " +
			"operator never harvested into ideas at close — read each one " +
			"to see whether the friction is a moe-side gap (your job to " +
			"surface) or a project-side todo (not your job).\n\n")
		for _, slug := range slugs {
			fmt.Fprintf(&b, "- `%s`: %d unchecked\n", slug, scan.FollowupCounts[slug])
		}
		b.WriteString("\n")
	}

	b.WriteString("Acknowledge in one or two sentences which signals look " +
		"most likely to surface moe-side friction (versus project-side " +
		"todos) and how you'd walk them with the operator. Don't write " +
		"the report yet — wait for the operator's go-ahead.\n")
	return b.String()
}
