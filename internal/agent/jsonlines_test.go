package agent

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestJSONLinesCancellationWithConcurrentStderr(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `printf 'ready\n'; while :; do echo diagnostic >&2; done &
wait`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := StartJSONLines(ctx, shell, []string{"-c", script}, os.Environ(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := p.Next(); err == nil {
		t.Fatal("expected cancellation")
	}
	if ctx.Err() == nil {
		t.Fatal("read ended before its deadline")
	}
}
