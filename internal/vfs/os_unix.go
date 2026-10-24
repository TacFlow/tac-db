//go:build linux || darwin

package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/TacFlow/tacdb/internal/errs"
)

func fileID(f *os.File) (FileID, error) {
	st, err := statT(f)
	if err != nil {
		return FileID{}, err
	}
	return FileID{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, nil
}

func statT(f *os.File) (*syscall.Stat_t, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, osErr("vfs.stat", f.Name(), err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errs.Newf(errs.CodeIO, "vfs.stat", "%q: no stat data", f.Name())
	}
	return st, nil
}

// checkObject applies SPEC §20.9 to an open file or directory: owner and
// mode bits here, extended ACLs in checkACL.
func checkObject(f *os.File, kind objectKind) error {
	const op = "vfs.checkPermissions"
	st, err := statT(f)
	if err != nil {
		return err
	}
	euid := uint32(os.Geteuid())
	perm := os.FileMode(st.Mode).Perm()
	switch kind {
	case kindCallerDir:
		if st.Uid != euid && st.Uid != 0 {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q is owned by uid %d", f.Name(), st.Uid)
		}
		if perm&0o022 != 0 {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q is writable by group or others (%v)", f.Name(), perm)
		}
	default:
		if st.Uid != euid {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q is owned by uid %d", f.Name(), st.Uid)
		}
		if perm&0o077 != 0 {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q is accessible by group or others (%v)", f.Name(), perm)
		}
	}
	return checkACL(f)
}

// secureCreated is a no-op on Unix: Create and Mkdir already pass 0600 and
// 0700, and the umask can only remove bits. checkObject verifies.
func secureCreated(*os.File, string, objectKind) error { return nil }

func renameIn(root *os.Root, _, oldName, newName string) error {
	return root.Rename(filepath.FromSlash(oldName), filepath.FromSlash(newName))
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// retryEINTR repeats a raw system call interrupted by a signal; the os
// package already does this for its own calls.
func retryEINTR(fn func() error) error {
	for {
		if err := fn(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func locked(name string) error {
	return errs.Newf(errs.CodeLocked, "vfs.Lock", "%q is locked by another process", name)
}
