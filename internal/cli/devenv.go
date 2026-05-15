package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modulecollective/moe/internal/banner"
	"github.com/modulecollective/moe/internal/project"
	"github.com/modulecollective/moe/internal/run"
)

// dev-env is the project hook event that turns a working tree (per-run
// sandbox or named workspace) into an isolated runtime for code- and
// test-stage agent activity. Setup scripts under
// projects/<p>/hooks/dev-env.d/* emit KEY=VALUE lines on stdout; the
// merged result is cached at <tree>/.moe/dev-env.env, sourced into the
// claude subprocess env on every subsequent stage open against the
// same tree, and re-sourced by `moe sdlc shell` so the operator's
// manual spot-check sees the same world the agent did.
//
// Teardown scripts under projects/<p>/hooks/dev-env-teardown.d/* run
// with the cached env sourced; sandbox runs invoke them at run close
// (alongside sandbox removal), and a future workspace teardown verb
// will invoke them when the operator scuttles a workspace.
//
// The hook directory layout matches pre-push: lex-sorted, executable
// only, dotfiles skipped, missing-or-empty is a no-op.

// devEnvCacheRel is the path under the working tree where the parsed
// KEY=VALUE output is cached after the first setup run. .moe/ is
// already moe-managed for workspaces (claim.json) and sandbox layout
// — adding dev-env.env alongside is shape-consistent.
const devEnvCacheRel = ".moe/dev-env.env"

// devEnvDirRel is the per-project hooks directory dev-env setup
// scripts live in. dev-env-teardown lives next to it under
// devEnvTeardownDirRel.
const (
	devEnvDirRel         = "dev-env.d"
	devEnvTeardownDirRel = "dev-env-teardown.d"
)

// devEnvSetupEnv resolves the env vars the claude subprocess (and any
// downstream `moe sdlc shell`) should see at stage open.
//
// First call against a working tree: walks projects/<p>/hooks/dev-env.d
// in lex order, executes each executable script with cwd = workTree
// and the standard MOE_* exported (plus MOE_WORKSPACE when md.Workspace
// is set), parses stdout as `KEY=VALUE` lines, and writes the merged
// result to <workTree>/.moe/dev-env.env. Subsequent calls re-source
// the cache without re-running setup.
//
// Returns the parsed map and true if the cache was minted on this
// call (so callers can log "running dev-env setup..." on first touch
// only). A project with no dev-env.d directory and no cache yields an
// empty map — the single-driver default.
func devEnvSetupEnv(root, workTree string, md *run.Metadata, stdout, stderr io.Writer) (map[string]string, bool, error) {
	cachePath := filepath.Join(workTree, devEnvCacheRel)
	if env, ok, err := readDevEnvCache(cachePath); err != nil {
		return nil, false, err
	} else if ok {
		// Cache hit short-circuits the setup walker — print one line
		// so the operator can tell a fast stage open (sourced cache)
		// apart from one that re-ran the scripts. Without this line
		// the cached path is silent and the operator can't tell why
		// a "running …" notice they expected didn't appear.
		banner.HookCacheHit(stdout, "dev-env", devEnvCacheRel)
		return env, false, nil
	}
	env, err := runDevEnvSetup(root, workTree, md, stdout, stderr)
	if err != nil {
		return nil, false, err
	}
	if err := writeDevEnvCache(cachePath, env); err != nil {
		return nil, false, err
	}
	return env, true, nil
}

// devEnvLoadCache returns the cached dev-env vars without re-running
// setup. Used by `moe sdlc shell` and by the teardown path so a
// session that never opened a code/test stage still gets the env back
// if some upstream call already minted it.
func devEnvLoadCache(workTree string) (map[string]string, bool, error) {
	return readDevEnvCache(filepath.Join(workTree, devEnvCacheRel))
}

// devEnvCachePath exports the cache location for callers that need to
// refer to it by path (refresh, teardown, tests).
func devEnvCachePath(workTree string) string {
	return filepath.Join(workTree, devEnvCacheRel)
}

// devEnvRunTeardown invokes projects/<p>/hooks/dev-env-teardown.d/* in
// lex order with the cached env sourced into each script's
// environment. The teardown scripts address resources by the same
// names the setup scripts emitted (`$DATABASE_URL`, `$MOE_DEV_TMPDIR`,
// etc.), so the cached file is the load-bearing record.
//
// A missing cache, missing teardown directory, or empty directory is
// a no-op — same shape as pre-push's "no scripts on disk" case.
// Returns the first non-zero exit as an error so callers can log it;
// callers must decide whether to halt or continue (sandbox-run close
// continues; refresh halts).
func devEnvRunTeardown(root, workTree string, md *run.Metadata, stdout, stderr io.Writer) error {
	env, ok, err := devEnvLoadCache(workTree)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing cached — either setup never ran, or refresh already
		// cleared it. No teardown work to do.
		return nil
	}
	return runDevEnvScripts(root, devEnvTeardownDirRel, workTree, md, env, stdout, stderr)
}

