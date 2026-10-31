package vfs

import (
	"errors"
	"sync"

	"github.com/TacFlow/tacdb/internal/errs"
)

// registry records the files whose database locks this process holds, by
// identity, so that a second handle in the same process fails with LOCKED
// whatever name it used (SPEC §8.3).
var registry = struct {
	sync.Mutex
	held map[FileID]bool
}{held: make(map[FileID]bool)}

func register(id FileID) error {
	registry.Lock()
	defer registry.Unlock()
	if registry.held[id] {
		return errs.New(errs.CodeLocked, "vfs.LockDatabase", "database already open in this process")
	}
	registry.held[id] = true
	return nil
}

func unregister(id FileID) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.held, id)
}

// DatabaseLock holds the locks of one database (SPEC §8.3, R13-04):
//
//  1. the lock file "<base>.lock", whose name never changes and which is
//     never removed;
//  2. the main file "<base>", so that a hard link under another name,
//     which has a different lock file, still conflicts;
//  3. an entry in the process registry for each of them.
type DatabaseLock struct {
	mode    LockMode
	files   []File
	ids     []FileID
	release sync.Once
}

// LockDatabase takes the database lock of base in fs, creating the lock
// file if needed. If the main file exists, it is locked too; a database
// that does not exist yet attaches its main file later with AttachMainFile.
func LockDatabase(fs FS, base string, mode LockMode) (*DatabaseLock, error) {
	if err := ValidName(base); err != nil {
		return nil, err
	}
	l := &DatabaseLock{mode: mode}
	lockFile, err := openOrCreate(fs, base+".lock")
	if err != nil {
		return nil, err
	}
	if err := l.add(lockFile); err != nil {
		l.Release()
		return nil, err
	}
	exists, err := fs.Exists(base)
	if err != nil {
		l.Release()
		return nil, err
	}
	if exists {
		open := fs.Open
		if mode == LockShared {
			open = fs.OpenReadOnly
		}
		main, err := open(base)
		if err != nil {
			l.Release()
			return nil, err
		}
		if err := l.add(main); err != nil {
			l.Release()
			return nil, err
		}
	}
	return l, nil
}

// AttachMainFile locks the main file of a database created after
// LockDatabase. The lock keeps its own handle; the caller keeps f.
func (l *DatabaseLock) AttachMainFile(fs FS, base string) error {
	open := fs.Open
	if l.mode == LockShared {
		open = fs.OpenReadOnly
	}
	main, err := open(base)
	if err != nil {
		return err
	}
	return l.add(main)
}

func (l *DatabaseLock) add(f File) error {
	id, err := f.ID()
	if err != nil {
		f.Close()
		return err
	}
	if err := register(id); err != nil {
		f.Close()
		return err
	}
	if err := f.Lock(l.mode); err != nil {
		unregister(id)
		f.Close()
		return err
	}
	l.files = append(l.files, f)
	l.ids = append(l.ids, id)
	return nil
}

// Release drops every lock. It is safe to call more than once.
func (l *DatabaseLock) Release() error {
	var first error
	l.release.Do(func() {
		for i, f := range l.files {
			if err := f.Unlock(); err != nil && first == nil {
				first = err
			}
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
			unregister(l.ids[i])
		}
	})
	return first
}

// openOrCreate opens name, creating it if it does not exist. Two processes
// racing to create it both end up opening the same file.
func openOrCreate(fs FS, name string) (File, error) {
	f, err := fs.Open(name)
	if err == nil || !errors.Is(err, errs.Sentinel(errs.CodeNotFound)) {
		return f, err
	}
	f, err = fs.Create(name)
	if errors.Is(err, errs.Sentinel(errs.CodeDestinationExists)) {
		return fs.Open(name)
	}
	return f, err
}
