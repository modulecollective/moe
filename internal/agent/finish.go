package agent

import "fmt"

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
// the exit status), then maps the exit to a timeout error when timedOut
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
		return sid, fmt.Errorf("%s timed out after %s", label, r.Timeout)
	}
	return sid, waitErr
}
