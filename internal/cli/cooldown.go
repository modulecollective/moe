package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/run"
)

// Stage quota cooldown: before a top-level agent turn starts, read the
// selected backend's subscription usage and, when a window is nearly
// spent, wait for it to reset instead of starting a turn the provider
// is about to cut off. Pacing, not reservation — five percent headroom
// can't promise a stage finishes, and other devices draw on the same
// quota after the check.
//
// The policy is fixed on purpose: one threshold, one per-stage budget,
// no knobs. The operator overrides per invocation (--no-cooldown, or a
// key at the attached terminal), never by configuration.
const (
	// cooldownThreshold is the used-percent at which a window blocks.
	cooldownThreshold = 95.0
	// cooldownBudget caps the total wait for one stage. A reset already
	// further away than what's left of it stops the stage at once rather
	// than sitting through it; repeated reads never renew it.
	cooldownBudget = 6 * time.Hour
	// cooldownReadTimeout bounds one quota subprocess.
	cooldownReadTimeout = 20 * time.Second
	// cooldownResetSlack is added to a reset time before re-reading, so
	// the read doesn't land on the boundary itself.
	cooldownResetSlack = 5 * time.Second
	// cooldownPollInterval paces re-reads when the provider gave no
	// usable reset (missing, already past, an explicit denial with no
	// window) and between transient read failures.
	cooldownPollInterval = time.Minute
	// cooldownReadRetries is how many transient read failures in a row
	// are retried before the stage stops.
	cooldownReadRetries = 2
)

// exitNotStarted is the exit code for a stage the cooldown refused to
// start: the quota stayed spent past the budget, the check itself
// couldn't be read, or the run went terminal while the stage waited. No
// agent ran and nothing was committed, so it is neither a stage failure
// (1) nor an operator interrupt (130); the cascade summary reads it as
// "not started". 75 is EX_TEMPFAIL — try again later.
const exitNotStarted = 75

// cooldownSkipped is the invocation-wide bypass: set by --no-cooldown at
// launch or by `a` at a waiting prompt, and never reset — "skip checks
// for the rest of this invocation" means the MoE process, including
// every stage a cascade or chain ride dispatches after it.
var cooldownSkipped atomic.Bool

// readStageQuota is the seam the gate reads through. Production asks
// the backend under the per-read deadline; tests swap it (TestMain
// admits by default so unrelated stage tests never spawn a probe).
var readStageQuota = func(ctx context.Context, agentName, model string, extraEnv []string) (agent.Quota, error) {
	a, err := agent.Get(agentName)
	if err != nil {
		return agent.Quota{}, err
	}
	qr, ok := a.(agent.QuotaReader)
	if !ok {
		return agent.Quota{}, fmt.Errorf("%w: backend %s has no quota reader", agent.ErrQuotaUnsupported, agentName)
	}
	ctx, cancel := context.WithTimeout(ctx, cooldownReadTimeout)
	defer cancel()
	return qr.ReadQuota(ctx, agent.QuotaRequest{Model: model, ExtraEnv: extraEnv})
}

// cooldownControl is one operator command typed at a waiting stage.
type cooldownControl int

const (
	// controlContinue starts this stage now; the next stage checks again.
	controlContinue cooldownControl = iota + 1
	// controlSkipAll starts this stage and skips every later check in
	// this invocation.
	controlSkipAll
	// controlCancel stops the stage, same as Ctrl-C.
	controlCancel
)

// parseCooldownControl maps one typed line to a control.
func parseCooldownControl(line string) (cooldownControl, bool) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "c":
		return controlContinue, true
	case "a":
		return controlSkipAll, true
	case "q":
		return controlCancel, true
	}
	return 0, false
}

// cooldownOutcome is how one gate check ended.
type cooldownOutcome int

const (
	cooldownAdmit cooldownOutcome = iota
	cooldownCancel
	cooldownStop
)

// cooldownGate is the wait policy for one stage. One gate per stage, so
// its budget spans every read the stage makes — including a re-check
// after the backend selection changed during a wait.
type cooldownGate struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	read  func(ctx context.Context, agentName, model string) (agent.Quota, error)
	// openControls starts the attached-terminal reader for the whole
	// check, first read included, so a control typed during a slow read
	// is honoured too. It returns a nil channel when the invocation is
	// unattended. The returned stop must not return until the reader has
	// let go of stdin.
	openControls func() (<-chan cooldownControl, func())
	stderr       io.Writer
	// resume is the ordinary invocation that re-runs this stage, printed
	// when the gate stops it.
	resume string

	// refreshChanged is set by the resolver when authentication env changes.
	refreshChanged bool
	start          time.Time
	bypassed       bool
}

var newCooldownGate = func(resume string, stderr io.Writer) *cooldownGate {
	g := &cooldownGate{
		now:   time.Now,
		after: time.After,
		read: func(ctx context.Context, name, model string) (agent.Quota, error) {
			return readStageQuota(ctx, name, model, nil)
		},
		openControls: openCooldownControls,
		stderr:       stderr,
		resume:       resume,
	}
	g.start = g.now()
	return g
}

