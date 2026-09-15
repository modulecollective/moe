package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

func newDevEnvTestRoot(t *testing.T) string {
	t.Helper()
	root := gittest.Init(t)
	gittest.Run(t, root, "commit", "--allow-empty", "-m", "seed bureaucracy")
	return root
}

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

func TestDevEnvCacheRevisionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, devEnvCacheRel)
	revision := strings.Repeat("a", 40)
	if err := writeDevEnvCacheRevision(cachePath, map[string]string{"TOKEN": "secret"}, "tele/fix-it", revision); err != nil {
		t.Fatal(err)
	}

	cache, ok, err := readDevEnvCacheRevision(cachePath)
	if err != nil || !ok {
		t.Fatalf("read cache: ok=%v err=%v", ok, err)
	}
	if cache.owner != "tele/fix-it" || cache.revision != revision {
		t.Fatalf("metadata = %q %q", cache.owner, cache.revision)
	}
	if cache.env["TOKEN"] != "secret" {
		t.Fatalf("TOKEN = %q", cache.env["TOKEN"])
	}
	if _, exported := cache.env["moe-dev-env-v1"]; exported {
		t.Fatal("cache metadata was exported as an environment variable")
	}
	body, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := devEnvCacheHeaderPrefix + "tele/fix-it " + revision + "\n"
	if !strings.HasPrefix(string(body), wantHeader) {
		t.Fatalf("cache missing header %q:\n%s", wantHeader, body)
	}
}

func TestDevEnvCacheRevisionRejectsMalformedAndUnsupportedMetadata(t *testing.T) {
	for _, header := range []string{
		"# moe-dev-env-v1 missing-revision",
		"# moe-dev-env-v1 tele/fix-it not-a-revision",
		"# moe-dev-env-v2 tele/fix-it " + strings.Repeat("a", 40),
	} {
		t.Run(header, func(t *testing.T) {
			cachePath := filepath.Join(t.TempDir(), "cache")
			if err := os.WriteFile(cachePath, []byte(header+"\nK=v\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readDevEnvCacheRevision(cachePath); err == nil {
				t.Fatalf("metadata %q should fail", header)
			}
			env, ok, err := readDevEnvCache(cachePath)
			if err != nil || !ok || env["K"] != "v" {
				t.Fatalf("raw teardown reader should ignore metadata: ok=%v env=%v err=%v", ok, env, err)
			}
		})
	}
}

func TestDevEnvHookRevisionScopesToCommittedExactRunChanges(t *testing.T) {
	root := newDevEnvTestRoot(t)
	md := &run.Metadata{Project: "tele", ID: "fix-it"}
	hook := filepath.Join(root, project.Dir("tele"), "hooks", devEnvDirRel, "10-env.sh")
	writeFile(t, hook, "v1\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed hooks")

	assertRevision := func(want string) {
		t.Helper()
		got, err := devEnvHookRevision(root, md)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("revision = %q, want %q", got, want)
		}
	}
	assertRevision(devEnvNoHookRevision)

	commit := func(path, body, message string) {
		t.Helper()
		writeFile(t, filepath.Join(root, path), body)
		gittest.Run(t, root, "add", "-A")
		gittest.Run(t, root, "commit", "-m", message)
	}
	commit(filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "10-env.sh"), "other-run\n",
		"other run\n\nMoE-Project: tele\nMoE-Run: fix-it-2\n")
	commit(filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "10-env.sh"), "hand-edit\n", "hand edit")
	commit(filepath.Join(project.Dir("other"), "hooks", devEnvDirRel, "10-env.sh"), "other-project\n",
		"other project\n\nMoE-Project: other\nMoE-Run: fix-it\n")
	commit("README.md", "unrelated\n", "unrelated\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	assertRevision(devEnvNoHookRevision)

	gittest.Run(t, root, "checkout", "-b", "session")
	commit(filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "10-env.sh"), "unlanded\n",
		"session hook\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	gittest.Run(t, root, "checkout", "main")
	assertRevision(devEnvNoHookRevision)

	commit(filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "20-own.sh"), "own\n",
		"own hook\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	assertRevision(gittest.HeadSHA(t, root))

	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", hook)
	gittest.Run(t, root, "commit", "-m", "mode hook\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	assertRevision(gittest.HeadSHA(t, root))
	gittest.Run(t, root, "mv",
		filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "10-env.sh"),
		filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel, "renamed.sh"))
	gittest.Run(t, root, "commit", "-m", "rename hooks\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	assertRevision(gittest.HeadSHA(t, root))
	gittest.Run(t, root, "rm", "-r", filepath.Join(project.Dir("tele"), "hooks", devEnvDirRel))
	gittest.Run(t, root, "commit", "-m", "delete hooks\n\nMoE-Project: tele\nMoE-Run: fix-it\n")
	assertRevision(gittest.HeadSHA(t, root))
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

func TestStaleDevEnvWritableDirFollowsSymlinks(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "linked-dir")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stale, err := staleDevEnvWritableDir(map[string]string{"MOE_HOME": link})
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("valid symlink reported stale: %+v", stale)
	}
}

