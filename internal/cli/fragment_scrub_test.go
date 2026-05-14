package cli

import (
	"regexp"
	"testing"

	moe "github.com/modulecollective/moe"
)

// TestFragmentsDoNotMentionNeighborCommands is the structural lock the
// stage-location header makes possible. Every embedded
// workflows/<wf>/<stage>.md fragment is scanned for `moe <wf> <stage'>`
// where <stage'> is any registered stage of <wf> — including the
// stage's own name. Fragments must stay on the lens (what to do at
// this stage); the location (where this stage sits, what the chain
// prompt will offer next) belongs to stageLocationSection, which is
// rendered from the DAG on every invocation. Drift between fragment
// prose and the workflow DAG — the staleness that motivated the
// header — becomes a failing test rather than silent narrative drift.
func TestFragmentsDoNotMentionNeighborCommands(t *testing.T) {
	for _, wfName := range WorkflowNames() {
		wf, err := LookupWorkflow(wfName)
		if err != nil {
			t.Fatalf("LookupWorkflow(%q): %v", wfName, err)
		}
		stages := wf.Stages()
		for _, stage := range stages {
			frag := moe.Stage(wfName, stage)
			if frag == "" {
				continue
			}
			for _, other := range stages {
				pat := regexp.MustCompile(`\bmoe ` + regexp.QuoteMeta(wfName) + ` ` + regexp.QuoteMeta(other) + `\b`)
				if loc := pat.FindStringIndex(frag); loc != nil {
					t.Errorf("workflows/%s/%s.md mentions `moe %s %s` (byte %d): transition language belongs in stageLocationSection, not in fragment prose",
						wfName, stage, wfName, other, loc[0])
				}
			}
		}
	}
}
