// Package chore loads project-level maintenance chores and computes
// whether they are due from the bureaucracy journal.
package chore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

const DefaultWorkflow = "sdlc"

type Definition struct {
	Project  string
	Name     string
	Trigger  string
	Workflow string
	Cooldown time.Duration
	Cadence  time.Duration
	Prompt   string
	EditedAt time.Time
}

func (d Definition) Key() string { return d.Project + "/" + d.Name }

type State struct {
	Definition       Definition
	Due              bool
	Reasons          []string
	LastCompleted    time.Time
	NextEligible     time.Time
	OpenRun          string
	CooldownBlocking bool
}

func (s State) ReasonString() string {
	if len(s.Reasons) == 0 {
		return "-"
	}
	return strings.Join(s.Reasons, ",")
}

func LoadAll(root string) ([]Definition, error) {
	projects, warnings, err := project.List(root)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("chore: project list has %d warning(s)", len(warnings))
	}
	var defs []Definition
	for _, p := range projects {
		ds, err := loadProject(root, p.ID)
		if err != nil {
			return nil, err
		}
		defs = append(defs, ds...)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Key() < defs[j].Key() })
	return defs, nil
}

func loadProject(root, projectID string) ([]Definition, error) {
	base := filepath.Join(root, project.Dir(projectID), "chores")
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("chore: read %s: %w", base, err)
	}
	var defs []Definition
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := loadOne(root, projectID, e.Name())
		if err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, nil
}

// choreJSON is the on-disk wire shape of chore.json. Durations stay
// strings so the d-suffix shorthand survives and the file stays
// hand-editable; parseDuration turns them into time.Duration.
type choreJSON struct {
	Trigger  string `json:"trigger,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Cadence  string `json:"cadence,omitempty"`
	Cooldown string `json:"cooldown,omitempty"`
}

func loadOne(root, projectID, name string) (Definition, error) {
	dir := filepath.Join(root, project.Dir(projectID), "chores", name)
	d := Definition{Project: projectID, Name: name, Workflow: DefaultWorkflow}
	b, err := os.ReadFile(filepath.Join(dir, "chore.json"))
	if err != nil {
		return d, fmt.Errorf("chore %s/%s: read chore.json: %w", projectID, name, err)
	}
	var cj choreJSON
	if err := json.Unmarshal(b, &cj); err != nil {
		return d, fmt.Errorf("chore %s/%s: parse chore.json: %w", projectID, name, err)
	}
	d.Trigger = cj.Trigger
	if cj.Workflow != "" {
		d.Workflow = cj.Workflow
	}
	if cj.Cooldown != "" {
		d.Cooldown, err = parseDuration(cj.Cooldown)
		if err != nil {
			return d, fmt.Errorf("chore %s/%s cooldown: %w", projectID, name, err)
		}
	}
	if cj.Cadence != "" {
		d.Cadence, err = parseDuration(cj.Cadence)
		if err != nil {
			return d, fmt.Errorf("chore %s/%s cadence: %w", projectID, name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "prompt.md")); err == nil {
		d.Prompt = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return d, fmt.Errorf("chore: read prompt.md: %w", err)
	}
	d.EditedAt = lastDefinitionEdit(root, filepath.ToSlash(filepath.Join(project.Dir(projectID), "chores", name)))
	return d, Validate(d)
}

func Validate(d Definition) error {
	if d.Project == "" || d.Name == "" {
		return fmt.Errorf("chore: project and name are required")
	}
	if d.Workflow == "" {
		return fmt.Errorf("chore %s: workflow is empty", d.Key())
	}
	if d.Trigger == "" && d.Cadence == 0 {
		return fmt.Errorf("chore %s: either trigger or cadence is required", d.Key())
	}
	if d.Cadence > 0 && d.Cooldown > 0 && d.Cadence < d.Cooldown {
		return fmt.Errorf("chore %s: cadence must be >= cooldown", d.Key())
	}
	if d.Trigger != "" && d.Trigger != "*" {
		if _, err := filepath.Match(d.Trigger, "x"); err != nil {
			return fmt.Errorf("chore %s trigger: %w", d.Key(), err)
		}
	}
	return nil
}

func EvaluateAll(defs []Definition, mds []*run.Metadata, idx *run.JournalIndex, now time.Time) []State {
	states := make([]State, 0, len(defs))
	for _, d := range defs {
		states = append(states, Evaluate(d, mds, idx, now))
	}
	return states
}

func Evaluate(d Definition, mds []*run.Metadata, idx *run.JournalIndex, now time.Time) State {
	s := State{Definition: d}
	for _, md := range mds {
		if idx.ChoreByRun[md.Project+"/"+md.ID] != d.Key() {
			continue
		}
		if isTerminal(md.Status) {
			when := idx.LastActivity[md.Project+"/"+md.ID]
			if when.After(s.LastCompleted) {
				s.LastCompleted = when
			}
			continue
		}
		if s.OpenRun == "" {
			s.OpenRun = md.ID
		}
	}
	// A `moe chore skip` marker counts as a completion as of its commit
	// time: fold it into LastCompleted so every downstream gate (cooldown
	// and all three reasons) treats the chore as satisfied then.
	if sk := idx.ChoreSkipped[d.Key()]; sk.After(s.LastCompleted) {
		s.LastCompleted = sk
	}
	if !s.LastCompleted.IsZero() && d.Cooldown > 0 {
		s.NextEligible = s.LastCompleted.Add(d.Cooldown)
		if now.Before(s.NextEligible) {
			s.CooldownBlocking = true
		}
	}
	if s.OpenRun == "" && !s.CooldownBlocking {
		if touched := idx.ChoreTouched[d.Key()]; !touched.IsZero() && (s.LastCompleted.IsZero() || touched.After(s.LastCompleted)) {
			s.Reasons = append(s.Reasons, "changed paths")
		}
		if !d.EditedAt.IsZero() && (s.LastCompleted.IsZero() || d.EditedAt.After(s.LastCompleted)) {
			s.Reasons = append(s.Reasons, "definition changed")
		}
		if d.Cadence > 0 && (s.LastCompleted.IsZero() || now.Sub(s.LastCompleted) >= d.Cadence) {
			s.Reasons = append(s.Reasons, "cadence")
		}
	}
	s.Due = len(s.Reasons) > 0
	return s
}

func MatchChangedPaths(defs []Definition, projectID string, paths []string) []string {
	var out []string
	for _, d := range defs {
		if d.Project != projectID || d.Trigger == "" {
			continue
		}
		if d.Trigger == "*" {
			out = append(out, d.Key())
			continue
		}
		for _, p := range paths {
			ok, _ := filepath.Match(d.Trigger, filepath.ToSlash(p))
			if ok {
				out = append(out, d.Key())
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
		if err != nil {
			return 0, err
		}
		return n * 24, nil
	}
	return time.ParseDuration(s)
}

func lastDefinitionEdit(root, rel string) time.Time {
	out, err := git.Output(root, "log", "-1", "--format=%ct", "--", rel)
	if err != nil {
		return time.Time{}
	}
	sec, err := time.ParseDuration(strings.TrimSpace(out) + "s")
	if err != nil {
		return time.Time{}
	}
	return time.Unix(int64(sec/time.Second), 0).UTC()
}

func isTerminal(status string) bool {
	switch status {
	case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		return true
	default:
		return false
	}
}
