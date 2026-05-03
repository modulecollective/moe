package project

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTime is injected into Register so Created is deterministic.
// 09:00 UTC keeps the calendar day stable across CI runner zones —
// Register formats Created with .Local(), and midnight UTC would flip
// to the prior day in any westward zone.
func fixedTime() time.Time { return time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC) }

// makeRemote builds a tiny bare git repo with one commit on `main` and
// returns its filesystem path, usable as a URL for ls-remote and submodule
// add. Using a local bare repo keeps tests hermetic — no network.
func makeRemote(t *testing.T) string {
	t.Helper()
	isolateGitConfig(t)
	work := t.TempDir()
	run(t, work, "git", "init", "-b", "main")
	run(t, work, "git", "config", "user.email", "test@example.com")
	run(t, work, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "init")

	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "git", "clone", "--bare", work, bare)
	return bare
}

func makeBureaucracy(t *testing.T) string {
	t.Helper()
	// Submodule add spawns a child `git clone`, which only sees this via the
	// process's global config — local repo config isn't inherited. Point
	// GIT_CONFIG_GLOBAL at a tempfile that allows file:// clones and also
	// disables signing/hooks the host may have enabled.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[protocol \"file\"]\n\tallow = always\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	root := t.TempDir()
	run(t, root, "git", "init", "-b", "main")
	run(t, root, "git", "config", "user.email", "test@example.com")
	run(t, root, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "bureaucracy.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "add", ".")
	run(t, root, "git", "commit", "-m", "init")
	return root
}

// isolateGitConfig points git at a scratch global/system config for the
// duration of the test so host settings (commit signing, templates, hooks)
// don't bleed into fixture commits.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	body := "[user]\n\temail = test@example.com\n\tname = Test\n" +
		"[init]\n\tdefaultBranch = main\n" +
		"[commit]\n\tgpgsign = false\n" +
		"[tag]\n\tgpgsign = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func TestRegisterHappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := makeRemote(t)
	root := makeBureaucracy(t)

	md, err := Register(root, remote, Options{Now: fixedTime})
	if err != nil {
		t.Fatal(err)
	}
	if md.ID != "remote" {
		t.Errorf("id=%q want remote (derived from .git URL)", md.ID)
	}
	if md.DefaultBranch != "main" {
		t.Errorf("default_branch=%q want main", md.DefaultBranch)
	}
	if md.Created != "2026-04-12" {
		t.Errorf("created=%q", md.Created)
	}
	if md.Status != "incubating" {
		t.Errorf("status=%q", md.Status)
	}
	if md.Submodule != "projects/remote/src" {
		t.Errorf("submodule=%q want projects/remote/src", md.Submodule)
	}

	// project.json on disk matches.
	b, err := os.ReadFile(filepath.Join(root, "projects/remote/project.json"))
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Metadata
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.ID != "remote" || roundTrip.Submodule != "projects/remote/src" {
		t.Errorf("bad round-trip: %+v", roundTrip)
	}

	// Submodule checkout exists at projects/remote/src, not directly at
	// projects/remote — that's what leaves room for project.json and
	// runs/ to be bureaucracy-tracked alongside the submodule.
	if _, err := os.Stat(filepath.Join(root, "projects/remote/src/README")); err != nil {
		t.Errorf("submodule not checked out at projects/remote/src: %v", err)
	}

	// Commit landed.
	log := gitOutput(t, root, "log", "--format=%s")
	if !strings.Contains(log, "Register project remote") {
		t.Errorf("commit missing from log: %q", log)
	}
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := makeRemote(t)
	root := makeBureaucracy(t)

	if _, err := Register(root, remote, Options{Now: fixedTime}); err != nil {
		t.Fatal(err)
	}
	_, err := Register(root, remote, Options{Now: fixedTime})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestUnregisterRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := makeRemote(t)
	root := makeBureaucracy(t)

	md, err := Register(root, remote, Options{Now: fixedTime})
	if err != nil {
		t.Fatal(err)
	}
	if err := Unregister(root, md.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"projects/" + md.ID,
		"projects/" + md.ID + "/src",
		"projects/" + md.ID + "/project.json",
		".git/modules/projects/" + md.ID + "/src",
	} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
	// .gitmodules no longer references the submodule.
	if b, err := os.ReadFile(filepath.Join(root, ".gitmodules")); err == nil {
		if strings.Contains(string(b), md.ID) {
			t.Errorf(".gitmodules still mentions %s: %s", md.ID, b)
		}
	}
	log := gitOutput(t, root, "log", "--format=%s")
	if !strings.Contains(log, "Unregister project "+md.ID) {
		t.Errorf("unregister commit missing: %q", log)
	}
	// Re-registering should succeed cleanly — tests that removal was complete.
	if _, err := Register(root, remote, Options{Now: fixedTime}); err != nil {
		t.Fatalf("re-register after unregister: %v", err)
	}
}

func TestUnregisterRefusesUnknown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := makeBureaucracy(t)
	err := Unregister(root, "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want not-registered error, got %v", err)
	}
}

func TestUnregisterRefusesExtraContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := makeRemote(t)
	root := makeBureaucracy(t)
	md, err := Register(root, remote, Options{Now: fixedTime})
	if err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(root, "projects", md.ID, "runs", "some-run")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err = Unregister(root, md.ID)
	if err == nil || !strings.Contains(err.Error(), "remove them manually") {
		t.Fatalf("want safety error, got %v", err)
	}
}

func TestRegisterRejectsBadDerivedID(t *testing.T) {
	_, err := Register(t.TempDir(), "https://example.com/Has Spaces.git", Options{})
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("want id-pattern error, got %v", err)
	}
}

func TestDeriveID(t *testing.T) {
	cases := map[string]string{
		"https://github.com/foo/bar.git":    "bar",
		"https://github.com/foo/bar":        "bar",
		"git@github.com:foo/Baz.git":        "baz",
		"/tmp/some/path/remote.git":         "remote",
		"https://example.com/x/y/repo.git/": "repo",
	}
	for in, want := range cases {
		got, err := deriveID(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("deriveID(%q)=%q want %q", in, got, want)
		}
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
