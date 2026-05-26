package serve

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// tailscaleIP4 returns the local node's Tailscale IPv4 address by
// shelling out to `tailscale ip -4`. The CLI prints one address per
// line; we take the first.
//
// If the binary isn't installed or the daemon isn't running the
// error explains the override path — Server's caller surfaces it to
// the operator with a "use --addr" hint.
func tailscaleIP4() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("`tailscale ip -4` failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("run tailscale: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", errors.New("tailscale returned an empty address")
	}
	if i := strings.IndexAny(s, " \n"); i >= 0 {
		s = s[:i]
	}
	return s, nil
}
