// Package banner renders the small set of one-line marks that frame
// moe-originated output in the operator's terminal — stage entry, stage
// exit, the dash header, and the dev-env / project-hook walker section
// headers. Content shapes live here; the styled-output primitives
// (TTY-aware Printf, NO_COLOR suppression) stay in cliout.
//
// One visual shape carries the run identity: the gradient one-liner
//
//	▓▒░ MINISTRY OF EVERYTHING ░▒▓  <suffix>
//
// rendered at every stage-session entry and at the top of every
// `moe dash`. The stage-exit footer mirrors the same gradient blocks
// (flipped) as a closing bookend so the top/bottom pair frames the
// stage block in scrollback without spending an extra line.
package banner

import (
	"bytes"
	"io"
	"time"

	"github.com/modulecollective/moe/internal/cliout"
)

// bar is the entry/dash mark: gradient block, label, gradient block.
// barOpen / barClose are the closing-bookend gradients used by the
// stage exit footer — same blocks, no inner label.
const (
	bar      = "▓▒░ MINISTRY OF EVERYTHING ░▒▓"
	barOpen  = "░▒▓"
	barClose = "▓▒░"
)

// StageEntry prints the top-of-stage banner: gradient mark, agent,
// workflow · stage, project + run. Fired once at the start of every
// stage session — the one place every stage-using verb funnels through.
func StageEntry(w io.Writer, agent, workflow, stage, project, run string) {
	cliout.Printf(w, "%s  [%s] %s · %s  ·  %s %s\n", bar, agent, workflow, stage, project, run)
}

// StageExit prints the stage-bottom footer. Flipped gradient blocks
// bookend a short status (`complete` when a commit landed, `no-op` for
// the "no document changes; nothing committed" return) plus the same
// project + run anchor as the entry. Skipped on error exits — pairing
// every error with a "complete" footer would be worse than the
// asymmetry.
func StageExit(w io.Writer, workflow, stage, project, runID string, committed bool) {
	status := "complete"
	if !committed {
		status = "no-op"
	}
	cliout.Printf(w, "%s %s %s  ·  %s %s %s\n", barOpen, stage, status, project, runID, barClose)
}

// Dash prints the dash-render mark, with the render timestamp appended
// after `dash`. The dash factory art used to carry its own sentence-case
// title with the timestamp; now that the banner is in scrollback the
// title line is gone and the timestamp lives here so the operator still
// sees when the dash was rendered.
func Dash(w io.Writer, now time.Time) {
	cliout.Printf(w, "%s  dash  %s\n", bar, now.Format("2006-01-02  15:04"))
}

// HookSection prints the section header for a hook-walker pass:
//
//	▸ <label>: <n> scripts in <dirRel>
//
// Called once per walker (dev-env setup, dev-env teardown, pre-push)
// before any script runs. Walkers that find no scripts skip this entry
// — empty walkers stay silent.
func HookSection(w io.Writer, label string, scriptCount int, dirRel string) {
	noun := "scripts"
	if scriptCount == 1 {
		noun = "script"
	}
	cliout.Printf(w, "▸ %s: %d %s in %s\n", label, scriptCount, noun, dirRel)
}

// HookCacheHit prints the dev-env "cache was reused, no setup ran" line.
// Replaces today's silent short-circuit so the operator can tell a fast
// stage open apart from one that re-ran the scripts.
func HookCacheHit(w io.Writer, label, cacheRel string) {
	cliout.Printf(w, "▸ %s cached (%s)\n", label, cacheRel)
}

// HookStart prints the per-script header that opens one script's
// output block.
func HookStart(w io.Writer, script string) {
	cliout.Printf(w, "→ %s\n", script)
}

// HookDone prints the per-script footer with wall-clock elapsed. One
// decimal place of seconds is enough resolution for setup scripts.
func HookDone(w io.Writer, script string, elapsed time.Duration) {
	cliout.Printf(w, "← %s (%.1fs)\n", script, elapsed.Seconds())
}

// IndentStderr wraps w so each line of script output is prefixed by
// two spaces — visually nesting the script's stderr under its HookStart
// header. Non-TTY destinations (test buffers, CI pipes) pass through
// unchanged so assertions on raw script output don't break.
func IndentStderr(w io.Writer) io.Writer {
	if !cliout.IsTTY(w) {
		return w
	}
	return &indenter{w: w, atStart: true}
}

// indenter prefixes each new line written through it with "  ". The
// atStart flag tracks whether the next byte begins a fresh line; bytes
// that immediately follow a `\n` get the prefix.
type indenter struct {
	w       io.Writer
	atStart bool
}

func (i *indenter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var buf bytes.Buffer
	for _, b := range p {
		if i.atStart && b != '\n' {
			buf.WriteString("  ")
			i.atStart = false
		}
		buf.WriteByte(b)
		if b == '\n' {
			i.atStart = true
		}
	}
	if _, err := i.w.Write(buf.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

var _ io.Writer = (*indenter)(nil)
