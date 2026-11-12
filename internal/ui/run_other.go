//go:build !linux && !darwin

package ui

import (
	"context"
	"io"
	"os"
)

// Run refuses off Linux and Darwin, before touching the terminal.
func Run(ctx context.Context, cfg Config, stdout *os.File, stderr io.Writer) error {
	return errPlatform
}
