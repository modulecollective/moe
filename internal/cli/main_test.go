package cli

import (
	"context"
	"os"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
)

// TestMain keeps the orchestration suite independent of provider accounts.
func TestMain(m *testing.M) {
	// Existing orchestration tests use fake executors and must not launch
	// authenticated quota processes. Cooldown tests replace this seam.
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		return agent.Quota{Windows: []agent.QuotaWindow{{ID: "test", UsedPercent: 10}}}, nil
	}
	os.Exit(m.Run())
}
