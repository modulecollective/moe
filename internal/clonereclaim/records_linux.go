//go:build linux

package clonereclaim

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"github.com/modulecollective/moe/internal/runowner"
)

const protocolVersion = 1
const identityMarker = "clone-identity.json"

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Identity is one physical clone, never just a reusable pathname. The external
// enrollment and clone-local marker must agree before any lifecycle mutation.
type Identity struct {
	Version    int    `json:"version"`
	Root       string `json:"root"`
	Project    string `json:"project"`
	Run        string `json:"run"`
	Path       string `json:"path"`
	Generation string `json:"generation"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	Mount      string `json:"mount"`
	Namespace  string `json:"mount_namespace"`
}

// Registry holds the pinned generation directory outside the disposable tree.
// Close it after use. Writes still require an explicit live run-owner handle.
type Registry struct {
	Identity Identity
	dir      *os.File
}

func (r *Registry) Close() error { return r.dir.Close() }

func registryPath(root, project, run string) string {
	return filepath.Join(root, ".moe", "clone-reclamation", project, run)
}

// Enroll creates an immutable generation at the guarded fresh-clone boundary,
// or after interactive host recovery has confirmed that legacy use stopped.
// Calling this is an enrollment act; ordinary inspection never auto-adopts.
func Enroll(h *runowner.Handle) (*Registry, error) {
	identity, tree, err := physicalIdentity(h, true)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	local, err := childDir(tree, ".moe", true)
	if err != nil {
		return nil, err
	}
	defer local.Close()
	if _, err := readRecord[Identity](local, identityMarker); err == nil {
		return nil, errors.New("clone already has an identity; validate it instead of re-enrolling")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	base, err := openRegistry(h, true)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	entries, err := base.ReadDir(1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) != 0 {
		return nil, errors.New("missing clone marker with existing recovery evidence; host recovery required")
	}
	identity.Generation, err = randomID()
	if err != nil {
		return nil, err
	}
	generation, err := privateChildDir(base, identity.Generation, true)
	if err != nil {
		return nil, err
	}
	if err := publishRecord(generation, "identity.json", identity); err != nil {
		generation.Close()
		return nil, err
	}
	// Publish external authority first. A crash before the marker is a held
	// partial enrollment, never an enrolled clone inferred from its name.
	if err := publishRecord(local, identityMarker, identity); err != nil {
		generation.Close()
		return nil, err
	}
	return &Registry{Identity: identity, dir: generation}, nil
}

// LoadRegistry refuses legacy, replaced and foreign-namespace trees. It reads
// only; even a free run guard does not authorize recovery of missing evidence.
func LoadRegistry(h *runowner.Handle) (*Registry, error) {
	actual, tree, err := physicalIdentity(h, false)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	local, err := childDir(tree, ".moe", false)
	if err != nil {
		return nil, err
	}
	defer local.Close()
	marker, err := readRecord[Identity](local, identityMarker)
	if err != nil {
		return nil, err
	}
	if !validRandomID(marker.Generation) {
		return nil, errors.New("invalid clone generation")
	}
	actual.Generation = marker.Generation
	if marker != actual {
		return nil, errors.New("clone identity or mount namespace changed; host recovery required")
	}
	base, err := openRegistry(h, false)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	generation, err := privateChildDir(base, marker.Generation, false)
	if err != nil {
		return nil, err
	}
	recorded, err := readRecord[Identity](generation, "identity.json")
	if err != nil || recorded != marker {
		generation.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("external clone identity disagrees with marker")
	}
	return &Registry{Identity: marker, dir: generation}, nil
}

func (r *Registry) validateOwner(h *runowner.Handle) error {
	if !h.Owns(r.Identity.Root, r.Identity.Project, r.Identity.Run) {
		return errors.New("clone reclamation requires held run ownership")
	}
	current, err := LoadRegistry(h)
	if err != nil {
		return err
	}
	defer current.Close()
	if current.Identity != r.Identity {
		return errors.New("clone generation changed")
	}
	return nil
}

func physicalIdentity(h *runowner.Handle, durable bool) (Identity, *os.File, error) {
	var id Identity
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return id, nil, errors.New("clone reclamation requires held run ownership")
	}
	if !filepath.IsAbs(h.Root) || filepath.Clean(h.Root) != h.Root || !namePattern.MatchString(h.Project) || !namePattern.MatchString(h.Run) {
		return id, nil, errors.New("invalid canonical run identity")
	}
	root, err := openAbsoluteDir(h.Root)
	if err != nil {
		return id, nil, err
	}
	defer root.Close()
	mount, err := mountID(root)
	if err != nil {
		return id, nil, err
	}
	parent := root
	var owned []*os.File
	defer func() {
		for _, f := range owned {
			f.Close()
		}
	}()
	for _, name := range []string{".moe", "clones", h.Project, h.Run} {
		next, err := childDir(parent, name, false)
		if err != nil {
			return id, nil, err
		}
		owned = append(owned, next)
		m, err := mountID(next)
		if err != nil {
			return id, nil, err
		}
		if m != mount {
			return id, nil, errors.New("clone path crosses a mount boundary")
		}
		// Fresh clone/owner creation predates this protocol and does not
		// promise parent fsync. Enrollment must persist those links too.
		// Loading an identity remains strictly read-only.
		if durable {
			if err := syncRecordFile(parent); err != nil {
				return id, nil, err
			}
			if err := syncRecordFile(next); err != nil {
				return id, nil, err
			}
		}
		parent = next
	}
	gitdir, err := childDir(parent, ".git", false)
	if err != nil {
		return id, nil, fmt.Errorf("clone must have an ordinary non-symlink Git directory: %w", err)
	}
	gitmount, err := mountID(gitdir)
	gitdir.Close()
	if err != nil {
		return id, nil, err
	}
	if gitmount != mount {
		return id, nil, errors.New("Git directory crosses a mount boundary")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(parent.Fd()), &stat); err != nil {
		return id, nil, err
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return id, nil, errors.New("clone root belongs to another uid")
	}
	namespace, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return id, nil, fmt.Errorf("cannot establish host mount namespace: %w", err)
	}
	if namespace == "" {
		return id, nil, errors.New("cannot establish host mount namespace")
	}
	fd, err := syscall.Dup(int(parent.Fd()))
	if err != nil {
		return id, nil, err
	}
	syscall.CloseOnExec(fd)
	id = Identity{Version: protocolVersion, Root: h.Root, Project: h.Project, Run: h.Run, Path: filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run), Device: uint64(stat.Dev), Inode: stat.Ino, Mount: mount, Namespace: namespace}
	return id, os.NewFile(uintptr(fd), id.Path), nil
}

func openRegistry(h *runowner.Handle, create bool) (*os.File, error) {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return nil, errors.New("clone reclamation requires held run ownership")
	}
	root, err := openAbsoluteDir(h.Root)
	if err != nil {
		return nil, err
	}
	parent := root
	for i, name := range []string{".moe", "clone-reclamation", h.Project, h.Run} {
		next, err := childDir(parent, name, create)
		parent.Close()
		if err != nil {
			return nil, err
		}
		if i > 0 {
			if err := privateDirectory(next); err != nil {
				next.Close()
				return nil, err
			}
		}
		parent = next
	}
	return parent, nil
}

func privateDirectory(f *os.File) error {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o700 {
		return errors.New("reclamation record directory must be private and owned by this uid")
	}
	return nil
}

func privateChildDir(parent *os.File, name string, create bool) (*os.File, error) {
	f, err := childDir(parent, name, create)
	if err != nil {
		return nil, err
	}
	if err := privateDirectory(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func openAbsoluteDir(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("expected canonical absolute directory")
	}
	f, err := os.Open("/")
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := childDir(f, part, false)
		f.Close()
		if err != nil {
			return nil, err
		}
		f = next
	}
	return f, nil
}

func childDir(parent *os.File, name string, create bool) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return nil, errors.New("invalid directory component")
	}
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) && create {
		if err := syscall.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
			return nil, err
		}
		fd, err = syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(parent.Name(), name))
	if create {
		if err := syncRecordFile(parent); err != nil {
			f.Close()
			return nil, err
		}
		if err := syncRecordFile(f); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func mountID(f *os.File) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", f.Fd()))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if after, ok := strings.CutPrefix(line, "mnt_id:"); ok && strings.TrimSpace(after) != "" {
			return strings.TrimSpace(after), nil
		}
	}
	return "", errors.New("mount identity unavailable")
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func validRandomID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

// syncRecordFile is injected only by serial durability fault tests.
var syncRecordFile = (*os.File).Sync

// publishRecord writes and syncs a private temp, links create-once, then syncs
// the parent. Readers never see partial JSON. EEXIST never overwrites evidence.
func publishRecord(parent *os.File, name string, value any) (err error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("invalid record filename")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("record size limit exceeded")
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := ".publish-" + id
	fd, err := syscall.Openat(int(parent.Fd()), tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { f.Close(); syscall.Unlinkat(int(parent.Fd()), tmp) }()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := syncRecordFile(f); err != nil {
		return err
	}
	oldname, err := syscall.BytePtrFromString(tmp)
	if err != nil {
		return err
	}
	newname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, parent.Fd(), uintptr(unsafe.Pointer(oldname)), parent.Fd(), uintptr(unsafe.Pointer(newname)), 0, 0)
	if errno != 0 {
		return errno
	}
	if err := syncRecordFile(parent); err != nil {
		return err
	}
	if err := syscall.Unlinkat(int(parent.Fd()), tmp); err != nil {
		return err
	}
	return syncRecordFile(parent)
}

func readRecord[T any](parent *os.File, name string) (T, error) {
	var out T
	fd, err := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return out, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return out, errors.New("invalid record file")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return out, err
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o600 {
		return out, errors.New("reclamation record file must be private and owned by this uid")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return out, err
	}
	if len(b) > 1<<20 {
		return out, errors.New("record size limit exceeded")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return out, errors.New("trailing record data")
	}
	return out, nil
}
