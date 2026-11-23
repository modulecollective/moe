//go:build linux

package clonereclaim

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/modulecollective/moe/internal/runowner"
)

type stoppedUse struct {
	Version      int    `json:"version"`
	Generation   string `json:"generation"`
	Attempt      string `json:"attempt"`
	Confirmation string `json:"confirmation"`
}

const stoppedConfirmation = "operator confirmed old binaries, shells, descendants and unmanaged writers stopped"

type viewApproval struct {
	Identity     Identity `json:"identity"`
	Namespace    string   `json:"mount_namespace"`
	Mount        string   `json:"mount"`
	Confirmation string   `json:"confirmation"`
}

func samePhysical(a, b Identity) bool { b.Namespace, b.Mount = a.Namespace, a.Mount; return a == b }
func (r *Registry) mount() string {
	if r.viewMount != "" {
		return r.viewMount
	}
	return r.Identity.Mount
}

func validateView(dir *os.File, saved, actual Identity) error {
	if saved.Namespace == actual.Namespace && saved.Mount == actual.Mount {
		return nil
	}
	f, err := privateChildDir(dir, "views", false)
	if err != nil {
		return errors.New("host mount view requires explicit revalidation")
	}
	defer f.Close()
	approval, err := readRecord[viewApproval](f, digest([]byte(actual.Namespace+"/"+actual.Mount))+".json")
	if err != nil {
		return err
	}
	if approval != (viewApproval{saved, actual.Namespace, actual.Mount, stoppedConfirmation}) {
		return errors.New("invalid host mount view approval")
	}
	return nil
}

// RecoveryRegistry reads the physical legacy/enrolled identity for a host prompt.
// It permits only a namespace change, never an inode/root/path substitution.
// No mutation occurs until the caller obtains explicit stopped-use confirmation.
func RecoveryRegistry(h *runowner.Handle) (*Registry, Identity, bool, error) {
	actual, tree, err := physicalIdentity(h, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return missingRecoveryRegistry(h)
		}
		return nil, actual, false, err
	}
	defer tree.Close()
	local, err := childDir(tree, ".moe", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, actual, true, nil
	}
	if err != nil {
		return nil, actual, false, err
	}
	defer local.Close()
	marker, err := readRecord[Identity](local, identityMarker)
	if errors.Is(err, os.ErrNotExist) {
		if err := completedGenerations(h); err != nil {
			return nil, actual, false, err
		}
		return nil, actual, true, nil
	}
	if err != nil {
		return nil, actual, false, err
	}
	actual.Generation = marker.Generation
	if !validRandomID(marker.Generation) || !samePhysical(marker, actual) {
		return nil, actual, false, errors.New("unknown or replaced physical clone cannot be recovered")
	}
	base, err := openRegistry(h, false)
	if err != nil {
		return nil, actual, false, err
	}
	defer base.Close()
	gen, err := privateChildDir(base, marker.Generation, false)
	if err != nil {
		return nil, actual, false, err
	}
	saved, err := readRecord[Identity](gen, "identity.json")
	if err != nil || saved != marker {
		gen.Close()
		return nil, actual, false, errors.New("enrollment authority differs from local marker")
	}
	return &Registry{Identity: saved, dir: gen, viewMount: actual.Mount}, actual, false, nil
}

