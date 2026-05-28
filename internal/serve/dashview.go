package serve

import (
	"math/rand"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// dashRowVM is one row in the HTML dash — already string-formatted
// so the template stays a flat presentation layer with no time math
// or bucket-constant inspection.
type dashRowVM struct {
	Project string
	Run     string
	Note    string
	When    string // dash.HumanAgo output
	// Live is true when the row's run is currently parented by this
	// serve process — the per-run page has buttons. Only meaningful
	// for active rows; backlog/completed always render Live=false.
	Live bool
	// Member is true for an active row that follows its chain parent in
	// the grouped order — the template indents it and draws a connector.
	Member bool
}

// dashVM is the data the dash template renders against. Same three
// buckets as the CLI dash; COMPLETED is pre-capped by CompletedCap
// unless showAll is set so the template doesn't need slice math.
type dashVM struct {
	Active         []dashRowVM
	Backlog        []dashRowVM
	Completed      []dashRowVM
	CompletedTotal int // pre-cap count; lets the header show "N of M"
	ProjectCount   int
	ActiveProjects int
	ShowAll        bool
	// FactoryArt is the same peripheral-vision rail the CLI dash draws
	// under its banner — backlog feed, station glyphs for active runs,
	// completed-output dots. One-line empty state, three lines populated.
	FactoryArt []string
}

func newDashVM(now time.Time, rows []dash.Row, projectCount, activeProjects int, showAll bool) dashVM {
	state := dash.FactoryStateFromRows(rows)
	r := rand.New(rand.NewSource(now.UnixNano()))
	vm := dashVM{
		ProjectCount:   projectCount,
		ActiveProjects: activeProjects,
		ShowAll:        showAll,
		FactoryArt:     dash.BuildFactoryArt(state, dash.ArtWidth, r),
	}
	for _, r := range rows {
		row := dashRowVM{
			Project: r.Project,
			Run:     r.Run,
			Note:    r.Note,
			When:    dash.HumanAgo(now, r.When),
			Member:  r.Member,
		}
		switch r.Bucket {
		case dash.BucketActiveRuns:
			vm.Active = append(vm.Active, row)
		case dash.BucketBacklog:
			vm.Backlog = append(vm.Backlog, row)
		case dash.BucketCompletedRuns:
			vm.Completed = append(vm.Completed, row)
		}
	}
	vm.CompletedTotal = len(vm.Completed)
	if !showAll && len(vm.Completed) > dash.CompletedCap {
		vm.Completed = vm.Completed[:dash.CompletedCap]
	}
	return vm
}
