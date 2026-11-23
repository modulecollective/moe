package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modulecollective/moe/internal/git/gittest"
)

func TestInspectBoundsAndIsolation(t *testing.T) {
	dir := gittest.InitSeeded(t)
	other := gittest.InitSeeded(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := Inspect(ctx, dir, 4096, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(out) != dir {
		t.Fatalf("wrong repository: %q %v", out, err)
	}
	out, err = Inspect(ctx, dir, 1, "log", "--format=%H")
	if err == nil || out != "" || !strings.Contains(err.Error(), "output limit exceeded") {
		t.Fatalf("truncated result accepted: %q %v", out, err)
	}
	if _, err := Inspect(context.Background(), dir, 4096, "status"); err == nil {
		t.Fatal("unbounded context accepted")
	}
	cancellable, stop := context.WithCancel(context.Background())
	defer stop()
	if _, err := Inspect(cancellable, dir, 4096, "status"); err == nil {
		t.Fatal("context without a deadline accepted")
	}
}

func TestInspectStdoutAndDeadline(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'inventory\\n'\nprintf 'diagnostic\\n' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := Inspect(ctx, dir, 4096, "example")
	if err != nil || out != "inventory\n" {
		t.Fatalf("stderr contaminated inventory: %q %v", out, err)
	}
	// The helper inherits both captured pipes. Cancellation must kill the
	// process group and return rather than waiting for the helper's sleep.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if out, err := Inspect(short, dir, 4096, "example"); out != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %q %v", out, err)
	}
}
