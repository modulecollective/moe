package run

import (
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/trailers"
)

func TestFollowupDestination(t *testing.T) {
	root := newTestRoot(t)
	key := "tele/source/other/fix"
	p, id, err := FollowupDestination(root, key)
	if err != nil || p != "" || id != "" {
		t.Fatalf("absent receipt=%s/%s %v", p, id, err)
	}
	receipt := func(project, run string) {
		t.Helper()
		gittest.Run(t, root, "commit", "--allow-empty", "-m", "Open run\n\n"+trailers.Block{Project: project, Run: run, FollowupKey: key}.String())
	}
	receipt("other", "fix-2")
	// A lifecycle event and another source's receipt cannot redirect this key.
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "close\n\n"+trailers.Block{Project: "other", Run: "fix-2", Closed: "true"}.String())
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "Open run\n\n"+trailers.Block{Project: "other", Run: "fix-3", FollowupKey: "tele/source-2/other/fix"}.String())
	p, id, err = FollowupDestination(root, key)
	if err != nil || p != "other" || id != "fix-2" {
		t.Fatalf("receipt=%s/%s %v", p, id, err)
	}
	receipt("other", "fix-4")
	if _, _, err := FollowupDestination(root, key); err == nil || !strings.Contains(err.Error(), "inconsistent destinations") {
		t.Fatalf("ambiguous receipt=%v", err)
	}
}

func TestFollowupDestinationRejectsInvalidBoundary(t *testing.T) {
	root := newTestRoot(t)
	for _, key := range []string{"tele/source/fix", "tele/source/other/fix/extra", "tele/../other/fix", "Tele/source/other/fix", "tele/source//fix"} {
		if _, _, err := FollowupDestination(root, key); err == nil {
			t.Fatalf("accepted invalid key %q", key)
		}
	}
	key := "tele/source/other/fix"
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "bad receipt\n\n"+trailers.Block{Project: "wrong", Run: "fix", FollowupKey: key}.String())
	if _, _, err := FollowupDestination(root, key); err == nil || !strings.Contains(err.Error(), "invalid destination") {
		t.Fatalf("malformed destination=%v", err)
	}
}
