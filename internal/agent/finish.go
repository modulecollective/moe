package agent

import (
	"fmt"
	"time"
)

// TimeoutError is what FinishOneShot returns when a one-shot turn was
// killed by its own deadline rather than exiting on its own. It is the
// sibling of ErrInterrupted — the other end-of-turn moe classifies
// rather than merely prints — but a type instead of a sentinel because
// the durable record wants the cap that fired, not just the fact: the
// stage's own journal commit stamps `MoE-Timed-Out: <cap>`, so sizing
// the cap later is a `git log --grep` rather than an archaeology pass
// over transcript revisions.
//
// Error() renders the same text as the fmt.Errorf it replaced, so
// callers that string-match the message are unaffected.
type TimeoutError struct {
	// Label names the backend, as passed to FinishOneShot
	// ("claude: -p", "codex: exec").
	Label string
	// Timeout is the cap that fired — OneShotRequest.Timeout.
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Label, e.Timeout)
}

// FinishOneShot is the shared post-Wait tail for the one-shot backends
// (claude -p, codex exec). Both call it after the progress goroutine
// drains and the wait completes, so the timeout path can't diverge
// between backends again — the divergence this helper was extracted to
// kill was codex returning "" with no transcript mirror on a deadline
// kill while claude returned the sid and mirrored.
//
// It drains the captured session id off sidCh, mirrors the transcript
// to r.ThreadPath when both a destination and a sid are available
// (best-effort: a copy error surfaces on r.Stderr but never overrides
// the exit status), then maps the exit to a *TimeoutError when timedOut
// or the raw waitErr otherwise.
//
// timedOut is computed by the caller from its own ctx —
// waitErr != nil && r.Timeout > 0 && ctx.Err() == context.DeadlineExceeded.
// copyTranscript is the backend's package-level CopyTranscript. label
// names the backend in the timeout error ("claude: -p", "codex: exec").
func FinishOneShot(sidCh <-chan string, r OneShotRequest, timedOut bool, waitErr error, label string, copyTranscript func(sid, dest string) (bool, error)) (string, error) {
	var sid string
	select {
	case sid = <-sidCh:
	default:
	}
	if r.ThreadPath != "" && sid != "" {
		if _, err := copyTranscript(sid, r.ThreadPath); err != nil && r.Stderr != nil {
			fmt.Fprintf(r.Stderr, "save transcript: %v\n", err)
		}
	}
	if timedOut {
		return sid, &TimeoutError{Label: label, Timeout: r.Timeout}
	}
	return sid, waitErr
}