func (r *Registry) ConfirmHostView(h *runowner.Handle, expected Identity) error {
	actual, tree, err := physicalIdentity(h, false)
	if errors.Is(err, os.ErrNotExist) {
		other, view, _, e := missingRecoveryRegistry(h)
		if e != nil {
			return e
		}
		defer other.Close()
		if other.Identity != r.Identity {
			return errors.New("pending recovery generation changed")
		}
		actual = view
		err = nil
	}
	if err != nil {
		return err
	}
	if tree != nil {
		defer tree.Close()
	}
	actual.Generation = r.Identity.Generation
	if actual != expected || !samePhysical(r.Identity, actual) {
		return errors.New("physical identity changed during host confirmation")
	}
	if tree != nil {
		if _, err := scanTree(tree, actual.Mount, false); err != nil {
			return err
		}
	}
	views, err := privateChildDir(r.dir, "views", true)
	if err != nil {
		return err
	}
	defer views.Close()
	name := digest([]byte(actual.Namespace+"/"+actual.Mount)) + ".json"
	approval := viewApproval{r.Identity, actual.Namespace, actual.Mount, stoppedConfirmation}
	if saved, err := readRecord[viewApproval](views, name); err == nil {
		if saved != approval {
			return errors.New("view approval differs")
		}
		return persistRecord(views, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return publishRecord(views, name, approval)
}

func missingRecoveryRegistry(h *runowner.Handle) (*Registry, Identity, bool, error) {
	var none Identity
	if _, err := os.Lstat(filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run)); !errors.Is(err, os.ErrNotExist) {
		return nil, none, false, errors.New("unknown live tree cannot be recovered as an absent clone")
	}
	root, err := openAbsoluteDir(h.Root)
	if err != nil {
		return nil, none, false, err
	}
	defer root.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(root.Fd()), &st); err != nil {
		return nil, none, false, err
	}
	mount, err := mountID(root)
	if err != nil {
		return nil, none, false, err
	}
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return nil, none, false, err
	}
	base, err := openRegistry(h, false)
	if err != nil {
		return nil, none, false, err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, none, false, err
	}
	if len(entries) > 4096 {
		return nil, none, false, errors.New("recovery generation inventory limit exceeded")
	}
	var found *Registry
	fail := func(err error) (*Registry, Identity, bool, error) {
		if found != nil {
			found.Close()
		}
		return nil, none, false, err
	}
	for _, e := range entries {
		if !validRandomID(e.Name()) {
			return fail(errors.New("unknown generation"))
		}
		f, err := privateChildDir(base, e.Name(), false)
		if err != nil {
			return fail(err)
		}
		id, err := readRecord[Identity](f, "identity.json")
		if err != nil {
			f.Close()
			return fail(err)
		}
		if id.Version != protocolVersion || id.Root != h.Root || id.Project != h.Project || id.Run != h.Run || id.Generation != e.Name() || id.Path != filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run) || id.RootDevice != uint64(st.Dev) || id.RootInode != st.Ino {
			f.Close()
			return fail(errors.New("unknown root/generation identity"))
		}
		r := &Registry{Identity: id, dir: f, viewMount: mount}
		done, err := r.Removed()
		if err != nil {
			r.Close()
			return fail(err)
		}
		if done {
			r.Close()
			continue
		}
		d, pending, err := r.PendingRemoval()
		if err != nil {
			r.Close()
			return fail(err)
		}
		if !pending {
			r.Close()
			return fail(errors.New("absent generation has no authorized quarantine"))
		}
		if err := r.InspectRemoval(h, d.ID, d.Basis); err != nil {
			r.Close()
			return fail(err)
		}
		if found != nil {
			r.Close()
			return fail(errors.New("multiple unresolved generations"))
		}
		found = r
	}
	if found == nil {
		return fail(errors.New("no physical generation remains to revalidate"))
	}
	actual := found.Identity
	actual.Mount, actual.Namespace = mount, ns
	return found, actual, false, nil
}

// AdoptCache is limited to explicit stopped-use recovery. Strict cache owner,
// version and current hook contracts must validate; existing ownership wins.
func (r *Registry) AdoptCache(h *runowner.Handle, setup, teardown []Script, free bool) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	history, err := r.resources()
	if err != nil {
		return err
	}
	if len(history) != 0 {
		return nil
	}
	if err := r.checkSetups(history); err != nil {
		return err
	}
	cache, exists, err := r.CacheBytes(h)
	if err != nil || !exists {
		return err
	}
	_, err = r.RecordResources(h, cache, setup, teardown, free)
	return err
}

func stopped(attempt *os.File, generation, id string) error {
	s, err := readRecord[stoppedUse](attempt, "stopped.json")
	if err != nil {
		return err
	}
	if s != (stoppedUse{protocolVersion, generation, id, stoppedConfirmation}) {
		return errors.New("invalid stopped-worker resolution")
	}
	return nil
}

