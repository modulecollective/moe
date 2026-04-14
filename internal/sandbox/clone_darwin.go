//go:build darwin

package sandbox

import (
	"syscall"
	"unsafe"
)

// clonefileat is Darwin BSD syscall 462. The libc wrapper `clonefile(src,
// dst, flags)` is a thin shim that calls clonefileat with AT_FDCWD for
// both dirfds; we call the underlying syscall directly so the package
// stays dependency-free.
//
// See xnu's bsd/kern/syscalls.master. The number is stable ABI — it has
// not moved across macOS releases.
const (
	sysClonefileat = 462
	atFDCWD        = -2 // <fcntl.h> AT_FDCWD on Darwin
)

// Clone makes an APFS copy-on-write clone of src at dst. dst must not
// exist. For directories the clone is recursive. src and dst must live
// on the same APFS volume.
//
// The call is O(metadata): data blocks are shared with the source until
// either side writes. That's what makes per-request clones cheap enough
// to create unconditionally on every `moe work` invocation.
func Clone(src, dst string) error {
	s, err := syscall.BytePtrFromString(src)
	if err != nil {
		return err
	}
	d, err := syscall.BytePtrFromString(dst)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(sysClonefileat,
		uintptr(atFDCWD),
		uintptr(unsafe.Pointer(s)),
		uintptr(atFDCWD),
		uintptr(unsafe.Pointer(d)),
		0, // flags: default (follow symlinks, copy ownership)
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
