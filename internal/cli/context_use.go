package cli

import (
	"flag"
	"io"
	"slices"
	"time"

	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/contextuse"
)

// `moe context-use` is the text rendering of internal/contextuse: does
// the steering a project writes (twin, lore, knowledge, intents) reach
// its agents, and what does nobody open. A hygiene report for the
// operator and the 90-day context-use-audit chore.

func init() {
	Register(&Command{
		Name:    "context-use",
		Summary: "report which twin, lore, knowledge and intent files stage agents read",
		Run:     runContextUse,
	})
}

func runContextUse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("context-use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectF := fs.String("project", "", "limit to one project")
	sinceF := fs.String("since", "90d", "only count stages whose last turn is within this window (e.g. 30d, 24h)")
	byBackendF := fs.Bool("by-backend", false, "adoption by backend and month, with named path misses")
	sessionsF := fs.Bool("sessions", false, "one JSON line per session, for hand review")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe context-use [--project <id>] [--since <dur>] [--by-backend | --sessions]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Reads every mirrored stage transcript and reports which context agents")
		moePrintln(stderr, "read: the twin, lore entries, the knowledge index and topics, intents,")
		moePrintln(stderr, "other runs. The default view leads with what nobody read and whether")
		moePrintln(stderr, "the twin fits through the codex result cap. Lore is bureaucracy-wide,")
		moePrintln(stderr, "so its counts only cover every reader without --project.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *byBackendF && *sessionsF {
		fs.Usage()
		return 2
	}
	window, err := chore.ParseDuration(*sinceF)
	if err != nil {
		moePrintf(stderr, "moe context-use: --since %q: %v\n", *sinceF, err)
		return 2
	}
	root, err := findRoot(stderr)
	if err != nil {
		return 1
	}
	if *projectF != "" {
		if err := requireProject(root, *projectF); err != nil {
			moePrintf(stderr, "moe context-use: %v\n", err)
			return 1
		}
	}
	sessions, err := contextuse.Gather(root, contextuse.Filter{Project: *projectF, Cutoff: time.Now().Add(-window)}, stderr)
	if err != nil {
		moePrintf(stderr, "moe context-use: %v\n", err)
		return 1
	}
	switch {
	case len(sessions) == 0 && !*sessionsF:
		moePrintln(stdout, "No stage transcripts in the window.")
	case *sessionsF:
		if err := contextuse.RenderSessions(stdout, sessions); err != nil {
			moePrintf(stderr, "moe context-use: %v\n", err)
			return 1
		}
	case *byBackendF:
		contextuse.RenderByBackend(stdout, sessions)
	default:
		projects := []string{*projectF}
		if *projectF == "" {
			projects = nil
			for _, s := range sessions {
				projects = append(projects, s.Project)
			}
			slices.Sort(projects)
			projects = slices.Compact(projects)
		}
		contextuse.RenderHygiene(stdout, sessions, contextuse.ReadInventory(root, projects), projects)
	}
	return 0
}
