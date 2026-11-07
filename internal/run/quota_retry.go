package run

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// ExitQuotaDeferred identifies a clock sweep that yielded without a failure.
// It crosses both local process exits and Rex's recorded exit status.
const ExitQuotaDeferred = 76

// QuotaTarget records the stage actually selected at admission. It contains
// scheduling evidence only, never the provider response or authentication.
type QuotaTarget struct {
	Project string `json:"project"`
	Run     string `json:"run"`
	Stage   string `json:"stage"`
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	Reason  string `json:"reason"`
}

// QuotaRetry is a harness-authored journal event. A nonempty token installs
// or updates one pulse's obligation; Resolves names only tokens observed by
// the invocation on entry. Replacing a token never grants stage execution.
type QuotaRetry struct {
	Project   string        `json:"project"`
	Token     string        `json:"token,omitempty"`
	NotBefore time.Time     `json:"not_before,omitzero"`
	Targets   []QuotaTarget `json:"targets,omitempty"`
	// Resolves pins the observed version too: a concurrent update of the
	// same pulse's aggregate must survive an older survey's resolution.
	Resolves map[string]string `json:"resolves,omitempty"`
}

// Fingerprint identifies a projected event's scheduling evidence, independent
// of which older obligations it replaced. Projected times have passed JSON
// decoding, so they are representable by the encoder too.
func (r QuotaRetry) Fingerprint() string {
	r.Resolves = nil
	b, _ := json.Marshal(r)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// NextQuotaRetry returns the earliest outstanding obligation for a project.
func (idx *JournalIndex) NextQuotaRetry(project string) time.Time {
	var next time.Time
	for _, r := range idx.QuotaRetries {
		if r.Project == project && (next.IsZero() || r.NotBefore.Before(next)) {
			next = r.NotBefore
		}
	}
	return next
}
