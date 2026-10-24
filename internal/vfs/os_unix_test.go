//go:build linux || darwin

package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/TacFlow/tacdb/internal/errs"
)

func secureTestDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestCreatedObjectsAreOwnerOnly(t *testing.T) {
	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, err := fsys.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := fsys.Mkdir("d"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"a": 0o600, "d": 0o700} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v, want %v", name, got, want)
		}
	}
}

// SEC-08: broad permissions are INSECURE_PERMISSIONS unless the directory
// is declared shared.
func TestPermissions(t *testing.T) {
	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, _ := fsys.Create("a")
	f.Close()
	if err := os.Chmod(filepath.Join(dir, "a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Open("a"); !isCode(err, errs.CodeInsecurePermissions) {
		t.Fatalf("0644 file: %v, want INSECURE_PERMISSIONS", err)
	}
	shared := openTestDir(t, dir, Options{AllowSharedDirectory: true})
	g, err := shared.Open("a")
	if err != nil {
		t.Fatalf("0644 file with AllowSharedDirectory: %v", err)
	}
	g.Close()

	for mode, ok := range map[os.FileMode]bool{0o700: true, 0o755: true, 0o775: false, 0o777: false, 0o1777: false} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		fsys, err := OpenDir(dir, Options{})
		if ok && err != nil {
			t.Errorf("directory %v: %v", mode, err)
		}
		if !ok && !isCode(err, errs.CodeInsecurePermissions) {
			t.Errorf("directory %v: %v, want INSECURE_PERMISSIONS", mode, err)
		}
		if fsys != nil {
			fsys.Close()
		}
	}
	os.Chmod(dir, 0o700)
}

// SEC-08 and R13-03: inside the database directory only regular files and
// directories are accepted.
func TestRejectsSymlinksAndSpecialFiles(t *testing.T) {
	dir := newDir(t)
	outside := newDir(t)
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("db.tacdb", filepath.Join(dir, "inner")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys := openTestDir(t, dir, Options{})
	for _, name := range []string{"link", "inner", "fifo"} {
		if _, err := fsys.Open(name); !isCode(err, errs.CodeUnsafePath) {
			t.Errorf("Open(%s): %v, want UNSAFE_PATH", name, err)
		}
		if _, err := fsys.Exists(name); !isCode(err, errs.CodeUnsafePath) {
			t.Errorf("Exists(%s): %v, want UNSAFE_PATH", name, err)
		}
	}
	if err := fsys.Rename("link", "other"); !isCode(err, errs.CodeUnsafePath) {
		t.Errorf("Rename of a symlink: %v, want UNSAFE_PATH", err)
	}
}

// The caller's path may go through a symbolic link: it is resolved once,
// so the alias reaches the same lock file.
func TestCallerPathSymlinkIsResolved(t *testing.T) {
	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, _ := fsys.Create("db.tacdb")
	f.Close()
	owner, err := LockDatabase(fsys, "db.tacdb", LockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	alias := filepath.Join(newDir(t), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if got := runHelper(t, alias, "exclusive"); got != helperLocked {
		t.Fatalf("symlink alias: exit %d, want %d", got, helperLocked)
	}
}

func TestRetryEINTR(t *testing.T) {
	calls := 0
	err := retryEINTR(func() error {
		calls++
		if calls < 3 {
			return unix.EINTR
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("retryEINTR: err %v after %d calls, want nil after 3", err, calls)
	}
	want := errors.New("other")
	if err := retryEINTR(func() error { return want }); err != want {
		t.Fatalf("retryEINTR passed %v, want %v", err, want)
	}
}