// devEnvClearCache deletes the cached env file. Idempotent — missing
// cache is a no-op. The refresh verb pairs this with a teardown call
// so the next stage open re-runs setup against a clean slate.
func devEnvClearCache(workTree string) error {
	err := os.Remove(filepath.Join(workTree, devEnvCacheRel))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("dev-env: clear cache: %w", err)
	}
	return nil
}

// readDevEnvCache loads the parsed KEY=VALUE map from cachePath.
// Returns (nil, false, nil) when the file doesn't exist — the same
// "no cache yet" signal callers branch on to decide whether to run
// setup.
func readDevEnvCache(cachePath string) (map[string]string, bool, error) {
	f, err := os.Open(cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("dev-env: read cache: %w", err)
	}
	defer f.Close()
	env, err := parseDevEnvLines(bufio.NewReader(f), nil)
	if err != nil {
		return nil, false, fmt.Errorf("dev-env: parse cache %s: %w", cachePath, err)
	}
	return env, true, nil
}

// writeDevEnvCache writes env back to cachePath as sorted KEY=VALUE
// lines, creating <workTree>/.moe/ if it doesn't already exist. Sorted
// output keeps the file diff-friendly for the rare case an operator
// peeks at it.
func writeDevEnvCache(cachePath string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return fmt.Errorf("dev-env: mkdir cache dir: %w", err)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	if err := os.WriteFile(cachePath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("dev-env: write cache: %w", err)
	}
	return nil
}

// runDevEnvSetup walks dev-env.d/* and accumulates each script's
// stdout into a merged KEY=VALUE map. Stderr passes through to the
// operator's terminal (so the script's human-readable status lines —
// "created postgres db myapp_dev_foo" — surface verbatim). Non-zero
// exit halts the chain and bubbles up.
func runDevEnvSetup(root, workTree string, md *run.Metadata, stdout, stderr io.Writer) (map[string]string, error) {
	dirRel := filepath.Join(project.Dir(md.Project), "hooks", devEnvDirRel)
	dir := filepath.Join(root, dirRel)
	scripts, err := listExecutables(dir)
	if err != nil {
		return nil, err
	}
	if len(scripts) == 0 {
		// No setup scripts — empty env is fine; the project is
		// operator-driven and never asked for isolation. Stay silent
		// like pre-push's no-scripts case so the section header isn't
		// announcing a walker with nothing to do.
		return map[string]string{}, nil
	}
	banner.HookSection(stdout, "dev-env setup", len(scripts), dirRel)
	env := map[string]string{}
	for _, script := range scripts {
		banner.HookStart(stdout, script)
		start := time.Now()
		// Indent script stderr under the per-script header. Setup
		// scripts emit short human status lines ("created postgres db
		// myapp_dev_foo") on stderr — KEY=VALUE goes via stdout — so
		// indenting groups them visually under the script that wrote
		// them. IndentStderr passes through on non-TTY destinations.
		out, runErr := runDevEnvSetupScript(filepath.Join(dir, script), workTree, md, env, banner.IndentStderr(stderr))
		banner.HookDone(stdout, script, time.Since(start))
		if runErr != nil {
			return nil, runErr
		}
		parsed, err := parseDevEnvLines(strings.NewReader(out), stderr)
		if err != nil {
			return nil, fmt.Errorf("dev-env: parse output of %s: %w", script, err)
		}
		for k, v := range parsed {
			env[k] = v
		}
	}
	return env, nil
}

// runDevEnvSetupScript executes a single setup script and returns its
// captured stdout. Earlier scripts' vars are exported into later
// scripts' env so a multi-script chain can layer state — e.g.,
// 10-port.sh emits PORT, 20-db.sh reads PORT.
func runDevEnvSetupScript(path, workTree string, md *run.Metadata, accumulated map[string]string, stderr io.Writer) (string, error) {
	cmd := exec.Command(path)
	cmd.Dir = workTree
	cmd.Env = append(devEnvBaseEnv(workTree, md), mapToEnv(accumulated)...)
	var stdoutBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dev-env: %s exited non-zero: %w", path, err)
	}
	return stdoutBuf.String(), nil
}

