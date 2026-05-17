package transcript

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestParseReal smoke-runs the parsers and renderer against the live
// thread-*.jsonl files sitting in the bureaucracy under the active
// run. Skips when the files aren't reachable (e.g. running outside a
// MoE clone, or before the bureaucracy is fetched), so the test is a
// safety net in dev but never the cause of a CI failure on a fresh
// checkout.
//
// What it catches: an upstream schema change that hits the head or
// tail of a real transcript before we update the adapter. The
// fixture tests cover the per-event shape; this test catches "did
// something break end-to-end on the actual files the operator
// renders."
func TestParseReal(t *testing.T) {
	cases := []struct {
		agent string
		path  string
	}{
		{
			agent: "claude",
			path:  realPath(t, "more-context-when-one-shot-bails-2026-05-16", "design", "thread-claude.jsonl"),
		},
		{
			agent: "codex",
			path:  realPath(t, "headless-codex-fails-with-permissions-2026-05-15-4", "code", "thread-codex.jsonl"),
		},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			if c.path == "" {
				t.Skip("real transcript not reachable from clone")
			}
			f, err := os.Open(c.path)
			if err != nil {
				t.Skipf("open %s: %v", c.path, err)
			}
			defer f.Close()
			ev, err := Parse(c.agent, f)
			if err != nil {
				t.Fatalf("parse %s: %v", c.path, err)
			}
			if len(ev) == 0 {
				t.Fatalf("parse %s: got 0 events, expected nontrivial transcript", c.path)
			}
			// Render to a discard writer to make sure no event
			// produces a write error.
			if err := Render(io.Discard, ev, RenderOptions{}); err != nil {
				t.Fatalf("render %s: %v", c.path, err)
			}
			// And buffer-render a tail so we exercise that path too.
			var buf bytes.Buffer
			if err := Render(&buf, Tail(ev, 5), RenderOptions{}); err != nil {
				t.Fatalf("render tail %s: %v", c.path, err)
			}
			if buf.Len() == 0 {
				t.Errorf("render tail produced empty output for %s", c.path)
			}
		})
	}
}

// realPath walks up from cwd looking for the bureaucracy worktree
// that holds projects/moe/runs/<run>/documents/<stage>/<file>. Returns
// "" when the path isn't reachable — caller skips. The walk handles
// running the test from either the clone or the bureaucracy worktree.
func realPath(t *testing.T, runID, stage, name string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	// Search upward for the bureaucracy root that contains the run.
	// We won't find it inside the per-run clone — the bureaucracy
	// sits next to .moe/clones, three levels up.
	suffix := filepath.Join("projects", "moe", "runs", runID, "documents", stage, name)
	dir := cwd
	for {
		// Common layouts: <bureaucracy>/.moe/clones/<proj>/<run>/...
		// or <bureaucracy>/.moe/worktrees/<uuid>/...
		// Walk up until we find a `.moe/` dir, then try the parent.
		if _, err := os.Stat(filepath.Join(dir, ".moe")); err == nil {
			// dir is the bureaucracy root or a worktree.
			candidate := filepath.Join(dir, suffix)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