// check reads quota for the selected backend and waits while it is
// spent. waited reports whether any time passed asleep — the caller's
// cue that main may have moved and the selection must be re-resolved.
func (g *cooldownGate) check(ctx context.Context, agentName, model string) (outcome cooldownOutcome, waited bool) {
	if ctx.Err() != nil {
		return cooldownCancel, false
	}
	if cooldownSkipped.Load() || g.bypassed {
		return cooldownAdmit, false
	}
	controls, stopControls := g.openControls()
	// Terminal readers must have let go of stdin before the agent is
	// dispatched — a goroutine still blocked on it would steal the
	// interactive agent's keystrokes.
	defer func() { stopControls() }()

	failures := 0
	lastStatus := ""
	for {
		if g.now().Sub(g.start) >= cooldownBudget {
			return g.stop(agentName + " exceeded the six-hour cooldown cap"), waited
		}
		q, ctl, err := g.readOnce(ctx, agentName, model, controls)
		if ctx.Err() != nil {
			return cooldownCancel, false
		}
		if ctl != 0 {
			return g.control(ctl), true
		}
		if g.now().Sub(g.start) >= cooldownBudget {
			return g.stop(agentName + " exceeded the six-hour cooldown cap"), waited
		}
		var pause time.Duration
		var status string
		if err != nil {
			if agent.QuotaPermanent(err) {
				return g.stop(fmt.Sprintf("%s quota check failed: %v", agentName, err)), waited
			}
			failures++
			if failures > cooldownReadRetries {
				return g.stop(fmt.Sprintf("%s quota check failed %d times: %v", agentName, failures, err)), waited
			}
			pause = cooldownPollInterval
			status = fmt.Sprintf("%s quota check failed (%v); retrying in %s", agentName, err, pause)
		} else {
			failures = 0
			for _, w := range q.Unresolved {
				if w.UsedPercent >= cooldownThreshold {
					return g.stop(fmt.Sprintf("%s %s %.0f%%: cannot identify which model this limit applies to", agentName, w.ID, w.UsedPercent)), waited
				}
			}
			blocking, until, poll := quotaBlock(q, g.now())
			if len(blocking) == 0 && !q.Denied {
				if waited {
					moePrintf(g.stderr, "cooldown: %s quota recovered; starting\n", agentName)
				}
				return cooldownAdmit, waited
			}
			reason := describeQuotaBlock(agentName, blocking, q)
			if poll {
				pause = cooldownPollInterval
			} else {
				pause = until.Add(cooldownResetSlack).Sub(g.now())
			}
			if remaining := cooldownBudget - g.now().Sub(g.start); (!until.IsZero() && until.Add(cooldownResetSlack).Sub(g.now()) > remaining) || pause > remaining {
				return g.stop(fmt.Sprintf("%s — beyond the %s cooldown cap", reason, formatCap(cooldownBudget))), waited
			}
			if poll {
				status = fmt.Sprintf("%s; checking again in %s", reason, pause)
			} else {
				status = fmt.Sprintf("%s; waiting until %s", reason, until.Local().Format("Mon 15:04"))
			}
		}
		if pause > cooldownBudget-g.now().Sub(g.start) {
			return g.stop(agentName + " exceeded the six-hour cooldown cap"), waited
		}
		// One line per change of reason, not a countdown: a poll that
		// re-reads the same blocked window stays quiet.
		if status != lastStatus {
			hint := "Ctrl-C cancels; re-run with --no-cooldown to skip the check"
			if controls != nil {
				hint = "c⏎ start now · a⏎ skip checks for this invocation · q⏎ or Ctrl-C cancel"
			}
			moePrintf(g.stderr, "cooldown: %s\n  %s\n", status, hint)
			lastStatus = status
		}
		waited = true
		select {
		case <-ctx.Done():
			return cooldownCancel, false
		case ctl := <-controls:
			return g.control(ctl), waited
		case <-g.after(pause):
			waited = true
		}
	}
}

// readOnce runs one quota read while still answering Ctrl-C and the
// operator's controls. A control or a cancel abandons the read; the
// read's own context kills its subprocess.
func (g *cooldownGate) readOnce(ctx context.Context, agentName, model string, controls <-chan cooldownControl) (agent.Quota, cooldownControl, error) {
	readCtx, cancel := context.WithTimeout(ctx, min(cooldownReadTimeout, cooldownBudget-g.now().Sub(g.start)))
	defer cancel()
	type result struct {
		q   agent.Quota
		err error
	}
	done := make(chan result, 1)
	go func() {
		q, err := g.read(readCtx, agentName, model)
		done <- result{q, err}
	}()
	select {
	case r := <-done:
		return r.q, 0, r.err
	case <-ctx.Done():
	case ctl := <-controls:
		cancel()
		<-done
		return agent.Quota{}, ctl, nil
	}
	cancel()
	<-done
	return agent.Quota{}, 0, ctx.Err()
}

