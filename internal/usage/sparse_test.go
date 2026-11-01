package usage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
)

func TestSparseUsageMatchesFull(t *testing.T) {
	root := newTestBureaucracy(t)
	for _, p := range []string{"alpha", "beta"} {
		seedRun(t, root, p, "history", "sdlc", run.StatusInProgress, time.Now(), nil)
		// No canvas: sparse checkout removes this entire stage directory.
		seedThread(t, root, p, "history", "design", "claude", claudeTurn(p, "claude-opus-4-8", 10, 20, 30))
	}
	previous := git.Hook
	defer func() { git.Hook = previous }()
	lists, blobs := 0, 0
	git.Hook = func(_ string, args []string, _ time.Duration, _ error) {
		if args[0] == "ls-files" {
			lists++
		}
		if args[0] == "cat-file" {
			blobs++
		}
	}
	var warnings bytes.Buffer
	full, err := Gather(root, Filter{}, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if lists != 1 || blobs != 0 {
		t.Fatalf("full scan: %d index lists, %d blob reads", lists, blobs)
	}
	lists, blobs = 0, 0
	s, err := session.Open(root, "alpha", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	sparse, err := Gather(s.WorktreePath, Filter{}, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if lists != 1 || blobs != 2 {
		t.Fatalf("sparse scan: %d index lists, %d blob reads", lists, blobs)
	}
	if full.Transcripts != 2 || !reflect.DeepEqual(full, sparse) {
		t.Fatalf("full=%+v sparse=%+v", full, sparse)
	}
	for _, p := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(s.WorktreePath, run.ThreadPathFor("claude", p, "history", "design"))); !os.IsNotExist(err) {
			t.Fatal("usage hydrated transcript")
		}
	}
	if warnings.Len() != 0 {
		t.Fatal(warnings.String())
	}
}
