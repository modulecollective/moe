package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/chore"
)

// checkChoreWorkflow is the registry half of chore validation, which
// internal/chore can't import: the workflow must exist and have stages,
// the two things openChoreInProcess needs of it at open.
func checkChoreWorkflow(name string) error {
	if strings.TrimSpace(name) == "" {
		name = chore.DefaultWorkflow
	}
	wf, err := LookupWorkflow(strings.TrimSpace(name))
	if err != nil {
		return &chore.InvalidError{Err: err}
	}
	if len(wf.Stages()) == 0 {
		return &chore.InvalidError{Err: fmt.Errorf("workflow %q has no stages", name)}
	}
	return nil
}

// choreCreate, choreUpdate and choreUpdateChecked are the chore writer
// behind the CLI verbs, the web form and moe ui, with the workflow
// checked against the registry first.
func choreCreate(root, projectID, name string, s chore.Spec) error {
	if err := checkChoreWorkflow(s.Workflow); err != nil {
		return err
	}
	return chore.Create(root, projectID, name, s)
}

func choreUpdate(root, projectID, name string, s chore.Spec) error {
	if err := checkChoreWorkflow(s.Workflow); err != nil {
		return err
	}
	return chore.Update(root, projectID, name, s)
}

func choreUpdateChecked(root, projectID, name string, expected, s chore.Spec) error {
	if err := checkChoreWorkflow(s.Workflow); err != nil {
		return err
	}
	return chore.UpdateChecked(root, projectID, name, expected, s)
}

// choreFlags are the scalar flags new and edit share.
type choreFlags struct {
	fs                                         *flag.FlagSet
	trigger, cadence, cooldown, when, workflow *string
}

func newChoreFlags(fs *flag.FlagSet) choreFlags {
	return choreFlags{
		fs:       fs,
		trigger:  fs.String("trigger", "", "path glob whose change makes the chore due"),
		cadence:  fs.String("cadence", "", "interval after which the chore is due (e.g. 30d, 12h)"),
		cooldown: fs.String("cooldown", "", "minimum time between runs"),
		when:     fs.String("when", "", "prose due-condition the pulse judges (exclusive with trigger and cadence)"),
		workflow: fs.String("workflow", "", "workflow the chore's runs use (default sdlc)"),
	}
}

// apply sets on s exactly the flags that were passed; an empty value
// clears the field. It reports how many were set.
func (f choreFlags) apply(s *chore.Spec) int {
	n := 0
	f.fs.Visit(func(fl *flag.Flag) {
		v := fl.Value.String()
		switch fl.Name {
		case "trigger":
			s.Trigger = v
		case "cadence":
			s.Cadence = v
		case "cooldown":
			s.Cooldown = v
		case "when":
			s.When = v
		case "workflow":
			s.Workflow = v
		default:
			return
		}
		n++
	})
	return n
}

func runChoreNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	flags := newChoreFlags(fs)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore new <project>/<name> [--trigger G] [--cadence D] [--cooldown D] [--when TEXT] [--workflow W]")
		moePrintln(stderr, "")
		moePrintln(stderr, "Opens $EDITOR on the chore's definition, flags prefilled, and commits")
		moePrintln(stderr, "it on save. Set one of trigger, cadence or when.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chore new: %v\n", err)
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	s := chore.Spec{Prompt: "# " + name + "\n"}
	flags.apply(&s)
	// Everything that can be refused before the operator types: the
	// name, the project, a taken name, the flag values one by one, and
	// the workflow. Whether the family is complete waits for the save.
	if err := s.CheckFields(projectID, name); err != nil {
		moePrintf(stderr, "chore new: %v\n", err)
		return 2
	}
	if err := requireProject(root, projectID); err != nil {
		moePrintf(stderr, "chore new: %v\n", err)
		return 1
	}
	if _, err := chore.LoadSpec(root, projectID, name); err == nil {
		moePrintf(stderr, "chore new: %s/%s already exists — moe chore edit changes it\n", projectID, name)
		return 1
	} else if !errors.Is(err, chore.ErrNotFound) {
		moePrintf(stderr, "chore new: %v\n", err)
		return 1
	}
	if err := checkChoreWorkflow(s.Workflow); err != nil {
		moePrintf(stderr, "chore new: %v\n", err)
		return 2
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "chore new: set $EDITOR or $VISUAL — chore new needs an editor")
		return 1
	}
	return choreEditorSave("chore new", projectID, name, s.Text(), "moe-chore-new-", true, func(edited chore.Spec) error {
		return choreCreate(root, projectID, name, edited)
	}, "added chore", stdout, stderr)
}

