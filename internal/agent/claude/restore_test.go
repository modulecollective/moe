package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/agent"
)

// TestRestoreTranscriptFromCacheGlobAndRewriteCwd is the primary
// regression for Option A's glob-restore path: a JSONL sitting under
// an old encoded-cwd bucket (the orphan shape pre-stable-cwd MoE was
// stranding 373 of on the box) gets copied into the new canonical
// path and its top-level cwd fields rewritten to match. After the
// call, `claude --resume <sid>` from the new cwd would land on a
// JSONL whose every record reports the new cwd.
func TestRestoreTranscriptFromCacheGlobAndRewriteCwd(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "9aecb232-4054-4d31-8433-f5b682562a9b"
	oldBucket := "-home-dev-work-bureaucracy--moe-worktrees-cae6a03a-1867-42c6-89b7-c5c196a87276"
	oldCwd := "/home/dev/work/bureaucracy/.moe/worktrees/cae6a03a-1867-42c6-89b7-c5c196a87276"
	body := jsonlLine(t, map[string]any{"type": "user", "cwd": oldCwd, "uuid": "u1"}) +
		jsonlLine(t, map[string]any{"type": "assistant", "cwd": oldCwd, "uuid": "u2"})
	writeFakeTranscript(t, cfg, oldBucket, sid, body)

	newCwd := filepath.Join(cfg, "stable", "cwd", "design")
	outcome, err := Agent{}.RestoreTranscript(sid, newCwd, "")
	if err != nil {
		t.Fatalf("RestoreTranscript: %v", err)
	}
	if outcome.Result != agent.RestoreFromCache {
		t.Fatalf("Result = %v, want RestoreFromCache", outcome.Result)
	}
	if outcome.Source != oldBucket {
		t.Fatalf("Source = %q, want %q", outcome.Source, oldBucket)
	}

	canonical := CanonicalTranscriptPath(newCwd, sid)
	got, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	// Every line must report the new cwd; no line may still carry the
	// old worktree path.
	for i, line := range strings.Split(strings.TrimRight(string(got), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if rec["cwd"] != newCwd {
			t.Errorf("line %d cwd = %v, want %q", i, rec["cwd"], newCwd)
		}
	}

	// Source file is left in place — `moe claude-cache gc` is the one
	// that reaps it. Mid-recovery deletion would lose history if the
	// caller crashes between copy and resume.
	if _, err := os.Stat(filepath.Join(cfg, "projects", oldBucket, sid+".jsonl")); err != nil {
		t.Errorf("source file should remain after restore; stat err=%v", err)
	}
}

// TestRestoreTranscriptFromMirrorWhenCacheMisses is the cross-machine
// fallback: with no cache hit anywhere under <config>/projects, the
// restore falls back to the bureaucracy-side mirror at mirrorPath and
// stages a rewritten copy at the canonical path. Cwd rewrite applies
// the same way.
func TestRestoreTranscriptFromMirrorWhenCacheMisses(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "1c8e2b9f-3441-4d5a-8e23-9d0f7c2b3a14"
	oldCwd := "/old/worktree/path"
	newCwd := "/new/stable/cwd"

	mirrorDir := t.TempDir()
	mirrorPath := filepath.Join(mirrorDir, "thread-claude.jsonl")
	body := jsonlLine(t, map[string]any{"type": "user", "cwd": oldCwd, "sessionId": sid})
	if err := os.WriteFile(mirrorPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := Agent{}.RestoreTranscript(sid, newCwd, mirrorPath)
	if err != nil {
		t.Fatalf("RestoreTranscript: %v", err)
	}
	if outcome.Result != agent.RestoreFromMirror {
		t.Fatalf("Result = %v, want RestoreFromMirror", outcome.Result)
	}
	if outcome.Source != mirrorPath {
		t.Fatalf("Source = %q, want %q", outcome.Source, mirrorPath)
	}

	got, err := os.ReadFile(CanonicalTranscriptPath(newCwd, sid))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(got))), &rec); err != nil {
		t.Fatalf("canonical not valid JSON: %v", err)
	}
	if rec["cwd"] != newCwd {
		t.Errorf("cwd = %v, want %q", rec["cwd"], newCwd)
	}
}

