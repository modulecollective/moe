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
	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runopen"
	"github.com/modulecollective/moe/internal/trailers"
)

func init() {
	g := NewCommandGroup("chore", "project chores: list, check, open, skip")
	g.Register(&Command{Name: "list", Summary: "list due project chores", Run: runChoreList})
	g.Register(&Command{Name: "check", Summary: "dry-run chore validation and due-state evaluation", Run: runChoreCheck})
	g.Register(&Command{Name: "open", Summary: "open the workflow run for a due chore", Run: runChoreOpen})
	g.Register(&Command{Name: "skip", Summary: "clear a due chore until it is next triggered", Run: runChoreSkip})
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

// openDueChore is the CLI-facing wrapper around openChoreInProcess: it
// maps the typed guard errors back to the stderr messages + exit 1 that
// `moe chore open` (and the chore-chain prompt) print, and emits the
// "opened chore …" stdout line on success. serve takes the typed-error
// path directly via OpenChore instead.
func openDueChore(root, projectID, choreName string, stdout, stderr io.Writer) (*run.Metadata, int) {
	res, err := openChoreInProcess(root, projectID, choreName)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return nil, 1
	}
	moePrintf(stdout, "opened chore %s/%s as %s/%s\n", projectID, choreName, res.Metadata.Project, res.Metadata.ID)
	return res.Metadata, 0
}

// choreNotFoundError is returned by openChoreInProcess when no chore
// matches project/name. serve maps it to 404.
type choreNotFoundError struct {
	Project string
	Name    string
}

func (e *choreNotFoundError) Error() string {
	return fmt.Sprintf("chore open: %s/%s not found", e.Project, e.Name)
}

// choreNotOpenableError is returned when the chore exists but its state
// forbids opening (an open run already exists, it's cooling down, or
// it's simply not due). serve maps it to 409. Reason is the human tail
// appended after the chore key.
type choreNotOpenableError struct {
	Key    string
	Reason string
}

func (e *choreNotOpenableError) Error() string {
	return fmt.Sprintf("chore open: %s %s", e.Key, e.Reason)
}

// choreOpenResult carries what a successful in-process chore open
// produced: the destination run metadata plus the workflow + first
// stage that callers must spawn to host the run (serve can't look the
// workflow up itself — it stays registry-free).
type choreOpenResult struct {
	Metadata   *run.Metadata
	Workflow   string
	FirstStage string
}

// openChoreInProcess is the single chore-open pipeline shared by the CLI
// verb and serve's OpenChore callback: gather → guard → workflow lookup
// → runopen.Open with the Chore trailer. Guard failures come back as
// typed errors (*choreNotFoundError / *choreNotOpenableError) so callers
// can branch on HTTP status or print to stderr; everything else is a
// plain error.
func openChoreInProcess(root, projectID, choreName string) (*choreOpenResult, error) {
	states, err := gatherChoreStates(root, projectID)
	if err != nil {
		return nil, err
	}
	var state *chore.State
	for i := range states {
		if states[i].Definition.Name == choreName {
			state = &states[i]
			break
		}
	}
	if state == nil {
		return nil, &choreNotFoundError{Project: projectID, Name: choreName}
	}
	if state.OpenRun != "" {
		return nil, &choreNotOpenableError{Key: state.Definition.Key(), Reason: fmt.Sprintf("already has open run %s/%s", projectID, state.OpenRun)}
	}
	if state.CooldownBlocking {
		return nil, &choreNotOpenableError{Key: state.Definition.Key(), Reason: "is cooling down until " + state.NextEligible.Format(time.RFC3339)}
	}
	if !state.Due {
		return nil, &choreNotOpenableError{Key: state.Definition.Key(), Reason: "is not due"}
	}
	wf, err := LookupWorkflow(state.Definition.Workflow)
	if err != nil {
		return nil, err
	}
	stages := wf.Stages()
	if len(stages) == 0 {
		return nil, fmt.Errorf("chore open: workflow %q has no stages", state.Definition.Workflow)
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
		return nil, err
	}
	return &choreOpenResult{Metadata: md, Workflow: state.Definition.Workflow, FirstStage: stages[0]}, nil
}

func runChoreSkip(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore skip", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore skip <project>/<chore>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Records the chore as satisfied as of now, clearing it from the")
		moePrintln(stderr, "dash until it is next triggered (a matching change, a definition")
		moePrintln(stderr, "edit, or the cadence elapsing). Use it to decline a chore or to")
		moePrintln(stderr, "mark one already done by hand.")
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
		moePrintf(stderr, "chore skip: %v\n", err)
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	states, err := gatherChoreStates(root, projectID)
	if err != nil {
		moePrintf(stderr, "%v\n", err)
		return 1
	}
	var state *chore.State
	for i := range states {
		if states[i].Definition.Name == choreName {
			state = &states[i]
			break
		}
	}
	if state == nil {
		moePrintf(stderr, "chore skip: %s/%s not found\n", projectID, choreName)
		return 1
	}
	// An open linked run is the live way the chore is being handled;
	// skipping behind its back would clear the dash while the run is
	// still in flight. Point the operator at it instead. (A not-due
	// chore is fine to skip — Decision 5 — so there's no Due gate.)
	if state.OpenRun != "" {
		moePrintf(stderr, "chore skip: %s has open run %s/%s — close it instead\n", state.Definition.Key(), projectID, state.OpenRun)
		return 1
	}
	key := state.Definition.Key()
	block := trailers.Block{ChoreSkipped: key}
	msg := "chore: skip " + key + "\n\n" + block.String()
	err = repolock.With(root, repolock.Options{Purpose: "chore-skip", Run: key}, func() error {
		return git.Run(root, "commit", "--allow-empty", "-m", msg)
	})
	if err != nil {
		moePrintf(stderr, "chore skip: %v\n", err)
		return 1
	}
	moePrintf(stdout, "skipped chore %s\n", key)
	return 0
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
