//go:build linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/run"
)

func TestCloneGCRejectsSameDeviceBindMount(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--mount", "--fork", exe, "-test.run=^TestCloneGCBindMountHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "MOE_TEST_CLONE_BIND=1")
	out, err := cmd.CombinedOutput()
	if err != nil && (strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "operation not permitted")) {
		t.Skipf("isolated bind fixture unavailable: %s", out)
	}
	if err != nil {
		t.Fatalf("bind fixture: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "same-device bind held before apply") {
		t.Fatalf("helper did not exercise the mount: %s", out)
	}
	t.Log(strings.TrimSpace(string(out)))
}

func TestCloneGCBindMountHelper(t *testing.T) {
	if os.Getenv("MOE_TEST_CLONE_BIND") != "1" {
		return
	}
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	_, path := gcFixture(t, run.StatusClosed, time.Now().UTC().Add(-10*24*time.Hour))
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "sentinel"), "survives")
	target := filepath.Join(path, "mounted")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount(outside, target, "", syscall.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Unmount(target, 0); err != nil {
			t.Error(err)
		}
	}()
	var a, b syscall.Stat_t
	if err := syscall.Stat(outside, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(target, &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev != b.Dev {
		t.Fatal("fixture is not a same-device bind")
	}
	report, code := gcReport(t, "--apply", "tele/finish")
	if code != 0 || report.Candidates[0].Reason != "unsafe_tree" || report.Candidates[0].Outcome != "held" {
		t.Fatalf("mount was not held: %d %+v", code, report)
	}
	if body, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(body) != "survives" {
		t.Fatal("outside mount contents changed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("bind clone disappeared")
	}
	t.Log("same-device bind held before apply")
}
