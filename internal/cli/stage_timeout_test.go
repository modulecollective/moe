package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/agent"
)

// TestHeadlessTimeoutStampsTurnCommit is the end-to-end probe for this
// run: it proves the executor's error actually crosses into the commit.
// A headless design turn runs against a fake agent that writes its
// canvas and then reports a deadline kill — the real shape, since the
// dominant wedge cuts an agent that has already edited. The turn's own
// journal commit must carry MoE-Timed-Out with the cap that fired, and
// `git log --grep` must find it; the same fake returning nil must leave
// the trailer off.
//
// The transcript a real kill leaves is overwritten by the next drive of
// the stage, so this commit is the only durable record of the cut. If
// the plumbing between FinishOneShot and commitTurn ever breaks, the
// symptom is silence — hence a test at the seam rather than at either
// end.
func TestHeadlessTimeoutStampsTurnCommit(t *testing.T) {
	t.Run("timed out", func(t *testing.T) {
		root, fake := setupResumeFixture(t, "fake-timeout-stamp")
		fake.oneShotErr = &agent.TimeoutError{Label: "claude: -p", Timeout: 90 * time.Minute}

		if code := driveDesignTurnCode(t, "fake-timeout-stamp", true); code == 0 {
			t.Fatal("a killed turn should exit non-zero")
		}

		body := gitLogFormat(t, root, 1, "HEAD", "%B")
		if !strings.Contains(body, "MoE-Timed-Out: 1h30m0s") {
			t.Fatalf("turn commit missing the timeout stamp:\n%s", body)
		}
		if !strings.Contains(body, "MoE-Document: design") {
			t.Errorf("stamp should ride the turn's own commit, which names the stage:\n%s", body)
		}
	})

	t.Run("clean turn", func(t *testing.T) {
		root, _ := setupResumeFixture(t, "fake-no-timeout-stamp")

		driveDesignTurn(t, "fake-no-timeout-stamp", true)

		body := gitLogFormat(t, root, 1, "HEAD", "%B")
		if strings.Contains(body, "MoE-Timed-Out") {
			t.Fatalf("ordinary turn should carry no timeout stamp:\n%s", body)
		}
	})
}
