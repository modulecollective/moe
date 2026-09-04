// Package serve — the token-spend report as a page.
//
// Same report `moe usage` prints, same package behind it
// (internal/usage): the totals strip, the workflow/stage/model buckets,
// and one row per run linking to that run's page. Read-only, so it
// serves on an unarmed serve like the rest of the browse routes.
//
// internal/usage is imported directly rather than crossing the seam as
// an Options callback, the same way input and runopen are: the report
// needs nothing from the workflow registry, so a callback would only be
// indirection.
package serve

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/usage"
)

// defaultUsageWindow matches `moe usage --since`'s default, so the page
// and the terminal answer the same question when neither is told
// otherwise.
const defaultUsageWindow = "30d"

// usageNum is one numeric table cell: the humanized string a reader sees, and
// the raw value the sort script compares. They are separate on purpose —
// the script never parses "147.3M", so the display can stay compact
// without the column sorting lexically. V is empty where there is no
// number to give (an unpriced dash, an untimed run), and those cells
// sort to the bottom whichever way the column points.
type usageNum struct {
	Text string
	V    string
}

func usageTokens(n int64) usageNum {
	return usageNum{Text: usage.HumanTokens(n), V: strconv.FormatInt(n, 10)}
}

func usageCount(n int) usageNum {
	return usageNum{Text: strconv.Itoa(n), V: strconv.Itoa(n)}
}

// usageNotional pairs a rendered cost string with the figure behind it. A
// dash means nothing in the row was priced, so there is no value to
// sort on; a starred figure is partial but still sortable.
func usageNotional(text string, dollars float64) usageNum {
	if text == "—" {
		return usageNum{Text: text}
	}
	return usageNum{Text: text, V: strconv.FormatFloat(dollars, 'f', 2, 64)}
}

// usageWhen renders a run's last activity as a date, sorting on unix
// seconds. An untimed run — nothing in the journal dates it — shows a
// dash and sorts last, which is where run order already puts it.
func usageWhen(t time.Time) usageNum {
	if t.IsZero() {
		return usageNum{Text: "—"}
	}
	return usageNum{Text: t.Local().Format("2006-01-02"), V: strconv.FormatInt(t.Unix(), 10)}
}

type usageVM struct {
	Serve servePanelVM
	// Project is the current filter, empty for the board-wide view; it
	// drives the crumb and the select's current option.
	Project  string
	Projects []string
	// Since is the window as the operator typed it, echoed back into the
	// form so a reload keeps it.
	Since string
	// Scope is the human name for Project: "all projects" or the id.
	Scope       string
	Transcripts int

	Totals   usageTotalsVM
	Buckets  []usageBucketVM
	Runs     []usageRunVM
	Unpriced []usageUnpricedVM
	Untimed  int
	Starred  bool
}

type usageTotalsVM struct {
	Steps    usageNum
	Input    usageNum
	Output   usageNum
	Total    usageNum
	Notional usageNum
}

type usageBucketVM struct {
	Workflow string
	Stage    string
	Model    string
	Runs     usageNum
	Steps    usageNum
	CacheW   usageNum
	CacheR   usageNum
	Output   usageNum
	Notional usageNum
}

type usageRunVM struct {
	Key      string
	URL      string
	Workflow string
	Stages   usageNum
	Steps    usageNum
	Input    usageNum
	Output   usageNum
	Total    usageNum
	Notional usageNum
	Last     usageNum
}

type usageUnpricedVM struct {
	Model  string
	Tokens string
}

