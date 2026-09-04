package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/usage"
)

// `moe usage` is the text rendering of internal/usage's report. The
// aggregation, the price table and the notional arithmetic all live in
// that package, because serve renders the same report as a page and
// cannot import cli. What stays here is the verb: flags, the project
// check, and the tabwriter tables.

func init() {
	Register(&Command{
		Name:    "usage",
		Summary: "sum token usage across run transcripts, with notional API dollars",
		Run:     runUsage,
	})
}

func runUsage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectF := fs.String("project", "", "limit to one project")
	sinceF := fs.String("since", "30d", "only count stages whose last turn is within this window (e.g. 7d, 24h)")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe usage [--project <id>] [--since <dur>]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Sums the token usage recorded in every mirrored stage transcript,")
		moePrintln(stderr, "with workflow/stage/model, selected rolling-window, per-run, and")
		moePrintln(stderr, "by-day views. Notional dollars are a comparability unit, not a bill.")
		moePrintln(stderr, "Each whole stage is dated by its last work turn, falling back to run")
		moePrintln(stderr, "activity; untimed transcripts stay in totals and are marked.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	window, err := chore.ParseDuration(*sinceF)
	if err != nil {
		moePrintf(stderr, "moe usage: --since %q: %v\n", *sinceF, err)
		return 2
	}

	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if *projectF != "" {
		if err := requireProject(root, *projectF); err != nil {
			moePrintf(stderr, "moe usage: %v\n", err)
			return 1
		}
	}
	now := time.Now()
	report, err := usage.Gather(root, usage.Filter{Project: *projectF, Cutoff: now.Add(-window)}, stderr)
	if err != nil {
		moePrintf(stderr, "moe usage: %v\n", err)
		return 1
	}
	renderUsage(stdout, report, *projectF, *sinceF)
	return 0
}

func renderUsage(w io.Writer, rep usage.Report, projectFilter, since string) {
	scope := "all projects"
	if projectFilter != "" {
		scope = projectFilter
	}
	moePrintf(w, "usage — %s · last %s · %d transcript(s)\n", scope, since, rep.Transcripts)
	if rep.Transcripts == 0 {
		moePrintln(w, "")
		moePrintln(w, "No stage transcripts in the window.")
		return
	}
	moePrintln(w, "")

	unpriced := rep.UnpricedTokens()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKFLOW\tSTAGE\tMODEL\tRUNS\tSTEPS\tCACHE-W\tCACHE-R\tOUTPUT\tNOTIONAL")
	for _, r := range rep.Buckets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			r.Workflow, r.Stage, r.Model, r.Runs, r.Usage.Steps,
			usage.HumanTokens(r.Usage.CacheWrite), usage.HumanTokens(r.Usage.CacheRead),
			usage.HumanTokens(r.Usage.Output), usage.ModelNotional(r.Model, r.Usage))
	}
	fmt.Fprintf(tw, "\t\tTOTAL\t\t%d\t%s\t%s\t%s\t%s\n",
		rep.Total.Steps, usage.HumanTokens(rep.Total.CacheWrite), usage.HumanTokens(rep.Total.CacheRead),
		usage.HumanTokens(rep.Total.Output), usage.Notional(rep.Dollars, rep.Total, unpriced))
	tw.Flush()

	moePrintln(w, "")
	moePrintln(w, "CURRENT ROLLING WINDOW")
	ww := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(ww, "LAST\tSTEPS\tINPUT\tOUTPUT\tTOTAL\tNOTIONAL")
	fmt.Fprintf(ww, "%s\t%d\t%s\t%s\t%s\t%s\n", since, rep.Total.Steps,
		usage.HumanTokens(usage.Input(rep.Total)), usage.HumanTokens(rep.Total.Output),
		usage.HumanTokens(usage.Tokens(rep.Total)),
		usage.Notional(rep.Dollars, rep.Total, unpriced))
	ww.Flush()

	moePrintln(w, "")
	moePrintf(w, "BY RUN (within last %s)\n", since)
	// The report arrives in run order, which is what the page defaults
	// to; the terminal table has always led with the most expensive run
	// and still does.
	byCost := make([]usage.Run, len(rep.Runs))
	copy(byCost, rep.Runs)
	usage.SortRunsByCost(byCost)
	rw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(rw, "RUN\tWORKFLOW\tSTAGES\tSTEPS\tINPUT\tOUTPUT\tTOTAL\tNOTIONAL")
	for _, r := range byCost {
		fmt.Fprintf(rw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			r.Key(), r.Workflow, r.Stages, r.Usage.Steps, usage.HumanTokens(usage.Input(r.Usage)),
			usage.HumanTokens(r.Usage.Output), usage.HumanTokens(usage.Tokens(r.Usage)),
			usage.Notional(r.Dollars, r.Usage, r.Unpriced))
	}
	rw.Flush()

	if len(rep.ByDay) > 0 {
		moePrintln(w, "")
		moePrintln(w, "BY DAY")
		days := make([]string, 0, len(rep.ByDay))
		for d := range rep.ByDay {
			days = append(days, d)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(days)))
		dw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, d := range days {
			day := rep.ByDay[d]
			fmt.Fprintf(dw, "%s\t%d steps\t%s in\t%s out\t%s\n",
				d, day.Usage.Steps, usage.HumanTokens(usage.Input(day.Usage)),
				usage.HumanTokens(day.Usage.Output),
				usage.Notional(day.Dollars, day.Usage, day.Unpriced))
		}
		dw.Flush()
	}

	if len(rep.Unpriced) > 0 {
		models := make([]string, 0, len(rep.Unpriced))
		for m := range rep.Unpriced {
			models = append(models, m)
		}
		sort.Strings(models)
		moePrintln(w, "")
		for _, m := range models {
			moePrintf(w, "no price on record for %s (%s tokens uncounted in the notional column)\n",
				m, usage.HumanTokens(rep.Unpriced[m]))
		}
		if rep.Starred() {
			moePrintln(w, "* starred totals exclude tokens from models with no price on record")
		}
	}
	if rep.Untimed > 0 {
		moePrintln(w, "")
		moePrintf(w, "%d untimed transcript(s) included in aggregate, current-window, and per-run totals; omitted from BY DAY\n", rep.Untimed)
	}
	moePrintln(w, "")
	moePrintln(w, "Notional dollars are API sticker prices, for comparing workflows — not a bill.")
}
