package input

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// PulseFileName is the pulse-notes record's basename. It lives in the
// root project's directory, projects/_cabinet/, so a note's commit moves
// the root project's journal tip — which is what wakes the heartbeat.
const PulseFileName = "pulse-notes.json"

// PulsePath returns the pulse-notes record's path relative to the
// bureaucracy root.
func PulsePath() string {
	return filepath.Join("projects", project.RootID, PulseFileName)
}

// pulseLabel names the pulse record in lock scopes and errors.
const pulseLabel = project.RootID + "/pulse-notes"

// LoadPulse reads the pulse-notes record: operator prose addressed to
// the survey itself rather than to a run. Same grammar as a run's
// record, notes only — the survey asks nothing back, so an entry with a
// question is malformed here. DeliveredTo holds the qualified pulse run
// that read the note. No record reads as an empty File.
func LoadPulse(root string) (File, error) {
	f, err := loadPath(root, PulsePath(), pulseLabel)
	if err != nil {
		return File{}, err
	}
	for _, e := range f.Notes {
		if e.IsPing() || e.AskedBy != "" {
			return File{}, fmt.Errorf("%w: %s: entry %d is a ping; the pulse record holds notes only", ErrMalformed, pulseLabel, e.ID)
		}
	}
	return f, nil
}

// AddPulse appends an operator note for the next pulse and commits it.
//
// The commit carries MoE-Project: _cabinet and no consent trailer: it
// is the operator's own act, so the heartbeat reads it as fresh work and
// the next tick sweeps. No MoE-Input-* trailer either: those carry a
// "<project>/<run>#<id>" address, and a pulse note names no run.
func AddPulse(root, text string) (Entry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Entry{}, fmt.Errorf("%w: note has no text", ErrEmpty)
	}
	var e Entry
	err := commit(root, PulsePath(), pulseLabel, "pulse-note", func() (File, string, error) {
		f, err := LoadPulse(root)
		if err != nil {
			return File{}, "", err
		}
		e = Entry{ID: len(f.Notes) + 1, Text: text}
		f.Notes = append(f.Notes, e)
		return f, fmt.Sprintf("Note for pulse #%d\n\n", e.ID) +
			trailers.Block{Project: project.RootID}.String(), nil
	})
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// MarkPulseDelivered stamps pulseRun — the qualified "<project>/<run>"
// of the survey that read them — on the notes its kickoff rendered, as
// one journal commit. Same contract as MarkDelivered: only the ids the
// prompt carried, read fresh under the lock, so a note added after the
// render stays pending for the next sweep; an empty list is a no-op with
// no lock and no commit.
//
// consent is the sweep's walkConsent(). Under a heartbeat walk that marks
// the commit as the machine's, so the sweep's exit does not read its own
// stamp as operator work and earn a follow-up sweep.
func MarkPulseDelivered(root, pulseRun string, ids []int, consent string) error {
	if len(ids) == 0 {
		return nil
	}
	projectID, runID, ok := strings.Cut(pulseRun, "/")
	if !ok || projectID == "" || runID == "" {
		return fmt.Errorf("input: pulse run %q is not <project>/<run>", pulseRun)
	}
	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return commit(root, PulsePath(), pulseLabel, "pulse-note-delivered", func() (File, string, error) {
		f, err := LoadPulse(root)
		if err != nil {
			return File{}, "", err
		}
		var marked []string
		for i := range f.Notes {
			e := &f.Notes[i]
			if !want[e.ID] || !e.Pending() {
				continue
			}
			e.DeliveredTo = pulseRun
			marked = append(marked, strconv.Itoa(e.ID))
		}
		if len(marked) == 0 {
			return File{}, "", run.ErrNothingToCommit
		}
		return f, fmt.Sprintf("Read pulse notes #%s in %s\n\n", strings.Join(marked, ","), pulseRun) +
			trailers.Block{
				Run:      runID,
				Project:  projectID,
				Workflow: "pulse",
				Consent:  consent,
			}.String(), nil
	})
}
