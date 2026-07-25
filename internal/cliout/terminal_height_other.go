//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package cliout

import (
	"fmt"
	"os"
)

func terminalHeight(f *os.File) (int, error) {
	return 0, fmt.Errorf("terminal sizing unavailable on this platform")
}
