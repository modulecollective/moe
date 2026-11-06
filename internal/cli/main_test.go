package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// TestMain keeps the orchestration suite independent of provider accounts.
func TestMain(m *testing.M) {
	// Existing orchestration tests use fake executors and must not launch
	// authenticated quota processes. Cooldown tests replace this seam.
	readStageQuota = func(context.Context, string, string, []string) (agent.Quota, error) {
		return agent.Quota{Windows: []agent.QuotaWindow{{ID: "test", UsedPercent: 10}}}, nil
	}
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "moe-cli-gitconfig-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test Git config:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	if err := gittest.WriteConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "test Git config:", err)
		return 1
	}
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": cfg,
		"GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "test Git config:", err)
			return 1
		}
	}
	return m.Run()
}
