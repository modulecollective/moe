package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/session"
	"github.com/modulecollective/moe/internal/trailers/trailerstest"
)

func TestLogSparseNormalAndDiagnostic(t *testing.T) {
	root := newTestBureaucracy(t)
	markBureaucracy(t, root)
	t.Setenv("NO_COLOR", "1")
	trailerstest.SeedRun(t, root, "moe", "history", "sdlc", run.StatusInProgress)
	writeThread(t, root, "moe", "history", "design", "claude", []string{`{"type":"user","message":{"role":"user","content":"normal history"}}`})
	writeThread(t, root, "moe", "history", "design", "codex", []string{`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"codex history"}]}}`})
	diagnostic := filepath.Join(diagnosticStageDir("moe", "history", "code"), "attempt")
	failure, _ := json.Marshal(stageFailure{Stage: "code", Agent: "claude", CanvasRefusal: "missing", TranscriptAvailable: true})
	dir := filepath.Join(root, diagnostic)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "failure.json"), failure, 0644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "--", filepath.Join(diagnostic, "failure.json"))
	gittest.WriteAndCommit(t, root, filepath.Join(diagnostic, "thread-claude.jsonl"), `{"type":"user","message":{"role":"user","content":"diagnostic history"}}`+"\n", "diagnostic: moe/history code canvas missing\n\nMoE-Project: moe\nMoE-Run: history\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Commit(t, root, "seed full bureaucracy")
	s, err := session.Open(root, "moe", "active", "design")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"moe/history", "design"}, {"moe/history", "design", "--agent", "claude"}, {"moe/history", "design", "--agent", "codex"}, {"moe/history", "code"}} {
		var full, sparse, errFull, errSparse bytes.Buffer
		t.Setenv("MOE_HOME", root)
		want := runLog("sdlc", "")(args, &full, &errFull)
		t.Setenv("MOE_HOME", s.WorktreePath)
		got := runLog("sdlc", "")(args, &sparse, &errSparse)
		// Diagnostic banners include a relative path, identical in both roots.
		if got != want || full.String() != sparse.String() || errFull.String() != errSparse.String() {
			t.Fatalf("%v: full %d %q %q; sparse %d %q %q", args, want, full.String(), errFull.String(), got, sparse.String(), errSparse.String())
		}
		if args[1] == "design" && len(args) == 2 && !strings.Contains(errSparse.String(), "both claude and codex") {
			t.Fatal("lost backend ambiguity")
		}
		if (len(args) == 4 || args[1] == "code") && got != 0 {
			t.Fatal(errSparse.String())
		}
		if len(args) == 4 && args[3] == "codex" && !strings.Contains(sparse.String(), "codex history") {
			t.Fatal(sparse.String())
		}
		if args[1] == "code" && !strings.Contains(sparse.String(), "diagnostic history") {
			t.Fatal(sparse.String())
		}
	}
	for _, path := range []string{run.ThreadPathFor("claude", "moe", "history", "design"), filepath.Join(diagnostic, "thread-claude.jsonl")} {
		if _, err := os.Stat(filepath.Join(s.WorktreePath, path)); !os.IsNotExist(err) {
			t.Fatalf("hydrated %s", path)
		}
	}
}
