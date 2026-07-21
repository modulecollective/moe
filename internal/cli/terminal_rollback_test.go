package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/wiki"
)

// failCloseCommits points root's core.hooksPath at a commit-msg hook
// that refuses any commit whose subject starts with "Close ". Unlike
// pulse_test.go's blanket failCommits, this leaves the harvest's own
// commits — createIdea's `work: update idea`, the scratch progress
// record — working, so the close reaches the status flip and dies on
// exactly the commit this rollback exists for. Returns a func that
// lifts the hook so the same fixture can retry.
func failCloseCommits(t *testing.T, root string) func() {
	t.Helper()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	script := "#!/bin/sh\ngrep -q '^Close ' \"$1\" && exit 1\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	return func() {
		t.Helper()
		gittest.Run(t, root, "config", "--unset", "core.hooksPath")
	}
}

// seedRollbackFixture stages a close that has real harvest output to
// strand: two follow-ups that fan out into idea runs, and a lore entry
// that promotes to an untracked lore/<slug>.md.
func seedRollbackFixture(t *testing.T, root string) {
	t.Helper()
	writeFollowups(t, root, "tele", "ship-it", strings.Join([]string{
		"# Follow-ups",
		"",
		"- [ ] `cleanup-foo` — Clean up foo helper",
		"- [ ] `chase-zlib` — Chase the zlib upgrade",
		"",
	}, "\n"))
	writeLoreFeedback(t, root, "tele", "ship-it", strings.Join([]string{
		"- [ ] `portable-fact` — A portable fact",
		"",
		"  applies-when: something happens",
		"",
		"  The body of the fact.",
		"",
	}, "\n"))
}

// TestCloseCommitFailureLeavesRunOpen is the core of this change: when
// the close commit fails, run.json must not be left saying "closed"
// with nothing committed. That state reads as closed on the dash and —
// because the dirty-tree gate is repo-wide — wedges every later close
// in the bureaucracy.
//
// What the rollback deliberately keeps is asserted here too: the idea
// runs the harvest already committed, and the scratch rewrites plus the
// promoted lore file left in the worktree. Those record which entries
// already fanned out, and harvest is idempotent over them on retry.
func TestCloseCommitFailureLeavesRunOpen(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	seedRollbackFixture(t, root)
	failCloseCommits(t, root)

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb); code == 0 {
		t.Fatalf("close succeeded despite a failing close commit; stdout=%q", out.String())
	}

	// The flip is walked back, and back to the *committed* bytes — not
	// just to a status string that happens to match.
	md, err := run.Load(root, "tele", "ship-it")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusInProgress {
		t.Errorf("status = %q, want %q — the failed close left the run flipped", md.Status, run.StatusInProgress)
	}
	runJSONRel := filepath.Join(run.Dir("tele", "ship-it"), "run.json")
	onDisk, err := os.ReadFile(filepath.Join(root, runJSONRel))
	if err != nil {
		t.Fatal(err)
	}
	committed := gittest.Output(t, root, "show", "HEAD:"+runJSONRel)
	if strings.TrimSpace(string(onDisk)) != committed {
		t.Errorf("run.json differs from HEAD after rollback:\non disk:\n%s\nHEAD:\n%s", onDisk, committed)
	}

	// Index clean: what the closure staged is unstaged again, so the
	// operator's next `git commit` can't sweep up a half-close.
	if staged := gittest.Output(t, root, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("index still holds staged paths after rollback:\n%s", staged)
	}

	// Kept on purpose: committed idea runs, plus the scratch rewrites
	// and the promoted lore file in the worktree.
	for _, slug := range []string{"cleanup-foo", "chase-zlib"} {
		if _, err := os.Stat(filepath.Join(root, run.Dir("tele", slug), "run.json")); err != nil {
			t.Errorf("harvested idea %s should survive the rollback: %v", slug, err)
		}
	}
	if got := readFollowups(t, root, "tele", "ship-it"); !strings.Contains(got, "- [x] `cleanup-foo`") {
		t.Errorf("followups.md rewrite should survive the rollback:\n%s", got)
	}
	if got := readLoreFeedback(t, root, "tele", "ship-it"); !strings.Contains(got, "- [x] `portable-fact`") {
		t.Errorf("feedback/lore.md rewrite should survive the rollback:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, wiki.LoreDirRel, "portable-fact.md")); err != nil {
		t.Errorf("promoted lore file should survive the rollback: %v", err)
	}
}

