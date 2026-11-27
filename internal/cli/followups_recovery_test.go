package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
)

// Recovery bypasses the normal SDLC openers. Its saved blocked turns must
// extract before the readiness refusal, but failed turns must not extract.
func TestSDLCRecoveryTaggedFollowups(t *testing.T) {
	for _, tc := range []struct{ stage, destination string }{
		{"design", "review"},
		{"build", "review"},
		{"build", "push"},
	} {
		t.Run(tc.stage+"-to-"+tc.destination, func(t *testing.T) {
			for _, mode := range []string{"", "fail", "interrupt", "unchanged", "no-output", "hygiene", "harvest-fail"} {
				t.Run("mode="+mode, func(t *testing.T) {
					root, md := taggedTurnFixture(t)
					if tc.stage == "build" {
						writeStageCanvas(t, root, md, "design", readyDesign)
						gittest.Commit(t, root, "seed prior design")
					}
					t.Setenv("MOE_TEST_TAGGED_MODE", mode)
					var out bytes.Buffer
					code := openRecoveryStageSession(md, tc.stage, tc.destination, true, "Resolve the blocking finding", &out, &out)
					if code == 0 {
						t.Fatalf("recovery should park: %s", &out)
					}
					idea, err := run.Load(root, "tele", "required-fix")
					if mode != "" {
						if err == nil {
							t.Fatalf("failed %s recovery extracted %+v", mode, idea)
						}
						if mode == "hygiene" && !strings.Contains(out.String(), "findings survived the fix turn") {
							t.Fatalf("hygiene recovery not exercised: %s", &out)
						}
						if mode != "harvest-fail" {
							return
						}
						if !strings.Contains(out.String(), "harvest:") {
							t.Fatalf("harvest failure not exercised: %s", &out)
						}
						if !strings.Contains(readFollowups(t, root, "tele", md.ID), "- [ ] `missing/required-fix`") {
							t.Fatal("failed harvest lost the filing")
						}
					} else {
						if err != nil || idea.Workflow != "idea" || idea.PromoteTo != "sdlc" {
							t.Fatalf("saved blocked recovery stranded tagged prerequisite: %+v %v; %s", idea, err, &out)
						}
						if !strings.Contains(out.String(), tc.stage+" not ready — parked at "+tc.stage) {
							t.Fatalf("recovery did not reach readiness gate: %s", &out)
						}
						audit := readFollowups(t, root, "tele", md.ID)
						if !strings.Contains(audit, "- [x] `required-fix` (sdlc)") || !strings.Contains(audit, "- [x] `needs-judgment`") {
							t.Fatalf("audit=%s", audit)
						}
					}
					if got := gitLog(t, root, "--format=%s", "--grep=^Update "+tc.stage+" for "); got == "" {
						t.Fatalf("recovery stage commit lost: %s", &out)
					}
					wf, _ := LookupWorkflow("sdlc")
					if ok, err := wf.CheckStageGate(root, md, tc.stage); err != nil || ok {
						t.Fatalf("source gate=(%v,%v)", ok, err)
					}
					if runStatus(t, root, "tele", md.ID) != run.StatusInProgress {
						t.Fatal("source status changed")
					}
					if _, err := os.Stat(filepath.Join(root, run.ContentDir("tele", md.ID, tc.destination))); !os.IsNotExist(err) {
						t.Fatalf("destination stage started: %v", err)
					}
					if st := gittest.Output(t, root, "status", "--porcelain"); st != "" {
						t.Fatalf("dirty settled recovery: %s", st)
					}
				})
			}
		})
	}
}
