package input

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
)

const commitFailureMarker = "input-test-rejected-commit"

func rejectInputCommits(t *testing.T, root, sabotage string) func() {
	t.Helper()
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	body := "#!/bin/sh\n" + sabotage + "\necho " + commitFailureMarker + " >&2\nexit 1\n"
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

type inputPathState struct {
	body    []byte
	existed bool
	index   string
}

func readInputPath(t *testing.T, root, rel string) inputPathState {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return inputPathState{b, err == nil, gittest.Output(t, root, "ls-files", "-s", "--", rel)}
}

func requireInputPath(t *testing.T, root, rel string, want inputPathState) {
	t.Helper()
	got := readInputPath(t, root, rel)
	if got.existed != want.existed || !bytes.Equal(got.body, want.body) || got.index != want.index {
		t.Fatalf("%s state = %+v (bytes %q), want %+v (bytes %q)", rel, got, got.body, want, want.body)
	}
}

// Seed before staging, so fixture commits cannot consume the evidence.
// The sibling catches accidentally scoping the commit to the run directory.
func stageUnrelatedInputWork(t *testing.T, root string) map[string]inputPathState {
	t.Helper()
	sibling := "projects/moe/runs/destination/operator.txt"
	tracked := []string{"other.txt", "deleted.txt", "mode.txt", sibling}
	for _, rel := range tracked {
		write(t, root, rel, "HEAD\n")
	}
	gittest.Run(t, root, append([]string{"add", "--"}, tracked...)...)
	gittest.Run(t, root, append([]string{"commit", "--only", "-m", "unrelated baseline", "--"}, tracked...)...)
	for _, rel := range []string{"other.txt", "added.txt", sibling} {
		write(t, root, rel, "index\n")
		gittest.Run(t, root, "add", "--", rel)
		write(t, root, rel, "working\n")
	}
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", "deleted.txt")
	gittest.Run(t, root, "update-index", "--chmod=+x", "--", "mode.txt")
	states := make(map[string]inputPathState)
	for _, rel := range append(tracked, "added.txt") {
		states[rel] = readInputPath(t, root, rel)
	}
	return states
}

func requireInputOnlyCommit(t *testing.T, root, head, rel string) {
	t.Helper()
	if got := gittest.Output(t, root, "rev-list", "--count", head+"..HEAD"); got != "1" {
		t.Fatalf("input mutation made %s commits, want 1", got)
	}
	if got := gittest.Output(t, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"); got != rel {
		t.Fatalf("input commit paths = %q, want only %q", got, rel)
	}
	if got := gittest.Output(t, root, "diff", "--cached", "--name-only", "--", rel); got != "" {
		t.Fatalf("committed input still staged: %q", got)
	}
	committed, err := git.Output(root, "show", "HEAD:"+rel)
	if err != nil || !bytes.Equal([]byte(committed), readInputPath(t, root, rel).body) {
		t.Fatalf("committed input differs from working record: %v", err)
	}
}

func TestInputCommitSuccessPreservesUnrelatedStaging(t *testing.T) {
	for _, verb := range []string{"add", "ask", "answer", "delivered", "carry"} {
		for _, state := range []string{"absent", "tracked", "untracked"} {
			if state == "absent" && (verb == "answer" || verb == "delivered") {
				continue
			}
			t.Run(verb+"/"+state, func(t *testing.T) {
				root := seedRoot(t)
				seedRun(t, root, "destination")
				rel := Path("moe", "destination")
				var before File
				if state != "absent" {
					before.Notes = []Entry{{ID: 1, Text: "original note"}}
					if verb == "answer" {
						before.Notes[0] = Entry{ID: 1, AskedBy: "moe/pulse-one", Question: "Which store?"}
					}
					if state == "tracked" {
						commitRecord(t, root, "destination", before, "seed input")
					} else if err := writeFile(filepath.Join(root, rel), before); err != nil {
						t.Fatal(err)
					}
				}
				if verb == "carry" {
					seedRun(t, root, "source")
					add(t, root, "source", "source HEAD")
				}
				foreign := stageUnrelatedInputWork(t, root)
				if state == "tracked" {
					// Success owns the complete new destination, even when
					// its previous index and working versions differ.
					write(t, root, rel, "{\"notes\":[{\"id\":1,\"text\":\"staged destination\"}]}\n")
					gittest.Run(t, root, "add", "--", rel)
					if err := writeFile(filepath.Join(root, rel), before); err != nil {
						t.Fatal(err)
					}
				}
				src := Path("moe", "source")
				sourceHEAD := gittest.Output(t, root, "ls-tree", "HEAD", "--", src)
				if verb == "carry" {
					write(t, root, src, "{\"notes\":[{\"id\":1,\"text\":\"source index\"}]}\n")
					gittest.Run(t, root, "add", "--", src)
					write(t, root, src, "{\"notes\":[{\"id\":1,\"text\":\"carry me\"},{\"id\":2,\"text\":\"done\",\"delivered_to\":\"idea\"}]}\n")
					foreign[src] = readInputPath(t, root, src)
				}
				head := gittest.HeadSHA(t, root)
				want := File{Notes: append([]Entry(nil), before.Notes...)}
				id := len(want.Notes) + 1
				ref := fmt.Sprintf("moe/destination#%d", id)
				var err error
				var trailer string
				switch verb {
				case "add":
					_, err = Add(root, "moe", "destination", "new note")
					want.Notes = append(want.Notes, Entry{ID: id, Text: "new note"})
					trailer = "MoE-Input-Added: " + ref
				case "ask":
					_, err = Ask(root, "moe", "destination", "moe/pulse-one", "Which store?", "dynamic")
					want.Notes = append(want.Notes, Entry{ID: id, AskedBy: "moe/pulse-one", Question: "Which store?"})
					trailer = "MoE-Input-Asked: " + ref
				case "answer":
					_, err = Answer(root, "moe", "destination", 1, "sqlite")
					want.Notes[0].Text = "sqlite"
					trailer = "MoE-Input-Answered: moe/destination#1"
				case "delivered":
					err = MarkDelivered(root, "moe", "destination", "code", []int{1}, "dynamic")
					want.Notes[0].DeliveredTo = "code"
					trailer = "MoE-Input-Delivered: moe/destination#1 code"
				case "carry":
					var n int
					n, err = Carry(root, "moe", "source", "moe", "destination", "dynamic", io.Discard)
					if err == nil && n != 1 {
						t.Fatalf("carried %d entries, want 1", n)
					}
					want.Notes = append(want.Notes, Entry{ID: id, Text: "carry me"})
					trailer = "input: carried moe/source#1 → " + ref
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := Load(root, "moe", "destination")
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("record = %+v, %v, want %+v", got, err, want)
				}
				requireInputOnlyCommit(t, root, head, rel)
				for path, state := range foreign {
					requireInputPath(t, root, path, state)
				}
				if got := gittest.Output(t, root, "ls-tree", "HEAD", "--", src); got != sourceHEAD {
					t.Fatalf("source HEAD changed: %q, want %q", got, sourceHEAD)
				}
				msg := gittest.Output(t, root, "log", "-1", "--format=%B")
				for _, line := range []string{trailer, "MoE-Run: destination", "MoE-Project: moe"} {
					if !strings.Contains(msg, line) {
						t.Fatalf("commit missing %q:\n%s", line, msg)
					}
				}
				machine := verb == "ask" || verb == "delivered" || verb == "carry"
				if strings.Contains(msg, "MoE-Consent: dynamic") != machine || strings.Contains(msg, "MoE-Document:") || strings.Contains(msg, "MoE-Workflow: sdlc") != (verb != "delivered") {
					t.Fatalf("unexpected input trailers:\n%s", msg)
				}
			})
		}
	}
}

func requireRejectedCommit(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "input: commit failed, nothing recorded") {
		t.Fatalf("failed commit = %v", err)
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("original commit cause not wrapped: %v", err)
	}
}

