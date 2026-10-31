package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A sync falls back to fsync whenever F_FULLFSYNC fails, for files and
// directories, and each fallback is counted; the result of fsync decides.
func TestFullSyncFallback(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := syncData(f); err != nil {
		t.Fatalf("real F_FULLFSYNC: %v", err)
	}
	orig := fullFsync
	t.Cleanup(func() { fullFsync = orig })
	for _, fail := range []error{nil, unix.ENOTSUP, unix.EIO} {
		fullFsync = func(uintptr) error { return fail }
		for name, sync := range map[string]func() error{
			"file":      func() error { return syncData(f) },
			"directory": func() error { return syncDir(d) },
		} {
			before := ReadStats().FullSyncFallbacks
			if err := sync(); err != nil {
				t.Fatalf("%s, F_FULLFSYNC error %v: %v", name, fail, err)
			}
			want := int64(0)
			if fail != nil {
				want = 1
			}
			if got := ReadStats().FullSyncFallbacks - before; got != want {
				t.Fatalf("%s, F_FULLFSYNC error %v: %d fallbacks counted, want %d", name, fail, got, want)
			}
		}
	}
	// The fallback's own failure is returned.
	fullFsync = func(uintptr) error { return unix.ENOTSUP }
	f.Close()
	if err := syncData(f); err == nil {
		t.Fatal("a closed file synced")
	}
}