func TestStaleDevEnvWritableDirClassifiesInvalidPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenLink := filepath.Join(t.TempDir(), "broken")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), brokenLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, path := range []string{missing, file, brokenLink} {
		stale, err := staleDevEnvWritableDir(map[string]string{"MOE_DEV_TMPDIR": path})
		if err != nil {
			t.Fatalf("path %q: %v", path, err)
		}
		if stale == nil || stale.key != "MOE_DEV_TMPDIR" || stale.path != path {
			t.Errorf("path %q: stale = %+v", path, stale)
		}
	}
}

func TestStaleDevEnvWritableDirIgnoresNonContractValues(t *testing.T) {
	stale, err := staleDevEnvWritableDir(map[string]string{
		"MOE_HOME":     "relative/path",
		"DATABASE_URL": filepath.Join(t.TempDir(), "missing"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stale != nil {
		t.Fatalf("non-contract value reported stale: %+v", stale)
	}
}

func TestStaleDevEnvWritableDirReturnsUnexpectedStatError(t *testing.T) {
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	stale, err := staleDevEnvWritableDir(map[string]string{"MOE_HOME": loop})
	if err == nil {
		t.Fatalf("expected symlink-loop stat error, got stale=%+v", stale)
	}
	if stale != nil {
		t.Fatalf("unexpected stat error must not be classified stale: %+v", stale)
	}
	if !strings.Contains(err.Error(), "stat cached MOE_HOME") {
		t.Fatalf("error does not name the cached key: %v", err)
	}
}

func TestStaleDevEnvWritableDirStatErrorOutranksStalePath(t *testing.T) {
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	env := map[string]string{
		"MOE_HOME":       filepath.Join(t.TempDir(), "missing"),
		"MOE_DEV_TMPDIR": loop,
	}

	stale, err := staleDevEnvWritableDir(env)
	if err == nil {
		t.Fatalf("expected stat error to prevent rebuild, got stale=%+v", stale)
	}
	if stale != nil {
		t.Fatalf("stat error must outrank an earlier stale path: %+v", stale)
	}
}

// TestDevEnvSetupEnvCachesScriptOutput: a project with a single
// dev-env.d/* script runs it on first call, caches the parsed output,
// and re-sources the cache on subsequent calls without re-running.
func TestDevEnvSetupEnvCachesScriptOutput(t *testing.T) {
	root := newDevEnvTestRoot(t)
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

func TestDevEnvSetupEnvRebuildsAfterOwnCommittedHookChange(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projectID := "tele"
	runID := "fix-it"
	setupDir := filepath.Join(root, project.Dir(projectID), "hooks", devEnvDirRel)
	teardownDir := filepath.Join(root, project.Dir(projectID), "hooks", devEnvTeardownDirRel)
	receipt := filepath.Join(t.TempDir(), "receipt")
	writeSetup := func(generation string) {
		t.Helper()
		body := fmt.Sprintf(`#!/bin/sh
if [ -e "$MOE_SANDBOX/.moe/dev-env.env" ]; then
  exit 9
fi
printf 'setup:%s\n' >> %q
printf 'GEN=%s\n'
`, generation, receipt, generation)
		writeFile(t, filepath.Join(setupDir, "10-env.sh"), body)
	}
	writeSetup("v1")
	writeFile(t, filepath.Join(teardownDir, "10-clean.sh"), fmt.Sprintf(`#!/bin/sh
printf 'teardown:%%s\n' "$GEN" >> %q
`, receipt))
	if err := os.Chmod(filepath.Join(setupDir, "10-env.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(teardownDir, "10-clean.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "seed dev env")

	workTree := t.TempDir()
	md := &run.Metadata{ID: runID, Project: projectID, Workflow: "sdlc"}
	env, fresh, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil || !fresh || env["GEN"] != "v1" {
		t.Fatalf("initial setup: env=%v fresh=%v err=%v", env, fresh, err)
	}
	// Model an environment minted by the pre-header implementation. Once
	// this run gains a hook commit, the legacy cache cannot prove it
	// incorporated that commit and must rebuild once.
	if err := writeDevEnvCache(filepath.Join(workTree, devEnvCacheRel), env); err != nil {
		t.Fatal(err)
	}

	writeSetup("v2")
	if err := os.Chmod(filepath.Join(setupDir, "10-env.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m",
		"update dev env\n\nMoE-Project: "+projectID+"\nMoE-Run: "+runID+"\n")
	wantRevision := gittest.HeadSHA(t, root)

	var stderr bytes.Buffer
	env, fresh, err = devEnvSetupEnv(root, workTree, md, io.Discard, &stderr)
	if err != nil || !fresh || env["GEN"] != "v2" {
		t.Fatalf("rebuild: env=%v fresh=%v err=%v stderr=%s", env, fresh, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "hook revision") {
		t.Fatalf("missing stale revision diagnostic: %q", stderr.String())
	}
	receiptBody, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(receiptBody), "setup:v1\nteardown:v1\nsetup:v2\n"; got != want {
		t.Fatalf("lifecycle = %q, want %q", got, want)
	}
	cache, ok, err := readDevEnvCacheRevision(filepath.Join(workTree, devEnvCacheRel))
	if err != nil || !ok || cache.owner != projectID+"/"+runID || cache.revision != wantRevision {
		t.Fatalf("cache metadata: ok=%v cache=%+v err=%v", ok, cache, err)
	}

	_, fresh, err = devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil || fresh {
		t.Fatalf("second v2 call: fresh=%v err=%v", fresh, err)
	}
	receiptBody, err = os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if string(receiptBody) != "setup:v1\nteardown:v1\nsetup:v2\n" {
		t.Fatalf("cache hit reran hooks: %q", receiptBody)
	}
}

func TestDevEnvSetupEnvAdoptsLegacyCacheWithoutOwnHookChange(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projectID := "tele"
	hookDir := filepath.Join(root, project.Dir(projectID), "hooks", devEnvDirRel)
	writeFile(t, filepath.Join(hookDir, "10-must-not-run.sh"), "#!/bin/sh\nexit 9\n")
	if err := os.Chmod(filepath.Join(hookDir, "10-must-not-run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "outside hook edit")

	workTree := t.TempDir()
	cachePath := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCache(cachePath, map[string]string{"GEN": "warm"}); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "new-run", Project: projectID, Workflow: "sdlc"}
	env, fresh, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil || fresh || env["GEN"] != "warm" {
		t.Fatalf("legacy adoption: env=%v fresh=%v err=%v", env, fresh, err)
	}
	cache, ok, err := readDevEnvCacheRevision(cachePath)
	if err != nil || !ok || cache.owner != "tele/new-run" || cache.revision != devEnvNoHookRevision {
		t.Fatalf("adopted metadata: ok=%v cache=%+v err=%v", ok, cache, err)
	}
}

func TestDevEnvSetupEnvMetadataErrorDoesNotRunTeardown(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projectID := "tele"
	receipt := filepath.Join(t.TempDir(), "receipt")
	teardown := filepath.Join(root, project.Dir(projectID), "hooks", devEnvTeardownDirRel, "10-clean.sh")
	writeFile(t, teardown, "#!/bin/sh\ntouch "+receipt+"\n")
	if err := os.Chmod(teardown, 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	cachePath := filepath.Join(workTree, devEnvCacheRel)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# moe-dev-env-v2 tele/fix-it " + strings.Repeat("a", 40) + "\nGEN=old\n"
	if err := os.WriteFile(cachePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "fix-it", Project: projectID, Workflow: "sdlc"}
	if _, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard); err == nil {
		t.Fatal("malformed metadata should fail")
	}
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("teardown ran through metadata error: %v", err)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil || string(got) != body {
		t.Fatalf("cache changed: body=%q err=%v", got, err)
	}
}

func TestDevEnvInspectCacheFindsOwnRevisionAndKeepsNewRunWarm(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projectID := "tele"
	hook := filepath.Join(root, project.Dir(projectID), "hooks", devEnvDirRel, "10-env.sh")
	writeFile(t, hook, "v1\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "outside hook")

	workTree := t.TempDir()
	cachePath := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCacheRevision(cachePath, map[string]string{"GEN": "warm"}, "tele/fix-it", devEnvNoHookRevision); err != nil {
		t.Fatal(err)
	}
	writeFile(t, hook, "v2\n")
	gittest.Run(t, root, "add", "-A")
	gittest.Run(t, root, "commit", "-m", "own hook\n\nMoE-Project: tele\nMoE-Run: fix-it\n")

	oldRun := &run.Metadata{Project: projectID, ID: "fix-it"}
	_, ok, staleDir, staleRevision, err := devEnvInspectCache(root, workTree, oldRun)
	if err != nil || !ok || staleDir != nil || !staleRevision {
		t.Fatalf("old holder: ok=%v staleDir=%v staleRevision=%v err=%v", ok, staleDir, staleRevision, err)
	}
	newRun := &run.Metadata{Project: projectID, ID: "next-run"}
	env, ok, staleDir, staleRevision, err := devEnvInspectCache(root, workTree, newRun)
	if err != nil || !ok || staleDir != nil || staleRevision || env["GEN"] != "warm" {
		t.Fatalf("new holder: env=%v ok=%v staleDir=%v staleRevision=%v err=%v", env, ok, staleDir, staleRevision, err)
	}
}

func TestDevEnvSetupEnvGitErrorPreservesCacheAndSkipsTeardown(t *testing.T) {
	root := t.TempDir()
	projectID := "tele"
	receipt := filepath.Join(t.TempDir(), "receipt")
	teardown := filepath.Join(root, project.Dir(projectID), "hooks", devEnvTeardownDirRel, "10-clean.sh")
	writeFile(t, teardown, "#!/bin/sh\ntouch "+receipt+"\n")
	if err := os.Chmod(teardown, 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	cachePath := filepath.Join(workTree, devEnvCacheRel)
	body := "GEN=old\n"
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "fix-it", Project: projectID, Workflow: "sdlc"}
	if _, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "resolve hook revision") {
		t.Fatalf("expected git revision error, got %v", err)
	}
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("teardown ran through git error: %v", err)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil || string(got) != body {
		t.Fatalf("cache changed: body=%q err=%v", got, err)
	}
}

func TestDevEnvSetupEnvValidWritableDirCacheHit(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projID := "tele"
	hookDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "10-must-not-run.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	writableDir := t.TempDir()
	cache := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCache(cache, map[string]string{"MOE_HOME": writableDir}); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "verify", Project: projID, Workflow: "sdlc"}

	var stdout bytes.Buffer
	env, fresh, err := devEnvSetupEnv(root, workTree, md, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("valid cache should remain a cache hit")
	}
	if env["MOE_HOME"] != writableDir {
		t.Fatalf("MOE_HOME = %q, want %q", env["MOE_HOME"], writableDir)
	}
	if !strings.Contains(stdout.String(), "dev-env cached") {
		t.Fatalf("missing cache-hit banner: %q", stdout.String())
	}
}

func TestDevEnvSetupEnvRebuildsStaleWritableDirCache(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projID := "tele"
	setupDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	teardownDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}

	receipt := filepath.Join(t.TempDir(), "receipt")
	counter := filepath.Join(t.TempDir(), "counter")
	oldDir := filepath.Join(t.TempDir(), "old-home")
	newDir := filepath.Join(t.TempDir(), "new-home")
	teardown := "#!/bin/sh\nprintf 'teardown:%s\\n' \"$MOE_HOME\" >> " + receipt + "\n"
	if err := os.WriteFile(filepath.Join(teardownDir, "10-cleanup.sh"), []byte(teardown), 0o755); err != nil {
		t.Fatal(err)
	}
	setup := "#!/bin/sh\n" +
		"if [ -e \"$MOE_SANDBOX/.moe/dev-env.env\" ]; then exit 1; fi\n" +
		"n=0\n" +
		"if [ -f " + counter + " ]; then n=$(cat " + counter + "); fi\n" +
		"n=$((n+1))\n" +
		"echo $n > " + counter + "\n" +
		"dir=" + newDir + "\n" +
		"if [ $n -eq 1 ]; then dir=" + oldDir + "; fi\n" +
		"mkdir -p \"$dir\"\n" +
		"printf 'setup:%s\\n' \"$dir\" >> " + receipt + "\n" +
		"echo MOE_HOME=$dir\n"
	if err := os.WriteFile(filepath.Join(setupDir, "10-create.sh"), []byte(setup), 0o755); err != nil {
		t.Fatal(err)
	}

	workTree := t.TempDir()
	cache := filepath.Join(workTree, devEnvCacheRel)
	md := &run.Metadata{ID: "verify", Project: projID, Workflow: "sdlc"}

	firstEnv, firstFresh, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard)
	if err != nil || !firstFresh {
		t.Fatalf("initial setup: fresh=%v err=%v", firstFresh, err)
	}
	if firstEnv["MOE_HOME"] != oldDir {
		t.Fatalf("initial MOE_HOME = %q, want %q", firstEnv["MOE_HOME"], oldDir)
	}
	if err := os.RemoveAll(oldDir); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	env, fresh, err := devEnvSetupEnv(root, workTree, md, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("rebuild stale cache: %v (stderr=%s)", err, stderr.String())
	}
	if !fresh {
		t.Fatal("stale cache rebuild should be reported as freshly minted")
	}
	if env["MOE_HOME"] != newDir {
		t.Fatalf("returned MOE_HOME = %q, want %q", env["MOE_HOME"], newDir)
	}
	body, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "setup:"+oldDir+"\nteardown:"+oldDir+"\nsetup:"+newDir+"\n"; got != want {
		t.Fatalf("lifecycle order = %q, want %q", got, want)
	}
	cached, ok, err := readDevEnvCache(cache)
	if err != nil || !ok {
		t.Fatalf("read rebuilt cache: ok=%v err=%v", ok, err)
	}
	if cached["MOE_HOME"] != newDir {
		t.Fatalf("cached MOE_HOME = %q, want %q", cached["MOE_HOME"], newDir)
	}
	for _, want := range []string{"MOE_HOME", oldDir, "stale; rebuilding"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stale diagnostic missing %q: %q", want, stderr.String())
		}
	}
}

func TestDevEnvSetupEnvStaleCacheTeardownFailurePreservesCache(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projID := "tele"
	teardownDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvTeardownDirRel)
	if err := os.MkdirAll(teardownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teardownDir, "10-fail.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	missing := filepath.Join(t.TempDir(), "vanished")
	cache := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCache(cache, map[string]string{"MOE_HOME": missing}); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "verify", Project: projID, Workflow: "sdlc"}

	if _, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard); err == nil {
		t.Fatal("expected teardown failure")
	}
	cached, ok, err := readDevEnvCache(cache)
	if err != nil || !ok {
		t.Fatalf("old cache should remain: ok=%v err=%v", ok, err)
	}
	if cached["MOE_HOME"] != missing {
		t.Fatalf("cached MOE_HOME = %q, want %q", cached["MOE_HOME"], missing)
	}
}

func TestDevEnvSetupEnvStaleCacheSetupFailureLeavesNoCache(t *testing.T) {
	root := newDevEnvTestRoot(t)
	projID := "tele"
	setupDir := filepath.Join(root, project.Dir(projID), "hooks", devEnvDirRel)
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(setupDir, "10-fail.sh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workTree := t.TempDir()
	cache := filepath.Join(workTree, devEnvCacheRel)
	if err := writeDevEnvCache(cache, map[string]string{"MOE_HOME": filepath.Join(t.TempDir(), "vanished")}); err != nil {
		t.Fatal(err)
	}
	md := &run.Metadata{ID: "verify", Project: projID, Workflow: "sdlc"}

	if _, _, err := devEnvSetupEnv(root, workTree, md, io.Discard, io.Discard); err == nil {
		t.Fatal("expected setup failure")
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache should be absent after setup failure: %v", err)
	}
}

// TestDevEnvSetupEnvNoHooksDirectory: a project that ships no
// dev-env.d/ returns an empty map and writes an empty cache — the
// "operator's real env" baseline that the design specifies as the
// no-hook case.
func TestDevEnvSetupEnvNoHooksDirectory(t *testing.T) {
	root := newDevEnvTestRoot(t)
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
	root := newDevEnvTestRoot(t)
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
	root := newDevEnvTestRoot(t)
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
	root := newDevEnvTestRoot(t)
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
	root := newDevEnvTestRoot(t)
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
	vanishedHome := filepath.Join(t.TempDir(), "vanished")
	// Pre-seed the cache as if dev-env.d had already produced it.
	if err := writeDevEnvCache(filepath.Join(workTree, devEnvCacheRel), map[string]string{
		"DATABASE_URL": "postgres://localhost/x",
		"MOE_HOME":     vanishedHome,
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
	want := "tearing down DATABASE_URL=postgres://localhost/x MOE_HOME=" + vanishedHome + "\n"
	if string(body) != want {
		t.Fatalf("receipt = %q\nwant      %q", body, want)
	}
}

// TestDevEnvRunTeardownNoCacheNoOp: no cache means setup never ran;
// teardown is a silent no-op.
func TestDevEnvRunTeardownNoCacheNoOp(t *testing.T) {
	root := newDevEnvTestRoot(t)
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