// Every writer must restore before returning failure and retry without
// consuming unrelated staging left in place across both attempts.
func TestInputCommitFailureRestoresAllWriters(t *testing.T) {
	for _, verb := range []string{"add", "ask", "answer", "delivered", "carry"} {
		t.Run(verb, func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "destination")
			rel := Path("moe", "destination")
			switch verb {
			case "answer":
				ask(t, root, "destination")
			case "delivered":
				add(t, root, "destination", "pending note")
			case "carry":
				seedRun(t, root, "source")
				add(t, root, "source", "carry me")
			}
			gittest.Commit(t, root, "seed runs")
			foreign := stageUnrelatedInputWork(t, root)
			before := readInputPath(t, root, rel)
			source := readInputPath(t, root, Path("moe", "source"))
			head := gittest.HeadSHA(t, root)
			retry := rejectInputCommits(t, root, "")
			mutate := func() error {
				switch verb {
				case "add":
					_, err := Add(root, "moe", "destination", "new note")
					return err
				case "ask":
					_, err := Ask(root, "moe", "destination", "moe/pulse-one", "Which store?", "dynamic")
					return err
				case "answer":
					_, err := Answer(root, "moe", "destination", 1, "sqlite")
					return err
				case "delivered":
					return MarkDelivered(root, "moe", "destination", "code", []int{1}, "")
				default:
					_, err := Carry(root, "moe", "source", "moe", "destination", "", io.Discard)
					return err
				}
			}
			err := mutate()
			requireInputPath(t, root, rel, before)
			requireRejectedCommit(t, err)
			requireInputPath(t, root, Path("moe", "source"), source)
			for path, state := range foreign {
				requireInputPath(t, root, path, state)
			}
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed commit moved HEAD")
			}
			retry()
			if err := mutate(); err != nil {
				t.Fatalf("retry: %v", err)
			}
			requireInputOnlyCommit(t, root, head, rel)
			for path, state := range foreign {
				requireInputPath(t, root, path, state)
			}
			f, err := Load(root, "moe", "destination")
			if err != nil || len(f.Notes) != 1 || f.Notes[0].ID != 1 {
				t.Fatalf("retry did not record exactly #1: %+v, %v", f, err)
			}
			e := f.Notes[0]
			switch verb {
			case "ask":
				if !e.Open() {
					t.Fatalf("retry did not open ping: %+v", e)
				}
			case "answer":
				if e.Open() || e.Text != "sqlite" {
					t.Fatalf("retry did not answer ping: %+v", e)
				}
			case "delivered":
				if e.DeliveredTo != "code" {
					t.Fatalf("retry did not stamp delivery: %+v", e)
				}
			default:
				if !e.Pending() {
					t.Fatalf("retry did not record pending note: %+v", e)
				}
			}
			requireInputPath(t, root, Path("moe", "source"), source)
		})
	}
}

