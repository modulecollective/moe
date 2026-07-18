package cli

import (
	"fmt"
	"strings"

	"github.com/modulecollective/moe/internal/run"
)

// The settled-runs block closed the backward half of the refile loop:
// decisions already made. This is the forward half — decisions already
// made about what happens *next*. Chain edges live only in journal
// trailers, whose effective state needs the BuildJournalIndex replay no
// surveying agent can cheaply or safely do by hand, and run.json shows
// which runs are in-progress but never their order. So a sweep can file
// a finding the very next chained run will fix, pick a Pull next the
// chain already covers, or spawn a duplicate of a queued fix under a
// different slug — the slug-dedupe guard in maybeSpawnFixRuns catches
// none of those.

// chainStateBlock renders the chain-state context block, or "" when
// nothing is sequenced. Best-effort like its siblings in
// pulseKickoffWithContext: a failed scan or index drops the block
// rather than failing the sweep.
//
// Selection reuses activeChainItems — the same grouping the dash and
// the chain editor render, so the three views cannot drift — then keeps
// only units with ≥ 2 members that touch this project. Orphans carry no
// order information (the agent already sees in-progress runs on disk),
// and a chain wholly inside another project is not this sweep's
// business. Chains may cross projects, so foreign members render with
// their qualified `<project>/<slug>` key and same-project members bare.
func chainStateBlock(root, projectID string) string {
	mds, err := run.Scan(root)
	if err != nil {
		return ""
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return ""
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	var lines []string
	for _, unit := range activeChainItems(mds, idx, byKey) {
		if len(unit) < 2 {
			continue
		}
		var members []string
		touches := false
		for _, it := range unit {
			md := byKey[it.Key]
			if md == nil {
				continue
			}
			label := md.ID
			if md.Project != projectID {
				label = it.Key
			} else {
				touches = true
			}
			members = append(members, fmt.Sprintf("`%s` (%s) — %s",
				label, md.Workflow, settledRunTitle(root, md)))
		}
		if !touches || len(members) < 2 {
			continue
		}
		lines = append(lines, "- "+strings.Join(members, " → "))
	}
	if len(lines) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Work already sequenced (chains of active runs, head first) — the order the " +
		"journal slice and the disk scan do not show you:\n\n")
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\nA chain headed by a chain run is the batch the operator will kick as-is. " +
		"Check three things against this list before writing. A finding an upcoming chained run " +
		"will already fix is not a finding — verify against current code first, same posture as " +
		"the merged-run rule. A Pull next pick a chained run already covers is noise. And a spawn " +
		"proposal matching a queued fix by *content* is a duplicate even under a fresh slug: the " +
		"harness dedupes slugs, you dedupe substance. Nothing here is identified by its slug — " +
		"match on what the run is about.")
	return sb.String()
}
