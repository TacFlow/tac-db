package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/TacFlow/tacdb/internal/errs"
)

// lockOffsetHigh places the lock at byte 2^62, far beyond any real file
// size: LockFileEx locks are mandatory and would otherwise block I/O
// (SPEC §8.3).
const lockOffsetHigh = 1 << 30

func lockFile(f *os.File, name string, mode LockMode) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == LockExclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errs.Newf(errs.CodeLocked, "vfs.Lock", "%q is locked by another process", name)
	}
	return osErr("vfs.Lock", name, err)
}

func unlockFile(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

func fileID(f *os.File) (FileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return FileID{}, osErr("vfs.ID", f.Name(), err)
	}
	return FileID{
		Device: uint64(info.VolumeSerialNumber),
		Inode:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

// syncData uses FlushFileBuffers, the only flush Windows offers.
func syncData(f *os.File) error { return f.Sync() }

// syncDir is a documented no-op: NTFS journals directory changes, and
// renames use MOVEFILE_WRITE_THROUGH (SPEC §8.3).
func syncDir(*os.File) error { return nil }

type fileAllocationInfo struct{ AllocationSize int64 }

const fileAllocationInfoClass = 5 // FileAllocationInfo

func preallocate(f *os.File, size int64) {
	// An allocation size below the end of file would truncate the file.
	fi, err := f.Stat()
	if err != nil || size <= fi.Size() {
		return
	}
	info := fileAllocationInfo{AllocationSize: size}
	windows.SetFileInformationByHandle(windows.Handle(f.Fd()), fileAllocationInfoClass,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

func renameIn(_ *os.Root, dir, oldName, newName string) error {
	from, err := windows.UTF16PtrFromString(filepath.Join(dir, filepath.FromSlash(oldName)))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Join(dir, filepath.FromSlash(newName)))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func isNoSpace(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

// Access rights that let a principal change a file or the entries of a
// directory.
const writeMask = 0x2 | 0x4 | 0x10 | 0x40 | 0x100 | // write data/add file, append/add subdirectory, write EA, delete child, write attributes
	0x10000 | 0x40000 | 0x80000 | // delete, write DAC, write owner
	0x40000000 | 0x10000000 // generic write, generic all

type trustedSIDs struct {
	user, system, admins, creatorOwner *windows.SID
	err                                error
}

var trusted = sync.OnceValue(func() trustedSIDs {
	var t trustedSIDs
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.err = err
		return t
	}
	t.user = u.User.Sid
	if t.system, err = windows.CreateWellKnownSid(windows.WinLocalSystemSid); err != nil {
		t.err = err
	}
	if t.admins, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid); err != nil {
		t.err = err
	}
	if t.creatorOwner, err = windows.CreateWellKnownSid(windows.WinCreatorOwnerSid); err != nil {
		t.err = err
	}
	return t
})

func (t trustedSIDs) owner(sid *windows.SID) bool {
	return sid.Equals(t.user) || sid.Equals(t.system) || sid.Equals(t.admins)
}

// checkObject applies SPEC §20.9 with the security descriptor of the open
// handle: the owner must be the user, SYSTEM or Administrators; objects
// created by tacdb grant access to nobody else, and the caller's
// directory grants write access to nobody else.
func checkObject(f *os.File, kind objectKind) error {
	const op = "vfs.checkPermissions"
	t := trusted()
	if t.err != nil {
		return errs.Wrap(errs.CodeIO, op, t.err, "reading the process token")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return osErr(op, f.Name(), err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return osErr(op, f.Name(), err)
	}
	if !t.owner(owner) {
		return errs.Newf(errs.CodeInsecurePermissions, op, "%q is owned by %s", f.Name(), owner)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return osErr(op, f.Name(), err)
	}
	if dacl == nil {
		return errs.Newf(errs.CodeInsecurePermissions, op, "%q has no DACL: everyone has full access", f.Name())
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return osErr(op, f.Name(), err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if t.owner(sid) || sid.Equals(t.creatorOwner) {
			continue
		}
		if kind == kindCallerDir && ace.Mask&writeMask == 0 {
			continue
		}
		return errs.Newf(errs.CodeInsecurePermissions, op, "%q grants access to %s", f.Name(), sid)
	}
	return nil
}

// secureCreated replaces the inherited DACL of an object just created by
// tacdb with a protected one that grants full control to the user, SYSTEM
// and Administrators only. It goes by path because the handle lacks
// WRITE_DAC.
func secureCreated(_ *os.File, absPath string, kind objectKind) error {
	const op = "vfs.secureCreated"
	t := trusted()
	if t.err != nil {
		return errs.Wrap(errs.CodeIO, op, t.err, "reading the process token")
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if kind == kindDir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	var entries []windows.EXPLICIT_ACCESS
	for _, sid := range []*windows.SID{t.user, t.system, t.admins} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return osErr(op, absPath, err)
	}
	err = windows.SetNamedSecurityInfo(absPath, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
	return osErr(op, absPath, err)
}
