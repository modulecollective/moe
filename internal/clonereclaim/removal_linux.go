//go:build linux

package clonereclaim

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"syscall"
	"unsafe"

	"github.com/modulecollective/moe/internal/run"
	"github.com/modulecollective/moe/internal/runowner"
)

// DisposalBasis is the journal and preservation evidence saved BEFORE hooks.
// A rebind has no terminal clock and always requires conservative preservation.
type DisposalBasis struct {
	Purpose      string               `json:"purpose"`
	Terminal     run.TerminalEvidence `json:"terminal"`
	Expired      bool                 `json:"retention_expired"`
	Preservation string               `json:"preservation"`
}

type Disposal struct {
	Version     int           `json:"version"`
	Generation  string        `json:"generation"`
	ID          string        `json:"id"`
	Basis       DisposalBasis `json:"basis"`
	Resource    string        `json:"resource,omitempty"`
	CacheDigest string        `json:"cache_sha256,omitempty"`
	Scripts     []Script      `json:"scripts,omitempty"`
}

type TreeEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Digest string `json:"sha256,omitempty"`
}

// UnsafeTreeError is a known confinement hold, distinct from an incomplete or
// failed filesystem probe (which the collector reports as an operational error).
type UnsafeTreeError struct{ Reason string }

func (e *UnsafeTreeError) Error() string { return e.Reason }

type removalAuthorization struct {
	Disposal  Disposal    `json:"disposal"`
	Identity  Identity    `json:"identity"`
	Inventory []TreeEntry `json:"inventory"`
}

type removedRecord struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Attempt    string `json:"attempt"`
}

func (r *Registry) Receipt() string { return r.dir.Name() }

