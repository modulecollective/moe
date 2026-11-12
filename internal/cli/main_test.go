package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/runsock"
	"github.com/modulecollective/moe/internal/runsock/runsocktest"
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
	// Every execution owner serves a run endpoint, and agent sandboxes
	// refuse AF_UNIX: run the package over an in-memory socket namespace
	// in a private runtime dir.
	sockets := runsocktest.New()
	defer runsock.SetTransport(sockets.Listen, sockets.Dial)()
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": cfg,
		"XDG_RUNTIME_DIR":   dir,
		"GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "test Git config:", err)
			return 1
		}
	}
	return m.Run()
}
