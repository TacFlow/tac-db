package vfs

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func chmodACL(t *testing.T, path, entry string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", entry, path).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a %q: %v: %s", entry, err, out)
	}
	// Deny entries would stop t.TempDir from removing the directory.
	t.Cleanup(func() { exec.Command("/bin/chmod", "-N", path).Run() })
}

// SEC-08 and R13-21: extended ACLs that grant write access make an object
// insecure; deny entries do not.
func TestDarwinACL(t *testing.T) {
	cases := []struct {
		entry string
		ok    bool
	}{
		{"everyone deny delete", true},
		{"everyone allow read", true},
		{"everyone allow write", false},
		{"everyone allow add_subdirectory,file_inherit,only_inherit", false},
	}
	for _, c := range cases {
		dir := newDir(t)
		chmodACL(t, dir, c.entry)
		fsys, err := OpenDir(dir, Options{})
		if c.ok && err != nil {
			t.Errorf("%q: %v", c.entry, err)
		}
		if !c.ok && !isCode(err, errs.CodeInsecurePermissions) {
			t.Errorf("%q: %v, want INSECURE_PERMISSIONS", c.entry, err)
		}
		if fsys != nil {
			fsys.Close()
		}
	}

	dir := newDir(t)
	fsys := openTestDir(t, dir, Options{})
	f, _ := fsys.Create("a")
	f.Close()
	chmodACL(t, filepath.Join(dir, "a"), "everyone allow write")
	if _, err := fsys.Open("a"); !isCode(err, errs.CodeInsecurePermissions) {
		t.Fatalf("file with an allow-write ACL: %v, want INSECURE_PERMISSIONS", err)
	}
}
