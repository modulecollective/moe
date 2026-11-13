package runcontrol

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

const commitFailureMarker = "runcontrol-test-rejected-commit"

func rejectControlCommits(t *testing.T, root, sabotage string) func() {
	t.Helper()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	body := "#!/bin/sh\nset -e\n" + sabotage + "\necho " + commitFailureMarker + " >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "config", "core.hooksPath", hooks)
	t.Cleanup(func() { gittest.Run(t, root, "config", "--unset", "core.hooksPath") })
	return func() {
		t.Helper()
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
	}
}

type controlPathState struct {
	body    []byte
	existed bool
	index   string
}

func readControlPath(t *testing.T, root, rel string) controlPathState {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return controlPathState{b, err == nil, gittest.Output(t, root, "ls-files", "-s", "--", rel)}
}

func requireControlPath(t *testing.T, root, rel string, want controlPathState) {
	t.Helper()
	got := readControlPath(t, root, rel)
	// Check bytes first so a failure on the old implementation proves
	// that restoring the working file alone is insufficient.
	if got.existed != want.existed || !bytes.Equal(got.body, want.body) {
		t.Fatalf("%s working bytes = %q (exists %t), want %q (exists %t)", rel, got.body, got.existed, want.body, want.existed)
	}
	if got.index != want.index {
		t.Fatalf("%s index entry = %q, want %q", rel, got.index, want.index)
	}
}

func requireControlRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "runcontrol: commit failed, nothing recorded") {
		t.Fatalf("failed commit = %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("original commit cause not wrapped: %v", err)
	}
}

// HEAD, staged bytes/mode and working bytes are deliberately distinct.
// Each public writer must preserve them and permit the same request's retry.
func TestControlCommitFailurePreservesOperatorWork(t *testing.T) {
	for _, verb := range []string{"submit", "settle", "resume"} {
		t.Run(verb, func(t *testing.T) {
			root := seedRoot(t)
			h := own(t, root)
			if verb == "resume" {
				submit(t, root, Request{ID: "original", Kind: KindPause})
			} else {
				submit(t, root, Request{ID: "original", Kind: KindFinish})
			}
			rel := Path("moe", "r1")
			original := readControlPath(t, root, rel).body
			gittest.WriteAndCommit(t, root, "other.txt", "HEAD\n", "other work")
			write(t, root, rel, string(original)+"\n")
			gittest.Run(t, root, "add", "--", rel)
			gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
			write(t, root, rel, string(original)+"\n\n")
			write(t, root, "other.txt", "staged\n")
			gittest.Run(t, root, "add", "--", "other.txt")
			write(t, root, "other.txt", "working\n")
			before := readControlPath(t, root, rel)
			other := readControlPath(t, root, "other.txt")
			beforeHead := head(t, root)
			retry := rejectControlCommits(t, root, "")
			mutate := func() error {
				switch verb {
				case "submit":
					_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
					return err
				case "settle":
					return Settle(root, "moe", "r1", "original", Settlement{State: StateApplied, Boundary: "stage code"}, "")
				default:
					_, released, err := Resume(root, "moe", "r1", h, Request{ID: "new"})
					if err == nil && !released {
						t.Fatal("retry did not release hold")
					}
					return err
				}
			}
			err := mutate()
			requireControlRejected(t, err)
			requireControlPath(t, root, rel, before)
			requireControlPath(t, root, "other.txt", other)
			if head(t, root) != beforeHead {
				t.Fatal("failed commit moved HEAD")
			}
			retry()
			if err := mutate(); err != nil {
				t.Fatalf("retry: %v", err)
			}
			f := load(t, root)
			switch verb {
			case "settle":
				r, _ := f.Find("original")
				if r.State != StateApplied || r.Boundary != "stage code" || len(f.Requests) != 1 {
					t.Fatalf("retry settlement = %+v", f)
				}
			case "resume":
				r, ok := f.Find("new")
				if !ok || len(f.Requests) != 2 || f.Held() || r.State != StateCompleted {
					t.Fatalf("retry resume = %+v", f)
				}
			case "submit":
				if _, ok := f.Find("new"); !ok || len(f.Requests) != 2 || !f.Held() || f.Hold.Request != "new" {
					t.Fatalf("retry submit = %+v", f)
				}
			}
		})
	}
}