func (g *cooldownGate) control(ctl cooldownControl) cooldownOutcome {
	switch ctl {
	case controlContinue:
		g.bypassed = true
		moePrintf(g.stderr, "cooldown: starting this stage without waiting\n")
	case controlSkipAll:
		cooldownSkipped.Store(true)
		moePrintf(g.stderr, "cooldown: starting; quota checks are off for the rest of this invocation\n")
	default:
		return cooldownCancel
	}
	return cooldownAdmit
}

func (g *cooldownGate) stop(reason string) cooldownOutcome {
	moePrintf(g.stderr, "not started: quota — %s\n  resume: %s  (add --no-cooldown to skip the check)\n", reason, g.resume)
	return cooldownStop
}

// quotaBlock picks the windows at or past the threshold and the moment
// all of them could have reset. poll is true when that moment isn't
// knowable — a blocking window with no reset or one already past, or an
// explicit denial with no blocking window to wait out — so the caller
// re-reads on the poll interval rather than spinning or guessing.
func quotaBlock(q agent.Quota, now time.Time) (blocking []agent.QuotaWindow, until time.Time, poll bool) {
	for _, w := range q.Windows {
		if w.UsedPercent < cooldownThreshold {
			continue
		}
		blocking = append(blocking, w)
		if w.ResetsAt.IsZero() || !w.ResetsAt.After(now) {
			poll = true
			continue
		}
		if w.ResetsAt.After(until) {
			until = w.ResetsAt
		}
	}
	if len(blocking) == 0 && q.Denied {
		poll = true
	}
	return blocking, until, poll
}

func describeQuotaBlock(agentName string, blocking []agent.QuotaWindow, q agent.Quota) string {
	parts := make([]string, 0, len(blocking)+1)
	for _, w := range blocking {
		s := fmt.Sprintf("%s %.0f%%", w.ID, w.UsedPercent)
		if !w.ResetsAt.IsZero() {
			s += " (resets " + w.ResetsAt.Local().Format("Mon 15:04") + ")"
		}
		parts = append(parts, s)
	}
	if q.Denied {
		parts = append(parts, "provider denies usage: "+q.Reason)
	}
	return agentName + " " + strings.Join(parts, ", ")
}

func formatCap(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimSuffix(d.String(), "0s"), "0m")
}

// stageCooldown is the pre-stage gate runStageTurn calls once the
// session is reserved and before anything is written to it. It returns
// 0 to admit, exitInterrupted for a cancel, exitNotStarted for a stop.
//
// selection points at the turn's resolved backend and model; when a wait
// passed, the session is reconciled with current main and the selection
// re-resolved from the fresh run state. A changed selection gets its own
// read under the same budget — no automatic switching, but an operator
// who switched backend mid-wait gets the backend they chose, checked.
func stageCooldown(g *cooldownGate, agentName, model *string, resolve func(*run.Metadata) (string, string, error), reconcile func() (*run.Metadata, error), stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for {
		outcome, waited := g.check(ctx, *agentName, *model)
		switch outcome {
		case cooldownCancel:
			moePrintf(stderr, "cooldown: cancelled; stage not started\n")
			return exitInterrupted
		case cooldownStop:
			return exitNotStarted
		}
		if !waited {
			return 0
		}
		md, err := reconcile()
		if err != nil {
			moePrintf(stderr, "not started: reconcile after cooldown: %v\n", err)
			return exitNotStarted
		}
		if terminal := []string{run.StatusClosed, run.StatusMerged, run.StatusPromoted}; slices.Contains(terminal, md.Status) {
			moePrintf(stderr, "not started: %s/%s became %s while waiting\n", md.Project, md.ID, md.Status)
			return exitNotStarted
		}
		nextAgent, nextModel, err := resolve(md)
		if err != nil {
			moePrintf(stderr, "not started: %v\n", err)
			return exitNotStarted
		}
		if nextAgent == *agentName && nextModel == *model && !g.refreshChanged {
			return 0
		}
		if nextAgent == *agentName && nextModel == *model {
			moePrintf(stderr, "cooldown: stage environment changed while waiting; checking again\n")
		} else {
			moePrintf(stderr, "cooldown: stage configuration changed while waiting (%s → %s); checking again\n", selectionLabel(*agentName, *model), selectionLabel(nextAgent, nextModel))
		}
		*agentName, *model = nextAgent, nextModel
		g.refreshChanged = false
	}
}

// selectionLabel renders a backend/model pair for the operator; the
// backend's default model has no name to print.
func selectionLabel(agentName, model string) string {
	if model == "" {
		return agentName
	}
	return agentName + " " + model
}

// cooldownControlsAttached reports whether this invocation has an
// operator at the terminal to type controls. A serve- or Rex-hosted
// child gets a PTY nobody types into, so isatty alone can't decide;
// the serve handshake rules it out first.
func cooldownControlsAttached() bool {
	if serveAgentSuppress() {
		return false
	}
	return stdinIsTerminal()
}
