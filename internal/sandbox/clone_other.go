//go:build !darwin

package sandbox

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Clone is the non-Darwin fallback: a straightforward recursive copy
// from src to dst. Semantically equivalent to the APFS path for the
// file-tree contents moe cares about (regular files, directories,
// symlinks). No data is shared on disk, so this is slower than the
// Darwin path — but Linux is a test/CI target, not the primary dev
// platform, so the cost is acceptable.
//
// Special files (devices, sockets, fifos) are not handled; a git
// working tree should never contain them.
func Clone(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("sandbox: clone destination %s already exists", dst)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		default:
			return copyRegularFile(path, target, info.Mode().Perm())
		}
	})
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
