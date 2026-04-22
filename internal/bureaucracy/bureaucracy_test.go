package bureaucracy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func writeMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, Marker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindWalksUpToMarker(t *testing.T) {
	root := t.TempDir()
	writeMarker(t, root)
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Find(nested, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	wantAbs, _ := filepath.Abs(root)
	gotAbs, _ := filepath.Abs(got)
	if gotAbs != wantAbs {
		t.Fatalf("got %q want %q", gotAbs, wantAbs)
	}
}

func TestFindReturnsNotFoundAtFilesystemRoot(t *testing.T) {
	dir := t.TempDir() // no marker anywhere up the chain to /
	_, err := Find(dir, noEnv)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestFindPrefersMoeHome(t *testing.T) {
	pwdRoot := t.TempDir()
	writeMarker(t, pwdRoot) // $PWD walk would find this
	homeRoot := t.TempDir()
	writeMarker(t, homeRoot)

	got, err := Find(pwdRoot, func(k string) string {
		if k == EnvHome {
			return homeRoot
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAbs, _ := filepath.Abs(homeRoot)
	if got != wantAbs {
		t.Fatalf("got %q want %q", got, wantAbs)
	}
}

func TestFindErrorsWhenMoeHomeLacksMarker(t *testing.T) {
	empty := t.TempDir()
	_, err := Find(t.TempDir(), func(k string) string {
		if k == EnvHome {
			return empty
		}
		return ""
	})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("expected descriptive error, got %v", err)
	}
}

func TestInitScaffoldsAndCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\temail = t@example.com\n\tname = T\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	if err := Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{Marker, "projects/.gitkeep", ".git"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	// requests/ is gone — projects/ is the sole top-level state dir.
	if _, err := os.Stat(filepath.Join(dir, "requests")); !os.IsNotExist(err) {
		t.Errorf("requests/ should not exist after init (err=%v)", err)
	}
	got, err := Find(dir, noEnv)
	if err != nil {
		t.Fatalf("Find after Init: %v", err)
	}
	if absGot, _ := filepath.Abs(got); absGot != mustAbs(t, dir) {
		t.Errorf("Find=%q want %q", absGot, dir)
	}
	// Init stages but does not commit — HEAD shouldn't resolve on a fresh repo.
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Errorf("expected HEAD to be unborn after init, but it resolved")
	}
	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("diff --cached: %v\n%s", err, out)
	}
	staged := strings.TrimSpace(string(out))
	for _, want := range []string{"bureaucracy.conf", "projects/.gitkeep"} {
		if !strings.Contains(staged, want) {
			t.Errorf("staged set missing %s:\n%s", want, staged)
		}
	}
	if strings.Contains(staged, "requests/.gitkeep") {
		t.Errorf("staged set should not include requests/.gitkeep:\n%s", staged)
	}
}

func TestInitWritesMoeGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\temail = t@example.com\n\tname = T\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	if err := Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(body), ".moe/") {
		t.Errorf(".gitignore missing .moe/: %q", body)
	}
}

func TestInitRefusesExistingMarker(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, dir)
	if err := Init(dir, ""); err == nil {
		t.Fatal("expected error when marker already present")
	}
}

func TestInitSetsRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\temail = t@example.com\n\tname = T\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	dir := t.TempDir()
	if err := Init(dir, "git@example.com:me/b.git"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remote get-url: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "git@example.com:me/b.git" {
		t.Errorf("origin=%q", out)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
