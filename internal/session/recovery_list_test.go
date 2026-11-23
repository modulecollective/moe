package session

import (
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestListForRecovery(t *testing.T) {
	root := newTestRoot(t)
	entries, err := ListForRecovery(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty inventory: %+v, %v", entries, err)
	}
	s, err := Open(root, "moe", "registered", "design")
	if err != nil {
		t.Fatal(err)
	}
	orphan := "session/moe/orphan/design"
	gittest.Run(t, root, "branch", orphan)
	entries, err = ListForRecovery(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("mixed inventory: %+v, %v", entries, err)
	}
	paths := make(map[string]string)
	for _, entry := range entries {
		paths[entry.Branch] = entry.WorktreePath
	}
	if path, ok := paths[orphan]; !ok || path != "" {
		t.Fatalf("orphan path = %q, present = %v", path, ok)
	}
	if paths[s.Branch] != s.WorktreePath {
		t.Fatalf("registered path = %q", paths[s.Branch])
	}
	managed, err := List(root)
	if err != nil || len(managed) != 1 || managed[0].Branch != s.Branch {
		t.Fatalf("managed List contract changed: %+v, %v", managed, err)
	}
}
