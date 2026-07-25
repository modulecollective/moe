//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package cliout

import (
	"fmt"
	"os"
)

func terminalSize(f *os.File) (int, int, error) {
	return 0, 0, fmt.Errorf("terminal sizing unavailable on this platform")
}
