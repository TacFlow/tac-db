package vfs

import (
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestLockDatabase(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("db.tacdb")
	f.Close()

	owner, err := LockDatabase(m, "db.tacdb", LockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDatabase(m, "db.tacdb", LockShared); !isCode(err, errs.CodeLocked) {
		t.Fatalf("second lock in the same process: %v, want LOCKED", err)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	r1, err := LockDatabase(m, "db.tacdb", LockShared)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Release()
	if ok, _ := m.Exists("db.tacdb.lock"); !ok {
		t.Fatal("lock file was not created")
	}
}

// The main file is locked too: a lock taken directly on it by another
// "process" (another handle) blocks the database lock even though the
// lock file is free.
func TestLockDatabaseLocksMainFile(t *testing.T) {
	m := NewMemFS()
	f, _ := m.Create("db.tacdb")
	other, _ := m.Open("db.tacdb")
	if err := other.Lock(LockExclusive); err != nil {
		t.Fatal(err)
	}
	if _, err := LockDatabase(m, "db.tacdb", LockExclusive); !isCode(err, errs.CodeLocked) {
		t.Fatalf("main file held elsewhere: %v, want LOCKED", err)
	}
	other.Close()
	l, err := LockDatabase(m, "db.tacdb", LockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	f.Close()
}

func TestAttachMainFile(t *testing.T) {
	m := NewMemFS()
	l, err := LockDatabase(m, "new.tacdb", LockExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	f, _ := m.Create("new.tacdb")
	defer f.Close()
	if err := l.AttachMainFile(m, "new.tacdb"); err != nil {
		t.Fatal(err)
	}
	other, _ := m.Open("new.tacdb")
	defer other.Close()
	if err := other.Lock(LockShared); !isCode(err, errs.CodeLocked) {
		t.Fatalf("attached main file is not locked: %v", err)
	}
}
