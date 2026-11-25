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

func TestWorkflowCloseCommitFailureRetainsSourceAndEnvironment(t *testing.T) {
	for _, workflow := range []string{"sdlc", "chat", "pulse"} {
		t.Run(workflow, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "finish", workflow, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			installCloseResourceHooks(t, root, "tele")
			resources := allocateCloseResources(t, root, "tele", "finish")
			allow := failCommits(t, root)
			defer allow()
			var log bytes.Buffer
			if code := Run([]string{workflow, "close", "tele/finish"}, &log, &log); code == 0 {
				t.Fatalf("commit unexpectedly succeeded: %s", &log)
			}
			resources.assertRetained(t)
			if got := loadRun(t, root, "tele", "finish").Status; got != run.StatusInProgress {
				t.Fatalf("failed commit terminalized run: %s", got)
			}
		})
	}
}

func TestWorkflowCloseTeardownFailureKeepsTerminalTruthAndCache(t *testing.T) {
	for _, workflow := range []string{"sdlc", "chat", "pulse"} {
		t.Run(workflow, func(t *testing.T) {
			root := seedCloseFixture(t, "tele", "finish", workflow, run.StatusInProgress)
			t.Setenv("MOE_HOME", root)
			installCloseResourceHooks(t, root, "tele")
			resources := allocateCloseResources(t, root, "tele", "finish")
			original, err := os.ReadFile(filepath.Join(resources.clone, devEnvCacheRel))
			if err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(root, "projects", "tele", "hooks", devEnvTeardownDirRel, "10-resources.sh")
			writeFile(t, script, "#!/bin/sh\nexit 21\n")
			gittest.Run(t, root, "add", script)
			gittest.Run(t, root, "commit", "-m", "test: fail resource teardown")
			var log bytes.Buffer
			if code := Run([]string{workflow, "close", "tele/finish"}, &log, &log); code != 1 {
				t.Fatalf("cleanup failure exit=%d: %s", code, &log)
			}
			if !strings.Contains(log.String(), "is closed; cleanup failed") {
				t.Fatalf("terminal truth missing from error: %s", &log)
			}
			resources.assertRetained(t)
			if after, err := os.ReadFile(filepath.Join(resources.clone, devEnvCacheRel)); err != nil || !bytes.Equal(after, original) {
				t.Fatal("failure changed original cache")
			}
			assertCloseRecorded(t, root, "tele", "finish", workflow)
		})
	}
}