// runDevEnvScripts is the generic walker shared by the teardown path
// (and any future per-event walker that doesn't parse stdout). Each
// script gets the cached env merged on top of the base MOE_* vars;
// stdout AND stderr stream straight to the operator since teardown
// scripts don't communicate via KEY=VALUE.
func runDevEnvScripts(root, eventDirRel, workTree string, md *run.Metadata, cached map[string]string, stdout, stderr io.Writer) error {
	dirRel := filepath.Join(project.Dir(md.Project), "hooks", eventDirRel)
	dir := filepath.Join(root, dirRel)
	scripts, err := listExecutables(dir)
	if err != nil {
		return err
	}
	if len(scripts) == 0 {
		return nil
	}
	// Label the section by the event dir name (e.g. "dev-env-teardown.d")
	// — same shape the section header takes for setup, just with the
	// event-specific prefix the design's walker discussion calls out.
	banner.HookSection(stdout, eventDirRel, len(scripts), dirRel)
	for _, script := range scripts {
		banner.HookStart(stdout, script)
		start := time.Now()
		// Teardown stdout + stderr are both human output today; we
		// pass both through raw so a script that wants to write a
		// status line above an indented detail block can compose its
		// own layout.
		cmd := exec.Command(filepath.Join(dir, script))
		cmd.Dir = workTree
		cmd.Env = append(devEnvBaseEnv(workTree, md), mapToEnv(cached)...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		runErr := cmd.Run()
		banner.HookDone(stdout, script, time.Since(start))
		if runErr != nil {
			return fmt.Errorf("dev-env: %s exited non-zero: %w", filepath.Join(eventDirRel, script), runErr)
		}
	}
	return nil
}

// listExecutables returns the sorted list of executable, non-dotfile
// names directly under dir. A missing directory is a no-op (nil, nil)
// — same shape pre-push uses for "project has no scripts."
func listExecutables(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dev-env: read %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("dev-env: stat %s: %w", e.Name(), err)
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// devEnvBaseEnv is the minimum env every dev-env script (setup or
// teardown) sees: the operator's environment plus the MOE_* vars
// keyed off the run. MOE_WORKSPACE is set if and only if the run uses
// a named workspace — scripts branch on its presence to do different
// setup for sandbox vs workspace runs.
func devEnvBaseEnv(workTree string, md *run.Metadata) []string {
	base := append([]string(nil), os.Environ()...)
	base = append(base,
		"MOE_PROJECT="+md.Project,
		"MOE_RUN="+md.ID,
		"MOE_WORKFLOW="+md.Workflow,
		"MOE_SANDBOX="+workTree,
	)
	if md.Workspace != "" {
		base = append(base, "MOE_WORKSPACE="+md.Workspace)
	}
	return base
}

// devEnvWritableDirKeys names the dev-env env vars whose values are
// expected to be local directories the agent should be allowed to
// write to during code/test stages. The list is intentionally small:
// each key has to earn its slot by being a real isolated-runtime
// directory, not a URL, token, or command fragment. Both MOE_HOME and
// MOE_DEV_TMPDIR are emitted by the moe project's own dev-env hooks —
// MOE_HOME is the isolated bureaucracy the test-stage `moe` subprocess
// writes into; MOE_DEV_TMPDIR carries the seed bare repo
// (`$MOE_DEV_TMPDIR/widget.git`) that `moe project add file://...`
// test-stage commands target with file-protocol git ops.
var devEnvWritableDirKeys = []string{"MOE_HOME", "MOE_DEV_TMPDIR"}

// devEnvWritableDirs filters env down to the values of
// devEnvWritableDirKeys that look like absolute local directories,
// cleaned and deduplicated while preserving the key declaration order.
// Empty values, relative paths, and missing keys are dropped silently
// — the dev-env hook owns directory creation, so a missing entry is
// the project's "no isolated runtime needed" signal rather than an
// error. The returned slice is nil when no key contributed a path.
func devEnvWritableDirs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, k := range devEnvWritableDirKeys {
		v := env[k]
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) {
			continue
		}
		clean := filepath.Clean(v)
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// mapToEnv returns m flattened into the "KEY=VALUE" form os.Environ
// produces — caller appends it to a cmd.Env slice. Keys aren't sorted
// because cmd.Env order doesn't matter to exec; if a key appears
// twice (base env had it, accumulated has it), the latter wins per
// stdlib semantics.
func mapToEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// parseDevEnvLines turns a stream of "KEY=VALUE" lines into a map.
// Blank lines and `#` comments are ignored. Lines that don't match
// the shape (no `=`, or an empty key) are skipped with a warning on
// warnDst (when non-nil) — same forgiving-but-loud parse contract
// pre-push uses for its trailers. Returns the merged map.
func parseDevEnvLines(r io.Reader, warnDst io.Writer) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(r)
	// Larger buffer than bufio's default 64k — a one-line PATH-ish
	// var with composed PATHS can blow past 64k on a heavy workspace.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			if warnDst != nil {
				moePrintf(warnDst, "dev-env: ignoring malformed line %d: %q\n", lineNum, line)
			}
			continue
		}
		// Preserve the value verbatim (no trim) so leading / trailing
		// whitespace inside a VALUE isn't silently stripped — a
		// project that wants whitespace can have it.
		out[k] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
