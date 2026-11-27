package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/stylesheet"
)

// pulseAdmission is a single invocation's successful admission. Only the
// newly minted survey receives it; rides still pass ordinary stage admission.
type pulseAdmission struct{ backend, model string }

func surveySelection(root string, now time.Time) (string, string, time.Time, error) {
	sheet, err := stylesheet.Load(root)
	if err == nil {
		err = sheet.Validate(stylesheetVocab())
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	return selectStageAgent(root, sheet, stageSessionOpts{}, &run.Metadata{Workflow: pulseWorkflow}, pulseDoc, "", now)
}

func surveyAdmissionState(root string, now time.Time) run.SurveyAdmission {
	name, model, _, err := surveySelection(root, now)
	if err != nil {
		return run.SurveyAdmission{Error: err.Error()}
	}
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		return run.SurveyAdmission{Error: err.Error()}
	}
	return idx.SurveyQuota(root, name, model)
}

// commitSurveyRetry writes only the journal event. No synthetic run, canvas,
// work turn or completion is needed, and a failed commit leaves no dirt.
func commitSurveyRetry(root string, e run.QuotaRetry) error {
	block, err := quotaRetryTrailer(e)
	if err != nil {
		return err
	}
	block.Project, block.Consent = project.RootID, walkConsent()
	return repolock.With(root, repolock.Options{Purpose: "survey-quota"}, func() error {
		entries, err := git.Status(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.Untracked() {
				return fmt.Errorf("journal has uncommitted changes at %s", entry.Path)
			}
		}
		return git.Run(root, "commit", "--allow-empty", "-m", "quota retry: survey admission\n\n"+block.String())
	})
}

func admitPulseSurvey(root string, stderr io.Writer) (*pulseAdmission, int) {
	idx, err := run.BuildJournalIndex(root)
	if err != nil {
		moePrintf(stderr, "pulse: quota journal: %v\n", err)
		return nil, 1
	}
	retries, err := idx.SurveyRetries(root)
	if err != nil {
		moePrintf(stderr, "pulse: survey retries: %v\n", err)
		return nil, 1
	}
	observed := map[string]string{}
	for token, r := range retries {
		observed[token] = r.Fingerprint()
	}
	name, model, _, err := surveySelection(root, agentDefaultNow())
	if err != nil {
		moePrintf(stderr, "pulse: selection: %v\n", err)
		return nil, 1
	}
	g := newCooldownGate("moe pulse new", stderr)
	// Pulse surveys are headless even when explicitly invoked.
	g.openControls = func() (<-chan cooldownControl, func()) { return nil, func() {} }
	g.selection = func() (string, string, time.Time, error) { return surveySelection(root, g.now()) }
	code := stageCooldown(g, &name, &model,
		func(*run.Metadata) (string, string, error) {
			n, m, _, err := surveySelection(root, g.now())
			return n, m, err
		},
		func() (*run.Metadata, error) { return &run.Metadata{Workflow: pulseWorkflow}, nil }, stderr)
	if code == exitQuotaDeferred {
		id, err := run.NewSessionID()
		if err != nil {
			moePrintf(stderr, "pulse: retry token: %v\n", err)
			return nil, 1
		}
		d := g.deferred
		d.target.Project, d.target.Stage = project.RootID, pulseDoc
		for _, prior := range retries {
			if len(retries) == 1 && prior.Survey && prior.NotBefore.Equal(d.retryAt) && len(prior.Targets) == 1 && prior.Targets[0] == d.target {
				moePrintf(stderr, "pulse: deferred for quota — %s\n", idx.SurveyQuota(root, name, model).Summary(g.now()))
				return nil, code
			}
		}
		e := run.QuotaRetry{Survey: true, Project: project.RootID, Token: project.RootID + "/survey-" + id, NotBefore: d.retryAt, Targets: []run.QuotaTarget{d.target}, Resolves: observed}
		if err := commitSurveyRetry(root, e); err != nil {
			moePrintf(stderr, "pulse: record survey retry: %v\n", err)
			return nil, 1
		}
		moePrintf(stderr, "pulse: deferred for quota — %s\n", (run.SurveyAdmission{Backend: name, Model: model, Reason: d.target.Reason, NotBefore: d.retryAt}).Summary(g.now()))
		return nil, code
	}
	if code != 0 {
		return nil, code
	}
	if len(observed) != 0 {
		if err := commitSurveyRetry(root, run.QuotaRetry{Project: project.RootID, Resolves: observed}); err != nil {
			moePrintf(stderr, "pulse: resolve survey retry: %v\n", err)
			return nil, 1
		}
	}
	return &pulseAdmission{backend: name, model: model}, 0
}
