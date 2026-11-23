//go:build linux

package clonereclaim

import (
	"errors"
	"github.com/modulecollective/moe/internal/run"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/modulecollective/moe/internal/runowner"
)

// Named shell receipts own only this run's shell lifetime. They never enroll
// the named workspace for resource reclamation or deletion.
func namedShellBase(h *runowner.Handle, create bool) (*os.File, error) {
	if h == nil || !h.Owns(h.Root, h.Project, h.Run) {
		return nil, errors.New("named shell requires held run ownership")
	}
	root, err := openAbsoluteDir(h.Root)
	if err != nil {
		return nil, err
	}
	parent := root
	for _, name := range []string{".moe", "run-shells", h.Project, h.Run} {
		next, err := childDir(parent, name, create)
		parent.Close()
		if err != nil {
			return nil, err
		}
		parent = next
	}
	if err := privateDirectory(parent); err != nil {
		parent.Close()
		return nil, err
	}
	return parent, nil
}

func namedShellIdentity(h *runowner.Handle, path string) (Identity, error) {
	id := Identity{Version: protocolVersion, Root: h.Root, Project: h.Project, Run: h.Run, Path: path}
	if filepath.Dir(path) != filepath.Join(h.Root, ".moe", "named", h.Project) || !run.ValidID(filepath.Base(path)) {
		return id, errors.New("invalid named shell target")
	}
	root, err := openAbsoluteDir(h.Root)
	if err != nil {
		return id, err
	}
	defer root.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(root.Fd()), &st); err != nil {
		return id, err
	}
	id.RootDevice, id.RootInode = uint64(st.Dev), st.Ino
	tree, err := openAbsoluteDir(path)
	if err != nil {
		return id, err
	}
	defer tree.Close()
	if err := syscall.Fstat(int(tree.Fd()), &st); err != nil {
		return id, err
	}
	id.Device, id.Inode = uint64(st.Dev), st.Ino
	gitdir, err := childDir(tree, ".git", false)
	if err != nil {
		return id, err
	}
	gitdir.Close()
	id.Mount, err = mountID(tree)
	if err != nil {
		return id, err
	}
	id.Namespace, err = os.Readlink("/proc/self/ns/mnt")
	return id, err
}

func BeginNamedShell(h *runowner.Handle, path string) (*Registry, error) {
	if err := CheckNamedShells(h); err != nil {
		return nil, err
	}
	id, err := namedShellIdentity(h, path)
	if err != nil {
		return nil, err
	}
	id.Generation, err = randomID()
	if err != nil {
		return nil, err
	}
	base, err := namedShellBase(h, true)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	f, err := privateChildDir(base, id.Generation, true)
	if err != nil {
		return nil, err
	}
	if err := publishRecord(f, "identity.json", id); err != nil {
		f.Close()
		return nil, err
	}
	return &Registry{Identity: id, dir: f, shellOnly: true}, nil
}

func (r *Registry) validateNamedShell(h *runowner.Handle) error {
	actual, err := namedShellIdentity(h, r.Identity.Path)
	if err != nil {
		return err
	}
	actual.Generation = r.Identity.Generation
	if actual != r.Identity {
		return errors.New("named shell target changed")
	}
	return nil
}

func loadShellRegistry(h *runowner.Handle, id Identity) (*Registry, error) {
	if id.Path == filepath.Join(h.Root, ".moe", "clones", h.Project, h.Run) {
		return LoadRegistry(h)
	}
	base, err := namedShellBase(h, false)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	f, err := privateChildDir(base, id.Generation, false)
	if err != nil {
		return nil, err
	}
	saved, err := readRecord[Identity](f, "identity.json")
	if err != nil || saved != id {
		f.Close()
		return nil, errors.New("named shell lifetime identity changed")
	}
	r := &Registry{Identity: id, dir: f, shellOnly: true}
	if err := r.validateNamedShell(h); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func CheckNamedShells(h *runowner.Handle) error {
	base, err := namedShellBase(h, false)
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
		return errors.New("named shell inventory limit exceeded")
	}
	for _, e := range entries {
		if !validRandomID(e.Name()) {
			return errors.New("unknown named shell lifetime")
		}
		f, err := privateChildDir(base, e.Name(), false)
		if err != nil {
			return err
		}
		id, err := readRecord[Identity](f, "identity.json")
		if err != nil {
			f.Close()
			return err
		}
		if id.Version != protocolVersion || id.Generation != e.Name() || id.Root != h.Root || id.Project != h.Project || id.Run != h.Run {
			f.Close()
			return errors.New("invalid named shell authority")
		}
		r := &Registry{Identity: id, dir: f, shellOnly: true}
		err = r.checkShellRecords()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// NamedShellRecovery opens only existing lifetime receipts. A stopped-use
// confirmation may discharge their starts without enrolling the named tree.
func NamedShellRecovery(h *runowner.Handle, path string) ([]*Registry, error) {
	base, err := namedShellBase(h, false)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	entries, err := base.ReadDir(4097)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 4096 {
		return nil, errors.New("named shell recovery limit exceeded")
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
			return fail(errors.New("unknown named shell identity"))
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
		r := &Registry{Identity: id, dir: f, shellOnly: true}
		if id.Version != protocolVersion || id.Generation != e.Name() || id.Root != h.Root || id.Project != h.Project || id.Run != h.Run || id.Path != path {
			f.Close()
			return fail(errors.New("named shell receipt does not match this run binding"))
		}
		if err := r.validateNamedShell(h); err != nil {
			f.Close()
			return fail(err)
		}
		out = append(out, r)
	}
	return out, nil
}
