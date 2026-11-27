package run

import (
	"fmt"
	"time"

	"github.com/modulecollective/moe/internal/project"
)

// SurveyRetries recognizes both run-independent admissions and legacy skipped
// root surveys. A root ride is not a survey merely because it uses _cabinet.
func (idx *JournalIndex) SurveyRetries(root string) (map[string]QuotaRetry, error) {
	out := map[string]QuotaRetry{}
	for token, r := range idx.QuotaRetries {
		if r.Project != project.RootID {
			continue
		}
		if r.Survey {
			out[token] = r
			continue
		}
		for _, target := range r.Targets {
			if target.Project != project.RootID || target.Stage != "pulse" {
				continue
			}
			md, err := Load(root, target.Project, target.Run)
			if err != nil {
				return nil, err
			}
			if md.Workflow == "pulse" {
				out[token] = r
				break
			}
		}
	}
	return out, nil
}

// SurveyAdmission is recorded quota evidence for the current unbound survey,
// not a promise of provider capacity. It is shared by the clock and boards.
type SurveyAdmission struct {
	Backend, Model, Reason string
	NotBefore              time.Time
	Error                  string
}

func (s SurveyAdmission) Held(now time.Time) bool { return s.Error != "" || s.NotBefore.After(now) }

func (s SurveyAdmission) Summary(now time.Time) string {
	if s.Error != "" {
		return "survey quota status unavailable: " + s.Error
	}
	if s.NotBefore.IsZero() {
		return ""
	}
	label := s.Backend
	if s.Model != "" {
		label += " " + s.Model
	}
	if s.NotBefore.After(now) {
		return fmt.Sprintf("%s quota exhausted · %s · retry after %s", label, s.Reason, s.NotBefore.Local().Format("Mon 2 Jan 15:04 MST"))
	}
	return label + " · eligible for quota recheck"
}

// SurveyQuota uses the latest applicable deadline: an expired observation
// cannot release another still-future hold for the same selection.
func (idx *JournalIndex) SurveyQuota(root, backend, model string) SurveyAdmission {
	retries, err := idx.SurveyRetries(root)
	if err != nil {
		return SurveyAdmission{Error: err.Error()}
	}
	var out SurveyAdmission
	for _, r := range retries {
		for _, t := range r.Targets {
			if t.Backend == backend && t.Model == model && (out.NotBefore.IsZero() || r.NotBefore.After(out.NotBefore)) {
				out = SurveyAdmission{Backend: backend, Model: model, Reason: t.Reason, NotBefore: r.NotBefore}
			}
		}
	}
	return out
}
