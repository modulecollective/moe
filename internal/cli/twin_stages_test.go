package cli

import (
	"bytes"
	"io"
	"testing"
)

// TestTwinStagesOpenReadOnlySandbox pins the capability this workflow
// exists to have: a reflect stage must be able to check a prose claim
// against the code that motivated it. Document-only twin stages could
// not — projects/<p>/src in the session worktree is an unpopulated
// submodule mountpoint, so the one grep that settles a naming question
// was impossible and the question got re-filed to feedback/twin.md pass
// after pass.
//
// The fix is the design/chat/pulse shape, and both flags are load-
// bearing: NeedsSandbox mints the per-run clone, EnforceSandboxBoundary
// keeps a reflect pass from editing the source it came to read. Twin
// never sets BoundaryAllowsCommits — unlike review, nothing here has a
// reason to commit — which is what earns the read-only prompt wording
// asserted in TestOperationalCoreReadOnlySandboxParagraph.
func TestTwinStagesOpenReadOnlySandbox(t *testing.T) {
	for _, stage := range []string{"vision", "architecture", "patterns", "operations", "glossary", "finalize"} {
		t.Run(stage, func(t *testing.T) {
			var got stageSessionOpts
			prev := runStageSession
			runStageSession = func(_, _, _ string, opts stageSessionOpts, _, _ io.Writer) int {
				got = opts
				return 0
			}
			t.Cleanup(func() { runStageSession = prev })

			var out, errb bytes.Buffer
			if code := runTwinStageSession(stage, "moe", "pass", true, "", &out, &errb); code != 0 {
				t.Fatalf("exit=%d stderr=%q", code, errb.String())
			}
			if !got.NeedsSandbox {
				t.Errorf("stage %q must open the per-run source clone", stage)
			}
			if !got.EnforceSandboxBoundary {
				t.Errorf("stage %q must refuse to close on a tracked-file change", stage)
			}
			if got.BoundaryAllowsCommits {
				t.Errorf("stage %q must not relax the boundary for commits", stage)
			}
		})
	}
}
