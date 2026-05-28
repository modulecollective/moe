package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// TestParseDevEnvLinesValidKeys: well-shaped KEY=VALUE lines land in
// the map; blank lines and comments are ignored.
func TestParseDevEnvLinesValidKeys(t *testing.T) {
	in := `# a comment
DATABASE_URL=postgres://localhost/foo

PORT=8080
MOE_DEV_TMPDIR=/tmp/abc
`
	env, err := parseDevEnvLines(strings.NewReader(in), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DATABASE_URL":   "postgres://localhost/foo",
		"PORT":           "8080",
		"MOE_DEV_TMPDIR": "/tmp/abc",
	}
	if len(env) != len(want) {
		t.Fatalf("len = %d, want %d, got %+v", len(env), len(want), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
}

// TestParseDevEnvLinesMalformedSkipped: lines without `=` or with an
// empty key are warned and skipped; valid lines still land.
func TestParseDevEnvLinesMalformedSkipped(t *testing.T) {
	var warns bytes.Buffer
	in := `no equals sign
=missing-key
GOOD=ok
`
	env, err := parseDevEnvLines(strings.NewReader(in), &warns)
	if err != nil {
		t.Fatal(err)
	}
	if env["GOOD"] != "ok" {
		t.Fatalf("GOOD = %q", env["GOOD"])
	}
	if _, ok := env[""]; ok {
		t.Fatal("empty key landed in env")
	}
	if !strings.Contains(warns.String(), "malformed line 1") {
		t.Fatalf("expected warning for line 1, got: %q", warns.String())
	}
}

// TestParseDevEnvLinesPreservesValueWhitespace: a value with trailing
// whitespace keeps it — projects that want whitespace can have it.
func TestParseDevEnvLinesPreservesValueWhitespace(t *testing.T) {
	in := "K=  trailing   \n"
	env, err := parseDevEnvLines(strings.NewReader(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["K"] != "  trailing   " {
		t.Fatalf("value = %q (expected leading/trailing whitespace preserved)", env["K"])
	}
}

// TestDevEnvCacheRoundTrip: writing and re-reading the cache file
// yields the same map, sorted on disk for diff-friendliness.
func TestDevEnvCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, devEnvCacheRel)
	env := map[string]string{
		"DATABASE_URL": "postgres://localhost/foo",
		"PORT":         "8080",
		"MOE_HOME":     "/tmp/bureaucracy",
	}
	if err := writeDevEnvCache(cache, env); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted output: DATABASE_URL first alphabetically.
	if !strings.HasPrefix(string(body), "DATABASE_URL=") {
		t.Fatalf("expected sorted output, got:\n%s", body)
	}
	got, ok, err := readDevEnvCache(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected cache to load")
	}
	if len(got) != len(env) {
		t.Fatalf("got %d entries, want %d", len(got), len(env))
	}
	for k, v := range env {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestDevEnvWritableDirsHappyPath: both recognised keys point at
// absolute, disjoint directories — both come back, cleaned, in key
// declaration order (MOE_HOME before MOE_DEV_TMPDIR).
func TestDevEnvWritableDirsHappyPath(t *testing.T) {
	env := map[string]string{
		"MOE_HOME":       "/tmp/bureaucracy/",
		"MOE_DEV_TMPDIR": "/tmp/devtmp//abc",
	}
	got := devEnvWritableDirs(env)
	want := []string{"/tmp/bureaucracy", "/tmp/devtmp/abc"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestDevEnvWritableDirsSkipsEmptyAndUnrelated: empty values and
// values for keys outside the allowlist are silently dropped — the
// allowlist is the only contract.
func TestDevEnvWritableDirsSkipsEmptyAndUnrelated(t *testing.T) {
	env := map[string]string{
		"MOE_HOME":     "",
		"DATABASE_URL": "/tmp/db",
		"PORT":         "8080",
	}
	if got := devEnvWritableDirs(env); got != nil {
		t.Fatalf("expected nil for empty/unrelated values, got %v", got)
	}
}

// TestDevEnvWritableDirsRejectsRelativePaths: a relative value would
// be ambiguous under a subprocess (cwd-relative? root-relative?), so
// the filter drops it rather than widening the sandbox unsafely.
func TestDevEnvWritableDirsRejectsRelativePaths(t *testing.T) {
	env := map[string]string{
		"MOE_HOME":       "relative/path",
		"MOE_DEV_TMPDIR": "/tmp/keepme",
	}
	got := devEnvWritableDirs(env)
	if len(got) != 1 || got[0] != "/tmp/keepme" {
		t.Fatalf("got %v, want [/tmp/keepme]", got)
	}
}

// TestDevEnvWritableDirsDeduplicates: a project that points both keys
// at the same directory (or one nested via path-equivalent cleaning)
// gets a single entry — repeated --add-dir <same-path> is harmless
// but noisy.
func TestDevEnvWritableDirsDeduplicates(t *testing.T) {
	env := map[string]string{
		"MOE_HOME":       "/tmp/shared",
		"MOE_DEV_TMPDIR": "/tmp/shared/",
	}
	got := devEnvWritableDirs(env)
	if len(got) != 1 || got[0] != "/tmp/shared" {
		t.Fatalf("got %v, want [/tmp/shared]", got)
	}
}

// TestDevEnvWritableDirsEmptyMap: a project that ships no dev-env
// hooks (or one whose hooks emit no recognised keys) returns nil —
// stage callers branch on the nil-vs-non-nil signal to decide whether
// to widen the sandbox at all.
func TestDevEnvWritableDirsEmptyMap(t *testing.T) {
	if got := devEnvWritableDirs(nil); got != nil {
		t.Fatalf("expected nil for nil map, got %v", got)
	}
	if got := devEnvWritableDirs(map[string]string{}); got != nil {
		t.Fatalf("expected nil for empty map, got %v", got)
	}
}

// TestDevEnvSetupEnvCachesScriptOutput: a project with a single
// dev-env.d/* script runs it on first call, caches the parsed output,
// and re-sources the cache on subsequent calls without re-running.
func TestDevEnvSetupEnvCachesScriptOutput(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	if err := os.MkdirAll(filepath.Join(root, project.Dir(projID)), 0o755); err != nil {
		t.Fatal(err)
	}
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Echo a different value each call so we can prove caching works.
	counterFile := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(counterFile, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
n=$(cat ` + counterFile + `)
n=$((n+1))
echo "$n" > ` + counterFile + `
echo "DEV_RUN=$n"
echo "DATABASE_URL=postgres://localhost/devenv-${MOE_RUN}"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-seed.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	md := &run.Metadata{ID: "verify", Project: projID, Workflow: "sdlc"}

	var stdout, stderr bytes.Buffer
	env1, fresh1, err := devEnvSetupEnv(root, workTree, md, &stdout, &stderr)
	if err != nil {
		t.Fatalf("first call: %v (stderr=%s)", err, stderr.String())
	}
	if !fresh1 {
		t.Fatal("first call should mint the cache")
	}
	if env1["DEV_RUN"] != "1" {
		t.Fatalf("DEV_RUN = %q on first call; expected 1", env1["DEV_RUN"])
	}
	if env1["DATABASE_URL"] != "postgres://localhost/devenv-verify" {
		t.Fatalf("DATABASE_URL = %q; MOE_RUN substitution failed", env1["DATABASE_URL"])
	}

	// Second call must hit the cache — DEV_RUN stays at "1".
	env2, fresh2, err := devEnvSetupEnv(root, workTree, md, &stdout, &stderr)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if fresh2 {
		t.Fatal("second call should not re-mint the cache")
	}
	if env2["DEV_RUN"] != "1" {
		t.Fatalf("DEV_RUN = %q on second call; cache wasn't used", env2["DEV_RUN"])
	}
}

// TestDevEnvSetupEnvNoHooksDirectory: a project that ships no
// dev-env.d/ returns an empty map and writes an empty cache — the
// "operator's real env" baseline that the design specifies as the
// no-hook case.
func TestDevEnvSetupEnvNoHooksDirectory(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	if err := os.MkdirAll(filepath.Join(root, project.Dir(projID)), 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc"}

	env, fresh, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !fresh {
		t.Fatal("first call against missing dir should still mint an empty cache")
	}
	if len(env) != 0 {
		t.Fatalf("expected empty env, got %+v", env)
	}
}

// TestDevEnvSetupEnvWorkspaceExportsMoeWorkspace: a workspace-bound
// run sees MOE_WORKSPACE in the script's environment so the script can
// branch on sandbox-vs-workspace.
func TestDevEnvSetupEnvWorkspaceExportsMoeWorkspace(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "WS=${MOE_WORKSPACE:-NONE}"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-ws.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc", Workspace: "warm"}

	env, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if env["WS"] != "warm" {
		t.Fatalf("WS = %q; expected the workspace name to flow through MOE_WORKSPACE", env["WS"])
	}
}

// TestDevEnvSetupEnvExportsMoeBureaucracy: the bureaucracy root flows
// through as MOE_BUREAUCRACY so setup scripts can read project metadata
// (project.json, hooks/, etc.) without walking up from MOE_SANDBOX.
// Pre-push hooks already see this var; this test pins the dev-env side.
func TestDevEnvSetupEnvExportsMoeBureaucracy(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "BUR=${MOE_BUREAUCRACY:-MISSING}"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-bur.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc"}

	env, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if env["BUR"] != root {
		t.Fatalf("BUR = %q; expected MOE_BUREAUCRACY=%q to flow through", env["BUR"], root)
	}
}

// TestDevEnvSetupEnvLaterScriptSeesEarlierVars: scripts run in lex
// order and later ones see earlier ones' exports — projects can
// layer state across scripts.
func TestDevEnvSetupEnvLaterScriptSeesEarlierVars(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	first := `#!/bin/sh
echo "PORT=8080"
`
	second := `#!/bin/sh
echo "BACKEND_URL=http://localhost:$PORT"
`
	if err := os.WriteFile(filepath.Join(hookDir, "10-port.sh"), []byte(first), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "20-backend.sh"), []byte(second), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc"}

	env, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if env["BACKEND_URL"] != "http://localhost:8080" {
		t.Fatalf("BACKEND_URL = %q; later script didn't inherit PORT", env["BACKEND_URL"])
	}
}

// TestDevEnvRunTeardownSourcesCache: teardown scripts see the cached
// env as exported variables.
func TestDevEnvRunTeardownSourcesCache(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	teardownDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "receipt")
	script := `#!/bin/sh
echo "tearing down DATABASE_URL=$DATABASE_URL MOE_HOME=$MOE_HOME" > ` + receipt + `
`
	if err := os.WriteFile(filepath.Join(teardownDir, "10-cleanup.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	// Pre-seed the cache as if dev-env.d had already produced it.
	if err := writeDevEnvCache(filepath.Join(workTree, devEnvCacheRel), map[string]string{
		"DATABASE_URL": "postgres://localhost/x",
		"MOE_HOME":     "/tmp/bureaucracy",
	}); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc"}

	if err := devEnvRunTeardown(root, workTree, md, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	want := "tearing down DATABASE_URL=postgres://localhost/x MOE_HOME=/tmp/bureaucracy\n"
	if string(body) != want {
		t.Fatalf("receipt = %q\nwant      %q", body, want)
	}
}

// TestDevEnvRunTeardownNoCacheNoOp: no cache means setup never ran;
// teardown is a silent no-op.
func TestDevEnvRunTeardownNoCacheNoOp(t *testing.T) {
	root := t.TempDir()
	projID := "tele"
	teardownDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop a script that would fail if reached — proves the no-op
	// short-circuit hits before any script runs.
	if err := os.WriteFile(filepath.Join(teardownDir, "10-fail.sh"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	md := &run.Metadata{ID: "x", Project: projID, Workflow: "sdlc"}

	if err := devEnvRunTeardown(root, workTree, md, io.Discard, io.Discard); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

// TestDevEnvClearCacheIdempotent: clearing twice is a no-op the second
// time, no error.
func TestDevEnvClearCacheIdempotent(t *testing.T) {
	workTree := t.TempDir()
	cache := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCache(cache, map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := devEnvClearCache(workTree); err != nil {
		t.Fatal(err)
	}
	if err := devEnvClearCache(workTree); err != nil {
		t.Fatalf("second clear must be a no-op: %v", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache still on disk: %v", err)
	}
}

// TestListExecutablesSkipsNonExecutableAndDotfiles: matches pre-push's
// listing semantics.
func TestListExecutablesSkipsNonExecutableAndDotfiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("10-run.sh", 0o755)
	mustWrite("20-skip-no-exec.sh", 0o644)
	mustWrite(".dotfile.sh", 0o755)
	mustWrite("30-also-run.sh", 0o755)

	names, err := listExecutables(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names, ",")
	if got != "10-run.sh,30-also-run.sh" {
		t.Fatalf("listExecutables = %q (expected only executable non-dotfile, in lex order)", got)
	}
}

// TestListExecutablesMissingDirIsNoOp: a project with no hooks dir
// returns (nil, nil).
func TestListExecutablesMissingDirIsNoOp(t *testing.T) {
	names, err := listExecutables(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if names != nil {
		t.Fatalf("missing dir = %v, want nil", names)
	}
}
