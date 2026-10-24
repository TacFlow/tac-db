package vfs

import (
	"encoding/binary"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/TacFlow/tacdb/internal/errs"
)

// lockFile uses flock, whose locks belong to the open file, unlike POSIX
// record locks (SPEC §8.3).
func lockFile(f *os.File, name string, mode LockMode) error {
	how := unix.LOCK_SH
	if mode == LockExclusive {
		how = unix.LOCK_EX
	}
	err := retryEINTR(func() error { return unix.Flock(int(f.Fd()), how|unix.LOCK_NB) })
	if errors.Is(err, unix.EWOULDBLOCK) {
		return locked(name)
	}
	return osErr("vfs.Lock", name, err)
}

func unlockFile(f *os.File) error {
	return retryEINTR(func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) })
}

// syncData uses F_FULLFSYNC through os.File.Sync: plain fsync does not
// reach the medium on macOS (SPEC §8.3).
func syncData(f *os.File) error { return f.Sync() }

func preallocate(f *os.File, size int64) {
	fi, err := f.Stat()
	if err != nil || size <= fi.Size() {
		return
	}
	st := unix.Fstore_t{Flags: unix.F_ALLOCATECONTIG | unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Length: size - fi.Size()}
	if retryEINTR(func() error { return unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st) }) != nil {
		st.Flags = unix.F_ALLOCATEALL
		retryEINTR(func() error { return unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st) })
	}
}

// syncDir uses F_FULLFSYNC and falls back to fsync when the file system
// does not support it on directories.
func syncDir(d *os.File) error {
	if err := d.Sync(); err != nil {
		return retryEINTR(func() error { return unix.Fsync(int(d.Fd())) })
	}
	return nil
}

// attrList mirrors struct attrlist of <sys/attr.h>.
type attrList struct {
	bitmapCount, reserved                            uint16
	commonAttr, volAttr, dirAttr, fileAttr, forkAttr uint32
}

// Layout of struct kauth_filesec and struct kauth_ace of <sys/kauth.h>.
const (
	kauthFilesecMagic = 0x012cc16d
	filesecHeader     = 44 // magic, owner and group GUIDs, entry count, flags
	aceSize           = 24 // applicable GUID, flags, rights
	acePermit         = 0x1
	aceKindMask       = 0xf
	// Rights that let a principal change a file or the entries of a
	// directory: write data/add file, delete, append/add subdirectory,
	// delete child, write attributes, write extended attributes, write
	// security, take ownership, generic all and generic write.
	aceWriteRights = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
)

// checkACL rejects an object whose extended ACL has an allow entry with
// write rights (SPEC §20.9, R13-21). Without cgo the owner's GUID cannot be
// mapped to a uid, so every allow entry counts, including inheritable
// ones; deny entries never count.
func checkACL(f *os.File) error {
	const op = "vfs.checkACL"
	buf := make([]byte, 4096)
	for {
		al := attrList{bitmapCount: 5, commonAttr: unix.ATTR_CMN_EXTENDED_SECURITY}
		err := retryEINTR(func() error {
			_, _, e := unix.Syscall6(unix.SYS_FGETATTRLIST, f.Fd(), uintptr(unsafe.Pointer(&al)),
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
			if e != 0 {
				return e
			}
			return nil
		})
		if err != nil {
			return osErr(op, f.Name(), err)
		}
		total := int(binary.NativeEndian.Uint32(buf))
		if total <= len(buf) {
			break
		}
		if total > 1<<20 {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q: ACL too large to inspect", f.Name())
		}
		buf = make([]byte, total)
	}
	refOff := 4 + int(int32(binary.NativeEndian.Uint32(buf[4:])))
	n := int(binary.NativeEndian.Uint32(buf[8:]))
	if n == 0 {
		return nil
	}
	if refOff < 12 || refOff+n > len(buf) || n < filesecHeader {
		return errs.Newf(errs.CodeInsecurePermissions, op, "%q: unrecognized ACL format", f.Name())
	}
	sec := buf[refOff : refOff+n]
	count := int(binary.NativeEndian.Uint32(sec[36:]))
	if binary.NativeEndian.Uint32(sec) != kauthFilesecMagic || filesecHeader+count*aceSize > len(sec) {
		return errs.Newf(errs.CodeInsecurePermissions, op, "%q: unrecognized ACL format", f.Name())
	}
	for i := 0; i < count; i++ {
		ace := sec[filesecHeader+i*aceSize:]
		flags, rights := binary.NativeEndian.Uint32(ace[16:]), binary.NativeEndian.Uint32(ace[20:])
		if flags&aceKindMask == acePermit && rights&aceWriteRights != 0 {
			return errs.Newf(errs.CodeInsecurePermissions, op, "%q: ACL entry %d grants write access", f.Name(), i)
		}
	}
	return nil
}
