package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/bureaucracy"
	"github.com/modulecollective/moe/internal/chore"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

func init() {
	g := NewCommandGroup("chore", "project chores: list, check, open")
	g.Register(&Command{Name: "list", Summary: "list due project chores", Run: runChoreList})
	g.Register(&Command{Name: "check", Summary: "dry-run chore validation and due-state evaluation", Run: runChoreCheck})
	g.Register(&Command{Name: "open", Summary: "open the workflow run for a due chore", Run: runChoreOpen})
	RegisterGroup(g)
}

func runChoreList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectFilter := fs.String("project", "", "only list chores for this project")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore list [--project <project>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	states, err := gatherChoreStates(root, *projectFilter)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	count := 0
	for _, s := range states {
		if s.Due {
			count++
		}
	}
	moePrintf(stdout, "CHORES (%d)\n", count)
	if count == 0 {
		moePrintln(stdout, "  (none)")
		return 0
	}
	now := time.Now()
	for _, s := range states {
		if !s.Due {
			continue
		}
		when := s.LastCompleted
		if touched := s.Definition.EditedAt; touched.After(when) {
			when = touched
		}
		moePrintf(stdout, "  %s\t%s\t%s\n", s.Definition.Key(), humanChoreAgo(now, when), s.ReasonString())
	}
	return 0
}

func runChoreCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectFilter := fs.String("project", "", "only check chores for this project")
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore check [--project <project>] [<project>/<chore>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	states, err := gatherChoreStates(root, *projectFilter)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	want := ""
	if fs.NArg() == 1 {
		want = fs.Arg(0)
	}
	found := false
	for _, s := range states {
		if want != "" && s.Definition.Key() != want {
			continue
		}
		found = true
		status := "not due"
		if s.Due {
			status = "due"
		}
		if s.OpenRun != "" {
			status = "open: " + s.Definition.Project + "/" + s.OpenRun
		}
		if s.CooldownBlocking {
			status = "cooldown until " + s.NextEligible.Format(time.RFC3339)
		}
		moePrintf(stdout, "%s\t%s\tworkflow=%s\treason=%s\n", s.Definition.Key(), status, s.Definition.Workflow, s.ReasonString())
	}
	if want != "" && !found {
		moePrintf(stderr, "chore check: %s not found\n", want)
		return 1
	}
	return 0
}

func runChoreOpen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore open <project>/<chore>")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, choreName, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chore open: %v\n", err)
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	md, code := openDueChore(root, projectID, choreName, stdout, stderr)
	if code != 0 {
		return code
	}
	return promptNextStage(root, md, "", stdout, stderr)
}

func openDueChore(root, projectID, choreName string, stdout, stderr io.Writer) (*run.Metadata, int) {
	states, err := gatherChoreStates(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return nil, 1
	}
	var state *chore.State
	for i := range states {
		if states[i].Definition.Name == choreName {
			state = &states[i]
			break
		}
	}
	if state == nil {
		moePrintf(stderr, "chore open: %s/%s not found\n", projectID, choreName)
		return nil, 1
	}
	if state.OpenRun != "" {
		moePrintf(stderr, "chore open: %s already has open run %s/%s\n", state.Definition.Key(), projectID, state.OpenRun)
		return nil, 1
	}
	if state.CooldownBlocking {
		moePrintf(stderr, "chore open: %s is cooling down until %s\n", state.Definition.Key(), state.NextEligible.Format(time.RFC3339))
		return nil, 1
	}
	if !state.Due {
		moePrintf(stderr, "chore open: %s is not due\n", state.Definition.Key())
		return nil, 1
	}
	wf, err := LookupWorkflow(state.Definition.Workflow)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return nil, 1
	}
	stages := wf.Stages()
	if len(stages) == 0 {
		moePrintf(stderr, "chore open: workflow %q has no stages\n", state.Definition.Workflow)
		return nil, 1
	}
	prompt := state.Definition.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = "# " + state.Definition.Name + "\n"
	}
	var md *run.Metadata
	err = repolock.With(root, repolock.Options{Purpose: "chore-open", Run: state.Definition.Key()}, func() error {
		m, err := runopen.Open(root, projectID, run.Options{
			IDBase:   state.Definition.Name,
			Workflow: state.Definition.Workflow,
			SeedDocs: map[string]string{stages[0]: prompt},
			Trailers: trailers.Block{Chore: state.Definition.Key()},
		})
		if err != nil {
			return err
		}
		md = m
		return nil
	})
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return nil, 1
	}
	moePrintf(stdout, "opened chore %s as %s/%s\n", state.Definition.Key(), md.Project, md.ID)
	return md, 0
}

func gatherChoreStates(root, projectFilter string) ([]chore.State, error) {
	defs, err := chore.LoadAll(root)
	if err != nil {
		return nil, err
	}
	if projectFilter != "" {
		filtered := defs[:0]
		for _, d := range defs {
			if d.Project == projectFilter {
				filtered = append(filtered, d)
			}
		}
		defs = filtered
	}
	mds, err := run.Scan(root)
	if err != nil {
		return nil, err
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, err
	}
	return chore.EvaluateAll(defs, mds, idx, time.Now()), nil
}

func choreRoot(stderr io.Writer) (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", false
	}
	root, err := bureaucracy.Find(cwd, os.Getenv)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return "", false
	}
	return root, true
}

func humanChoreAgo(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