func TestControlCommitFailurePreservesAbsentIndexEntry(t *testing.T) {
	for _, state := range []string{"new", "untracked", "staged deletion"} {
		t.Run(state, func(t *testing.T) {
			root := seedRoot(t)
			rel := Path("moe", "r1")
			if state != "new" {
				write(t, root, rel, "{\"requests\":[]}\n\n")
			}
			if state == "staged deletion" {
				gittest.Run(t, root, "add", "--", rel)
				gittest.Run(t, root, "commit", "-m", "control baseline")
				gittest.Run(t, root, "rm", "--cached", "--", rel)
			}
			before := readControlPath(t, root, rel)
			if before.index != "" {
				t.Fatalf("fixture has index entry: %q", before.index)
			}
			beforeHead := head(t, root)
			retry := rejectControlCommits(t, root, "")
			_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
			requireControlRejected(t, err)
			requireControlPath(t, root, rel, before)
			if head(t, root) != beforeHead {
				t.Fatal("failed commit moved HEAD")
			}
			retry()
			submit(t, root, Request{ID: "new", Kind: KindPause})
		})
	}
}

func TestControlCommitFailureSnapshotsUnderLock(t *testing.T) {
	root := seedRoot(t)
	submit(t, root, Request{ID: "f1", Kind: KindFinish})
	rel := Path("moe", "r1")
	var before controlPathState
	testLocked = func() {
		testLocked = nil
		f := load(t, root)
		f.Requests = append(f.Requests, Request{ID: "other", Kind: KindPark, State: StateAccepted})
		if err := writeFile(filepath.Join(root, rel), f); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, root, "add", "--", rel)
		gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
		b := readControlPath(t, root, rel).body
		write(t, root, rel, string(b)+"\n")
		before = readControlPath(t, root, rel)
	}
	t.Cleanup(func() { testLocked = nil })
	beforeHead := head(t, root)
	rejectControlCommits(t, root, "")
	_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
	requireControlRejected(t, err)
	requireControlPath(t, root, rel, before)
	if head(t, root) != beforeHead {
		t.Fatal("failed commit moved HEAD")
	}
}

func TestControlStagingFailureRestores(t *testing.T) {
	root := seedRoot(t)
	rel := Path("moe", "r1")
	write(t, root, rel, "{\"requests\":[]}\n\n")
	write(t, root, ".git/info/exclude", rel+"\n")
	before := readControlPath(t, root, rel)
	beforeHead := head(t, root)
	_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
	if err == nil || !strings.Contains(err.Error(), "nothing recorded") || !strings.Contains(err.Error(), "ignored") {
		t.Fatalf("staging failure = %v", err)
	}
	requireControlPath(t, root, rel, before)
	if head(t, root) != beforeHead {
		t.Fatal("staging failure moved HEAD")
	}
}

func TestControlRefusesUnmergedIndex(t *testing.T) {
	root := seedRoot(t)
	rel := Path("moe", "r1")
	gittest.WriteAndCommit(t, root, rel, "{\"requests\":[]}\n", "base")
	gittest.Run(t, root, "checkout", "-b", "other")
	gittest.WriteAndCommit(t, root, rel, "{\"revision\":1,\"requests\":[]}\n", "other")
	gittest.Run(t, root, "checkout", "main")
	gittest.WriteAndCommit(t, root, rel, "{\"revision\":2,\"requests\":[]}\n", "ours")
	if err := git.Run(root, "merge", "other"); err == nil {
		t.Fatal("fixture did not conflict")
	}
	write(t, root, rel, "{\"revision\":3,\"requests\":[]}\n")
	before := readControlPath(t, root, rel)
	if len(strings.Split(before.index, "\n")) != 3 {
		t.Fatalf("fixture has no three-stage conflict: %q", before.index)
	}
	beforeHead := head(t, root)
	_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
	if err == nil || !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), rel) {
		t.Fatalf("unmerged refusal = %v", err)
	}
	requireControlPath(t, root, rel, before)
	if head(t, root) != beforeHead {
		t.Fatal("unmerged refusal moved HEAD")
	}
}

