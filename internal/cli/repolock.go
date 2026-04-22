package cli

import (
	"github.com/modulecollective/moe/internal/repolock"
)

// withRepoLock acquires the bureaucracy-wide lock at <root>/.moe/lock,
// runs fn, and releases the lock. fn's error (or an acquire error) is
// returned. Every moe command that mutates the bureaucracy working
// tree or index wraps its mutation in this helper so concurrent
// invocations don't clobber each other.
//
// Long-lived operations (stage sessions) do NOT wrap their whole run
// here — they take the lock only around the short setup/close windows
// and run the body on a throwaway branch in a git worktree. See
// internal/session for that dance.
func withRepoLock(root string, opts repolock.Options, fn func() error) error {
	l, err := repolock.Acquire(root, opts)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}
