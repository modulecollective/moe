package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
)

// The snooze: an operator-authored hold on the heartbeat.
//
// An armed serve sweeps on a 20-minute clock and each sweep can spend
// agent turns. The only brake before this was killing the process, which
// also takes down the dash and every run serve is parenting. A snooze
// pauses the clock and nothing else — clicks, `!!!!` tails and hand-run
// pulses stay live throughout, because those spend only when the
// operator asks them to.
//
// Two properties are load-bearing:
//
//   - **It always expires.** An indefinite off-switch makes an armed
//     serve that will never act — a standing lie on the dash. The worst
//     case of forgetting a snooze is that sweeps resume, which is the
//     state running armed consented to. The indefinite off-switch is
//     still stopping the process.
//   - **It lives outside serve.json.** That file is serve's runtime
//     record: written on serve's events, removed on clean shutdown. A
//     snooze must be settable while serve is down and must survive a
//     restart — a restart mid-snooze silently resuming spend would
//     defeat the whole case. The file is also the transport between the
//     CLI verbs and the running serve, which keeps the CLI free of an
//     HTTP client.

// SnoozePath is where the hold lives: one RFC3339 instant, the moment
// sweeps resume. Under `.moe/`, which carries a `*` gitignore, so a
// snooze never dirties the tree.
func SnoozePath(root string) string {
	return filepath.Join(root, ".moe", "snooze")
}

// ReadSnooze reports whether a snooze is in force at now, and when it
// lifts. Absent is the ordinary case and reads as no snooze with no
// error; an expired file reads the same way, so nothing has to sweep up
// after itself. A file that won't parse is worth a word — it means
// something wrote nonsense where a brake belongs — but it still reads as
// "not snoozed": failing open is the direction that matches the rest of
// the heartbeat, where a broken read drops a project rather than the
// tick.
func ReadSnooze(root string, now time.Time) (until time.Time, snoozed bool, err error) {
	body, err := os.ReadFile(SnoozePath(root))
	if os.IsNotExist(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	until, err = time.Parse(time.RFC3339, strings.TrimSpace(string(body)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("serve: parse %s: %w", SnoozePath(root), err)
	}
	return until, now.Before(until), nil
}

// WriteSnooze holds sweeps until the given instant. Atomic — tmp then
// rename — so a tick reading mid-write never sees half a timestamp.
func WriteSnooze(root string, until time.Time) error {
	dir, err := repolock.EnsureDir(root)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "snooze.tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(until.Format(time.RFC3339) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, SnoozePath(root))
}

// ClearSnooze wakes the heartbeat now. Absent is success: "not snoozed"
// is the state the caller asked for either way.
func ClearSnooze(root string) error {
	err := os.Remove(SnoozePath(root))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// SnoozeClock spells a resume instant the way both dashes show it —
// "09:00", in the operator's own zone. A snooze is hours long and read
// against a wall clock ("is it awake by the time I'm up?"), so the time
// of day is the whole of what's useful; the date would be noise on every
// snooze that isn't overnight.
func SnoozeClock(until time.Time) string {
	return until.Local().Format("15:04")
}
