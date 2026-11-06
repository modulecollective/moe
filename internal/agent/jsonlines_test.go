package agent

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestJSONLinesCancellationWithConcurrentStderr(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `printf 'ready\n'; while :; do echo diagnostic >&2; done &
wait`
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p, err := StartJSONLines(ctx, shell, []string{"-c", script}, os.Environ(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Next(); err == nil {
		t.Fatal("expected cancellation")
	}
	p.Close()
	if ctx.Err() == nil {
		t.Fatal("read ended before its deadline")
	}
}