func TestInputFailureDoesNotContaminateLaterCommit(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	add(t, root, "destination", "original note")
	rel := Path("moe", "destination")
	before := gittest.Output(t, root, "ls-tree", "HEAD", "--", rel)
	retry := rejectInputCommits(t, root, "")
	_, err := Add(root, "moe", "destination", "rejected note")
	requireRejectedCommit(t, err)
	retry()
	gittest.WriteAndCommit(t, root, "unrelated.txt", "later work\n", "unrelated writer")
	if got := gittest.Output(t, root, "ls-tree", "HEAD", "--", rel); got != before {
		t.Fatalf("unrelated commit journaled rejected input: %q, want %q", got, before)
	}
}

func TestInputCommitFailurePreservesOperatorWork(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		t.Run(fmt.Sprintf("tracked=%t", tracked), func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "destination")
			rel := Path("moe", "destination")
			original := "{\"notes\":[{\"id\":1,\"text\":\"operator note\"}]}\n"
			gittest.WriteAndCommit(t, root, "other.txt", "HEAD\n", "other work")
			if tracked {
				gittest.WriteAndCommit(t, root, rel, original, "HEAD input")
				write(t, root, rel, original+"\n")
				gittest.Run(t, root, "add", "--", rel)
				// Preserve staged mode as well as staged content.
				gittest.Run(t, root, "update-index", "--chmod=+x", "--", rel)
			}
			write(t, root, rel, original+"\n\n")
			write(t, root, "other.txt", "staged\n")
			gittest.Run(t, root, "add", "--", "other.txt")
			write(t, root, "other.txt", "working\n")
			before := readInputPath(t, root, rel)
			other := readInputPath(t, root, "other.txt")
			head := gittest.HeadSHA(t, root)
			rejectInputCommits(t, root, "")
			_, err := Add(root, "moe", "destination", "rejected note")
			requireInputPath(t, root, rel, before)
			requireRejectedCommit(t, err)
			requireInputPath(t, root, "other.txt", other)
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed commit moved HEAD")
			}
		})
	}
}

func TestInputCommitFailureSnapshotsUnderLock(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	rel := Path("moe", "destination")
	var before inputPathState
	var head string
	onceLocked(t, func() {
		commitRecord(t, root, "destination", File{Notes: []Entry{{ID: 1, Text: "intervening note"}}}, "intervening writer")
		before = readInputPath(t, root, rel)
		head = gittest.HeadSHA(t, root)
		rejectInputCommits(t, root, "")
	})
	_, err := Add(root, "moe", "destination", "rejected note")
	requireInputPath(t, root, rel, before)
	requireRejectedCommit(t, err)
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("failed commit moved HEAD")
	}
}

func TestInputStagingFailureRestores(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	rel := Path("moe", "destination")
	write(t, root, ".git/info/exclude", rel+"\n")
	before := readInputPath(t, root, rel)
	head := gittest.HeadSHA(t, root)
	_, err := Add(root, "moe", "destination", "ignored input")
	if err == nil || !strings.Contains(err.Error(), "nothing recorded") || !strings.Contains(err.Error(), "ignored") {
		t.Fatalf("staging failure = %v", err)
	}
	requireInputPath(t, root, rel, before)
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("staging failure moved HEAD")
	}
}