func runChoreEdit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore edit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	flags := newChoreFlags(fs)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore edit <project>/<name> [--trigger G] [--cadence D] [--cooldown D] [--when TEXT] [--workflow W]")
		moePrintln(stderr, "")
		moePrintln(stderr, "With flags, sets exactly those fields (an empty value clears one) and")
		moePrintln(stderr, "commits. With none, opens $EDITOR on the whole definition.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chore edit: %v\n", err)
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	cur, err := chore.LoadSpec(root, projectID, name)
	if err != nil {
		moePrintf(stderr, "chore edit: %v\n", err)
		return 1
	}
	next := cur
	if flags.apply(&next) > 0 {
		if err := choreUpdate(root, projectID, name, next); err != nil {
			moePrintf(stderr, "chore edit: %v\n", err)
			return 1
		}
		if next == cur {
			moePrintf(stdout, "chore %s/%s unchanged\n", projectID, name)
			return 0
		}
		moePrintf(stdout, "edited chore %s/%s\n", projectID, name)
		return 0
	}
	if os.Getenv("VISUAL") == "" && os.Getenv("EDITOR") == "" {
		moePrintln(stderr, "chore edit: set $EDITOR or $VISUAL, or pass the fields to change as flags")
		return 1
	}
	return choreEditorSave("chore edit", projectID, name, cur.Text(), "moe-chore-edit-", false, func(edited chore.Spec) error {
		return choreUpdateChecked(root, projectID, name, cur, edited)
	}, "edited chore", stdout, stderr)
}

// choreEditorSave pops $EDITOR on the text form and saves what comes
// back. A save that fails to parse or is refused keeps the edited text
// and names its path, so nothing typed is lost. create: the seed is a
// proposal, not what is on disk, so saving it untouched still adds the
// chore.
func choreEditorSave(verb, projectID, name, seed, prefix string, create bool, save func(chore.Spec) error, done string, stdout, stderr io.Writer) int {
	text, tmpPath, code := captureEditorBody(prefix, seed, stderr)
	if code != 0 {
		if tmpPath != "" {
			moePrintf(stderr, "%s: your edited definition is preserved at %s\n", verb, tmpPath)
		}
		return code
	}
	s, err := chore.ParseSpecText(text)
	if err == nil {
		err = save(s)
	}
	if err != nil {
		moePrintf(stderr, "%s: %v\n", verb, err)
		moePrintf(stderr, "%s: your edited definition is preserved at %s\n", verb, tmpPath)
		return 1
	}
	os.RemoveAll(filepath.Dir(tmpPath))
	if !create && s.Text() == seed {
		// The writer committed nothing; saying "edited" would claim a change.
		moePrintf(stdout, "chore %s/%s unchanged\n", projectID, name)
		return 0
	}
	moePrintf(stdout, "%s %s/%s\n", done, projectID, name)
	return 0
}

func runChoreRm(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chore rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		moePrintln(stderr, "usage: moe chore rm <project>/<name>")
		moePrintln(stderr, "")
		moePrintln(stderr, "Removes the chore's definition in one commit. An open run it")
		moePrintln(stderr, "started carries on; git history is the undo.")
	}
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	projectID, name, err := splitProjectRun(fs.Arg(0))
	if err != nil {
		moePrintf(stderr, "chore rm: %v\n", err)
		return 2
	}
	root, ok := choreRoot(stderr)
	if !ok {
		return 1
	}
	if err := chore.Remove(root, projectID, name); err != nil {
		moePrintf(stderr, "chore rm: %v\n", err)
		return 1
	}
	moePrintf(stdout, "removed chore %s/%s\n", projectID, name)
	return 0
}
