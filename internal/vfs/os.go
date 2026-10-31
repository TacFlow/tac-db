package vfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Options controls OpenDir.
type Options struct {
	// AllowSharedDirectory disables the owner, mode and ACL checks of SPEC
	// §20.9, for directories that are deliberately shared.
	AllowSharedDirectory bool
}

// objectKind tells checkObject which permission rule applies (SPEC §20.9).
type objectKind int

const (
	kindFile      objectKind = iota // file created by tacdb: 0600, our owner
	kindDir                         // directory created by tacdb: 0700, our owner
	kindCallerDir                   // the caller's database directory
)

// OpenDir opens the database directory at path. The path is resolved once,
// following symbolic links; from then on every operation is confined to
// the directory with os.Root and refuses entries that are not regular
// files or directories (SPEC §7.1, R13-03).
func OpenDir(path string, opts Options) (FS, error) {
	const op = "vfs.OpenDir"
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, op, err, "")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, osErr(op, path, err)
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, osErr(op, path, err)
	}
	fsys := &osFS{root: root, dir: resolved, opts: opts}
	if !opts.AllowSharedDirectory {
		d, err := root.Open(".")
		if err != nil {
			root.Close()
			return nil, osErr(op, path, err)
		}
		err = checkObject(d, kindCallerDir)
		d.Close()
		if err != nil {
			root.Close()
			return nil, err
		}
	}
	return fsys, nil
}

type osFS struct {
	root *os.Root
	dir  string // resolved absolute path of the directory
	opts Options
}

// stat returns the entry of a valid name without following a final
// symbolic link, refusing anything that is not a regular file or a
// directory.
func (o *osFS) stat(op, name string) (fs.FileInfo, error) {
	fi, err := o.root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, osErr(op, name, err)
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return nil, errs.Newf(errs.CodeUnsafePath, op, "%q is not a regular file or directory (%v)", name, fi.Mode().Type())
	}
	return fi, nil
}

// checkParent refuses a name whose directory component is not a real
// directory.
func (o *osFS) checkParent(op, name string) error {
	parent := dirOf(name)
	if parent == "." {
		return nil
	}
	fi, err := o.stat(op, parent)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errs.Newf(errs.CodeUnsafePath, op, "%q is not a directory", parent)
	}
	return nil
}

func (o *osFS) Create(name string) (File, error) {
	const op = "vfs.Create"
	if err := ValidName(name); err != nil {
		return nil, err
	}
	if err := o.checkParent(op, name); err != nil {
		return nil, err
	}
	f, err := o.root.OpenFile(filepath.FromSlash(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, osErr(op, name, err)
	}
	if err := o.harden(f, name, kindFile); err != nil {
		f.Close()
		o.root.Remove(filepath.FromSlash(name))
		return nil, err
	}
	return &osFile{f: f, name: name}, nil
}

// harden secures an object just created by tacdb and verifies the result,
// so that inherited ACLs cannot leave it open to others (SPEC §20.9).
func (o *osFS) harden(f *os.File, name string, kind objectKind) error {
	if o.opts.AllowSharedDirectory {
		return nil
	}
	if err := secureCreated(f, filepath.Join(o.dir, filepath.FromSlash(name)), kind); err != nil {
		return err
	}
	return checkObject(f, kind)
}

func (o *osFS) Open(name string) (File, error) { return o.open(name, os.O_RDWR) }

func (o *osFS) OpenReadOnly(name string) (File, error) { return o.open(name, os.O_RDONLY) }

func (o *osFS) open(name string, flag int) (File, error) {
	const op = "vfs.Open"
	if err := ValidName(name); err != nil {
		return nil, err
	}
	fi, err := o.stat(op, name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errs.Newf(errs.CodeUnsafePath, op, "%q is not a regular file", name)
	}
	f, err := o.root.OpenFile(filepath.FromSlash(name), flag, 0)
	if err != nil {
		return nil, osErr(op, name, err)
	}
	if err := o.sameAndSafe(f, fi, kindFile); err != nil {
		f.Close()
		return nil, err
	}
	return &osFile{f: f, name: name}, nil
}

// sameAndSafe checks that the opened object is the one Lstat saw (no swap
// in between) and that its permissions follow SPEC §20.9.
func (o *osFS) sameAndSafe(f *os.File, want fs.FileInfo, kind objectKind) error {
	got, err := f.Stat()
	if err != nil {
		return osErr("vfs.Open", f.Name(), err)
	}
	if !os.SameFile(got, want) {
		return errs.Newf(errs.CodeUnsafePath, "vfs.Open", "%q changed while being opened", f.Name())
	}
	if o.opts.AllowSharedDirectory {
		return nil
	}
	return checkObject(f, kind)
}

func (o *osFS) Mkdir(name string) error {
	const op = "vfs.Mkdir"
	if err := ValidName(name); err != nil {
		return err
	}
	if err := o.checkParent(op, name); err != nil {
		return err
	}
	if err := o.root.Mkdir(filepath.FromSlash(name), 0o700); err != nil {
		return osErr(op, name, err)
	}
	d, err := o.root.Open(filepath.FromSlash(name))
	if err != nil {
		return osErr(op, name, err)
	}
	defer d.Close()
	if err := o.harden(d, name, kindDir); err != nil {
		o.root.Remove(filepath.FromSlash(name))
		return err
	}
	return nil
}

func (o *osFS) Remove(name string) error {
	const op = "vfs.Remove"
	if err := ValidName(name); err != nil {
		return err
	}
	if _, err := o.stat(op, name); err != nil {
		return err
	}
	return osErr(op, name, o.root.Remove(filepath.FromSlash(name)))
}

func (o *osFS) Rename(oldName, newName string) error {
	const op = "vfs.Rename"
	if err := ValidName(oldName); err != nil {
		return err
	}
	if err := ValidName(newName); err != nil {
		return err
	}
	if dirOf(oldName) != dirOf(newName) {
		return errs.Newf(errs.CodeInvalidArgument, op, "%q and %q are in different directories", oldName, newName)
	}
	fi, err := o.stat(op, oldName)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errs.Newf(errs.CodeUnsafePath, op, "%q is not a regular file", oldName)
	}
	if fi, err := o.root.Lstat(filepath.FromSlash(newName)); err == nil && !fi.Mode().IsRegular() {
		return errs.Newf(errs.CodeUnsafePath, op, "%q is not a regular file", newName)
	}
	return osErr(op, oldName, renameIn(o.root, o.dir, oldName, newName))
}

