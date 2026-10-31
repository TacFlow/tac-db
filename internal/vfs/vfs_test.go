package vfs

import (
	"errors"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

func TestValidName(t *testing.T) {
	ok := []string{".", "db.tacdb", "db.tacdb.lock", "db.tacdb-wal/0000000000000001.wal", "a_b-c.1"}
	for _, n := range ok {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v", n, err)
		}
	}
	bad := []string{"", "..", "../x", "/abs", "a/b/c", ".hidden", "a b", "a\\b", "x.", "con", "NUL.txt", "lpt1.db", "a//b", "a/"}
	for _, n := range bad {
		if err := ValidName(n); !isCode(err, errs.CodeUnsafePath) {
			t.Errorf("ValidName(%q) = %v, want UNSAFE_PATH", n, err)
		}
	}
}

func mustWrite(t *testing.T, f File, off int64, s string) {
	t.Helper()
	if err := f.WriteAt([]byte(s), off); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, f File) string {
	t.Helper()
	n, err := f.Size()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, n)
	if err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	return string(b)
}
