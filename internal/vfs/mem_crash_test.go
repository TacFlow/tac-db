package vfs

import (
	"bytes"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

// newSynced creates a file holding data, synced together with its entry.
func newSynced(t *testing.T, m *MemFS, name string, data []byte) File {
	t.Helper()
	f, err := m.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("."); err != nil {
		t.Fatal(err)
	}
	return f
}

// A surviving unsynced write may keep any subset of its sectors, so a
// later sector can survive while an earlier one is lost.
func TestMemSectorSubset(t *testing.T) {
	sawGap := false
	for seed := uint64(0); seed < 64 && !sawGap; seed++ {
		m := NewMemFS()
		f := newSynced(t, m, "a", make([]byte, 4*512))
		if err := f.WriteAt(bytes.Repeat([]byte{1}, 4*512), 0); err != nil {
			t.Fatal(err)
		}
		m.Crash(CrashOptions{Seed: seed, SectorSubset: true})
		img, _ := m.ReadDurable("a")
		var kept [4]bool
		for s := range kept {
			sector := img[s*512 : (s+1)*512]
			switch {
			case bytes.Equal(sector, bytes.Repeat([]byte{1}, 512)):
				kept[s] = true
			case !bytes.Equal(sector, make([]byte, 512)):
				t.Fatalf("seed %d: sector %d is torn inside", seed, s)
			}
		}
		for s := 1; s < 4; s++ {
			if kept[s] && !kept[s-1] {
				sawGap = true
			}
		}
	}
	if !sawGap {
		t.Fatal("no crash kept a later sector without an earlier one")
	}
}

// A crash point before a call stops it; after a call, the call takes
// effect and then the power is lost. Every handle is dead afterwards.
func TestMemCrashPoint(t *testing.T) {
	for _, after := range []bool{false, true} {
		m := NewMemFS()
		f := newSynced(t, m, "wal", []byte("old"))
		m.InjectCrash(CrashPoint{Op: OpSync, Name: "w*", Nth: 1, After: after})
		if err := f.WriteAt([]byte("new"), 0); err != nil {
			t.Fatal(err)
		}
		err := f.Sync()
		if after != (err == nil) {
			t.Fatalf("after=%v: Sync returned %v", after, err)
		}
		if m.Crashes() != 1 {
			t.Fatalf("after=%v: %d crashes", after, m.Crashes())
		}
		want := "old"
		if after {
			want = "new"
		}
		if img, _ := m.ReadDurable("wal"); string(img) != want {
			t.Fatalf("after=%v: durable image %q, want %q", after, img, want)
		}
		if err := f.WriteAt([]byte("x"), 0); err == nil {
			t.Fatalf("after=%v: handle survived the crash point", after)
		}
		if _, err := m.Open("wal"); err == nil {
			t.Fatalf("after=%v: the dead process opened a file", after)
		}
		m.Restart()
		g, err := m.Open("wal")
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Sync(); err != nil {
			t.Fatalf("after=%v: the crash point fired twice: %v", after, err)
		}
	}
}

// A crash point on a directory operation keeps only the synced entries.
func TestMemCrashPointRename(t *testing.T) {
	m := NewMemFS()
	newSynced(t, m, "spare.tmp", []byte("segment"))
	m.InjectCrash(CrashPoint{Op: OpRename, Nth: 1, After: true})
	if err := m.Rename("spare.tmp", "0000000000000002.wal"); err != nil {
		t.Fatal(err)
	}
	m.Restart()
	if ok, _ := m.Exists("0000000000000002.wal"); ok {
		t.Fatal("a rename without SyncDir survived the crash")
	}
	if ok, _ := m.Exists("spare.tmp"); !ok {
		t.Fatal("the old name was lost")
	}
}

// Preallocate reserves what fits, without changing the size of the file,
// and never fails for space: the File contract says a failed reservation
// is not an error.
func TestMemPreallocate(t *testing.T) {
	m := NewMemFS()
	m.SetCapacity(10_000)
	f, err := m.Create("seg")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Preallocate(8_000); err != nil {
		t.Fatal(err)
	}
	if size, _ := f.Size(); size != 0 {
		t.Fatalf("size %d after Preallocate", size)
	}
	g, err := m.Create("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.WriteAt(make([]byte, 3_000), 0); !isCode(err, errs.CodeNoSpace) {
		t.Fatalf("write beyond the space left by the reservation: %v", err)
	}
	if err := f.WriteAt(make([]byte, 8_000), 0); err != nil {
		t.Fatalf("write inside the reservation: %v", err)
	}
	if err := f.Preallocate(20_000); err != nil {
		t.Fatalf("a reservation beyond the capacity is not an error: %v", err)
	}
	if err := g.Truncate(1 << 20); err != nil {
		t.Fatalf("extending a file is sparse: %v", err)
	}
}

// After a crash point the process is gone: no call changes the image the
// next process finds, until Restart.
func TestMemDeadAfterCrashPoint(t *testing.T) {
	m := NewMemFS()
	newSynced(t, m, "a", []byte("keep"))
	m.InjectCrash(CrashPoint{Op: OpSync, Nth: 1})
	f, _ := m.Open("a")
	if err := f.Sync(); err == nil {
		t.Fatal("the crash point did not stop the call")
	}
	if err := m.Remove("a"); err == nil {
		t.Fatal("the dead process removed a file")
	}
	if err := m.SyncDir("."); err == nil {
		t.Fatal("the dead process synced a directory")
	}
	if _, err := m.List("."); err == nil {
		t.Fatal("the dead process listed a directory")
	}
	m.Restart()
	if ok, _ := m.Exists("a"); !ok {
		t.Fatal("the file was lost")
	}
}

// A crash point after a call fires even when the call fails.
func TestMemCrashPointAfterAFailedCall(t *testing.T) {
	m := NewMemFS()
	newSynced(t, m, "a", []byte("x"))
	m.InjectCrash(CrashPoint{Op: OpRemove, Nth: 1, After: true})
	if err := m.Remove("missing"); err == nil {
		t.Fatal("removing a missing file succeeded")
	}
	if m.Crashes() != 1 {
		t.Fatal("the crash point did not fire after the failed call")
	}
}

// Crash points apply only to calls that change state.
func TestMemCrashPointOps(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a crash point on reads was accepted")
		}
	}()
	NewMemFS().InjectCrash(CrashPoint{Op: OpRead, Nth: 1})
}

