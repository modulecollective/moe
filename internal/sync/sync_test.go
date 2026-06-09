package sync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
)

// journalPushFixture is a local repo with main tracking a bare origin —
// the smallest shape on which WithJournalPush's push leg is observable.
func journalPushFixture(t *testing.T) (root, origin string) {
	t.Helper()
	root = t.TempDir()
	gittest.InitAt(t, root)
	gittest.Run(t, root, "checkout", "-b", "main")
	gittest.Commit(t, root, "seed")
	origin = gittest.InitBare(t)
	gittest.Run(t, root, "remote", "add", "origin", origin)
	gittest.Run(t, root, "push", "-u", "origin", "main")
	return root, origin
}

// TestWithJournalPushRacesCommitToOrigin: fn's commit reaches origin
// before the verb returns — the whole point of the shared write-edge.
func TestWithJournalPushRacesCommitToOrigin(t *testing.T) {
	root, origin := journalPushFixture(t)

	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: record something")
		return nil
	})
	if err != nil {
		t.Fatalf("WithJournalPush: %v\nstderr=%s", err, stderr.String())
	}
	if local, remote := gittest.HeadSHA(t, root), gittest.HeadSHA(t, origin); local != remote {
		t.Fatalf("origin main = %s, want local HEAD %s", remote, local)
	}
}

// TestWithJournalPushSkipsPushOnFnError: a failing fn surfaces its
// error unchanged and origin never sees a push.
func TestWithJournalPushSkipsPushOnFnError(t *testing.T) {
	root, origin := journalPushFixture(t)
	originBefore := gittest.HeadSHA(t, origin)

	sentinel := errors.New("verb failed")
	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: half-done")
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if got := gittest.HeadSHA(t, origin); got != originBefore {
		t.Fatalf("origin advanced on fn failure: %s -> %s", originBefore, got)
	}
}

// TestWithJournalPushUnreachableOriginWarnsNotFails: the push leg is
// best-effort — a dead origin costs one stderr line, never the verb.
func TestWithJournalPushUnreachableOriginWarnsNotFails(t *testing.T) {
	root, _ := journalPushFixture(t)
	gittest.Run(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	var stdout, stderr bytes.Buffer
	err := WithJournalPush(root, repolock.Options{Purpose: "test"}, &stdout, &stderr, func() error {
		gittest.Commit(t, root, "journal: record something")
		return nil
	})
	if err != nil {
		t.Fatalf("WithJournalPush should warn, not fail: %v", err)
	}
	if !strings.Contains(stderr.String(), "[auto-sync skipped]") {
		t.Fatalf("missing warn line, stderr=%q", stderr.String())
	}
}

func TestParseGitmodulesIncludesBranch(t *testing.T) {
	dir := t.TempDir()
	content := `[submodule "foo"]
	path = projects/foo/src
	url = https://example.com/foo.git
	branch = trunk
[submodule "bar"]
	path = projects/bar/src
	url = https://example.com/bar.git
`
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ParseGitmodules(filepath.Join(dir, ".gitmodules"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	if got[0].Branch != "trunk" {
		t.Fatalf("foo branch: want trunk, got %q", got[0].Branch)
	}
	if got[1].Branch != "" {
		t.Fatalf("bar branch: want empty (so resolver falls back to main), got %q", got[1].Branch)
	}
}

func TestParseGitmodulesMissingFileIsNil(t *testing.T) {
	got, err := ParseGitmodules(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing .gitmodules should return (nil, nil), got err=%v", err)
	}
	if got != nil {
		t.Fatalf("missing .gitmodules should return nil entries, got %v", got)
	}
}

func TestProjectIDForSubmodulePath(t *testing.T) {
	cases := map[string]string{
		"projects/moe/src":     "moe",
		"projects/foo-bar/src": "foo-bar",
		"projects/moe":         "", // not the canonical shape
		"vendor/thing":         "",
		"":                     "",
	}
	for in, want := range cases {
		if got := ProjectIDForSubmodulePath(in); got != want {
			t.Errorf("ProjectIDForSubmodulePath(%q) = %q, want %q", in, got, want)
		}
	}
}