// TestCloseRetriesAfterCommitFailure is the other half: the rollback is
// only worth anything if `moe <wf> close` is retryable as-is. The retry
// must land one close commit carrying run.json plus the stranded
// rewrites and lore file, and must not re-file the ideas the first
// attempt already committed.
func TestCloseRetriesAfterCommitFailure(t *testing.T) {
	root := seedCloseFixture(t, "tele", "ship-it", "sdlc", run.StatusInProgress)
	t.Setenv("MOE_HOME", root)
	t.Setenv("NO_COLOR", "1")
	seedRollbackFixture(t, root)
	unfail := failCloseCommits(t, root)

	var out, errb bytes.Buffer
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb); code == 0 {
		t.Fatalf("close succeeded despite a failing close commit; stdout=%q", out.String())
	}
	unfail()

	out.Reset()
	errb.Reset()
	if code := Run([]string{"sdlc", "close", "--no-edit", "tele/ship-it"}, &out, &errb); code != 0 {
		t.Fatalf("retry exit=%d stderr=%q", code, errb.String())
	}

	md, err := run.Load(root, "tele", "ship-it")
	if err != nil {
		t.Fatal(err)
	}
	if md.Status != run.StatusClosed {
		t.Fatalf("status = %q after retry, want closed", md.Status)
	}
	head := gitLog(t, root, "-1", "--name-only", "--format=%s")
	for _, want := range []string{
		"Close sdlc run tele/ship-it",
		filepath.Join(run.Dir("tele", "ship-it"), "run.json"),
		run.FollowupsPath("tele", "ship-it"),
		run.FeedbackPath("tele", "ship-it", "lore"),
		filepath.Join(wiki.LoreDirRel, "portable-fact.md"),
	} {
		if !strings.Contains(head, want) {
			t.Errorf("close commit missing %q:\n%s", want, head)
		}
	}
	if status := gittest.Output(t, root, "status", "--porcelain"); status != "" {
		t.Errorf("tree dirty after the retried close:\n%s", status)
	}

	// Idempotency: the harvest ran twice over followups.md but the
	// second pass saw `[x]` lines, so no duplicate idea runs.
	entries, err := os.ReadDir(filepath.Join(root, "projects", "tele", "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	if len(ids) != 3 {
		t.Errorf("runs under tele = %v, want exactly ship-it plus the two harvested ideas", ids)
	}
}

// TestReconcileCommitFailureLeavesRunPushed pins the same rollback on
// sync's reconcile arm. GitHub stays the durable truth for a pushed
// run, so a reconcile whose commit failed must leave run.json saying
// pushed — the next `moe sync` re-flips it.
func TestReconcileCommitFailureLeavesRunPushed(t *testing.T) {
	f := newReconcileFixture(t, run.StatusPushed)
	fakeGh(t, map[string]string{
		f.prURL: `{"state":"MERGED","mergeCommit":{"oid":"abc1234deadbeef"}}`,
	})
	failCommits(t, f.root)

	var stdout, stderr bytes.Buffer
	if _, err := reconcilePushedRuns(f.root, "" /*all projects*/, &stdout, &stderr); err == nil {
		t.Fatalf("reconcile succeeded despite a failing commit; stdout=%q", stdout.String())
	}

	if md := f.reload(); md.Status != run.StatusPushed {
		t.Errorf("status = %q, want %q — a failed reconcile commit left the run flipped", md.Status, run.StatusPushed)
	}
	if staged := gittest.Output(t, f.root, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("index still holds staged paths after rollback:\n%s", staged)
	}
}
