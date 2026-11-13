package codex

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/modulecollective/moe/internal/agent"
)

// A failed capture must not stop pipe drainage or replace the process result.
// Each stream has a single writer; errors are collected only after Wait.
type captureWriter struct {
	file *os.File
	err  error
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.err == nil {
		_, w.err = w.file.Write(p)
	}
	return len(p), nil
}

func (w *captureWriter) close() error {
	return errors.Join(w.err, w.file.Close())
}

func captureStreams(c *agent.OneShotCapture, warnings io.Writer) (io.Writer, io.Writer, func()) {
	if c == nil {
		return nil, nil, func() {}
	}
	var out, errOut *captureWriter
	finish := func() {
		var captureErr error
		if out != nil {
			captureErr = errors.Join(captureErr, out.close())
		}
		if errOut != nil {
			captureErr = errors.Join(captureErr, errOut.close())
		}
		if captureErr != nil {
			if c.Error != "" {
				c.Error += "; "
			}
			c.Error += captureErr.Error()
		}
		if c.Error != "" {
			fmt.Fprintf(warnings, "codex: capture raw streams: %s\n", c.Error)
		}
	}
	dir, err := os.MkdirTemp("", "moe-codex-exec-*")
	if err != nil {
		c.Error = err.Error()
		return nil, nil, finish
	}
	c.Dir = dir
	open := func(name string, path *string) *captureWriter {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if c.Error != "" {
				c.Error += "; "
			}
			c.Error += err.Error()
			return nil
		}
		*path = p
		return &captureWriter{file: f}
	}
	out = open("stdout.jsonl", &c.StdoutPath)
	errOut = open("stderr.log", &c.StderrPath)
	// Avoid returning a typed nil as io.Writer if opening one stream failed.
	var stdout, stderr io.Writer
	if out != nil {
		stdout = out
	}
	if errOut != nil {
		stderr = errOut
	}
	return stdout, stderr, finish
}
