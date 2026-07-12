package serve

import (
	"encoding/json"
	"html/template"
	"math/rand"
	"time"

	"github.com/modulecollective/moe/internal/dash"
)

// factoryFrameCount is how many factory-art frames the dash bakes into
// the page for the client cross-fade. Lives in code, not config —
// single-operator project.
const factoryFrameCount = 10

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

// bannerArtVM is the shared banner-art block both the home dash and a
// project hub draw between the banner and the run sections: the daily
// activity histogram over the factory-art rail, plus the frames the
// client cross-fades through. Embedded in dashVM and hubVM so the
// {{define "bannerart"}} partial renders the same way on both pages.
type bannerArtVM struct {
	// FactoryArt is the peripheral-vision rail the CLI dash also draws
	// under its banner — backlog feed, station glyphs for active runs,
	// completed-output dots. One-line empty state, three lines populated.
	// It is frame[0] of the frames below: the server-rendered, no-JS fallback.
	FactoryArt []string
	// Histogram is the daily run-activity chart drawn between the banner
	// and the factory art — HistRows bar rows under a caption, or a single
	// "(quiet)" line in the cold state. Static text (a per-render
	// snapshot), so unlike the factory frames it carries no JS animation.
	Histogram []string
	// FactoryFramesJSON is json.Marshal of all factory-art frames, embedded
	// in the page so the client can cross-fade through them without an XHR.
	// It is template.JS, not string: html/template treats <script> content
	// as a JS value context and would string-wrap a plain string (breaking
	// JSON.parse), so the pre-marshalled JSON is emitted verbatim. Safe to do
	// so because json.Marshal escapes <,>,& — no </script> breakout possible.
	FactoryFramesJSON template.JS
}

// newBannerArt builds the banner-art block from the (already
// project-scoped, if filtering) dash rows and the trailing-HistDays
// activity counts. The factory art reflects exactly the rows passed in,
// so a project hub gets its own rail and histogram.
func newBannerArt(now time.Time, rows []dash.Row, histogram []int) bannerArtVM {
	state := dash.FactoryStateFromRows(rows)
	r := rand.New(rand.NewSource(now.UnixNano()))
	frames := dash.BuildFactoryFrames(state, dash.ArtWidth, factoryFrameCount, r)
	framesJSON, _ := json.Marshal(frames) // [][]string never fails to marshal
	return bannerArtVM{
		FactoryArt:        frames[0],
		Histogram:         dash.BuildActivityHistogram(histogram),
		FactoryFramesJSON: template.JS(framesJSON),
	}
}

// dashVM is the data the dash template renders against. Same three
// buckets as the CLI dash; COMPLETED is pre-capped by CompletedCap
// unless showAll is set so the template doesn't need slice math.
type dashVM struct {
	bannerArtVM
	Active         []dashRowVM
	Chores         []dashRowVM
	Backlog        []dashRowVM
	Completed      []dashRowVM
	CompletedTotal int // pre-cap count; lets the header show "N of M"
	ProjectCount   int
	ActiveProjects int
	ShowAll        bool
	// Insecure mirrors Options.Insecure: the "new run" / "new plan"
	// links spawn an agent, so they only render in insecure mode. The
	// "new idea" link is journal-only and always renders.
	Insecure bool
}

func newDashVM(now time.Time, rows []dash.Row, projectCount, activeProjects int, histogram []int, showAll bool) dashVM {
	vm := dashVM{
		bannerArtVM:    newBannerArt(now, rows, histogram),
		ProjectCount:   projectCount,
		ActiveProjects: activeProjects,
		ShowAll:        showAll,
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
		case dash.BucketChores:
			vm.Chores = append(vm.Chores, row)
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
