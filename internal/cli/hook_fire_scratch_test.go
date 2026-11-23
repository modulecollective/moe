package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git"
	"github.com/modulecollective/moe/internal/git/gittest"
	"github.com/modulecollective/moe/internal/repolock"
	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
	"github.com/modulecollective/moe/internal/sandbox"
)

func hookFireFixture(t *testing.T) string {
	t.Helper()
	gittest.SetupEnv(t)
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MOE_HOME", root)
	fireWrite(t, filepath.Join(root, "bureaucracy.conf"), "")
	fireWrite(t, filepath.Join(root, ".gitignore"), ".moe/\nprojects/tele/src/\n")
	fireWrite(t, filepath.Join(root, "projects/tele/project.json"), `{"id":"tele","status":"incubating","submodule":"projects/tele/src","remote":"projects/tele/src","default_branch":"main"}`)
	src := filepath.Join(root, "projects/tele/src")
	fireWrite(t, filepath.Join(src, "source"), "local source\n")
	for _, item := range []struct {
		dir  string
		args []string
	}{
		{src, []string{"init", "-b", "main"}}, {src, []string{"add", "."}}, {src, []string{"commit", "-m", "source"}},
		{root, []string{"init", "-b", "main"}}, {root, []string{"add", "."}}, {root, []string{"commit", "-m", "bureaucracy"}},
	} {
		if err := git.Run(item.dir, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fireWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fireCLI(event string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run([]string{"hook", "fire", "tele", event}, &out, &errb)
	return code, out.String(), errb.String()
}

func fireSandbox(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if path, ok := strings.CutPrefix(line, "sandbox: "); ok {
			return path
		}
	}
	t.Fatalf("missing sandbox path: %s", out)
	return ""
}

func fireSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[strings.TrimPrefix(path, root)] = string(body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHookFirePreservesRunClones(t *testing.T) {
	for _, id := range []string{"hook-fire-live", "hook-fire-1700000000"} {
		for _, owned := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/owned=%t", id, owned), func(t *testing.T) {
				root := hookFireFixture(t)
				var md *run.Metadata
				if err := repolock.With(root, repolock.Options{Purpose: "create collision run"}, func() error {
					var err error
					md, err = run.New(root, "tele", run.Options{ID: id, Workflow: "sdlc"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				clone, err := sandbox.Ensure(root, "tele", md.ID)
				if err != nil {
					t.Fatal(err)
				}
				fireWrite(t, filepath.Join(clone, "uncommitted-sentinel"), "keep my work")
				before := fireSnapshot(t, clone)
				metadata := fireSnapshot(t, filepath.Join(root, run.Dir("tele", id)))
				if owned {
					guard, err := runowner.Acquire(root, "tele", id)
					if err != nil {
						t.Fatal(err)
					}
					defer guard.Release()
				}
				writeHookScript(t, root, "tele", "dev-env", "10-safe.sh", "#!/bin/sh\necho COLLISION=survived\n")
				code, out, errb := fireCLI("dev-env")
				if code != 0 {
					t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errb)
				}
				if got, err := os.ReadFile(filepath.Join(clone, "uncommitted-sentinel")); err != nil || string(got) != "keep my work" {
					t.Fatalf("run sentinel deleted or changed: %q, %v", got, err)
				}
				if got := fireSnapshot(t, clone); !equalFireSnapshots(before, got) {
					t.Fatal("run clone bytes changed")
				}
				if got := fireSnapshot(t, filepath.Join(root, run.Dir("tele", id))); !equalFireSnapshots(metadata, got) {
					t.Fatal("run metadata changed")
				}
				if owned {
					other, err := runowner.Acquire(root, "tele", id)
					if other != nil {
						other.Release()
					}
					if !runowner.IsBusy(err) {
						t.Fatalf("owner no longer exclusive: %v", err)
					}
				}
			})
		}
	}
}

func equalFireSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

func TestHookFireCompletedLifecycle(t *testing.T) {
	root := hookFireFixture(t)
	writeHookScript(t, root, "tele", "dev-env", "10-env.sh", `#!/bin/sh
printf '%s|%s|%s' "$MOE_RUN" "$MOE_WORKFLOW" "$MOE_SANDBOX" > "$MOE_SANDBOX/receipt"
echo Z=last
echo A=first
`)
	head, err := git.HEAD(root)
	if err != nil {
		t.Fatal(err)
	}
	var previous string
	for range 2 {
		code, out, errb := fireCLI("dev-env")
		if code != 0 {
			t.Fatalf("exit=%d out=%s err=%s", code, out, errb)
		}
		clone := fireSandbox(t, out)
		wrapper := filepath.Dir(clone)
		id := filepath.Base(wrapper)
		if filepath.Dir(wrapper) != filepath.Join(root, ".moe/hook-fire/tele") || filepath.Base(clone) != "sandbox" {
			t.Fatalf("wrong path: %s", clone)
		}
		if !strings.Contains(out, "A=first\nZ=last\n") {
			t.Fatalf("env not sorted: %s", out)
		}
		if previous != "" {
			if wrapper == previous {
				t.Fatal("reused wrapper")
			}
			if _, err := os.Stat(previous); !os.IsNotExist(err) {
				t.Fatalf("prior completion retained: %v", err)
			}
		}
		body, err := os.ReadFile(filepath.Join(clone, "receipt"))
		if err != nil || string(body) != id+"|hook-fire|"+clone {
			t.Fatalf("hook context=%q err=%v", body, err)
		}
		branch, err := git.Output(clone, "branch", "--show-current")
		if err != nil || strings.TrimSpace(branch) != "moe/"+id {
			t.Fatalf("branch=%q err=%v", branch, err)
		}
		body, err = os.ReadFile(filepath.Join(wrapper, hookFireCompletion))
		if err != nil {
			t.Fatal(err)
		}
		record, err := decodeHookFireRecord(body)
		if err != nil || record != (hookFireRecord{1, "tele", id}) {
			t.Fatalf("completion=%+v err=%v", record, err)
		}
		previous = wrapper
	}
	if after, err := git.HEAD(root); err != nil || head != after {
		t.Fatalf("fire journaled: %s -> %s (%v)", head, after, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "projects/tele/runs")); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
		t.Fatalf("fire registered runs: %v %v", entries, err)
	}
}

func TestHookFireFailuresRetainUnmarkedScratch(t *testing.T) {
	for _, failure := range []string{"clone", "checkout", "setup", "dispatch", "teardown", "publication"} {
		t.Run(failure, func(t *testing.T) {
			root := hookFireFixture(t)
			script := "#!/bin/sh\necho READY=yes\n"
			switch failure {
			case "clone":
				// A materialized source with an unborn HEAD fails clone checkout.
				src := filepath.Join(root, "projects/tele/src")
				if err := git.Run(src, "checkout", "--orphan", "unborn"); err != nil {
					t.Fatal(err)
				}
			case "checkout":
				// Git's branch hook is installed by the source-independent template.
				template := t.TempDir()
				fireWrite(t, filepath.Join(template, "info/exclude"), "")
				fireWrite(t, filepath.Join(template, "hooks/post-checkout"), fmt.Sprintf("#!/bin/sh\nif [ -f %q ]; then exit 8; fi\ntouch %q\n", filepath.Join(template, "checked"), filepath.Join(template, "checked")))
				if err := os.Chmod(filepath.Join(template, "hooks/post-checkout"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GIT_TEMPLATE_DIR", template)
			case "setup":
				script = "#!/bin/sh\necho setup-failed >&2\nexit 7\n"
			case "dispatch":
				writeHookScript(t, root, "tele", "pre-push", "10-fail.sh", "#!/bin/sh\necho dispatch-failed >&2\nexit 7\n")
			case "teardown":
				writeHookScript(t, root, "tele", "dev-env-teardown", "10-fail.sh", "#!/bin/sh\necho teardown-failed >&2\nexit 7\n")
			case "publication":
				script = "#!/bin/sh\nmkdir \"$MOE_SANDBOX/../completed.json\"\n"
			}
			writeHookScript(t, root, "tele", "dev-env", "10-env.sh", script)
			code, out, errb := fireCLI("pre-push")
			if code != 1 {
				t.Fatalf("exit=%d out=%s err=%s", code, out, errb)
			}
			if failure == "checkout" && !strings.Contains(errb, "sandbox: checkout -b") {
				t.Fatalf("wrong failure: %s", errb)
			}
			if failure == "clone" && !strings.Contains(errb, "git checkout HEAD") {
				t.Fatalf("wrong failure: %s", errb)
			}
			clone := fireSandbox(t, out)
			wrapper := filepath.Dir(clone)
			if _, err := os.Stat(wrapper); err != nil {
				t.Fatalf("failure tree lost: %v", err)
			}
			if info, err := os.Stat(filepath.Join(wrapper, hookFireCompletion)); err == nil && info.Mode().IsRegular() {
				t.Fatal("failure marked completed")
			}
			if failure == "publication" && !strings.Contains(errb, "publish completion") {
				t.Fatalf("publication diagnostic missing: %s", errb)
			}
			if errb == "" {
				t.Fatal("failure has no diagnostic")
			}
			if err := cleanPriorHookFireSandboxes(root, "tele", io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wrapper); err != nil {
				t.Fatalf("cleanup removed failure: %v", err)
			}
			if failure == "clone" {
				if err := git.Run(filepath.Join(root, "projects/tele/src"), "checkout", "main"); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "checkout" {
				if err := os.Remove(filepath.Join(os.Getenv("GIT_TEMPLATE_DIR"), "hooks/post-checkout")); err != nil {
					t.Fatal(err)
				}
			}
			for _, event := range hookFireEvents {
				writeHookScript(t, root, "tele", event, "10-env.sh", "#!/bin/sh\nexit 0\n")
			}
			for _, event := range []string{"pre-push", "dev-env-teardown"} {
				writeHookScript(t, root, "tele", event, "10-fail.sh", "#!/bin/sh\nexit 0\n")
			}
			code, out, errb = fireCLI("pre-push")
			if code != 0 {
				t.Fatalf("retry failed: exit=%d out=%s err=%s", code, out, errb)
			}
			if filepath.Dir(fireSandbox(t, out)) == wrapper {
				t.Fatal("retry reused failed scratch")
			}
			if _, err := os.Stat(wrapper); err != nil {
				t.Fatalf("retry removed failure evidence: %v", err)
			}
		})
	}
}

func plantFireRecord(t *testing.T, root, name string) string {
	t.Helper()
	wrapper := filepath.Join(root, ".moe/hook-fire/tele", name)
	if err := os.MkdirAll(filepath.Join(wrapper, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(hookFireRecord{1, "tele", name})
	if err != nil {
		t.Fatal(err)
	}
	fireWrite(t, filepath.Join(wrapper, hookFireCompletion), string(body))
	return wrapper
}

func TestCleanPriorHookFireSandboxesOwnership(t *testing.T) {
	root := t.TempDir()
	// Even valid records cannot authorize touching the old clone namespace.
	legacy := filepath.Join(root, ".moe/clones/tele/hook-fire-1700000000")
	fireWrite(t, filepath.Join(legacy, "sandbox/sentinel"), "legacy")
	fireWrite(t, filepath.Join(legacy, hookFireCompletion), `{"version":1,"project":"tele","wrapper":"hook-fire-1700000000"}`)
	legacyRoot := filepath.Dir(legacy)
	fireWrite(t, filepath.Join(legacyRoot, "hook-fire-unregistered/sentinel"), "unmarked legacy")
	old := fireSnapshot(t, legacyRoot)
	records := map[string]string{
		"missing": "", "malformed": "{", "null": "null", "version": `{"version":2,"project":"tele","wrapper":"hook-fire-version"}`,
		"project":      `{"version":1,"project":"other","wrapper":"hook-fire-project"}`,
		"wrapper":      `{"version":1,"project":"tele","wrapper":"hook-fire-other"}`,
		"unknown":      `{"version":1,"project":"tele","wrapper":"hook-fire-unknown","path":"sandbox"}`,
		"duplicate":    `{"version":1,"version":1,"project":"tele","wrapper":"hook-fire-duplicate"}`,
		"trailing":     `{"version":1,"project":"tele","wrapper":"hook-fire-trailing"} {}`,
		"absent-field": `{"version":1,"project":"tele"}`,
	}
	before := make(map[string]map[string]string)
	for name, body := range records {
		wrapper := plantFireRecord(t, root, "hook-fire-"+name)
		if name == "missing" {
			if err := os.Remove(filepath.Join(wrapper, hookFireCompletion)); err != nil {
				t.Fatal(err)
			}
		} else {
			fireWrite(t, filepath.Join(wrapper, hookFireCompletion), body)
		}
	}
	for name := range records {
		before[name] = fireSnapshot(t, filepath.Join(root, ".moe/hook-fire/tele", "hook-fire-"+name))
	}
	good := plantFireRecord(t, root, "hook-fire-completed")
	unrelated := plantFireRecord(t, root, "unrelated")
	var errb bytes.Buffer
	if err := cleanPriorHookFireSandboxes(root, "tele", &errb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatalf("valid completion not removed: %v", err)
	}
	for name := range records {
		path := filepath.Join(root, ".moe/hook-fire/tele", "hook-fire-"+name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("ambiguous entry lost: %s: %v", path, err)
		}
		if !equalFireSnapshots(before[name], fireSnapshot(t, path)) {
			t.Errorf("ambiguous entry changed: %s", path)
		}
		if !strings.Contains(errb.String(), path) {
			t.Errorf("no retention diagnostic for %s", path)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal(err)
	}
	if !equalFireSnapshots(old, fireSnapshot(t, legacyRoot)) {
		t.Fatal("legacy bytes changed")
	}
}

func TestCleanPriorHookFireSandboxesSymlinks(t *testing.T) {
	for _, part := range []string{"namespace", "project", "wrapper", "record", "sandbox", "interior"} {
		t.Run(part, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, ".moe/clones/tele/hook-fire-live")
			fireWrite(t, filepath.Join(target, "sentinel"), "keep")
			// A valid-looking external wrapper must not be adopted through a link.
			fireWrite(t, filepath.Join(target, hookFireCompletion), `{"version":1,"project":"tele","wrapper":"hook-fire-links"}`)
			if err := os.MkdirAll(filepath.Join(target, "sandbox"), 0o755); err != nil {
				t.Fatal(err)
			}
			before := fireSnapshot(t, target)
			wrapper := plantFireRecord(t, root, "hook-fire-links")
			path := map[string]string{"namespace": filepath.Join(root, ".moe/hook-fire"), "project": filepath.Join(root, ".moe/hook-fire/tele"), "wrapper": wrapper, "record": filepath.Join(wrapper, hookFireCompletion), "sandbox": filepath.Join(wrapper, "sandbox"), "interior": filepath.Join(wrapper, "sandbox/link")}[part]
			if part != "interior" {
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
			linkTarget := target
			if part == "record" {
				linkTarget = filepath.Join(target, hookFireCompletion)
			}
			if err := os.Symlink(linkTarget, path); err != nil {
				t.Fatal(err)
			}
			var errb bytes.Buffer
			err := cleanPriorHookFireSandboxes(root, "tele", &errb)
			if part == "namespace" || part == "project" {
				if err == nil {
					t.Fatal("unsafe parent accepted")
				}
				if _, _, err := mintHookFireSandbox(root, "tele"); err == nil {
					t.Fatal("mint followed unsafe parent")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if part == "interior" {
					if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
						t.Fatalf("completed wrapper retained: %v", err)
					}
				} else {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("symlink removed: %v", err)
					}
					if !strings.Contains(errb.String(), wrapper) {
						t.Fatalf("missing warning: %s", errb.String())
					}
				}
			}
			if !equalFireSnapshots(before, fireSnapshot(t, target)) {
				t.Fatal("external target changed")
			}
		})
	}
}

func TestCleanPriorHookFireSandboxesMissingDir(t *testing.T) {
	if err := cleanPriorHookFireSandboxes(t.TempDir(), "tele", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCleanPriorHookFireSandboxesConcurrent(t *testing.T) {
	root := t.TempDir()
	wrapper := plantFireRecord(t, root, "hook-fire-completed")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- cleanPriorHookFireSandboxes(root, "tele", io.Discard) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
		t.Fatalf("wrapper remains: %v", err)
	}
}

func TestHookFireConcurrentActiveScratch(t *testing.T) {
	root := hookFireFixture(t)
	// File handshakes, rather than elapsed time, delimit the first hook's use.
	entered := filepath.Join(root, "entered")
	release := filepath.Join(root, "release")
	writeHookScript(t, root, "tele", "dev-env", "10-wait.sh", fmt.Sprintf(`#!/bin/sh
if mkdir %q 2>/dev/null; then
 printf '%%s' "$MOE_SANDBOX" > %q
 while [ ! -f %q ]; do sleep 0.01; done
 test -f "$MOE_SANDBOX/source" || exit 9
fi
echo HANDSHAKE=done
`, filepath.Join(root, "first"), entered, release))
	type result struct {
		code     int
		out, err string
	}
	done := make(chan result, 1)
	go func() { code, out, err := fireCLI("dev-env"); done <- result{code, out, err} }()
	// Always release and join the hook before fixture cleanup, even on failure.
	joined := false
	defer func() {
		fireWrite(t, release, "")
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("hook did not stop")
			}
		}
	}()
	var first string
	deadline := time.After(10 * time.Second)
wait:
	for {
		select {
		case result := <-done:
			joined = true
			t.Fatalf("first exited before handshake: %+v", result)
		case <-deadline:
			t.Fatal("hook did not reach handshake")
		default:
			if body, err := os.ReadFile(entered); err == nil && len(body) > 0 {
				first = string(body)
				break wait
			}
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(first), hookFireCompletion)); !os.IsNotExist(err) {
		t.Fatalf("active hook marked complete: %v", err)
	}
	code, out, errb := fireCLI("dev-env")
	if code != 0 {
		t.Fatalf("second exit=%d out=%s err=%s", code, out, errb)
	}
	second := fireSandbox(t, out)
	if first == second {
		t.Fatal("concurrent fires shared clone")
	}
	if _, err := os.Stat(filepath.Join(first, "source")); err != nil {
		t.Fatalf("active clone removed: %v", err)
	}
	if !strings.Contains(errb, filepath.Dir(first)) {
		t.Fatalf("no active retention diagnostic: %s", errb)
	}
	fireWrite(t, release, "")
	select {
	case result := <-done:
		joined = true
		if result.code != 0 {
			t.Fatalf("first failed: %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first hook did not join")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(first), hookFireCompletion)); err != nil {
		t.Fatal(err)
	}
}

func TestHookFireSimultaneousAllocation(t *testing.T) {
	root := hookFireFixture(t)
	type result struct {
		path string
		err  error
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for range 2 {
		go func() { <-start; path, _, err := mintHookFireSandbox(root, "tele"); done <- result{path, err} }()
	}
	close(start)
	first, second := <-done, <-done
	if first.err != nil || second.err != nil {
		t.Fatalf("allocation errors: %v, %v", first.err, second.err)
	}
	if first.path == second.path {
		t.Fatalf("allocations collided: %s", first.path)
	}
}

func TestHookFireScratchPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires an unprivileged user")
	}
	t.Run("unreadable-parent-refuses-before-mint", func(t *testing.T) {
		root := hookFireFixture(t)
		wrapper := plantFireRecord(t, root, "hook-fire-completed")
		parent := filepath.Dir(wrapper)
		if err := os.Chmod(parent, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(parent, 0o755)
		code, out, errb := fireCLI("dev-env")
		if code != 1 || strings.Contains(out, "sandbox:") || !strings.Contains(errb, "permission denied") {
			t.Fatalf("exit=%d out=%s err=%s", code, out, errb)
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil || len(entries) != 1 {
			t.Fatalf("allocation after unsafe read: %v %v", entries, err)
		}
	})
	t.Run("unreadable-record-retained", func(t *testing.T) {
		root := t.TempDir()
		wrapper := plantFireRecord(t, root, "hook-fire-unreadable")
		marker := filepath.Join(wrapper, hookFireCompletion)
		if err := os.Chmod(marker, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(marker, 0o600)
		var errb bytes.Buffer
		if err := cleanPriorHookFireSandboxes(root, "tele", &errb); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(wrapper); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errb.String(), "permission denied") {
			t.Fatalf("missing warning: %s", errb.String())
		}
	})
	t.Run("unremovable-entry-does-not-block-fire", func(t *testing.T) {
		root := hookFireFixture(t)
		wrapper := plantFireRecord(t, root, "hook-fire-stuck")
		protected := filepath.Join(wrapper, "sandbox")
		fireWrite(t, filepath.Join(protected, "keep"), "cannot unlink")
		if err := os.Chmod(protected, 0o555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(protected, 0o755)
		code, out, errb := fireCLI("dev-env")
		if code != 0 {
			t.Fatalf("exit=%d out=%s err=%s", code, out, errb)
		}
		if _, err := os.Stat(filepath.Join(protected, "keep")); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errb, wrapper) || !strings.Contains(errb, "permission denied") {
			t.Fatalf("missing removal warning: %s", errb)
		}
		if filepath.Dir(fireSandbox(t, out)) == wrapper {
			t.Fatal("reused stuck wrapper")
		}
	})
}

func TestCleanPriorHookFireSandboxesUnexpectedShapes(t *testing.T) {
	for _, shape := range []string{"wrapper-file", "record-dir", "sandbox-file", "extra-file"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			wrapper := plantFireRecord(t, root, "hook-fire-shape")
			switch shape {
			case "wrapper-file":
				if err := os.RemoveAll(wrapper); err != nil {
					t.Fatal(err)
				}
				fireWrite(t, wrapper, "keep")
			case "record-dir":
				path := filepath.Join(wrapper, hookFireCompletion)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "sandbox-file":
				path := filepath.Join(wrapper, "sandbox")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				fireWrite(t, path, "keep")
			case "extra-file":
				fireWrite(t, filepath.Join(wrapper, "notes"), "operator notes")
			}
			var errb bytes.Buffer
			if err := cleanPriorHookFireSandboxes(root, "tele", &errb); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(wrapper); err != nil {
				t.Fatalf("unexpected shape removed: %v", err)
			}
			if !strings.Contains(errb.String(), wrapper) {
				t.Fatalf("missing warning: %s", errb.String())
			}
		})
	}
}

// Run the same CLI dispatcher in a separate process so a real signal can
// interrupt hook execution without terminating the test runner.
func TestHookFireSubprocessHelper(t *testing.T) {
	if os.Getenv("MOE_HOOK_FIRE_HELPER") != "1" {
		return
	}
	os.Exit(Run([]string{"hook", "fire", "tele", "dev-env"}, os.Stdout, os.Stderr))
}

func TestHookFireInterruptedScratch(t *testing.T) {
	root := hookFireFixture(t)
	entered := filepath.Join(root, "entered")
	writeHookScript(t, root, "tele", "dev-env", "10-wait.sh", fmt.Sprintf(`#!/bin/sh
printf '%%s' "$MOE_SANDBOX" > %q
while :; do sleep 1; done
`, entered))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestHookFireSubprocessHelper$")
	cmd.Env = append(os.Environ(), "MOE_HOOK_FIRE_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if !waited {
			_ = cmd.Wait()
		}
	}()
	var clone string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(entered); err == nil && len(body) > 0 {
			clone = string(body)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if clone == "" {
		t.Fatal("child hook did not reach handshake")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil {
		t.Fatal("interrupted CLI succeeded")
	}
	wrapper := filepath.Dir(clone)
	if _, err := os.Stat(filepath.Join(wrapper, hookFireCompletion)); !os.IsNotExist(err) {
		t.Fatalf("interrupted fire marked complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, "source")); err != nil {
		t.Fatal(err)
	}
	if err := cleanPriorHookFireSandboxes(root, "tele", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Fatalf("interrupted clone removed: %v", err)
	}
}

func TestHookFireUnknownEventDoesNotClean(t *testing.T) {
	root := hookFireFixture(t)
	wrapper := plantFireRecord(t, root, "hook-fire-completed")
	legacy := filepath.Join(root, ".moe/clones/tele/hook-fire-legacy/sentinel")
	fireWrite(t, legacy, "keep")
	code, out, errb := fireCLI("bogus")
	if code != 2 || strings.Contains(out, "sandbox:") || !strings.Contains(errb, "unknown event") {
		t.Fatalf("exit=%d out=%s err=%s", code, out, errb)
	}
	if _, err := os.Stat(wrapper); err != nil {
		t.Fatalf("typo cleaned completed scratch: %v", err)
	}
	if body, err := os.ReadFile(legacy); err != nil || string(body) != "keep" {
		t.Fatalf("typo touched legacy clone: %q %v", body, err)
	}
	entries, err := os.ReadDir(filepath.Dir(wrapper))
	if err != nil || len(entries) != 1 {
		t.Fatalf("typo allocated scratch: %v %v", entries, err)
	}
}
