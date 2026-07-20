package cli

import (
	"time"

	"github.com/modulecollective/moe/internal/dash"
	"github.com/modulecollective/moe/internal/run"
)

// chainMembers is the head page's read of live chain truth: the batch
// hanging off a chain head, head→tail, plus the qualified key of a live
// parent the head is itself chained under ("" when it heads its own
// chain).
//
// The walk is the one `moe chain kick` rides — follow ChainedChild from
// the head and stop at the first child that isn't live, exactly where
// maybeRideChain stops. A page whose job is "review this batch before
// kicking it" has to show the batch the kick will actually walk, not
// every edge ever stamped. So a member that ships or closes drops off
// the list, and the members behind it go with it: their edges are still
// on record, but the ride stops there and `moe chain edit` is what
// re-strings them.
//
// Rows come from one unfiltered dash snapshot rather than a
// GatherRunRow per member. GatherRunRow re-scans on every call, and a
// chain can cross projects (chain edit is global), so one global gather
// is both cheaper and the only correct scope. A member the dash filters
// out entirely still gets a row, built from its metadata — the count
// must not lie about what kick will ride.
func chainMembers(root, projectID, slug string, now time.Time) ([]dash.Row, string, error) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, "", err
	}
	mds, err := run.Scan(root)
	if err != nil {
		return nil, "", err
	}
	byKey := make(map[string]*run.Metadata, len(mds))
	for _, md := range mds {
		byKey[md.Project+"/"+md.ID] = md
	}

	head := projectID + "/" + slug
	chainedUnder := liveChainParentOf(head, idx.ChainedChild, byKey)

	snap, err := GatherDashSnapshot(root, now, DashFilter{})
	if err != nil {
		return nil, "", err
	}
	rowByKey := make(map[string]dash.Row, len(snap.Rows))
	for _, r := range snap.Rows {
		rowByKey[r.Project+"/"+r.Run] = r
	}

	var members []dash.Row
	// seen cycle-guards the walk the same way OrderChainUnits' consumed
	// map does: a hand-edited journal can describe a loop, and a page
	// render is no place to hang.
	seen := map[string]bool{head: true}
	prev := head
	for cur := idx.ChainedChild[head]; run.ChainChildLive(cur, byKey) && !seen[cur]; cur = idx.ChainedChild[cur] {
		seen[cur] = true
		// The edge that put this member here is the one from the run
		// before it — the dash rows are gathered globally and carry the
		// attribution for whatever unit they landed in, so re-derive it
		// against this walk's parent rather than trusting the row's.
		consent, groomed := idx.EdgeConsent[prev+" "+cur]
		prev = cur
		row, ok := rowByKey[cur]
		if !ok {
			md := byKey[cur]
			row = dash.Row{
				Project: md.Project,
				Run:     md.ID,
				Note:    md.Workflow,
				When:    idx.LastActivity[cur],
			}
		}
		row.EdgeAgent, row.EdgeConsent = groomed, consent
		members = append(members, row)
	}
	return members, chainedUnder, nil
}
