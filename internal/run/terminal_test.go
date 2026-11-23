package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func terminalCommit(t *testing.T, root string, md *Metadata, at time.Time) string {
	t.Helper()
	p := filepath.Join(root, Dir(md.Project, md.ID), "run.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(md)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", p)
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + at.Format(time.RFC3339), "GIT_AUTHOR_DATE=" + at.Format(time.RFC3339)}, "commit", "--allow-empty", "-m", "state\n\nMoE-Project: p\nMoE-Run: r\nMoE-Workflow: sdlc\n")
	return gittest.HeadSHA(t, root)
}

func TestLatestTerminalTransition(t *testing.T) {
	for _, status := range []string{StatusClosed, StatusMerged, StatusPromoted} {
		t.Run(status, func(t *testing.T) {
			root := gittest.Init(t)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			md := &Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: StatusInProgress}
			terminalCommit(t, root, md, at.Add(-time.Hour))
			md.Status = status
			want := terminalCommit(t, root, md, at)
			// A later metadata edit and unrelated commit must not reset age.
			md.Agent = "codex"
			terminalCommit(t, root, md, at.Add(time.Hour))
			gittest.Commit(t, root, "unrelated")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			evidence, err := LatestTerminalTransition(ctx, root, md, at.Add(CloneRetention))
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Commit != want || !evidence.At.Equal(at) {
				t.Fatalf("wrong transition: %+v", evidence)
			}
			if evidence.RetentionExpired(at.Add(CloneRetention - time.Nanosecond)) {
				t.Fatal("expired early")
			}
			if !evidence.RetentionExpired(at.Add(CloneRetention)) {
				t.Fatal("not expired at exactly 168h")
			}
			md.Status = StatusInProgress
			terminalCommit(t, root, md, at.Add(2*time.Hour))
			if _, err := LatestTerminalTransition(ctx, root, md, at.Add(30*24*time.Hour)); err == nil {
				t.Fatal("reopened run got evidence")
			}
			md.Status = status
			want = terminalCommit(t, root, md, at.Add(3*time.Hour))
			evidence, err = LatestTerminalTransition(ctx, root, md, at.Add(CloneRetention))
			if err != nil || evidence.Commit != want || evidence.RetentionExpired(at.Add(CloneRetention)) {
				t.Fatalf("reclose: %+v, %v", evidence, err)
			}
		})
	}
}

func TestTerminalEvidenceRefusesUnprovableHistory(t *testing.T) {
	for _, kind := range []string{"initial-terminal", "future", "uncommitted", "unattributed", "contradictory", "wrong-identity", "replacement", "graft", "promisor", "shallow"} {
		t.Run(kind, func(t *testing.T) {
			root := gittest.Init(t)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			md := &Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: StatusInProgress}
			if kind != "initial-terminal" {
				terminalCommit(t, root, md, at.Add(-time.Hour))
			}
			if kind == "contradictory" {
				md.Status = StatusMerged
				terminalCommit(t, root, md, at.Add(-time.Minute))
			}
			md.Status = StatusClosed
			sha := terminalCommit(t, root, md, at)
			now := at.Add(CloneRetention)
			switch kind {
			case "future":
				now = at.Add(-time.Second)
			case "uncommitted":
				md.Agent = "codex"
			case "unattributed":
				gittest.Run(t, root, "commit", "--amend", "-m", "missing attribution")
			case "wrong-identity":
				md.ID = "wrong"
			case "replacement":
				gittest.Run(t, root, "update-ref", "refs/replace/"+sha, sha)
			case "graft":
				if err := os.WriteFile(filepath.Join(root, ".git", "info", "grafts"), []byte(sha+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "promisor":
				gittest.Run(t, root, "config", "remote.origin.promisor", "true")
			case "shallow":
				if err := os.WriteFile(filepath.Join(root, ".git", "shallow"), []byte(sha+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if got, err := LatestTerminalTransition(ctx, root, md, now); err == nil {
				t.Fatalf("accepted %s: %+v", kind, got)
			} else if kind == "shallow" && !strings.Contains(err.Error(), "shallow journal history") {
				t.Fatalf("did not reach shallow journal guard: %v", err)
			}
		})
	}
}

func TestNonterminalNeverExpires(t *testing.T) {
	for _, status := range []string{StatusInProgress, StatusPushed, "paused", "unknown"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := LatestTerminalTransition(ctx, t.TempDir(), &Metadata{Status: status}, time.Now().AddDate(100, 0, 0))
		cancel()
		if err == nil {
			t.Fatalf("accepted %s", status)
		}
	}
}

func TestTerminalTransitionAcceptsSyncAttributionAndRejectsConflictingWorkflow(t *testing.T) {
	root := gittest.Init(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	md := &Metadata{ID: "r", Project: "p", Workflow: "sdlc", Status: StatusPushed}
	terminalCommit(t, root, md, at.Add(-time.Hour))
	md.Status = StatusClosed
	terminalCommit(t, root, md, at)
	message := "sync: p/r closed\n\nMoE-Project: p\nMoE-Run: r\nMoE-Document: push\nMoE-Closed: fixture\n"
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + at.Format(time.RFC3339)}, "commit", "--amend", "-m", message)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got, err := LatestTerminalTransition(ctx, root, md, at.Add(CloneRetention)); err != nil || !got.At.Equal(at) {
		t.Fatalf("sync evidence rejected: %+v %v", got, err)
	}
	gittest.RunWithEnv(t, root, []string{"GIT_COMMITTER_DATE=" + at.Format(time.RFC3339)}, "commit", "--amend", "-m", message+"MoE-Workflow: chat\n")
	if _, err := LatestTerminalTransition(ctx, root, md, at.Add(CloneRetention)); err == nil {
		t.Fatal("conflicting workflow trailer accepted")
	}
}
