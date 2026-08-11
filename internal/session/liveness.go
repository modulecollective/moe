package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modulecollective/moe/internal/repolock"
)

// A session branch says "a stage is open here" and nothing else. That
// was enough while every session had a human behind it: the branch
// outliving its process meant "go look". Under a resident heartbeat it
// isn't, because the two cases the branch collapses need opposite
// answers — a robot session whose process died may be abandoned and
// retried, a human's may only ever be surfaced.
//
// The liveness record is the missing half. It sits beside the branch,
// records who claimed the session and whether that claimant is still
// running, and is deleted by every clean ending (Close, Abandon). So a
// record that survives its process is exactly an orphan, and its
// Machine bit says whose orphan it is.
//
// Two rules make it safe to act on:
//
//   - **Absence is unknown, never operator.** No record — a session
//     opened by an older binary, a claim that failed to write — reads
//     as unreapable, same as a human's. The mark semantics the journal
//     already uses, applied to a file.
//   - **Dead means provably dead.** Same host, pid gone *and* heartbeat
//     stale. A machine walk the operator typed into a tmux pane is
//     someone else's child but very much alive, and reaping it would
//     pull the worktree out from under a running agent.
//
// The staleness shape is repolock's, deliberately: same owner string,
// same probe, same ambiguity-reads-as-alive rule.

// StaleAfter is the heartbeat age past which a claim stops vouching for
// its process. Generous next to repolock's twenty seconds because the
// cost profile is inverted: a lock takeover that guesses wrong wastes a
// wait, while a reap that guesses wrong destroys a live session's
// worktree. Ten minutes is far past any beat interval and far short of
// the sessions this exists to recover, which have been dead since the
// last box reboot.
const StaleAfter = 10 * time.Minute

// beatInterval is how often a claimed session rewrites heartbeat_at.
// Variable rather than const so tests can shorten it.
var beatInterval = 30 * time.Second

// Claim is the on-disk record of who is inside a session right now.
type Claim struct {
	Branch string `json:"branch"`
	// Machine reports that a machine walk opened this session — a bang
	// cascade, a chain kick, a heartbeat sweep. False means an operator
	// typed a bare stage verb; absent (no record at all) means unknown.
	// Only a true here makes a dead session reapable.
	Machine bool `json:"machine"`
	// Owner is "<host>/<pid>", repolock's shape.
	Owner       string    `json:"owner"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// ClaimPath is where a session's record lives. Under <root>/.moe, which
// carries a `*` .gitignore (repolock drops it on first acquire, and
// every session open runs under the lock), so records never dirty the
// tree.
//
// Exported because the record is machine-readable state a test — or a
// future dash surface — has legitimate reason to stage or inspect;
// writing one is deliberately not exported, since Hold is the only
// honest way to claim a session.
func ClaimPath(root, projectID, runID, docID string) string {
	return filepath.Join(root, ".moe", "claims", projectID, runID, docID+".json")
}

// Hold claims s for this process: writes the record and starts the
// heartbeat. The returned release stops the heartbeat; it does *not*
// delete the record, because a turn that ends without Close or Abandon
// is precisely the orphan the reap looks for. Close and Abandon delete.
//
// machine is the caller's own knowledge of who is driving — cli passes
// its ride-walk flag. Best-effort by construction: a claim that can't
// be written leaves the session unmarked, which reads as unknown and so
// is never reaped. Losing the record can only ever cost a retry.
func Hold(s *Session, machine bool) (release func(), err error) {
	host, hostErr := os.Hostname()
	if hostErr != nil {
		// An unidentifiable host is not fatal — it just makes the record
		// unreapable, since Dead requires a same-host owner.
		host = ""
	}
	now := time.Now().UTC()
	c := Claim{
		Branch:      s.Branch,
		Machine:     machine,
		Owner:       repolock.OwnerString(host),
		StartedAt:   now,
		HeartbeatAt: now,
	}
	path := ClaimPath(s.Root, s.Project, s.Run, s.Doc)
	if err := writeClaim(path, c); err != nil {
		return func() {}, err
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(beatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				c.HeartbeatAt = time.Now().UTC()
				// A failed beat is silent: the record only ages, and an aged
				// record on a live pid still reads as alive.
				_ = writeClaim(path, c)
			}
		}
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		close(stop)
	}, nil
}

// ReadClaim returns the record for a session, or ok=false when there is
// none (including an unreadable or malformed one — both read as
// unknown, which is the safe answer).
func ReadClaim(root, projectID, runID, docID string) (Claim, bool) {
	body, err := os.ReadFile(ClaimPath(root, projectID, runID, docID))
	if err != nil {
		return Claim{}, false
	}
	var c Claim
	if err := json.Unmarshal(body, &c); err != nil {
		return Claim{}, false
	}
	return c, true
}

// clearClaim removes a session's record. Called by the clean endings;
// a missing file is success.
func clearClaim(s *Session) {
	_ = os.Remove(ClaimPath(s.Root, s.Project, s.Run, s.Doc))
}

// Reapable reports whether s is an orphan a machine may abandon: a
// machine-marked claim whose process is provably gone. Every other
// shape — no claim, an operator claim, a claim owned by another host, a
// live pid, a fresh heartbeat — returns false, and those sessions are
// surfaced rather than touched.
//
// Both dead signals are required, not either. The pid probe alone
// misfires on pid reuse; the heartbeat alone misfires on a process
// stopped at a debugger or starved off the scheduler. Together they are
// the "provably dead" the operator's rule asks for.
func Reapable(s *Session, now time.Time) bool {
	c, ok := ReadClaim(s.Root, s.Project, s.Run, s.Doc)
	if !ok || !c.Machine {
		return false
	}
	host, err := os.Hostname()
	if err != nil {
		return false
	}
	owner, pid, ok := repolock.ParseOwner(c.Owner)
	if !ok || owner != host {
		// Another box's claim, or one we can't parse. We cannot probe its
		// pid, so we cannot prove it dead.
		return false
	}
	if repolock.ProcessAlive(pid) {
		return false
	}
	if c.HeartbeatAt.IsZero() || now.Sub(c.HeartbeatAt) <= StaleAfter {
		return false
	}
	return true
}

func writeClaim(path string, c Claim) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("session: mkdir claim dir: %w", err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("session: marshal claim: %w", err)
	}
	// Atomic swap, so a concurrent reader never sees a half-written
	// record — the same tmp+rename discipline repolock's writer uses.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("session: write claim: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("session: rename claim: %w", err)
	}
	return nil
}
