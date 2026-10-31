package vfs

import (
	"bytes"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestMemCrashKeepsOnlyDurableState(t *testing.T) {
	m := NewMemFS()
	f, err := m.Create("a")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, 0, "synced")
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("."); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, 0, "LOST!!")
	g, _ := m.Create("b") // the directory is not synced again: "b" is lost
	mustWrite(t, g, 0, "x")
	g.Sync()

	m.Crash(CrashOptions{})
	if err := f.WriteAt([]byte("z"), 0); !isCode(err, errs.CodeClosed) {
		t.Fatalf("handle survived the crash: %v", err)
	}
	f2, err := m.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f2); got != "synced" {
		t.Fatalf("after crash a = %q, want %q", got, "synced")
	}
	if ok, _ := m.Exists("b"); ok {
		t.Fatal("b survived without SyncDir")
	}
}

func TestMemRenameNeedsSyncDir(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("tmp")
	mustWrite(t, f, 0, "new")
	f.Sync()
	m.SyncDir(".")
	if err := m.Rename("tmp", "final"); err != nil {
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if ok, _ := m.Exists("final"); ok {
		t.Fatal("rename survived without SyncDir")
	}
	if ok, _ := m.Exists("tmp"); !ok {
		t.Fatal("old name lost")
	}
	m.Rename("tmp", "final")
	m.SyncDir(".")
	m.Crash(CrashOptions{})
	if ok, _ := m.Exists("final"); !ok {
		t.Fatal("rename lost after SyncDir")
	}
}

func TestMemTornWritesAreSectorPrefixes(t *testing.T) {
	for seed := uint64(0); seed < 50; seed++ {
		m := NewMemFS()
		f, _ := m.Create("a")
		f.Sync()
		m.SyncDir(".")
		data := bytes.Repeat([]byte{0xAB}, 2048)
		mustWrite(t, f, 0, string(data))
		m.Crash(CrashOptions{Seed: seed, TornWrites: true})
		img, _ := m.ReadDurable("a")
		if len(img)%512 != 0 || !bytes.Equal(img, data[:len(img)]) {
			t.Fatalf("seed %d: torn image of %d bytes is not a sector prefix", seed, len(img))
		}
	}
}

func TestMemFaults(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("a")
	m.Inject(Fault{Op: OpWrite, Name: "a", Nth: 2})
	mustWrite(t, f, 0, "one")
	if err := f.WriteAt([]byte("two"), 3); !isCode(err, errs.CodeIO) {
		t.Fatalf("second write: %v, want IO", err)
	}
	mustWrite(t, f, 3, "three")

	m.Inject(Fault{Op: OpWrite, Name: "a", Nth: 4, ShortWrite: true})
	err := f.WriteAt([]byte("abcd"), 0)
	if !isCode(err, errs.CodeIO) {
		t.Fatalf("short write: %v, want IO", err)
	}
	if got := readAll(t, f); got != "abethree" {
		t.Fatalf("after short write = %q, want %q", got, "abethree")
	}

	m.Inject(Fault{Op: OpSync, Nth: 1})
	if err := f.Sync(); !isCode(err, errs.CodeIO) {
		t.Fatalf("sync: %v, want IO", err)
	}
}

func TestMemFaultPattern(t *testing.T) {
	m := NewMemFS()
	m.Mkdir("db.tacdb-wal")
	m.Inject(Fault{Op: OpSync, Name: "db.tacdb-wal/*"})
	seg, _ := m.Create("db.tacdb-wal/0000000000000001.wal")
	if err := seg.Sync(); !isCode(err, errs.CodeIO) {
		t.Fatalf("sync of a segment: %v, want IO", err)
	}
	main, _ := m.Create("db.tacdb")
	if err := main.Sync(); err != nil {
		t.Fatalf("sync of the main file: %v", err)
	}
}

func TestMemCrashTruncate(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("a")
	mustWrite(t, f, 0, "0123456789")
	f.Sync()
	m.SyncDir(".")
	m.Crash(CrashOptions{Truncate: map[string]int64{"a": 4, "missing": 1}})
	if img, _ := m.ReadDurable("a"); string(img) != "0123" {
		t.Fatalf("truncated image = %q, want %q", img, "0123")
	}
	g, _ := m.Open("a")
	if got := readAll(t, g); got != "0123" {
		t.Fatalf("after crash a = %q", got)
	}
	m.Crash(CrashOptions{Truncate: map[string]int64{"a": -1}})
	if img, _ := m.ReadDurable("a"); len(img) != 0 {
		t.Fatalf("negative size must cut to zero, got %q", img)
	}
}

func TestMemCapacity(t *testing.T) {
	m := NewMemFS()
	m.SetCapacity(10)
	f, _ := m.Create("a")
	mustWrite(t, f, 0, "0123456789")
	if err := f.WriteAt([]byte("x"), 10); !isCode(err, errs.CodeNoSpace) {
		t.Fatalf("write past capacity: %v, want NO_SPACE", err)
	}
	mustWrite(t, f, 0, "overwrite!") // no growth, no new space
}

func TestMemReadPastEnd(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("a")
	mustWrite(t, f, 0, "abc")
	if err := f.ReadAt(make([]byte, 4), 0); !isCode(err, errs.CodeIO) {
		t.Fatalf("short read: %v, want IO", err)
	}
}

func TestMemCreateIsExclusive(t *testing.T) {
	m := NewMemFS()
	if _, err := m.Create("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("a"); !isCode(err, errs.CodeDestinationExists) {
		t.Fatalf("second create: %v, want DESTINATION_EXISTS", err)
	}
	if _, err := m.Open("missing"); !isCode(err, errs.CodeNotFound) {
		t.Fatalf("open missing: %v, want NOT_FOUND", err)
	}
	if _, err := m.Open("../x"); !isCode(err, errs.CodeUnsafePath) {
		t.Fatalf("open ../x: %v, want UNSAFE_PATH", err)
	}
}

func TestMemSubdirectory(t *testing.T) {
	m := NewMemFS()
	if err := m.Mkdir("db.tacdb-wal"); err != nil {
		t.Fatal(err)
	}
	f, err := m.Create("db.tacdb-wal/0000000000000001.wal")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	names, err := m.List("db.tacdb-wal")
	if err != nil || len(names) != 1 || names[0] != "0000000000000001.wal" {
		t.Fatalf("List = %v, %v", names, err)
	}
	if err := m.Remove("db.tacdb-wal"); !isCode(err, errs.CodeIO) {
		t.Fatalf("remove non-empty dir: %v, want IO", err)
	}
}
