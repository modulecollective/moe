//go:build !linux

package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

func TestUnsupportedCloneApplyRetainsSourceStage(t *testing.T) {
	root := seedCloseFixture(t, "tele", "fresh-stage", "sdlc", run.StatusInProgress)
	seedProjectWithSubmodule(t, root, "tele")
	path := cleanTestClone(t, root, "tele", "fresh-stage")
	writeFile(t, filepath.Join(path, "experiment"), "retained work")
	result := collectClone(root, "tele", "fresh-stage", true, true, time.Now(), io.Discard, io.Discard)
	if result.Outcome != "held" || result.Reason != "unsupported_platform" {
		t.Fatalf("unsupported apply: %+v", result)
	}
	got, err := os.ReadFile(filepath.Join(path, "experiment"))
	if err != nil || string(got) != "retained work" {
		t.Fatalf("lost work: %q %v", got, err)
	}
}
