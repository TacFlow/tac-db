package vfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

// The lock tests re-execute this test binary as a second process.
func TestMain(m *testing.M) {
	if dir := os.Getenv("TACDB_VFS_LOCK_HELPER"); dir != "" {
		os.Exit(lockHelper(dir, os.Getenv("TACDB_VFS_LOCK_MODE")))
	}
	os.Exit(m.Run())
}

// Exit codes of the helper process.
const (
	helperAcquired = 0
	helperFailed   = 1
	helperLocked   = 3
)

func lockHelper(dir, mode string) int {
	fsys, err := OpenDir(dir, Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return helperFailed
	}
	m := LockExclusive
	if mode == "shared" {
		m = LockShared
	}
	l, err := LockDatabase(fsys, "db.tacdb", m)
	if errs.CodeOf(err) == errs.CodeLocked {
		return helperLocked
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return helperFailed
	}
	l.Release()
	return helperAcquired
}

func runHelper(t *testing.T, dir, mode string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TACDB_VFS_LOCK_HELPER="+dir, "TACDB_VFS_LOCK_MODE="+mode)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return helperAcquired
	case errors.As(err, &exit):
		if exit.ExitCode() == helperFailed {
			t.Logf("helper output: %s", out)
		}
		return exit.ExitCode()
	}
	t.Fatalf("running helper: %v", err)
	return -1
}

// newDir returns an empty directory that passes the caller-directory
// checks of SPEC §20.9 on every platform.
func newDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	secureTestDir(t, dir)
	return dir
}

