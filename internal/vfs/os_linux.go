package vfs

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile uses open file description locks, which belong to the open
// file and survive the closing of other descriptors of the same file,
// unlike POSIX record locks (SPEC §8.3).
func lockFile(f *os.File, name string, mode LockMode) error {
	lk := unix.Flock_t{Type: unix.F_RDLCK, Whence: io.SeekStart}
	if mode == LockExclusive {
		lk.Type = unix.F_WRLCK
	}
	err := retryEINTR(func() error { return unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lk) })
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
		return locked(name)
	}
	return osErr("vfs.Lock", name, err)
}

func unlockFile(f *os.File) error {
	lk := unix.Flock_t{Type: unix.F_UNLCK, Whence: io.SeekStart}
	return retryEINTR(func() error { return unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lk) })
}

func syncData(f *os.File) error {
	return retryEINTR(func() error { return unix.Fdatasync(int(f.Fd())) })
}

func preallocate(f *os.File, size int64) {
	retryEINTR(func() error { return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size) })
}

func syncDir(d *os.File) error { return d.Sync() }

// checkACL is a no-op on Linux: the mask entry of a POSIX ACL is the group
// class of the mode bits, which checkObject already bounds.
func checkACL(*os.File) error { return nil }