// TestRestoreTranscriptMissingEverywhere is the true-fresh-start signal:
// no cache hit, no mirror file (empty mirrorPath, or mirror absent) →
// RestoreMissing, no canonical file written. Stage.go re-mints on this
// outcome.
func TestRestoreTranscriptMissingEverywhere(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sid := "7d2a5e1c-90b3-4c11-a4d2-2e5b1c0a9f33"

	outcome, err := Agent{}.RestoreTranscript(sid, "/some/cwd", "")
	if err != nil {
		t.Fatalf("RestoreTranscript: %v", err)
	}
	if outcome.Result != agent.RestoreMissing {
		t.Fatalf("Result = %v, want RestoreMissing", outcome.Result)
	}
	if _, err := os.Stat(CanonicalTranscriptPath("/some/cwd", sid)); !os.IsNotExist(err) {
		t.Errorf("canonical path should not exist on RestoreMissing; stat err=%v", err)
	}

	// Mirror path that doesn't exist on disk is also RestoreMissing,
	// not an error — the legitimate "no fallback either" state.
	outcome, err = Agent{}.RestoreTranscript(sid, "/some/cwd", "/nonexistent/mirror.jsonl")
	if err != nil {
		t.Fatalf("RestoreTranscript missing mirror: %v", err)
	}
	if outcome.Result != agent.RestoreMissing {
		t.Fatalf("Result = %v, want RestoreMissing with missing mirror", outcome.Result)
	}
}

// TestRewriteTopLevelCwdPreservesNonCwdFields is the structural-safety
// test for the JSONL rewriter: only the top-level cwd field is touched;
// every other field passes through byte-for-byte (modulo JSON map
// re-ordering, which we don't pin), and lines that don't carry a
// top-level cwd are unchanged.
func TestRewriteTopLevelCwdPreservesNonCwdFields(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		newCwd     string
		wantCwd    string
		wantFields map[string]any
	}{
		{
			name:    "rewrites top-level cwd",
			line:    `{"type":"user","cwd":"/old","sessionId":"s1"}`,
			newCwd:  "/new",
			wantCwd: "/new",
			wantFields: map[string]any{
				"type": "user", "cwd": "/new", "sessionId": "s1",
			},
		},
		{
			name:    "leaves line without cwd unchanged",
			line:    `{"type":"file-history-snapshot","messageId":"m1"}`,
			newCwd:  "/new",
			wantCwd: "", // absent
			wantFields: map[string]any{
				"type": "file-history-snapshot", "messageId": "m1",
			},
		},
		{
			name:    "leaves nested cwd inside message content alone",
			line:    `{"type":"user","cwd":"/old","message":{"content":"the cwd is /old"}}`,
			newCwd:  "/new",
			wantCwd: "/new",
			wantFields: map[string]any{
				"type":    "user",
				"cwd":     "/new",
				"message": map[string]any{"content": "the cwd is /old"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteTopLevelCwd([]byte(tc.line), tc.newCwd)
			var gotMap map[string]any
			if err := json.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("rewritten line not JSON: %v\n%s", err, got)
			}
			for k, v := range tc.wantFields {
				gv, ok := gotMap[k]
				if !ok {
					t.Errorf("missing key %q", k)
					continue
				}
				gj, _ := json.Marshal(gv)
				wj, _ := json.Marshal(v)
				if string(gj) != string(wj) {
					t.Errorf("key %q: got %s, want %s", k, gj, wj)
				}
			}
		})
	}
}

// TestRewriteTopLevelCwdPassThroughOnUnparseable: a non-JSON line (rare
// in practice; defensive) passes through verbatim so a malformed source
// doesn't poison the canonical copy.
func TestRewriteTopLevelCwdPassThroughOnUnparseable(t *testing.T) {
	in := []byte("not-json-at-all")
	got := rewriteTopLevelCwd(in, "/new")
	if string(got) != "not-json-at-all" {
		t.Fatalf("non-JSON line modified: got %q", got)
	}
}

// jsonlLine marshals m to a single JSON line plus a trailing newline.
// Used to assemble multi-line fixture bodies inline without escaping
// embedded quotes.
func jsonlLine(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}
