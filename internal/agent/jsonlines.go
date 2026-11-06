package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/modulecollective/moe/internal/procgroup"
)

// JSONLines is a short-lived child spoken to in newline-delimited JSON
// over stdio — the shape both backends' non-inference control
// interfaces use (claude's stream-json control requests, codex's
// app-server). The child runs in its own process group so a cancelled
// ctx kills it and anything it spawned, which is also what unblocks a
// pending Next.
type JSONLines struct {
	ctx    context.Context
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	stderr bytes.Buffer
}

// StartJSONLines starts bin with args under ctx. env replaces the
// child's environment; dir is its working directory.
func StartJSONLines(ctx context.Context, bin string, args, env []string, dir string) (*JSONLines, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	procgroup.Isolate(cmd)
	p := &JSONLines{ctx: ctx, cmd: cmd}
	cmd.Stderr = &p.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.stdin = in
	p.lines = bufio.NewScanner(out)
	// Usage replies are small, but claude's init event lists every
	// command and model; leave generous headroom.
	p.lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return p, nil
}

// Send writes v as one JSON line.
func (p *JSONLines) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Next returns the child's next non-blank stdout line. Once the child
// exits or ctx ends it returns an error carrying the cause and the tail
// of the child's stderr.
func (p *JSONLines) Next() ([]byte, error) {
	for p.lines.Scan() {
		if line := bytes.TrimSpace(p.lines.Bytes()); len(line) > 0 {
			return line, nil
		}
	}
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	cause := p.lines.Err()
	if cause == nil {
		cause = io.ErrUnexpectedEOF
	}
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("%w (stderr: %s)", cause, tail)
	}
	return nil, cause
}

// Close ends the child: stdin closes, the group is killed, and the
// child is reaped. The reading has already been taken by then, so the
// exit status is noise and is dropped.
func (p *JSONLines) Close() {
	_ = p.stdin.Close()
	if p.cmd.Cancel != nil {
		_ = p.cmd.Cancel()
	} else if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}
