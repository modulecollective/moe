package project

import (
	"fmt"
	"path/filepath"

	"github.com/modulecollective/moe/internal/git"
)

// The per-project mode: the operator's standing answer to "how much may
// the clock do here".
//
// It is a cap on the *heartbeat*, not on the operator. Everything typed
// — `!`, `!!`, `!!!`, `moe chain kick`, stage verbs, serve's per-run
// chips, and a hand-typed `moe pulse new --dynamic` — runs in every
// mode. The typed word is consent whatever the standing config says; the
// mode governs only what the machine does uninvoked. Serve's global
// arming stays above all three: an unarmed serve automates nothing
// anywhere.
//
// Stored as one field in project.json, absent meaning auto, so a
// bureaucracy that has never heard of modes keeps today's behaviour with
// no migration.

// Mode is a project's cap on machine-started work.
type Mode string

const (
	// ModePaused: no automated work at all. The heartbeat never sweeps
	// the project, so not even an agent turn is spent looking.
	ModePaused Mode = "paused"
	// ModeSafe: the heartbeat sweeps as ever — survey, groom, park, chore
	// nomination — but the kick starts only threads carrying an explicit
	// operator mark. Safe keeps the machine proposing; it stops the
	// machine disposing.
	ModeSafe Mode = "safe"
	// ModeAuto: today's behaviour, and the default.
	ModeAuto Mode = "auto"
)

// Modes lists the three in the order every surface offers them —
// most restrictive first, so the switch reads as a dial.
var Modes = []Mode{ModePaused, ModeSafe, ModeAuto}

// ParseMode validates an operator-typed mode word.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModePaused, ModeSafe, ModeAuto:
		return Mode(s), nil
	}
	return "", fmt.Errorf("project: unknown mode %q (want one of: paused, safe, auto)", s)
}

// ModeOf reports md's mode. Absent reads as auto — that is the whole
// migration story for every project registered before this existed.
//
// So does an unrecognised value, which can only arrive by hand-editing:
// the write path validates through ParseMode, and reading a typo as one
// of the restrictive modes would freeze a project with nothing on any
// surface explaining why.
func ModeOf(md *Metadata) Mode {
	if md == nil {
		return ModeAuto
	}
	switch Mode(md.Mode) {
	case ModePaused:
		return ModePaused
	case ModeSafe:
		return ModeSafe
	}
	return ModeAuto
}

// ReadMode loads one project's mode from disk. Separate from ModeOf
// because the callers that already hold a Metadata (the heartbeat gate
// walks project.List) must not fork a second read, while the ones that
// arrive with only an id (the kick, inside a sweep child) have to.
func ReadMode(root, id string) (Mode, error) {
	md, err := Load(root, id)
	if err != nil {
		return ModeAuto, err
	}
	return ModeOf(md), nil
}

// SetMode writes a project's mode and commits it on main.
//
// The commit carries no machine trailers on purpose: a mode flip is an
// operator act, so it reads as one to every journal consumer. That also
// buys the dynamics for free — the project's journal tip moves, so
// flipping to safe or auto wakes the heartbeat's moved leg on the next
// tick and newly licensed work starts without anyone remembering to
// pulse.
//
// The caller owns the repolock and the journal push (see
// sync.WithJournalPush); this is the disk write and the commit.
func SetMode(root, id string, mode Mode) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("project: id %q must match %s", id, idPattern)
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return err
	}
	md, err := Load(root, id)
	if err != nil {
		return err
	}
	// This copy is the defence, not a courtesy: serve's handler keeps
	// none of its own, and the idempotence its doc comment promises
	// rides on this returning nil. Without it an unchanged write would
	// stage nothing and reach the commit below with nothing to commit.
	if ModeOf(md) == mode {
		return nil
	}
	// auto is the absent state, not a value: writing it out would leave
	// every project that ever visited safe carrying a field that means
	// "the default", and the two spellings would drift apart on every
	// surface that reads the file directly.
	md.Mode = ""
	if mode != ModeAuto {
		md.Mode = string(mode)
	}
	rel := filepath.Join(Dir(id), "project.json")
	if err := writeJSON(filepath.Join(root, rel), md); err != nil {
		return err
	}
	if err := git.Run(root, "add", rel); err != nil {
		return fmt.Errorf("project: git add: %w", err)
	}
	msg := fmt.Sprintf("Set project %s mode: %s", id, mode)
	if err := git.Run(root, "commit", "-m", msg); err != nil {
		return fmt.Errorf("project: git commit: %w", err)
	}
	return nil
}