// ConfirmStopped resolves uncertain workers with immutable receipts. It neither
// fabricates supervisor success nor discards the original intent/snapshot.
func (r *Registry) ConfirmStopped(h *runowner.Handle) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	resolve := func(parent *os.File, start, complete string) error {
		entries, err := parent.ReadDir(4097)
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries) > 4096 {
			return errors.New("recovery inventory limit exceeded")
		}
		for _, e := range entries {
			if !validRandomID(e.Name()) {
				return errors.New("invalid recovery attempt")
			}
			f, err := privateChildDir(parent, e.Name(), false)
			if err != nil {
				return err
			}
			// Strict readers check the start/intent grammar before normal admission;
			// a resolution never repairs missing/malformed start records.
			if _, _, err := readRegularAt(f, start, 1<<20); err != nil {
				f.Close()
				return err
			}
			_, _, err = readRegularAt(f, complete, 1<<20)
			if errors.Is(err, os.ErrNotExist) {
				if err = stopped(f, r.Identity.Generation, e.Name()); errors.Is(err, os.ErrNotExist) {
					err = publishRecord(f, "stopped.json", stoppedUse{protocolVersion, r.Identity.Generation, e.Name(), stoppedConfirmation})
				}
			}
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	shells, err := privateChildDir(r.dir, "shells", false)
	if err == nil {
		err = resolve(shells, "start.json", "complete.json")
		shells.Close()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	history, err := r.resources()
	if err != nil {
		return err
	}
	for _, s := range history {
		f, err := r.resourceDir(s.ID, false)
		if err != nil {
			return err
		}
		attempts, err := privateChildDir(f, "attempts", false)
		f.Close()
		if err == nil {
			err = resolve(attempts, "intent.json", "teardown-complete.json")
			attempts.Close()
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type scriptApproval struct {
	Version     int      `json:"version"`
	Generation  string   `json:"generation"`
	Resource    string   `json:"resource"`
	CacheDigest string   `json:"cache_sha256"`
	Original    []Script `json:"original"`
	Approved    []Script `json:"approved"`
}

func approvalName(s []Script) string { b, _ := json.Marshal(s); return digest(b) + ".json" }

func (r *Registry) scriptsApproved(s Resources, scripts []Script) bool {
	if reflect.DeepEqual(scripts, s.Teardown) {
		return true
	}
	f, err := r.resourceDir(s.ID, false)
	if err != nil {
		return false
	}
	defer f.Close()
	a, err := privateChildDir(f, "approvals", false)
	if err != nil {
		return false
	}
	defer a.Close()
	saved, err := readRecord[scriptApproval](a, approvalName(scripts))
	if err != nil {
		return false
	}
	return saved.Version == protocolVersion && saved.Generation == s.Generation && saved.Resource == s.ID && saved.CacheDigest == s.Digest && reflect.DeepEqual(saved.Original, s.Teardown) && reflect.DeepEqual(saved.Approved, scripts)
}

// ScriptChange exposes digests only, never cache secrets, for a separate explicit
// approval against original addresses. Missing/renamed hooks cannot be waived.
func (r *Registry) ScriptChange(h *runowner.Handle, current []Script) ([]Script, bool, error) {
	if err := r.validateOwner(h); err != nil {
		return nil, false, err
	}
	history, err := r.resources()
	if err != nil {
		return nil, false, err
	}
	if len(history) == 0 {
		return nil, false, nil
	}
	s := history[len(history)-1]
	if len(current) != len(s.Teardown) {
		return nil, false, errors.New("restore all original teardown hooks before recovery")
	}
	for i := range current {
		if current[i].Path != s.Teardown[i].Path {
			return nil, false, errors.New("restore original teardown hook paths")
		}
	}
	return s.Teardown, !r.scriptsApproved(s, current), nil
}
func (r *Registry) ApproveScripts(h *runowner.Handle, current []Script) error {
	_, changed, err := r.ScriptChange(h, current)
	if err != nil || !changed {
		return err
	}
	paths := make([]string, len(current))
	for i, s := range current {
		paths[i] = s.Path
	}
	fresh, err := ScriptIdentities(h.Root, paths)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh, current) {
		return errors.New("scripts changed during confirmation")
	}
	history, err := r.resources()
	if err != nil {
		return err
	}
	s := history[len(history)-1]
	f, err := r.resourceDir(s.ID, false)
	if err != nil {
		return err
	}
	defer f.Close()
	a, err := privateChildDir(f, "approvals", true)
	if err != nil {
		return err
	}
	defer a.Close()
	return publishRecord(a, approvalName(current), scriptApproval{protocolVersion, s.Generation, s.ID, s.Digest, s.Teardown, current})
}

// RepairSetup completes a publication interrupted after cache bytes were
// retained. It uses only those bytes and original scripts; it never reruns setup.
func (r *Registry) RepairSetup(h *runowner.Handle) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	base, err := privateChildDir(r.dir, "setups", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 4096 {
		return errors.New("setup recovery inventory limit exceeded")
	}
	for _, e := range entries {
		f, err := r.setupDir(e.Name(), false)
		if err != nil {
			return err
		}
		intent, err := readRecord[SetupIntent](f, "intent.json")
		if err != nil {
			f.Close()
			return err
		}
		_, doneErr := readRecord[setupComplete](f, "complete.json")
		f.Close()
		if doneErr == nil {
			continue
		}
		if !errors.Is(doneErr, os.ErrNotExist) {
			return doneErr
		}
		if intent.Version != protocolVersion || intent.Generation != r.Identity.Generation || intent.ID != e.Name() || intent.Digest != digest(intent.Cache) {
			return errors.New("invalid setup recovery intent")
		}
		if err := r.recoverUnpublished(intent); err != nil {
			return err
		}
		history, err := r.resources()
		if err != nil {
			return err
		}
		var snapshot Resources
		for _, s := range history {
			if s.Digest == intent.Digest && reflect.DeepEqual(s.Setup, intent.Setup) && reflect.DeepEqual(s.Teardown, intent.Teardown) {
				cleared, err := r.readCleared(s)
				if err != nil {
					return err
				}
				if cleared == nil {
					snapshot = s
				}
			}
		}
		if snapshot.ID == "" {
			// Restore the retained exact cache if rollback cleared it. This authorizes
			// idempotent teardown of the original allocation, never cache substitution.
			cache, exists, err := r.CacheBytes(h)
			if err != nil {
				return err
			}
			if exists && !reflect.DeepEqual(cache, intent.Cache) {
				return errors.New("cache changed since interrupted setup")
			}
			if !exists {
				local, err := openAbsoluteDir(filepath.Join(r.Identity.Path, ".moe"))
				if err != nil {
					return err
				}
				n, err := syscall.Openat(int(local.Fd()), cacheName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
				fd := os.NewFile(uintptr(n), cacheName)
				if err == nil {
					_, err = fd.Write(intent.Cache)
					if err == nil {
						err = syncRecordFile(fd)
					}
					fd.Close()
				}
				if err == nil {
					err = syncRecordFile(local)
				}
				local.Close()
				if err != nil {
					return err
				}
			}
			snapshot, err = r.RecordResources(h, intent.Cache, intent.Setup, intent.Teardown, len(intent.Setup) == 0 && emptyCache(intent.Cache))
			if err != nil {
				return err
			}
		}
		if err := r.CompleteSetup(h, intent, snapshot); err != nil {
			return err
		}
	}
	return nil
}

type publicationRecovery struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Resource   string `json:"resource"`
	Setup      string `json:"setup"`
	Digest     string `json:"cache_sha256"`
}

func (r *Registry) unpublishedResolved(dir *os.File, id string) bool {
	saved, err := readRecord[publicationRecovery](dir, "recovered-publication.json")
	if err != nil {
		return false
	}
	if saved.Version != protocolVersion || saved.Generation != r.Identity.Generation || saved.Resource != id || !validRandomID(saved.Setup) {
		return false
	}
	setup, err := r.setupDir(saved.Setup, false)
	if err != nil {
		return false
	}
	defer setup.Close()
	intent, err := readRecord[SetupIntent](setup, "intent.json")
	return err == nil && intent.Version == protocolVersion && intent.Generation == saved.Generation && intent.ID == saved.Setup && intent.Digest == saved.Digest && intent.Digest == digest(intent.Cache)
}

func (r *Registry) recoverUnpublished(intent SetupIntent) error {
	probe := Resources{Version: protocolVersion, Generation: r.Identity.Generation, ID: intent.ID, Sequence: 1, Cache: intent.Cache, Digest: intent.Digest, Setup: intent.Setup, Teardown: intent.Teardown, ResourceFree: len(intent.Setup) == 0 && emptyCache(intent.Cache)}
	if err := r.validateResources(probe); err != nil {
		return err
	}
	base, err := privateChildDir(r.dir, "resources", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 4096 {
		return errors.New("resource recovery inventory limit exceeded")
	}
	for _, e := range entries {
		f, err := r.resourceDir(e.Name(), false)
		if err != nil {
			return err
		}
		_, err = readRecord[Resources](f, "ownership.json")
		if errors.Is(err, os.ErrNotExist) && !r.unpublishedResolved(f, e.Name()) {
			// An empty publication directory is retained permanently. Its resolution
			// points to the independently durable setup bytes used for new ownership.
			err = publishRecord(f, "recovered-publication.json", publicationRecovery{protocolVersion, r.Identity.Generation, e.Name(), intent.ID, intent.Digest})
		} else if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func emptyCache(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" && line[0] != '#' {
			return false
		}
	}
	return true
}