func openTestDir(t *testing.T, dir string, opts Options) FS {
	t.Helper()
	fsys, err := OpenDir(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	return fsys
}

func TestConformanceMem(t *testing.T) {
	conformance(t, func(*testing.T) FS { return NewMemFS() })
}

func TestConformanceOS(t *testing.T) {
	conformance(t, func(t *testing.T) FS { return openTestDir(t, newDir(t), Options{}) })
}

// conformance holds the behaviour that every FS shares.
func conformance(t *testing.T, mk func(*testing.T) FS) {
	t.Run("create, write, read", func(t *testing.T) {
		fsys := mk(t)
		f, err := fsys.Create("a")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		mustWrite(t, f, 0, "hello")
		mustWrite(t, f, 5, " world")
		if got := readAll(t, f); got != "hello world" {
			t.Fatalf("read %q", got)
		}
		if err := f.ReadAt(make([]byte, 20), 0); !isCode(err, errs.CodeIO) {
			t.Fatalf("read past end: %v, want IO", err)
		}
		if _, err := fsys.Create("a"); !isCode(err, errs.CodeDestinationExists) {
			t.Fatalf("second create: %v, want DESTINATION_EXISTS", err)
		}
	})
	t.Run("truncate and preallocate", func(t *testing.T) {
		fsys := mk(t)
		f, _ := fsys.Create("a")
		defer f.Close()
		mustWrite(t, f, 0, "0123456789")
		if err := f.Truncate(4); err != nil {
			t.Fatal(err)
		}
		if err := f.Preallocate(1 << 20); err != nil {
			t.Fatal(err)
		}
		if err := f.Preallocate(2); err != nil { // below the size: must not truncate
			t.Fatal(err)
		}
		if n, _ := f.Size(); n != 4 {
			t.Fatalf("size after truncate and preallocate = %d, want 4", n)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.SyncData(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("open, read only, missing", func(t *testing.T) {
		fsys := mk(t)
		f, _ := fsys.Create("a")
		f.Close()
		ro, err := fsys.OpenReadOnly("a")
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		if err := ro.WriteAt([]byte("x"), 0); err == nil {
			t.Fatal("write through a read-only handle succeeded")
		}
		if _, err := fsys.Open("missing"); !isCode(err, errs.CodeNotFound) {
			t.Fatalf("open missing: %v, want NOT_FOUND", err)
		}
		if _, err := fsys.Open("../a"); !isCode(err, errs.CodeUnsafePath) {
			t.Fatalf("open ../a: %v, want UNSAFE_PATH", err)
		}
	})
	t.Run("rename, remove, list, exists", func(t *testing.T) {
		fsys := mk(t)
		f, _ := fsys.Create("tmp")
		mustWrite(t, f, 0, "new")
		f.Close()
		old, _ := fsys.Create("final")
		old.Close()
		if err := fsys.Rename("tmp", "final"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.SyncDir("."); err != nil {
			t.Fatal(err)
		}
		g, _ := fsys.Open("final")
		if got := readAll(t, g); got != "new" {
			t.Fatalf("after rename final = %q", got)
		}
		g.Close()
		if err := fsys.Mkdir("w"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Rename("final", "w/final"); !isCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("rename across directories: %v, want INVALID_ARGUMENT", err)
		}
		names, err := fsys.List(".")
		if err != nil || fmt.Sprint(names) != "[final w]" {
			t.Fatalf("List = %v, %v", names, err)
		}
		if err := fsys.Remove("final"); err != nil {
			t.Fatal(err)
		}
		if ok, err := fsys.Exists("final"); ok || err != nil {
			t.Fatalf("Exists after remove = %v, %v", ok, err)
		}
	})
	t.Run("subdirectory", func(t *testing.T) {
		fsys := mk(t)
		if err := fsys.Mkdir("db.tacdb-wal"); err != nil {
			t.Fatal(err)
		}
		f, err := fsys.Create("db.tacdb-wal/0000000000000001.wal")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := fsys.SyncDir("db.tacdb-wal"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsys.Create("missing/x"); !isCode(err, errs.CodeNotFound) {
			t.Fatalf("create in missing directory: %v, want NOT_FOUND", err)
		}
	})
	t.Run("locks between handles", func(t *testing.T) {
		fsys := mk(t)
		a, _ := fsys.Create("a")
		defer a.Close()
		b, _ := fsys.Open("a")
		defer b.Close()
		if err := a.Lock(LockShared); err != nil {
			t.Fatal(err)
		}
		if err := b.Lock(LockShared); err != nil {
			t.Fatalf("two shared locks: %v", err)
		}
		b.Unlock()
		a.Unlock()
		if err := a.Lock(LockExclusive); err != nil {
			t.Fatal(err)
		}
		if err := b.Lock(LockShared); !isCode(err, errs.CodeLocked) {
			t.Fatalf("shared over exclusive: %v, want LOCKED", err)
		}
		a.Unlock()
		if err := b.Lock(LockExclusive); err != nil {
			t.Fatalf("exclusive after unlock: %v", err)
		}
	})
	t.Run("identity", func(t *testing.T) {
		fsys := mk(t)
		a, _ := fsys.Create("a")
		defer a.Close()
		a2, _ := fsys.Open("a")
		defer a2.Close()
		b, _ := fsys.Create("b")
		defer b.Close()
		ida, _ := a.ID()
		ida2, _ := a2.ID()
		idb, _ := b.ID()
		if ida != ida2 || ida == idb {
			t.Fatalf("IDs: a=%v a2=%v b=%v", ida, ida2, idb)
		}
	})
}

// OPS-03: a second process cannot take the database lock, whatever path
// it uses.
func TestLockAcrossProcesses(t *testing.T) {
	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, err := fsys.Create("db.tacdb")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	owner, err := LockDatabase(fsys, "db.tacdb", LockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if got := runHelper(t, dir, "exclusive"); got != helperLocked {
		t.Fatalf("second process, exclusive: exit %d, want %d", got, helperLocked)
	}
	if got := runHelper(t, dir, "shared"); got != helperLocked {
		t.Fatalf("second process, shared: exit %d, want %d", got, helperLocked)
	}

	// A hard link under another name has another lock file, but the same
	// main file (R13-04).
	t.Run("hard-link alias", func(t *testing.T) {
		alias := newDir(t)
		if err := os.Link(filepath.Join(dir, "db.tacdb"), filepath.Join(alias, "db.tacdb")); err != nil {
			t.Skipf("hard links unsupported here: %v", err)
		}
		if got := runHelper(t, alias, "exclusive"); got != helperLocked {
			t.Fatalf("hard-link alias: exit %d, want %d", got, helperLocked)
		}
	})

	owner.Release()
	if got := runHelper(t, dir, "exclusive"); got != helperAcquired {
		t.Fatalf("after release: exit %d, want %d", got, helperAcquired)
	}

	reader, err := LockDatabase(fsys, "db.tacdb", LockShared)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Release()
	if got := runHelper(t, dir, "shared"); got != helperAcquired {
		t.Fatalf("two readers: exit %d, want %d", got, helperAcquired)
	}
	if got := runHelper(t, dir, "exclusive"); got != helperLocked {
		t.Fatalf("owner while a reader holds the lock: exit %d, want %d", got, helperLocked)
	}
}

func TestOpenDirRejectsMissingAndFiles(t *testing.T) {
	dir := newDir(t)
	if _, err := OpenDir(filepath.Join(dir, "missing"), Options{}); !isCode(err, errs.CodeNotFound) {
		t.Fatalf("missing directory: %v, want NOT_FOUND", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(file, Options{}); err == nil {
		t.Fatal("OpenDir on a regular file succeeded")
	}
}

func TestPreallocateFailureIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the size limit below is a Unix rlimit")
	}
	fsys := openTestDir(t, newDir(t), Options{})
	f, _ := fsys.Create("a")
	defer f.Close()
	if err := f.Preallocate(1 << 62); err != nil {
		t.Fatalf("Preallocate of an impossible size: %v", err)
	}
}
