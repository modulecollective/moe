// Package chore loads project-level maintenance chores and computes
// whether they are due from the bureaucracy journal.
package chore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	// When is the operator's one-line prose due-condition — the judged
	// due-ness family. A judged chore is never mechanically due; the
	// pulse survey evaluates this criterion against what landed and
	// nominates the chore through the gate when it holds.
	When     string
	Prompt   string
	EditedAt time.Time
}

func (d Definition) Key() string { return d.Project + "/" + d.Name }

// Judged reports whether this chore's due-ness is a judgment rather than
// a glob or a clock. Judged and mechanical are exclusive families
// (Validate enforces it), so this is also "not mechanical".
func (d Definition) Judged() bool { return d.When != "" }

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

// Status describes the evaluated state without implying a recent survey verdict
// or promising when work will execute.
func (s State) Status() string {
	switch {
	case s.OpenRun != "":
		return "open run " + s.OpenRun
	case s.CooldownBlocking:
		return "cooling down until " + s.NextEligible.Local().Format("2006-01-02 15:04 MST")
	case s.Due:
		return "due — " + s.ReasonString()
	case s.Definition.Judged():
		return "judged — pulse decides"
	default:
		return "not due"
	}
}

// Summary is the authored condition or mechanical schedule for a standing row.
func (d Definition) Summary() string {
	if d.Judged() {
		return "when " + d.When
	}
	var parts []string
	if d.Trigger != "" {
		parts = append(parts, "on "+d.Trigger)
	}
	if d.Cadence > 0 {
		cadence := d.Cadence.String()
		if d.Cadence%(24*time.Hour) == 0 {
			cadence = fmt.Sprintf("%dd", d.Cadence/(24*time.Hour))
		}
		parts = append(parts, "every "+cadence)
	}
	return strings.Join(parts, " · ")
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

func loadOne(root, projectID, name string) (Definition, error) {
	s, err := LoadSpec(root, projectID, name)
	if err != nil {
		return Definition{Project: projectID, Name: name, Workflow: DefaultWorkflow}, err
	}
	d, err := s.parse(projectID, name)
	if err != nil {
		return d, err
	}
	d.EditedAt = lastDefinitionEdit(root, Dir(projectID, name))
	return d, Validate(d)
}

func Validate(d Definition) error {
	if d.Project == "" || d.Name == "" {
		return fmt.Errorf("chore: project and name are required")
	}
	if d.Workflow == "" {
		return fmt.Errorf("chore %s: workflow is empty", d.Key())
	}
	// Judged and mechanical are exclusive families: a chore with two
	// masters has unclear semantics (does the glob still fire while the
	// judgment says no?) and no instance needs it. Loosen with evidence.
	if d.When != "" && (d.Trigger != "" || d.Cadence > 0) {
		return fmt.Errorf("chore %s: when is exclusive with trigger and cadence", d.Key())
	}
	if d.When == "" && d.Trigger == "" && d.Cadence == 0 {
		return fmt.Errorf("chore %s: one of trigger, cadence, or when is required", d.Key())
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
	// A judged chore is never mechanically due — its condition is prose
	// the pulse survey evaluates, not a glob or a clock — so it collects
	// no reasons. Everything above still applies: the open-run guard, the
	// cooldown, and LastCompleted are what the nomination path checks.
	if s.OpenRun == "" && !s.CooldownBlocking && !d.Judged() {
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

// ParseDuration is time.ParseDuration plus a "d" (day) suffix, the unit
// chore cooldowns and `moe usage --since` are both written in.
func ParseDuration(s string) (time.Duration, error) {
	if before, ok := strings.CutSuffix(s, "d"); ok {
		n, err := time.ParseDuration(before + "h")
		if err != nil {
			return 0, err
		}
		return n * 24, nil
	}
	return time.ParseDuration(s)
}

// lastDefinitionEdit returns the commit time of the newest commit
// touching rel, memoized on HEAD (see editMemo).
func lastDefinitionEdit(root, rel string) time.Time {
	head, err := git.HEAD(root)
	if err != nil {
		// No readable HEAD — nothing to key on. Fall through to the
		// walk, which fails to the zero time as it always has.
		t, _ := walkDefinitionEdit(root, rel)
		return t
	}
	editMemo.Lock()
	defer editMemo.Unlock()
	e, ok := editMemo.entries[root]
	if ok && e.head == head {
		if t, ok := e.edits[rel]; ok {
			return t
		}
	} else {
		if editMemo.entries == nil {
			editMemo.entries = make(map[string]*editMemoEntry, 1)
		}
		e = &editMemoEntry{head: head, edits: map[string]time.Time{}}
		editMemo.entries[root] = e
	}
	t, err := walkDefinitionEdit(root, rel)
	if err == nil {
		e.edits[rel] = t
	}
	return t
}

// editMemo caches definition-edit times per bureaucracy root. `git log
// -1 -- <path>` is a pure function of the commit graph reachable from
// HEAD, so while HEAD holds still the cached answer is exact; a new
// commit drops the root's whole map and each path re-walks once. The
// walk is a path-limited trip back through the whole journal (~100ms
// per chore on a long one) and LoadAll runs it per definition on every
// `moe ui` discovery pass, so this memo is what keeps an idle board
// cheap. Same shape as run's journalMemo: HEAD lives in the value so a
// new commit replaces the entry, and the lock is held across the walk
// so concurrent misses cost one walk.
var editMemo struct {
	sync.Mutex
	entries map[string]*editMemoEntry
}

type editMemoEntry struct {
	head  string
	edits map[string]time.Time
}

// walkDefinitionEdit is the uncached walk. A path with no commits is a
// successful walk returning the zero time — exact for this HEAD, so the
// memo keeps it. Only a failed git invocation returns an error, which
// the memo declines to store so a transient failure retries next call.
func walkDefinitionEdit(root, rel string) (time.Time, error) {
	out, err := git.Output(root, "log", "-1", "--format=%ct", "--", rel)
	if err != nil {
		return time.Time{}, err
	}
	sec, err := time.ParseDuration(strings.TrimSpace(out) + "s")
	if err != nil {
		return time.Time{}, nil
	}
	return time.Unix(int64(sec/time.Second), 0).UTC(), nil
}

func isTerminal(status string) bool {
	switch status {
	case run.StatusClosed, run.StatusMerged, run.StatusPromoted:
		return true
	default:
		return false
	}
}
