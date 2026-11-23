//go:build !linux

package cli

import "testing"

// Unsupported platforms execute ordinary source stages without enrollment;
// reclamation remains held independently of this fixture helper.
func enrollTestClone(t *testing.T, root, p, id string) { t.Helper() }