func TestInputRefusesUnmergedIndex(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	rel := Path("moe", "destination")
	gittest.WriteAndCommit(t, root, rel, "{\"notes\":[]}\n", "base")
	gittest.Run(t, root, "checkout", "-b", "other")
	gittest.WriteAndCommit(t, root, rel, "{\"notes\":[{\"id\":1,\"text\":\"other\"}]}\n", "other")
	gittest.Run(t, root, "checkout", "main")
	gittest.WriteAndCommit(t, root, rel, "{\"notes\":[{\"id\":1,\"text\":\"ours\"}]}\n", "ours")
	if err := git.Run(root, "merge", "other"); err == nil {
		t.Fatal("fixture did not conflict")
	}
	// Resolve the working bytes without resolving the index.
	write(t, root, rel, "{\"notes\":[{\"id\":1,\"text\":\"working\"}]}\n")
	before := readInputPath(t, root, rel)
	if len(strings.Split(before.index, "\n")) != 3 {
		t.Fatalf("fixture has no three-stage conflict: %q", before.index)
	}
	head := gittest.HeadSHA(t, root)
	_, err := Add(root, "moe", "destination", "rejected note")
	if err == nil || !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), rel) {
		t.Fatalf("unmerged refusal = %v", err)
	}
	requireInputPath(t, root, rel, before)
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("unmerged refusal moved HEAD")
	}
}

func TestInputCommitFailureReportsRestorationFailures(t *testing.T) {
	for _, breakIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("index=%t", breakIndex), func(t *testing.T) {
			root := seedRoot(t)
			seedRun(t, root, "destination")
			add(t, root, "destination", "original note")
			rel := Path("moe", "destination")
			before := readInputPath(t, root, rel)
			head := gittest.HeadSHA(t, root)
			// A directory blocks replacement even when tests run as root.
			sabotage := fmt.Sprintf("rm -- '%s'\nmkdir -- '%s'\n", rel, rel)
			if breakIndex {
				sabotage += "mv .git/index .git/index.saved\nmkdir .git/index\n"
			}
			rejectInputCommits(t, root, sabotage)
			_, err := Add(root, "moe", "destination", "rejected note")
			if err == nil || !strings.Contains(err.Error(), commitFailureMarker) || !strings.Contains(err.Error(), "restore file") || !strings.Contains(err.Error(), rel) || strings.Contains(err.Error(), "nothing recorded") {
				t.Fatalf("restoration failure = %v", err)
			}
			if breakIndex {
				if !strings.Contains(err.Error(), "restore index") {
					t.Fatalf("index failure omitted: %v", err)
				}
			} else if got := gittest.Output(t, root, "ls-files", "-s", "--", rel); got != before.index {
				t.Fatalf("index not restored despite file failure: %q, want %q", got, before.index)
			}
			if gittest.HeadSHA(t, root) != head {
				t.Fatal("failed commit moved HEAD")
			}
		})
	}
}

func TestInputNothingToCommitRemainsNoOp(t *testing.T) {
	root := seedRoot(t)
	seedRun(t, root, "destination")
	add(t, root, "destination", "unchanged")
	seedRun(t, root, "source")
	commitRecord(t, root, "source", File{Notes: []Entry{{ID: 1, Text: "done", DeliveredTo: "idea"}}}, "delivered source")
	foreign := stageUnrelatedInputWork(t, root)
	before := readInputPath(t, root, Path("moe", "destination"))
	head := gittest.HeadSHA(t, root)
	rejectInputCommits(t, root, "touch hook-ran")
	err := commit(root, "moe", "destination", "input-test", func() (File, string, error) {
		f, err := Load(root, "moe", "destination")
		return f, "no change", err
	})
	if err != nil || gittest.HeadSHA(t, root) != head {
		t.Fatalf("no-change commit = %v", err)
	}
	if err := MarkDelivered(root, "moe", "destination", "code", []int{99}, ""); err != nil {
		t.Fatalf("prepare no-op = %v", err)
	}
	if err := MarkDelivered(root, "moe", "destination", "code", nil, ""); err != nil {
		t.Fatalf("empty delivery = %v", err)
	}
	if err := MarkDelivered(root, "moe", "source", "code", []int{1}, ""); err != nil {
		t.Fatalf("already delivered = %v", err)
	}
	for _, source := range []string{"no-record", "source"} {
		if n, err := Carry(root, "moe", source, "moe", "destination", "", io.Discard); err != nil || n != 0 {
			t.Fatalf("no-op carry from %s = %d, %v", source, n, err)
		}
	}
	if gittest.HeadSHA(t, root) != head {
		t.Fatal("no-op moved HEAD")
	}
	if _, err := os.Stat(filepath.Join(root, "hook-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op ran commit hook: %v", err)
	}
	requireInputPath(t, root, Path("moe", "destination"), before)
	for path, state := range foreign {
		requireInputPath(t, root, path, state)
	}
}
