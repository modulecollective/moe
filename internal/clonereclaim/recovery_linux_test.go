//go:build linux

package clonereclaim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryStoppedWorkersDoesNotInventSuccess(t *testing.T) {
	f := ownedResourceFixture(t, "")
	start, dir, err := f.r.beginShell("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()
	if err := f.r.CheckShells(f.h); err == nil {
		t.Fatal("missing completion admitted")
	}
	if err := f.r.ConfirmStopped(f.h); err != nil {
		t.Fatal(err)
	}
	if err := f.r.CheckShells(f.h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.r.Receipt(), "shells", start.ID, "complete.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("operator confirmation forged supervisor completion")
	}
	plan := f.plan(t, false)
	intent, attempt, err := f.r.beginCleanup(f.h, plan)
	if err != nil {
		t.Fatal(err)
	}
	attempt.Close()
	if _, err := f.r.PlanCleanup(f.h, "refresh", f.scripts, true); err == nil {
		t.Fatal("interrupted worker automatically retried")
	}
	if err := f.r.ConfirmStopped(f.h); err != nil {
		t.Fatal(err)
	}
	plan = f.plan(t, true)
	if err := f.r.ExecuteCleanup(context.Background(), f.h, plan, f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.count(t) != 1 {
		t.Fatal("recovery reran or skipped actual cleanup")
	}
	if _, err := os.Stat(filepath.Join(f.r.resourcePath(plan.Resources.ID), "attempts", intent.ID, "teardown-complete.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stopped worker became successful")
	}
	if _, err := os.Stat(f.r.Identity.Path); err != nil {
		t.Fatal("recovery deleted clone")
	}
}

func TestRecoveryApprovesExactDigestAgainstOriginalSnapshot(t *testing.T) {
	f := ownedResourceFixture(t, "")
	history, err := f.r.resources()
	if err != nil {
		t.Fatal(err)
	}
	original := history[0]
	script := filepath.Join(f.h.Root, f.scripts[0].Path)
	put(t, script, "#!/bin/sh\nset -eu\nprintf 'run\\n' >> \"$TRACE\"\nif [ -f \"$OWNED\" ]; then rm -- \"$OWNED\"; fi\n# approved replacement\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	current, err := ScriptIdentities(f.h.Root, []string{f.scripts[0].Path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.PlanCleanup(f.h, "refresh", current, true); err == nil {
		t.Fatal("changed digest silently accepted")
	}
	if _, _, err := f.r.ScriptChange(f.h, nil); err == nil {
		t.Fatal("missing hook waived")
	}
	if err := f.r.ApproveScripts(f.h, current); err != nil {
		t.Fatal(err)
	}
	plan, err := f.r.PlanCleanup(f.h, "refresh", current, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.ExecuteCleanup(context.Background(), f.h, plan, f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	history, err = f.r.resources()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(history[0].Cache, original.Cache) || history[0].Digest != original.Digest || history[0].Teardown[0] != original.Teardown[0] {
		t.Fatal("approval rewrote original ownership")
	}
	if f.count(t) != 1 {
		t.Fatal("approved production hook not executed")
	}
}

func TestRecoveryRepairsInterruptedSetupPublication(t *testing.T) {
	f := ownedResourceFixture(t, "")
	if err := f.r.ExecuteCleanup(context.Background(), f.h, f.plan(t, false), f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	intent, err := f.r.BeginSetup(f.h, f.cache, nil, f.scripts)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.r.Identity.Path, ".moe", cacheName), string(f.cache))
	sync := syncRecordFile
	syncRecordFile = func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return errors.New("publication fault")
		}
		return sync(file)
	}
	_, err = f.r.RecordResources(f.h, f.cache, nil, f.scripts, false)
	syncRecordFile = sync
	if err == nil {
		t.Fatal("fault did not fire")
	}
	if err := f.r.RepairSetup(f.h); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ValidateEnvironment(f.h); err != nil {
		t.Fatal("repaired cache not usable:", err)
	}
	dir, err := f.r.setupDir(intent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	completed, err := readRecord[setupComplete](dir, "complete.json")
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.r.resources()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || completed.Resource != history[1].ID {
		t.Fatal("publication reused cleared ownership")
	}
	if err := f.r.RepairSetup(f.h); err != nil {
		t.Fatal("repeat recovery:", err)
	}
}
