// Package vfs is the file layer of tacdb (SPEC §8.3, §20.9). An FS is
// confined to the directory of one database; nothing above this package
// calls the operating system's file functions.
//
// Two implementations exist: OpenDir, backed by the operating system and
// confined with os.Root, and MemFS, an in-memory file system that keeps a
// durable image apart from the current one and injects faults.
package vfs

import (
	"regexp"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
)

// FS is a directory holding one database. Names are relative, use "/" as
// separator and have at most two components (see ValidName).
type FS interface {
	// Create creates a new file exclusively; it fails with
	// DESTINATION_EXISTS if the name exists.
	Create(name string) (File, error)
	// Open opens an existing regular file for reading and writing.
	Open(name string) (File, error)
	// OpenReadOnly opens an existing regular file for reading.
	OpenReadOnly(name string) (File, error)
	// Mkdir creates a directory exclusively.
	Mkdir(name string) error
	// Remove removes a file or an empty directory.
	Remove(name string) error
	// Rename atomically replaces newName with oldName; both must be in the
	// same directory.
	Rename(oldName, newName string) error
	// SyncDir makes the entries of a directory durable; "." is the
	// database directory.
	SyncDir(name string) error
	// List returns the sorted entry names of a directory.
	List(name string) ([]string, error)
	// Exists reports whether name exists.
	Exists(name string) (bool, error)
	// Close releases the directory.
	Close() error
}

// File is an open file. ReadAt and WriteAt either transfer every byte or
// return an error: a short transfer is an IO error.
type File interface {
	ReadAt(p []byte, off int64) error
	WriteAt(p []byte, off int64) error
	Size() (int64, error)
	Truncate(size int64) error
	// Preallocate reserves space up to size without changing Size. A
	// failure to reserve is not an error; the file simply grows on write.
	Preallocate(size int64) error
	Sync() error
	// SyncData makes the data durable; metadata that does not affect
	// reading the data back may stay behind.
	SyncData() error
	// Lock takes an advisory lock without waiting; it fails with LOCKED if
	// another holder conflicts.
	Lock(mode LockMode) error
	Unlock() error
	// ID returns the identity of the underlying file.
	ID() (FileID, error)
	Close() error
}

// LockMode is the kind of an advisory lock.
type LockMode int

// Lock modes.
const (
	LockShared LockMode = iota + 1
	LockExclusive
)

// FileID identifies a file independently of its name: device and inode on
// Unix, volume serial number and file index on Windows.
type FileID struct {
	Device, Inode uint64
}

var componentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

var reservedWindowsNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidName reports whether name is a portable database name: one or two
// components separated by "/", each starting with a letter or digit and
// made of letters, digits, '.', '_' and '-', not ending with '.', and not a
// reserved Windows device name. Anything else is UNSAFE_PATH (SPEC §20.9).
func ValidName(name string) error {
	if name == "." {
		return nil
	}
	parts := strings.Split(name, "/")
	if len(parts) > 2 {
		return unsafePath(name, "more than two components")
	}
	for _, p := range parts {
		if !componentRE.MatchString(p) || strings.HasSuffix(p, ".") {
			return unsafePath(name, "invalid component")
		}
		base, _, _ := strings.Cut(p, ".")
		if reservedWindowsNames[strings.ToUpper(base)] {
			return unsafePath(name, "reserved device name")
		}
	}
	return nil
}

func unsafePath(name, why string) error {
	return errs.Newf(errs.CodeUnsafePath, "vfs.ValidName", "%q: %s", name, why)
}

// dirOf returns the directory component of a valid name, or ".".
func dirOf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}
	return "."
}
