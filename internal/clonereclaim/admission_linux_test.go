//go:build linux

package clonereclaim

import (
	"context"
	"io"
	"os"
	"testing"
)

func TestRecoveryAdmissionKeepsLegacyMissingAndUncertainTreesHeld(t *testing.T) {
	for _, kind := range []string{"legacy", "missing", "shell", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "cleanup" {
				f := ownedResourceFixture(t, "")
				put(t, f.fail, "fail")
				if err := f.r.ExecuteCleanup(context.Background(), f.h, f.plan(t, false), f.env, io.Discard, io.Discard); err == nil {
					t.Fatal("failure fixture succeeded")
				}
				if err := CheckRunRecovery(f.h); err == nil {
					t.Fatal("unfinished cleanup admitted")
				}
				return
			}
			h, path := registryFixture(t)
			if kind == "legacy" {
				if err := CheckRunRecovery(h); err == nil {
					t.Fatal("legacy tree admitted")
				}
				return
			}
			r, err := Enroll(h)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := CheckRunRecovery(h); err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				if err := os.Rename(path, path+"-retained"); err != nil {
					t.Fatal(err)
				}
			} else {
				_, attempt, err := r.beginShell("/bin/sh")
				if err != nil {
					t.Fatal(err)
				}
				attempt.Close()
			}
			if err := CheckRunRecovery(h); err == nil {
				t.Fatal("uncertain generation admitted")
			}
		})
	}
}
