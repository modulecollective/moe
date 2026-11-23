//go:build linux

package cli

import (
	"testing"

	"github.com/modulecollective/moe/internal/clonereclaim"
	"github.com/modulecollective/moe/internal/runowner"
)

func enrollTestClone(t *testing.T, root, p, id string) {
	t.Helper()
	h, err := runowner.Acquire(root, p, id)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	r, err := clonereclaim.Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}