// handleUsage renders the spend report. Both query params are optional
// and both mirror the CLI's flags: ?project scopes it (the project hub
// links here that way, which is the whole per-project story — there is
// no separate page), ?since sets the window in the same grammar
// --since takes. A malformed window is a 400 rather than a silent
// zero-length window, matching the CLI's exit 2.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID := q.Get("project")
	if projectID != "" {
		if !slugRe.MatchString(projectID) {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat(filepath.Join(s.opts.Root, project.Dir(projectID))); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	since := q.Get("since")
	if since == "" {
		since = defaultUsageWindow
	}
	window, err := chore.ParseDuration(since)
	if err != nil {
		http.Error(w, "usage: since "+since+": "+err.Error(), http.StatusBadRequest)
		return
	}

	rep, err := usage.Gather(s.opts.Root,
		usage.Filter{Project: projectID, Cutoff: time.Now().Add(-window)}, s.syncWriter())
	if err != nil {
		http.Error(w, "usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "usage.html", s.newUsageVM(rep, projectID, since))
}

func (s *Server) newUsageVM(rep usage.Report, projectID, since string) usageVM {
	scope := "all projects"
	if projectID != "" {
		scope = projectID
	}
	unpriced := rep.UnpricedTokens()
	vm := usageVM{
		Serve:       s.activity.panel(time.Now().UTC()),
		Project:     projectID,
		Projects:    s.projectIDs(),
		Since:       since,
		Scope:       scope,
		Transcripts: rep.Transcripts,
		Totals: usageTotalsVM{
			Steps:    usageCount(rep.Total.Steps),
			Input:    usageTokens(usage.Input(rep.Total)),
			Output:   usageTokens(rep.Total.Output),
			Total:    usageTokens(usage.Tokens(rep.Total)),
			Notional: usageNotional(usage.Notional(rep.Dollars, rep.Total, unpriced), rep.Dollars),
		},
		Untimed: rep.Untimed,
		Starred: rep.Starred(),
	}
	for _, b := range rep.Buckets {
		cost, _ := usage.NotionalCost(b.Model, b.Usage)
		vm.Buckets = append(vm.Buckets, usageBucketVM{
			Workflow: b.Workflow,
			Stage:    b.Stage,
			Model:    b.Model,
			Runs:     usageCount(b.Runs),
			Steps:    usageCount(b.Usage.Steps),
			CacheW:   usageTokens(b.Usage.CacheWrite),
			CacheR:   usageTokens(b.Usage.CacheRead),
			Output:   usageTokens(b.Usage.Output),
			Notional: usageNotional(usage.ModelNotional(b.Model, b.Usage), cost),
		})
	}
	// rep.Runs is already in run order — last activity, newest first —
	// which is the page's default. A click on a header re-sorts in the
	// browser; nothing here reorders.
	for _, rn := range rep.Runs {
		vm.Runs = append(vm.Runs, usageRunVM{
			Key:      rn.Key(),
			URL:      "/run/" + rn.Project + "/" + rn.ID,
			Workflow: rn.Workflow,
			Stages:   usageCount(rn.Stages),
			Steps:    usageCount(rn.Usage.Steps),
			Input:    usageTokens(usage.Input(rn.Usage)),
			Output:   usageTokens(rn.Usage.Output),
			Total:    usageTokens(usage.Tokens(rn.Usage)),
			Notional: usageNotional(usage.Notional(rn.Dollars, rn.Usage, rn.Unpriced), rn.Dollars),
			Last:     usageWhen(rn.Last),
		})
	}
	for _, model := range slices.Sorted(maps.Keys(rep.Unpriced)) {
		vm.Unpriced = append(vm.Unpriced, usageUnpricedVM{
			Model:  model,
			Tokens: usage.HumanTokens(rep.Unpriced[model]),
		})
	}
	return vm
}

// projectIDs backs the filter select. A project.List failure leaves the
// select with nothing but "all projects" and a log line — the page's job
// is the report, and a broken registry shouldn't take it down.
func (s *Server) projectIDs() []string {
	mds, _, err := project.List(s.opts.Root)
	if err != nil {
		s.logf("usage: project list: %v", err)
		return nil
	}
	out := make([]string, 0, len(mds))
	for _, p := range mds {
		out = append(out, p.ID)
	}
	return out
}
