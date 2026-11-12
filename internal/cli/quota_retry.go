package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/trailers"
)

// clockQuota belongs to one heartbeat pulse, bound/restored like consent.
// Dispatchers keep integer results; admission carries typed details here.
var clockQuota *pulseQuotaRetries

type pulseQuotaRetries struct {
	root, project, pulse string
	observed             map[string]string
	event                run.QuotaRetry
	survey               bool
	// A claimed CI repair cannot be continued by a new sweep. Later
	// quota exhaustion yields, but must not issue a retry for that repair.
	repairClaimed bool
}

func newPulseQuotaRetries(root, project string) (*pulseQuotaRetries, error) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return nil, err
	}
	q := &pulseQuotaRetries{root: root, project: project, survey: true, observed: map[string]string{}}
	for token, r := range idx.QuotaRetries {
		if r.Project == project {
			q.observed[token] = r.Fingerprint()
		}
	}
	return q, nil
}

func quotaRetryTrailer(e run.QuotaRetry) (trailers.Block, error) {
	b, err := json.Marshal(e)
	return trailers.Block{QuotaRetry: string(b)}, err
}

func (q *pulseQuotaRetries) record(d quotaDeferral, stderr io.Writer) error {
	if q.repairClaimed {
		moePrintf(stderr, "pulse: CI repair %s/%s stopped at %s for quota; claim remains spent — fresh operator input is required\n", d.target.Project, d.target.Run, d.target.Stage)
		return nil
	}
	e := q.event
	e.Project, e.Token = q.project, q.project+"/"+q.pulse
	e.Resolves = q.observed
	if e.NotBefore.IsZero() || d.retryAt.Before(e.NotBefore) {
		e.NotBefore = d.retryAt
	}
	e.Targets = append(append([]run.QuotaTarget(nil), e.Targets...), d.target)
	if q.survey {
		block, err := quotaRetryTrailer(e)
		if err != nil {
			return err
		}
		note := pulseSkipNote(d.target.Reason) + "\n" + renderQuotaRetries(e)
		if err := closePulseRunStamped(q.root, q.project, q.pulse, "quota skip note", func([]byte) []byte { return []byte(note) }, stderr, block); err != nil {
			return err
		}
	} else if err := q.commit(e); err != nil {
		return err
	}
	q.event = e
	moePrintf(stderr, "pulse: deferred for quota — retry survey on the first heartbeat at or after %s\n", e.NotBefore.Format("2006-01-02 15:04:05 MST"))
	return nil
}

func pulseSkipNote(reason string) string {
	return "# Pulse\n\nSweep skipped — " + reason + ". No survey started.\nNo report; the previous pulse's report remains the baseline.\n"
}

func renderQuotaRetries(e run.QuotaRetry) string {
	var b strings.Builder
	b.WriteString("## Quota deferrals\n\n")
	fmt.Fprintf(&b, "Fresh survey eligible on the first heartbeat at or after %s.\n\n", e.NotBefore.UTC().Format("2006-01-02 15:04:05 MST"))
	for _, t := range e.Targets {
		fmt.Fprintf(&b, "- %s/%s %s (%s) — %s\n", t.Project, t.Run, t.Stage, selectionLabel(t.Backend, t.Model), t.Reason)
	}
	return b.String()
}

// commit updates only the closed pulse's harness section. On failure restore
// our bytes and staging so a failed record cannot dirty the whole journal.
func (q *pulseQuotaRetries) commit(e run.QuotaRetry) error {
	block, err := quotaRetryTrailer(e)
	if err != nil {
		return err
	}
	block.Run, block.Project, block.Workflow, block.Consent = q.pulse, q.project, pulseWorkflow, walkConsent()
	return repolock.With(q.root, repolock.Options{Purpose: "quota-retry", Run: q.project + "/" + q.pulse}, func() error {
		entries, err := git.Status(q.root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Untracked() {
				return fmt.Errorf("journal has uncommitted changes at %s", entry.Path)
			}
		}
		rel := run.ContentPath(q.project, q.pulse, pulseDoc)
		path := filepath.Join(q.root, rel)
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(before)
		if i := strings.Index(body, "\n## Quota deferrals\n"); i >= 0 {
			body = body[:i]
		}
		if e.Token != "" {
			body = strings.TrimRight(body, "\n") + "\n\n" + renderQuotaRetries(e)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
		err = run.Stage(q.root, rel)
		if err == nil {
			// A resolution changes scheduling evidence, not necessarily
			// canvas bytes, so the journal event may be an empty commit.
			err = git.Run(q.root, "commit", "--allow-empty", "-m", "quota retry: "+q.project+"/"+q.pulse+"\n\n"+block.String())
		}
		if err != nil {
			restoreErr := os.WriteFile(path, before, 0o644)
			resetErr := git.Run(q.root, "reset", "-q", "--", rel)
			if restoreErr != nil || resetErr != nil {
				return fmt.Errorf("%w (restore: %v; unstage: %v)", err, restoreErr, resetErr)
			}
		}
		return err
	})
}

func (q *pulseQuotaRetries) finish() error {
	if len(q.observed) == 0 || q.event.Token != "" {
		return nil
	}
	return q.commit(run.QuotaRetry{Project: q.project, Resolves: q.observed})
}

// Genuine failures dominate quota pacing; interruption stops the walk.
func combinePulseExit(current, next int) int {
	if next == exitInterrupted {
		return next
	}
	// A yield to operator control is the operator's decision, not a
	// failed ride: no backoff, and the sweep carries on.
	if next == exitYielded {
		return current
	}
	if next != 0 && (current == 0 || current == exitQuotaDeferred) {
		return next
	}
	return current
}
