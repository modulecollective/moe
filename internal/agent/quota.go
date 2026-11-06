package agent

import (
	"context"
	"errors"
	"time"
)

// QuotaWindow is one subscription usage window. Its containing collection
// records whether it applies to the turn. ID is a structural bucket name
// (claude's `five_hour`, codex's `codex.primary`), never a display
// label. UsedPercent is normalised to 0–100 at the adapter boundary.
// ResetsAt is zero when the provider did not say.
type QuotaWindow struct {
	ID          string
	UsedPercent float64
	ResetsAt    time.Time
}

// Quota is one fresh usage reading. Windows holds only the buckets that
// apply to the requested backend/model — an adapter that cannot tell
// whether a bucket applies reports it in Unresolved.
// Denied is the provider's explicit "ordinary usage is not allowed right
// now", which can contradict a low percentage; Reason names it.
//
// A missing reading is never a zero reading: an adapter that cannot
// establish a reading returns an error, not an empty Quota.
type Quota struct {
	// Unresolved windows could apply to this stage, but lack a usable model
	// identity. Low readings can admit safely; high readings cannot justify
	// waiting on a potentially unrelated model and must report unavailable.
	Unresolved []QuotaWindow
	Windows    []QuotaWindow
	Denied     bool
	Reason     string
}

// QuotaRequest is what a quota read needs to authenticate as the turn
// it guards would. Model is the resolved `--model` value ("" = the
// backend's default); ExtraEnv rides the same environment filter the
// executor applies.
type QuotaRequest struct {
	Model    string
	ExtraEnv []string
}

// QuotaReader is implemented by backends that can read their
// subscription usage without running inference. A short-lived
// subprocess per read; ctx bounds it and kills it on cancel.
type QuotaReader interface {
	ReadQuota(ctx context.Context, r QuotaRequest) (Quota, error)
}

var (
	// ErrQuotaUnsupported reports a backend, auth mode or CLI version
	// with no usable usage interface. Permanent: retrying won't help.
	ErrQuotaUnsupported = errors.New("quota reading unsupported")
	// ErrQuotaMalformed reports a reply that is missing required data or
	// fails to parse. Permanent for the same reason.
	ErrQuotaMalformed = errors.New("quota reply malformed")
)

// QuotaPermanent reports whether err is a read failure that retrying
// cannot fix. Anything else (a timeout, a child that died, a transport
// error) is treated as transient.
func QuotaPermanent(err error) bool {
	return errors.Is(err, ErrQuotaUnsupported) || errors.Is(err, ErrQuotaMalformed)
}
