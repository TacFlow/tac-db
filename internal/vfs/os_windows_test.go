package vfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func secureTestDir(t *testing.T, dir string) {
	t.Helper()
	if err := secureCreated(nil, dir, kindDir); err != nil {
		t.Fatal(err)
	}
}

func icacls(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
		t.Fatalf("icacls %v: %v: %s", args, err, out)
	}
}

// SEC-08 and R13-21: an ACE that gives write access to another account
// makes the caller's directory insecure; objects created by tacdb grant
// access to nobody else.
func TestWindowsDACL(t *testing.T) {
	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, err := fsys.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	g, err := fsys.Open("a")
	if err != nil {
		t.Fatalf("file created by tacdb: %v", err)
	}
	g.Close()

	icacls(t, filepath.Join(dir, "a"), "/grant", "*S-1-1-0:(R)") // Everyone: read
	if _, err := fsys.Open("a"); !isCode(err, errs.CodeInsecurePermissions) {
		t.Fatalf("file readable by Everyone: %v, want INSECURE_PERMISSIONS", err)
	}

	icacls(t, dir, "/grant", "*S-1-1-0:(R)")
	readable, err := OpenDir(dir, Options{})
	if err != nil {
		t.Fatalf("caller directory readable by Everyone: %v", err)
	}
	readable.Close() // an open root would keep t.TempDir from removing dir
	icacls(t, dir, "/grant", "*S-1-1-0:(OI)(CI)(W)")
	if _, err := OpenDir(dir, Options{}); !isCode(err, errs.CodeInsecurePermissions) {
		t.Fatalf("caller directory writable by Everyone: %v, want INSECURE_PERMISSIONS", err)
	}
}

// SEC-08 and R13-21: a junction inside the database directory is refused.
func TestWindowsJunction(t *testing.T) {
	dir := newDir(t)
	target := newDir(t)
	link := filepath.Join(dir, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v: %s", err, out)
	}
	fsys := openTestDir(t, dir, Options{})
	if _, err := fsys.Exists("j"); !isCode(err, errs.CodeUnsafePath) {
		t.Fatalf("Exists(junction): %v, want UNSAFE_PATH", err)
	}
	if _, err := fsys.List("j"); !isCode(err, errs.CodeUnsafePath) {
		t.Fatalf("List(junction): %v, want UNSAFE_PATH", err)
	}
	if fi, err := os.Lstat(link); err == nil {
		t.Logf("junction mode: %v", fi.Mode())
	}
}
