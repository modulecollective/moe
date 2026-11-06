//go:build !unix

package cli

// openCooldownControls offers no terminal controls off unix: there is
// no cancellable stdin read to build them on. Ctrl-C and --no-cooldown
// still work.
func openCooldownControls() (<-chan cooldownControl, func()) {
	return nil, func() {}
}
