package chore

import (
	"slices"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

// countEditWalks installs a git.Hook that counts the definition-edit
// walks (`git log -1 --format=%ct -- <path>`) and returns the counter.
func countEditWalks(t *testing.T) *int {
	t.Helper()
	previous := git.Hook
	t.Cleanup(func() { git.Hook = previous })
	n := new(int)
	git.Hook = func(_ string, args []string, _ time.Duration, _ error) {
		if len(args) > 0 && args[0] == "log" && slices.Contains(args, "--format=%ct") {
			*n++
		}
	}
	return n
}

// commitAt stages everything and commits with both dates pinned, so
// EditedAt is a known value rather than "now, to the second".
func commitAt(t *testing.T, root, msg string, when time.Time) {
	t.Helper()
	stamp := when.Format(time.RFC3339)
	gittest.Run(t, root, "add", "-A")
	gittest.RunWithEnv(t, root, []string{
		"GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_DATE=" + stamp,
	}, "commit", "--allow-empty", "-q", "-m", msg)
}

func loadEditedAt(t *testing.T, root string) map[string]time.Time {
	t.Helper()
	defs, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	got := map[string]time.Time{}
	for _, d := range defs {
		got[d.Key()] = d.EditedAt
	}
	return got
}

// TestLastDefinitionEditMemoizedOnHEAD pins the memo's contract against
// the real LoadAll path: a still HEAD serves every definition from the
// memo, a commit under a chore re-walks and advances that chore's
// EditedAt, and a commit elsewhere also re-walks (the key is HEAD, not
// the path) while leaving EditedAt where it was.
func TestLastDefinitionEditMemoizedOnHEAD(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeChore(t, root, "projects/moe/project.json", `{"id":"moe"}`)
	writeChore(t, root, "projects/moe/chores/alpha/chore.json", `{"cadence":"24h"}`)
	writeChore(t, root, "projects/moe/chores/beta/chore.json", `{"cadence":"24h"}`)
	commitAt(t, root, "seed chores", t0)

	walks := countEditWalks(t)

	// Hit: two loads on one HEAD walk once per definition in total.
	first := loadEditedAt(t, root)
	second := loadEditedAt(t, root)
	if *walks != 2 {
		t.Errorf("two loads on one HEAD: %d walks, want 2 (one per chore)", *walks)
	}
	for _, k := range []string{"moe/alpha", "moe/beta"} {
		if !first[k].Equal(t0) || !second[k].Equal(first[k]) {
			t.Errorf("%s: first=%v second=%v, want both %v", k, first[k], second[k], t0)
		}
	}

	// Invalidation: an edit under alpha re-walks and advances alpha.
	t1 := t0.Add(time.Hour)
	writeChore(t, root, "projects/moe/chores/alpha/prompt.md", "do the thing\n")
	commitAt(t, root, "edit alpha", t1)
	*walks = 0
	got := loadEditedAt(t, root)
	if *walks != 2 {
		t.Errorf("after a commit: %d walks, want 2 (HEAD moved, every path re-walks)", *walks)
	}
	if !got["moe/alpha"].Equal(t1) {
		t.Errorf("alpha EditedAt = %v, want %v after its edit", got["moe/alpha"], t1)
	}
	if !got["moe/beta"].Equal(t0) {
		t.Errorf("beta EditedAt = %v, want unchanged %v", got["moe/beta"], t0)
	}

	// Unrelated commit: HEAD moved, so the walk re-runs, but neither
	// chore's answer changes.
	writeChore(t, root, "elsewhere.txt", "x\n")
	commitAt(t, root, "unrelated", t1.Add(time.Hour))
	*walks = 0
	got = loadEditedAt(t, root)
	if *walks != 2 {
		t.Errorf("after an unrelated commit: %d walks, want 2", *walks)
	}
	if !got["moe/alpha"].Equal(t1) || !got["moe/beta"].Equal(t0) {
		t.Errorf("unrelated commit moved EditedAt: alpha=%v beta=%v", got["moe/alpha"], got["moe/beta"])
	}
}

// TestLastDefinitionEditDoesNotCacheFailure: a walk whose git call
// fails is retried rather than pinned until the next commit. A path
// outside the repository makes `git log` exit 128 while HEAD stays
// readable, so the memo is consulted and must decline to store.
func TestLastDefinitionEditDoesNotCacheFailure(t *testing.T) {
	root := t.TempDir()
	gittest.InitAt(t, root)
	commitAt(t, root, "seed", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	walks := countEditWalks(t)

	for range 2 {
		if got := lastDefinitionEdit(root, "../outside"); !got.IsZero() {
			t.Fatalf("failed walk returned %v, want the zero time", got)
		}
	}
	if *walks != 2 {
		t.Errorf("failed walk ran %d times over two calls, want 2 (not cached)", *walks)
	}

	// Control: a path with no commits is a successful walk — its zero
	// answer is exact for this HEAD and is cached.
	*walks = 0
	lastDefinitionEdit(root, "never-committed")
	lastDefinitionEdit(root, "never-committed")
	if *walks != 1 {
		t.Errorf("uncommitted path walked %d times over two calls, want 1", *walks)
	}
}