// Artifacts exempts only the exact validated marker and owned cache bytes.
func (r *Registry) Artifacts(h *runowner.Handle) (map[string]string, error) {
	if err := r.validateOwner(h); err != nil {
		return nil, err
	}
	f, err := openAbsoluteDir(filepath.Join(r.Identity.Path, ".moe"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	marker, _, err := readRegularAt(f, identityMarker, 1<<20)
	if err != nil {
		return nil, err
	}
	out := map[string]string{filepath.Join(".moe", identityMarker): digest(marker)}
	cache, exists, err := r.CacheBytes(h)
	if err != nil {
		return nil, err
	}
	if exists {
		history, err := r.resources()
		if err != nil {
			return nil, err
		}
		if len(history) == 0 || !reflect.DeepEqual(cache, history[len(history)-1].Cache) {
			return nil, errors.New("cache lacks matching ownership")
		}
		out[filepath.Join(".moe", cacheName)] = digest(cache)
	}
	return out, nil
}

func (r *Registry) CheckTree(h *runowner.Handle, hash bool) ([]TreeEntry, error) {
	if err := r.validateOwner(h); err != nil {
		return nil, err
	}
	f, err := openAbsoluteDir(r.Identity.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanTree(f, r.mount(), hash)
}

func scanTree(root *os.File, mount string, hash bool) ([]TreeEntry, error) {
	var out []TreeEntry
	var bytesRead int64
	var walk func(*os.File, string) error
	walk = func(dir *os.File, rel string) error {
		m, err := mountID(dir)
		if err != nil {
			return err
		}
		if m != mount {
			return &UnsafeTreeError{Reason: "nested mount in clone"}
		}
		entries, err := dir.ReadDir(8193 - len(out))
		if err != nil && err != io.EOF {
			return err
		}
		if len(entries)+len(out) > 8192 {
			return errors.New("clone inventory limit exceeded")
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		// A bare nested repository is unsafe too; .git/ itself contains this shape.
		if rel != "" && rel != ".git" {
			names := map[string]bool{}
			for _, e := range entries {
				names[e.Name()] = true
			}
			if names["HEAD"] && names["objects"] && names["refs"] {
				return &UnsafeTreeError{Reason: "nested bare repository in clone"}
			}
		}
		for _, e := range entries {
			path := filepath.Join(rel, e.Name())
			if e.Name() == ".git" && rel != "" {
				return &UnsafeTreeError{Reason: "nested repository in clone"}
			}
			fd, err := syscall.Openat(int(dir.Fd()), e.Name(), 0x200000|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			var st syscall.Stat_t
			err = syscall.Fstat(fd, &st)
			probe := os.NewFile(uintptr(fd), path)
			entryMount, merr := mountID(probe)
			probe.Close()
			if err != nil {
				return err
			}
			if merr != nil {
				return merr
			}
			if entryMount != mount {
				return &UnsafeTreeError{Reason: "nested mount in clone"}
			}
			item := TreeEntry{Path: path, Mode: st.Mode, Device: uint64(st.Dev), Inode: st.Ino}
			switch st.Mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				child, err := childDir(dir, e.Name(), false)
				if err != nil {
					return err
				}
				out = append(out, item)
				err = walk(child, path)
				child.Close()
				if err != nil {
					return err
				}
				continue
			case syscall.S_IFREG:
				if hash {
					fd, err := syscall.Openat(int(dir.Fd()), e.Name(), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
					if err != nil {
						return err
					}
					file := os.NewFile(uintptr(fd), path)
					var actual syscall.Stat_t
					err = syscall.Fstat(fd, &actual)
					if err == nil && (actual.Dev != st.Dev || actual.Ino != st.Ino || actual.Mode != st.Mode) {
						err = errors.New("file changed during inventory")
					}
					sum := sha256.New()
					var n int64
					if err == nil {
						n, err = io.Copy(sum, io.LimitReader(file, (1<<30)-bytesRead+1))
						bytesRead += n
					}
					file.Close()
					if err != nil {
						return err
					}
					if bytesRead > 1<<30 {
						return errors.New("clone byte inventory limit exceeded")
					}
					item.Digest = hex.EncodeToString(sum.Sum(nil))
				}
			case syscall.S_IFLNK:
				// readlink through this pinned parent; the link itself is never followed.
				target, err := os.Readlink(filepath.Join("/proc/self/fd", fmt.Sprint(dir.Fd()), e.Name()))
				if err != nil {
					return err
				}
				if hash {
					item.Digest = digest([]byte(target))
				}
			default:
				return &UnsafeTreeError{Reason: "special file in clone"}
			}
			out = append(out, item)
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Registry) BeginDisposal(h *runowner.Handle, basis DisposalBasis, plan CleanupPlan) (Disposal, error) {
	var d Disposal
	basis.Terminal.At = basis.Terminal.At.UTC()
	basis.Terminal.Deadline = basis.Terminal.Deadline.UTC()
	if err := r.validateOwner(h); err != nil {
		return d, err
	}
	if basis.Purpose != "terminal" && basis.Purpose != "rebind" {
		return d, errors.New("invalid disposal purpose")
	}
	if basis.Purpose == "terminal" && (!run.IsTerminal(basis.Terminal.Status) || basis.Terminal.Commit == "") {
		return d, errors.New("missing terminal basis")
	}
	if basis.Purpose == "rebind" && basis.Expired {
		return d, errors.New("live rebind cannot expire local work")
	}
	if !basis.Expired && basis.Preservation != "independently_preserved" {
		return d, errors.New("missing preservation basis")
	}
	if _, err := r.CheckTree(h, !basis.Expired); err != nil {
		return d, err
	}
	base, err := privateChildDir(r.dir, "disposals", true)
	if err != nil {
		return d, err
	}
	defer base.Close()
	id, err := randomID()
	if err != nil {
		return d, err
	}
	attempt, err := privateChildDir(base, id, true)
	if err != nil {
		return d, err
	}
	defer attempt.Close()
	d = Disposal{Version: protocolVersion, Generation: r.Identity.Generation, ID: id, Basis: basis, Scripts: plan.Scripts}
	if plan.Resources != nil {
		d.Resource = plan.Resources.ID
		d.CacheDigest = plan.Resources.Digest
	}
	return d, publishRecord(attempt, "intent.json", d)
}

// AuthorizeRemoval is called only after shared teardown/cache clear, followed by
// a fresh journal and preservation decision. The complete inventory permits a
// retry to distinguish partial deletion from new or changed early-retained work.
func (r *Registry) AuthorizeRemoval(h *runowner.Handle, d Disposal, plan CleanupPlan) error {
	if err := r.validateOwner(h); err != nil {
		return err
	}
	if !plan.CacheCleared && plan.Resources != nil {
		return errors.New("resources are not durably cleared")
	}
	inventory, err := r.CheckTree(h, !d.Basis.Expired)
	if err != nil {
		return err
	}
	attempt, err := r.disposalDir(d.ID)
	if err != nil {
		return err
	}
	defer attempt.Close()
	saved, err := readRecord[Disposal](attempt, "intent.json")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(saved, d) {
		return errors.New("disposal intent changed")
	}
	if err := r.checkRemovalTeardown(plan); err != nil {
		return err
	}
	if plan.Resources != nil && (d.Resource != plan.Resources.ID || d.CacheDigest != plan.Resources.Digest) {
		return errors.New("resource ownership changed since disposal intent")
	}
	if (plan.Resources == nil) != (d.Resource == "") {
		return errors.New("disposal resource evidence changed")
	}
	if err := publishRecord(attempt, "teardown-complete.json", plan); err != nil {
		return err
	}
	return publishRecord(attempt, "removal-authorized.json", removalAuthorization{d, r.Identity, inventory})
}

func (r *Registry) disposalDir(id string) (*os.File, error) {
	if !validRandomID(id) {
		return nil, errors.New("invalid disposal id")
	}
	base, err := privateChildDir(r.dir, "disposals", false)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	return privateChildDir(base, id, false)
}

// InspectRemoval proves a pending authorization without syncing or mutating it.
func (r *Registry) InspectRemoval(h *runowner.Handle, id string, basis DisposalBasis) error {
	if h == nil || !h.Owns(r.Identity.Root, r.Identity.Project, r.Identity.Run) {
		return errors.New("removal inspection requires held ownership")
	}
	if err := r.checkShellRecords(); err != nil {
		return err
	}
	if err := CheckNamedShells(h); err != nil {
		return err
	}
	attempt, err := r.disposalDir(id)
	if err != nil {
		return err
	}
	defer attempt.Close()
	auth, err := readRecord[removalAuthorization](attempt, "removal-authorized.json")
	if err != nil {
		return err
	}
	intent, err := readRecord[Disposal](attempt, "intent.json")
	if err != nil {
		return err
	}
	if auth.Identity != r.Identity || !reflect.DeepEqual(auth.Disposal, intent) || intent.ID != id || intent.Generation != r.Identity.Generation || intent.Version != protocolVersion || intent.Basis.Purpose != basis.Purpose || !sameTerminal(intent.Basis.Terminal, basis.Terminal) || (intent.Basis.Expired && !basis.Expired) {
		return errors.New("pending removal authority changed")
	}
	plan, err := readRecord[CleanupPlan](attempt, "teardown-complete.json")
	if err != nil {
		return err
	}
	if err := r.checkRemovalTeardown(plan); err != nil {
		return err
	}
	tree, err := childDir(attempt, "tree", false)
	if errors.Is(err, os.ErrNotExist) {
		q, e := readRecord[removedRecord](attempt, "quarantined.json")
		if e == nil {
			if q != (removedRecord{protocolVersion, r.Identity.Generation, id}) {
				return errors.New("invalid quarantine receipt")
			}
			return nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if err := r.validateOwner(h); err != nil {
			return err
		}
		tree, err = openAbsoluteDir(r.Identity.Path)
	}
	if err != nil {
		return err
	}
	defer tree.Close()
	physical := r.Identity
	physical.Mount = r.mount()
	if err := matchesTree(tree, physical); err != nil {
		return err
	}
	current, err := scanTree(tree, r.mount(), !basis.Expired)
	if err != nil {
		return err
	}
	if !basis.Expired {
		saved := map[string]TreeEntry{}
		for _, e := range auth.Inventory {
			saved[e.Path] = e
		}
		for _, e := range current {
			if saved[e.Path] != e {
				return errors.New("pending early tree has changed since authorization")
			}
		}
	}
	return nil
}

// FinishRemoval may also run with an absent live path. It touches only the
// authorized inode in the private tree slot, never a replacement at that path.
func (r *Registry) FinishRemoval(h *runowner.Handle, id string, basis DisposalBasis) error {
	if h == nil || !h.Owns(r.Identity.Root, r.Identity.Project, r.Identity.Run) {
		return errors.New("removal requires held ownership")
	}
	if err := r.checkShellRecords(); err != nil {
		return err
	}
	if err := CheckNamedShells(h); err != nil {
		return err
	}
	attempt, err := r.disposalDir(id)
	if err != nil {
		return err
	}
	defer attempt.Close()
	auth, err := readRecord[removalAuthorization](attempt, "removal-authorized.json")
	if err != nil {
		return err
	}
	intent, err := readRecord[Disposal](attempt, "intent.json")
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(intent, auth.Disposal) {
		return errors.New("removal authorization differs from original intent")
	}
	if auth.Identity != r.Identity || auth.Disposal.ID != id || auth.Disposal.Generation != r.Identity.Generation || auth.Disposal.Version != protocolVersion {
		return errors.New("removal authorization identity mismatch")
	}
	if auth.Disposal.Basis.Purpose != basis.Purpose || !sameTerminal(auth.Disposal.Basis.Terminal, basis.Terminal) || (auth.Disposal.Basis.Expired && !basis.Expired) {
		return errors.New("removal journal/retention authority changed")
	}
	plan, err := readRecord[CleanupPlan](attempt, "teardown-complete.json")
	if err != nil {
		return err
	}
	if plan.Resources != nil && (!plan.CacheCleared || plan.Resources.Generation != r.Identity.Generation) {
		return errors.New("removal lacks completed teardown")
	}
	if err := r.checkRemovalTeardown(plan); err != nil {
		return err
	}
	for _, name := range []string{"intent.json", "teardown-complete.json", "removal-authorized.json"} {
		if err := persistRecord(attempt, name); err != nil {
			return err
		}
	}
	if plan.Resources != nil {
		if err := r.persistCleanup(*plan.Resources, plan.CompletedAttempt); err != nil {
			return err
		}
		resource, err := r.resourceDir(plan.Resources.ID, false)
		if err != nil {
			return err
		}
		err = persistRecord(resource, "cache-cleared.json")
		resource.Close()
		if err != nil {
			return err
		}
	}
	done, err := readRecord[removedRecord](attempt, "removed.json")
	if err == nil {
		if done != (removedRecord{protocolVersion, r.Identity.Generation, id}) {
			return errors.New("invalid removed receipt")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tree, err := childDir(attempt, "tree", false)
	if errors.Is(err, os.ErrNotExist) {
		// Before the rename, a valid live generation is mandatory. If that inode
		// is absent, only a durable quarantine receipt can prove completed removal.
		moved, moveErr := readRecord[removedRecord](attempt, "quarantined.json")
		if moveErr == nil {
			if moved != (removedRecord{protocolVersion, r.Identity.Generation, id}) {
				return errors.New("invalid quarantine evidence")
			}
			return publishRecord(attempt, "removed.json", removedRecord{protocolVersion, r.Identity.Generation, id})
		}
		if !errors.Is(moveErr, os.ErrNotExist) {
			return moveErr
		}
		if err := r.validateOwner(h); err != nil {
			return err
		}
		parent, err := openAbsoluteDir(filepath.Dir(r.Identity.Path))
		if err != nil {
			return err
		}
		defer parent.Close()
		if err := renameCloneTree(parent, filepath.Base(r.Identity.Path), attempt, "tree"); err != nil {
			return err
		}
		tree, err = childDir(attempt, "tree", false)
		if err != nil {
			return errors.Join(err, renameNoReplace(attempt, "tree", parent, filepath.Base(r.Identity.Path)), syncRecordFile(parent), syncRecordFile(attempt))
		}
		physical := r.Identity
		physical.Mount = r.mount()
		if err := matchesTree(tree, physical); err != nil {
			tree.Close()
			restore := renameNoReplace(attempt, "tree", parent, filepath.Base(r.Identity.Path))
			return errors.Join(err, restore, syncRecordFile(parent), syncRecordFile(attempt))
		}
		if err := syncRecordFile(parent); err != nil {
			tree.Close()
			return err
		}
		if err := syncRecordFile(attempt); err != nil {
			tree.Close()
			return err
		}
		if err := publishRecord(attempt, "quarantined.json", removedRecord{protocolVersion, r.Identity.Generation, id}); err != nil {
			tree.Close()
			return err
		}
	}
	if tree == nil {
		return err
	}
	defer tree.Close()
	physical := r.Identity
	physical.Mount = r.mount()
	if err := matchesTree(tree, physical); err != nil {
		return err
	}
	// Resume after a rename whose directory fsync or quarantine publication failed.
	if err := syncRecordFile(attempt); err != nil {
		return err
	}
	parent, err := openAbsoluteDir(filepath.Dir(r.Identity.Path))
	if err != nil {
		return err
	}
	err = syncRecordFile(parent)
	parent.Close()
	if err != nil {
		return err
	}
	q := removedRecord{protocolVersion, r.Identity.Generation, id}
	if saved, err := readRecord[removedRecord](attempt, "quarantined.json"); errors.Is(err, os.ErrNotExist) {
		if err := publishRecord(attempt, "quarantined.json", q); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if saved != q {
		return errors.New("invalid quarantine receipt")
	}
	if err := persistRecord(attempt, "quarantined.json"); err != nil {
		return err
	}
	current, err := scanTree(tree, r.mount(), !basis.Expired)
	if err != nil {
		return err
	}
	if !basis.Expired {
		admitted := map[string]TreeEntry{}
		for _, e := range auth.Inventory {
			admitted[e.Path] = e
		}
		for _, e := range current {
			if admitted[e.Path] != e {
				return errors.New("private tree has changed since removal authorization")
			}
		}
	}
	if err := deleteTree(tree, r.mount()); err != nil {
		return err
	}
	if err := unlinkDirectory(attempt, "tree"); err != nil {
		return err
	}
	if err := syncRecordFile(attempt); err != nil {
		return err
	}
	return publishRecord(attempt, "removed.json", removedRecord{protocolVersion, r.Identity.Generation, id})
}

func (r *Registry) checkRemovalTeardown(plan CleanupPlan) error {
	history, err := r.resources()
	if err != nil {
		return err
	}
	if err := r.checkSetups(history); err != nil {
		return err
	}
	if plan.Resources == nil {
		if len(history) != 0 {
			return errors.New("removal omitted allocated resources")
		}
		return nil
	}
	if len(history) == 0 || !reflect.DeepEqual(history[len(history)-1], *plan.Resources) {
		return errors.New("removal resource snapshot changed")
	}
	latest := history[len(history)-1]
	complete, _, err := r.inspectAttempts(latest)
	if err != nil {
		return err
	}
	cleared, err := r.readCleared(latest)
	if err != nil {
		return err
	}
	if complete == "" || cleared == nil || complete != plan.CompletedAttempt || cleared.Attempt != complete {
		return errors.New("removal teardown/clear authority missing")
	}
	return nil
}

func sameTerminal(a, b run.TerminalEvidence) bool {
	return a.Commit == b.Commit && a.Status == b.Status && a.At.Equal(b.At) && a.Deadline.Equal(b.Deadline)
}

func matchesTree(f *os.File, id Identity) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	m, err := mountID(f)
	if err != nil {
		return err
	}
	if uint64(st.Dev) != id.Device || st.Ino != id.Inode || m != id.Mount {
		return errors.New("quarantined directory is a replacement; contents retained")
	}
	return nil
}

func deleteTree(dir *os.File, mount string) error {
	m, err := mountID(dir)
	if err != nil {
		return err
	}
	if m != mount {
		return errors.New("mount changed during removal")
	}
	if _, err := dir.Seek(0, 0); err != nil {
		return err
	}
	entries, err := dir.ReadDir(8193)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 8192 {
		return errors.New("removal inventory limit exceeded")
	}
	for _, e := range entries {
		child, err := childDir(dir, e.Name(), false)
		if err == nil {
			err = deleteTree(child, mount)
			child.Close()
			if err != nil {
				return err
			}
			if err := unlinkDirectory(dir, e.Name()); err != nil {
				return err
			}
		} else if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			if err := syscall.Unlinkat(int(dir.Fd()), e.Name()); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return syncRecordFile(dir)
}

func unlinkDirectory(parent *os.File, name string) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, parent.Fd(), uintptr(unsafe.Pointer(p)), 0x200)
	if errno != 0 {
		return errno
	}
	return nil
}

func renameNoReplace(from *os.File, old string, to *os.File, name string) error {
	// syscall deliberately freezes its API. Use renameat2 only on verified
	// architectures; unsupported systems hold instead of overwriting a tree.
	var trap uintptr
	switch runtime.GOARCH {
	case "amd64":
		trap = 316
	case "arm64", "riscv64", "loong64":
		trap = 276
	case "386":
		trap = 353
	default:
		return errors.New("confined rename unsupported on this architecture")
	}
	a, err := syscall.BytePtrFromString(old)
	if err != nil {
		return err
	}
	b, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(trap, from.Fd(), uintptr(unsafe.Pointer(a)), to.Fd(), uintptr(unsafe.Pointer(b)), 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// Serial tests replace this one rename to substitute the admitted pathname at
// the boundary. Restoration always uses the raw no-overwrite primitive.
var renameCloneTree = renameNoReplace

// Registries includes generations with missing live clones, so interrupted
// private-tree removals remain visible to GC. It never adopts a tree.
func Registries(h *runowner.Handle) ([]*Registry, error) {
	base, err := openRegistry(h, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("generation inventory limit exceeded")
	}
	var out []*Registry
	fail := func(err error) ([]*Registry, error) {
		for _, r := range out {
			r.Close()
		}
		return nil, err
	}
	for _, e := range entries {
		if !validRandomID(e.Name()) {
			return fail(errors.New("unknown generation entry"))
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
		if id.Version != protocolVersion || id.Generation != e.Name() || id.Root != h.Root || id.Project != h.Project || id.Run != h.Run || id.Path != filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run) {
			f.Close()
			return fail(errors.New("invalid recorded generation"))
		}
		root, err := openAbsoluteDir(h.Root)
		if err != nil {
			f.Close()
			return fail(err)
		}
		var st syscall.Stat_t
		err = syscall.Fstat(int(root.Fd()), &st)
		m, merr := mountID(root)
		root.Close()
		if err != nil || merr != nil || uint64(st.Dev) != id.RootDevice || st.Ino != id.RootInode {
			f.Close()
			return fail(errors.New("recorded root identity changed"))
		}
		ns, err := os.Readlink("/proc/self/ns/mnt")
		actual := id
		actual.Namespace, actual.Mount = ns, m
		archived := &Registry{Identity: id, dir: f, viewMount: m}
		viewErr := validateView(f, id, actual)
		if err == nil && viewErr != nil {
			done, derr := archived.Removed()
			if derr != nil {
				f.Close()
				return fail(derr)
			}
			if done {
				viewErr = nil
			}
		}
		if err != nil || viewErr != nil {
			f.Close()
			return fail(errors.New("generation mount namespace requires host revalidation"))
		}
		out = append(out, &Registry{Identity: id, dir: f, viewMount: m})
	}
	return out, nil
}

func (r *Registry) PendingRemoval() (Disposal, bool, error) {
	base, err := privateChildDir(r.dir, "disposals", false)
	if errors.Is(err, os.ErrNotExist) {
		return Disposal{}, false, nil
	}
	if err != nil {
		return Disposal{}, false, err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return Disposal{}, false, err
	}
	if len(entries) > 4096 {
		return Disposal{}, false, errors.New("disposal inventory limit exceeded")
	}
	var pending Disposal
	for _, e := range entries {
		f, err := r.disposalDir(e.Name())
		if err != nil {
			return pending, false, err
		}
		d, err := readRecord[Disposal](f, "intent.json")
		if err != nil {
			f.Close()
			return pending, false, err
		}
		if d.Version != protocolVersion || d.ID != e.Name() || d.Generation != r.Identity.Generation {
			f.Close()
			return pending, false, errors.New("invalid disposal intent")
		}
		_, err = readRecord[removedRecord](f, "removed.json")
		if errors.Is(err, os.ErrNotExist) {
			_, err = readRecord[removalAuthorization](f, "removal-authorized.json")
			if err == nil {
				if pending.ID != "" {
					f.Close()
					return pending, false, errors.New("multiple pending removal authorizations")
				}
				pending = d
			}
			// An intent without authorization has not relocated the clone. Resource
			// recovery governs whether it can be retried with a new disposal intent.
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		f.Close()
		if err != nil {
			return pending, false, err
		}
	}
	return pending, pending.ID != "", nil
}

func (r *Registry) Removed() (bool, error) {
	base, err := privateChildDir(r.dir, "disposals", false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return false, err
	}
	if len(entries) > 4096 {
		return false, errors.New("disposal inventory limit exceeded")
	}
	for _, e := range entries {
		f, err := r.disposalDir(e.Name())
		if err != nil {
			return false, err
		}
		done, err := readRecord[removedRecord](f, "removed.json")
		if err == nil {
			if done != (removedRecord{protocolVersion, r.Identity.Generation, e.Name()}) {
				f.Close()
				return false, errors.New("invalid removed receipt")
			}
			auth, aerr := readRecord[removalAuthorization](f, "removal-authorized.json")
			intent, ierr := readRecord[Disposal](f, "intent.json")
			plan, perr := readRecord[CleanupPlan](f, "teardown-complete.json")
			tree, terr := childDir(f, "tree", false)
			if tree != nil {
				tree.Close()
			}
			f.Close()
			if err := errors.Join(aerr, ierr, perr); err != nil {
				return false, err
			}
			if auth.Identity != r.Identity || !reflect.DeepEqual(auth.Disposal, intent) || intent.ID != e.Name() || intent.Generation != r.Identity.Generation {
				return false, errors.New("removed receipt lacks matching original authority")
			}
			if !errors.Is(terr, os.ErrNotExist) {
				return false, errors.New("removed receipt contradicts surviving private tree")
			}
			if err := r.checkRemovalTeardown(plan); err != nil {
				return false, err
			}
			return true, nil
		}
		f.Close()
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
