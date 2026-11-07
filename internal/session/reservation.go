package session

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/git"
)

// UnresolvedError names durable reservations, not necessarily live processes.
type UnresolvedError struct {
	Project, Run string
	Branches     []string
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("run %s/%s has unresolved sessions: %s\n"+
		"finish the turn normally; for an interrupted turn, inspect and preserve its work,\n"+
		"then resume the stage or use `moe session resolve <branch>`; use\n"+
		"`moe session abandon <branch>` only after stopping its owner and choosing to discard its work",
		e.Project, e.Run, strings.Join(e.Branches, ", "))
}

// RunReservations strictly reads refs, including orphan and retired-document
// refs. It acquires no lock; mutation/admission callers must hold repolock.
func RunReservations(root, projectID, runID string) ([]string, error) {
	prefix := "refs/heads/session/" + projectID + "/" + runID + "/"
	out, err := git.Output(root, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return nil, fmt.Errorf("read run session reservations: %w", err)
	}
	var branches []string
	for ref := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(ref, prefix) {
			branches = append(branches, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return branches, nil
}

// RequireUnreserved fences destructive run mutations. Claim age never grants
// permission to discard the work represented by a surviving ref.
func RequireUnreserved(root, projectID, runID string) error {
	branches, err := RunReservations(root, projectID, runID)
	if err != nil {
		return err
	}
	if len(branches) != 0 {
		return &UnresolvedError{Project: projectID, Run: runID, Branches: branches}
	}
	return nil
}

// Reserve inspects, opens/resumes and publishes the initial claim atomically
// under the caller's repository lock. Workflow entry policy stays with callers.
func Reserve(root, projectID, runID, docID string, machine bool) (*Session, func(), error) {
	branches, err := RunReservations(root, projectID, runID)
	if err != nil {
		return nil, nil, err
	}
	if len(branches) != 0 {
		c, ok := ReadClaim(root, projectID, runID, docID)
		if len(branches) != 1 || branches[0] != BranchName(projectID, runID, docID) || !ok || c.Branch != branches[0] || !claimDead(c, time.Now()) {
			return nil, nil, &UnresolvedError{Project: projectID, Run: runID, Branches: branches}
		}
	}
	s, err := Open(root, projectID, runID, docID)
	if err != nil {
		return nil, nil, err
	}
	release, err := Hold(s, machine)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("claim session %s: %w", s.Branch, err), CancelReservation(s))
	}
	return s, release, nil
}