func (o *osFS) SyncDir(name string) error {
	const op = "vfs.SyncDir"
	if err := ValidName(name); err != nil {
		return err
	}
	fi, err := o.stat(op, name)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errs.Newf(errs.CodeInvalidArgument, op, "%q is not a directory", name)
	}
	d, err := o.root.Open(filepath.FromSlash(name))
	if err != nil {
		return osErr(op, name, err)
	}
	defer d.Close()
	return osErr(op, name, syncDir(d))
}

func (o *osFS) List(name string) ([]string, error) {
	const op = "vfs.List"
	if err := ValidName(name); err != nil {
		return nil, err
	}
	fi, err := o.stat(op, name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errs.Newf(errs.CodeInvalidArgument, op, "%q is not a directory", name)
	}
	d, err := o.root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, osErr(op, name, err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, osErr(op, name, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	return names, nil
}

func (o *osFS) Exists(name string) (bool, error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	_, err := o.stat("vfs.Exists", name)
	if errs.CodeOf(err) == errs.CodeNotFound {
		return false, nil
	}
	return err == nil, err
}

func (o *osFS) Close() error { return osErr("vfs.Close", o.dir, o.root.Close()) }

type osFile struct {
	f    *os.File
	name string
}

func (f *osFile) ReadAt(p []byte, off int64) error {
	_, err := f.f.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return osErr("vfs.ReadAt", f.name, err)
}

func (f *osFile) WriteAt(p []byte, off int64) error {
	n, err := f.f.WriteAt(p, off)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return osErr("vfs.WriteAt", f.name, err)
}

func (f *osFile) Size() (int64, error) {
	fi, err := f.f.Stat()
	if err != nil {
		return 0, osErr("vfs.Size", f.name, err)
	}
	return fi.Size(), nil
}

func (f *osFile) Truncate(size int64) error {
	return osErr("vfs.Truncate", f.name, f.f.Truncate(size))
}

func (f *osFile) Preallocate(size int64) error {
	preallocate(f.f, size) // best effort (SPEC §8.3)
	return nil
}

func (f *osFile) Sync() error { return osErr("vfs.Sync", f.name, f.f.Sync()) }

func (f *osFile) SyncData() error { return osErr("vfs.SyncData", f.name, syncData(f.f)) }

func (f *osFile) Lock(mode LockMode) error { return lockFile(f.f, f.name, mode) }

func (f *osFile) Unlock() error { return osErr("vfs.Unlock", f.name, unlockFile(f.f)) }

func (f *osFile) ID() (FileID, error) { return fileID(f.f) }

func (f *osFile) Close() error { return osErr("vfs.Close", f.name, f.f.Close()) }

// osErr maps an operating system error to a tacdb error code.
func osErr(op, name string, err error) error {
	if err == nil {
		return nil
	}
	var e *errs.Error
	if errors.As(err, &e) {
		return err
	}
	code := errs.CodeIO
	switch {
	case errors.Is(err, fs.ErrNotExist):
		code = errs.CodeNotFound
	case errors.Is(err, fs.ErrExist):
		code = errs.CodeDestinationExists
	case isNoSpace(err):
		code = errs.CodeNoSpace
	case strings.Contains(err.Error(), "path escapes"):
		code = errs.CodeUnsafePath
	}
	return errs.Wrap(code, op, err, name)
}
