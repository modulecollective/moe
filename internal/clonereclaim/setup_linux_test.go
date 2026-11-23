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

func TestSetupPublicationFailureCannotReuseCache(t *testing.T) {
	f := ownedResourceFixture(t, "")
	if err := f.r.ExecuteCleanup(context.Background(), f.h, f.plan(t, false), f.env, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	intent, err := f.r.BeginSetup(f.h, f.cache, nil, f.scripts)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.r.Identity.Path, ".moe", cacheName), string(f.cache))
	original := syncRecordFile
	defer func() { syncRecordFile = original }()
	injected := errors.New("ownership file sync failed")
	syncRecordFile = func(file *os.File) error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return injected
		}
		return file.Sync()
	}
	if _, err := f.r.RecordResources(f.h, f.cache, nil, f.scripts, false); !errors.Is(err, injected) {
		t.Fatalf("ownership failure: %v", err)
	}
	syncRecordFile = original
	if err := f.r.ValidateEnvironment(f.h); err == nil {
		t.Fatal("cache reused despite failed ownership publication")
	}
	dir, err := f.r.setupDir(intent.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	recorded, err := readRecord[SetupIntent](dir, "intent.json")
	if err != nil || !bytes.Equal(recorded.Cache, f.cache) {
		t.Fatal("original resource addresses lost after publication failure")
	}
}

func TestSetupCompletionBindsExactOwnedCache(t *testing.T) {
	h, path := registryFixture(t)
	r, err := Enroll(h)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cache := []byte("# moe-dev-env-v1 p/r none\n")
	intent, err := r.BeginSetup(h, cache, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(path, ".moe", cacheName), string(cache))
	snapshot, err := r.RecordResources(h, cache, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateEnvironment(h); err == nil {
		t.Fatal("unfinished setup became reusable")
	}
	wrong := intent
	wrong.Cache = []byte("different")
	if err := r.CompleteSetup(h, wrong, snapshot); err == nil {
		t.Fatal("changed setup intent accepted")
	}
	if err := r.CompleteSetup(h, intent, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateEnvironment(h); err != nil {
		t.Fatal(err)
	}
}