func TestControlIndexReadFailureRefusesBeforeWrite(t *testing.T) {
	root := seedRoot(t)
	rel := Path("moe", "r1")
	write(t, root, rel, "{\"requests\":[]}\n\n")
	before := readControlPath(t, root, rel)
	beforeHead := head(t, root)
	if err := os.Rename(filepath.Join(root, ".git/index"), filepath.Join(root, ".git/index.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git/index"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
	if err == nil || !strings.Contains(err.Error(), "read index entry") || !strings.Contains(err.Error(), rel) {
		t.Fatalf("index read failure = %v", err)
	}
	if err := os.Remove(filepath.Join(root, ".git/index")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, ".git/index.saved"), filepath.Join(root, ".git/index")); err != nil {
		t.Fatal(err)
	}
	requireControlPath(t, root, rel, before)
	if head(t, root) != beforeHead {
		t.Fatal("index read failure moved HEAD")
	}
}

func TestControlCommitFailureReportsRestorationFailures(t *testing.T) {
	for _, breakIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("index=%t", breakIndex), func(t *testing.T) {
			root := seedRoot(t)
			submit(t, root, Request{ID: "f1", Kind: KindFinish})
			rel := Path("moe", "r1")
			b := readControlPath(t, root, rel).body
			write(t, root, rel, string(b)+"\n")
			gittest.Run(t, root, "add", "--", rel)
			before := readControlPath(t, root, rel)
			beforeHead := head(t, root)
			// Directory replacement fails even when tests run as root.
			sabotage := fmt.Sprintf("rm -- '%s'\nmkdir -- '%s'\n", rel, rel)
			if breakIndex {
				sabotage += "mv .git/index .git/index.saved\nmkdir .git/index\n"
			}
			rejectControlCommits(t, root, sabotage)
			_, err := Submit(root, "moe", "r1", Request{ID: "new", Kind: KindPause})
			if err == nil || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), "restore file") || strings.Contains(err.Error(), "nothing recorded") {
				t.Fatalf("restoration failure = %v", err)
			}
			if breakIndex {
				if !strings.Contains(err.Error(), "restore index") {
					t.Fatalf("index failure omitted: %v", err)
				}
			} else if got := gittest.Output(t, root, "ls-files", "-s", "--", rel); got != before.index {
				t.Fatalf("index not restored despite file failure: %q, want %q", got, before.index)
			}
			if head(t, root) != beforeHead {
				t.Fatal("failed commit moved HEAD")
			}
		})
	}
}

func TestControlNothingToCommitRemainsNoOp(t *testing.T) {
	root := seedRoot(t)
	req := submit(t, root, Request{ID: "p1", Kind: KindPause})
	rel := Path("moe", "r1")
	before := readControlPath(t, root, rel)
	beforeHead := head(t, root)
	rejectControlCommits(t, root, "")
	// Exercise StageAndCommit's sentinel as well as prepare's no-op.
	if err := commit(root, "moe", "r1", "control-test", func(*File) (string, error) { return "no change", nil }); err != nil {
		t.Fatalf("no-change commit = %v", err)
	}
	if got := submit(t, root, req); got != req {
		t.Fatalf("no-op request = %+v, want %+v", got, req)
	}
	requireControlPath(t, root, rel, before)
	if head(t, root) != beforeHead {
		t.Fatal("no-op moved HEAD")
	}
}
