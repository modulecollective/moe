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
// a finding the very next chained run will fix, aim a thread at an
// order the chain already records, or spawn a duplicate of a queued fix
// under a different slug — the slug-dedupe guard in pulseMinter.mint
// catches none of those.

// chainStateBlock renders the chain-state context block, or "" when
// nothing is sequenced. Best-effort like its siblings in
// pulseKickoffWithContext: the sweep's shared scan is best-effort and
// a failure drops the block rather than failing the sweep.
//
// Selection reuses activeChainItems — the same grouping the dash and
// the chain editor render, so the three views cannot drift — then keeps
// only units with ≥ 2 members that touch this project. Orphans carry no
// order information (the agent already sees in-progress runs on disk),
// and a chain wholly inside another project is not this sweep's
// business. Chains may cross projects, so foreign members render with
// their qualified `<project>/<slug>` key and same-project members bare.
//
// The one exception to ≥ 2: a unit whose head has a settled chain parent
// is kept at any size, and every kept unit renders that parent as a
// leading `parent (wf, status) →` term. Grouping needs both endpoints
// active, so a two-item chain collapses to a bare one-member unit the
// moment its first item ships — and the sweep would then read the queued
// tail as unordered, which is exactly backwards: it is the next thing to
// run. Pure orphans (no settled parent either) stay dropped.
func chainStateBlock(sc *pulseScan, projectID string) string {
	root, mds, idx, byKey := sc.root, sc.mds, sc.idx, sc.byKey

	label := func(md *run.Metadata) (string, bool) {
		if md.Project != projectID {
			return md.Project + "/" + md.ID, false
		}
		return md.ID, true
	}

	graph := sc.graph
	var lines []string
	for _, unit := range activeChainItems(graph, mds, idx) {
		// A settled predecessor of the unit head is the whole reason a
		// one-member unit can still be a thread: the run ahead of it
		// shipped, the edge stayed, and this is the item it feeds.
		var prefix string
		if len(unit) > 0 {
			if p := graph.TerminalParentOf(unit[0].Key); p != "" {
				if pmd := byKey[p]; pmd != nil {
					plabel, _ := label(pmd)
					prefix = fmt.Sprintf("`%s` (%s, %s) → ", plabel, pmd.Workflow, pmd.Status)
				}
			}
		}
		if len(unit) < 2 && prefix == "" {
			continue // orphan; skip before paying for settledRunTitle's file reads
		}
		var members []string
		touches := false
		for _, it := range unit {
			md := byKey[it.Key]
			if md == nil {
				continue
			}
			l, mine := label(md)
			touches = touches || mine
			members = append(members, fmt.Sprintf("`%s` (%s) — %s",
				l, md.Workflow, settledRunTitle(root, md)))
		}
		if !touches || len(members) == 0 || (len(members) < 2 && prefix == "") {
			continue
		}
		lines = append(lines, "- "+prefix+strings.Join(members, " → "))
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
	sb.WriteString("\nA line that opens with a settled run — `(sdlc, merged) →` — is a thread already " +
		"executing: that item shipped and the active run after it is what runs next. Read it as " +
		"ordered and in flight, not as a loose run or a deliberate un-threading.\n")
	sb.WriteString("\nEach line is a thread the operator (or a confident groom) will kick as-is. " +
		"Check two things against this list before writing. A finding an upcoming chained run " +
		"will already fix is not a finding — verify against current code first, same posture as " +
		"the merged-run rule. And a spawn " +
		"proposal matching a queued fix by *content* is a duplicate even under a fresh slug: the " +
		"harness dedupes slugs, you dedupe substance. Nothing here is identified by its slug — " +
		"match on what the run is about.\n\n" +
		"This is also your grooming map: a `chain` group's `onto` names any run above, and " +
		"extending an existing thread beats forking a new one.")
	return sb.String()
}