// Torn writes are cut at absolute sector boundaries of the file.
func TestMemTornAtAbsoluteSectors(t *testing.T) {
	for seed := uint64(0); seed < 32; seed++ {
		m := NewMemFS()
		f := newSynced(t, m, "a", make([]byte, 2048))
		if err := f.WriteAt(bytes.Repeat([]byte{1}, 1024), 256); err != nil { // sectors 0 to 2
			t.Fatal(err)
		}
		m.Crash(CrashOptions{Seed: seed, SectorSubset: true})
		img, _ := m.ReadDurable("a")
		for _, r := range [][2]int{{256, 512}, {512, 1024}, {1024, 1280}} {
			part := img[r[0]:r[1]]
			if !bytes.Equal(part, bytes.Repeat([]byte{1}, len(part))) && !bytes.Equal(part, make([]byte, len(part))) {
				t.Fatalf("seed %d: bytes %d..%d torn inside a sector", seed, r[0], r[1])
			}
		}
	}
}

// A size set by Truncate is sparse: only written bytes take space, so a
// write into the extended region can still run out of it.
func TestMemSparseTruncate(t *testing.T) {
	m := NewMemFS()
	m.SetCapacity(10_000)
	f, err := m.Create("seg")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAt(make([]byte, 8_000), 0); err != nil {
		t.Fatalf("write inside the capacity: %v", err)
	}
	if err := f.WriteAt(make([]byte, 4_000), 8_000); !isCode(err, errs.CodeNoSpace) {
		t.Fatalf("write beyond the capacity in a sparse region: %v", err)
	}
}
