package btreetest

import (
	"context"
	"errors"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var ctx = context.Background()

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

type pins struct{ used int }

func (p *pins) Acquire(n int) error { p.used += n; return nil }
func (p *pins) Release(n int)       { p.used -= n }

// overflow writes an overflow page holding text on a fresh page.
func overflow(t *testing.T, tx *Txn, id page.ID, text string) {
	t.Helper()
	b, err := tx.Fresh(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := page.EncodeOverflow(b, id, []byte(text)); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, r page.Reader, id page.ID) string {
	t.Helper()
	p := &pins{}
	ref, err := r.Page(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Release()
	return string(page.OverflowData(ref.Bytes()))
}

func TestCommitAndReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	s, err := Create(ctx, fs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for gen := 0; gen < 3; gen++ {
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(); !isCode(err, errs.CodeTxInUse) {
			t.Fatalf("second transaction: %v", err)
		}
		tx.SetPageCount(tx.PageCount() + 1)
		id := page.ID(tx.PageCount() - 1)
		overflow(t, tx, id, "generation")
		tx.SetRoot(0, id)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st := s.State()
	if st.Generation != 4 || st.CommitID != 3 || st.PageCount != 5 {
		t.Fatalf("state %+v", st)
	}
	s.Close()
	if s, err = Open(ctx, fs, Options{}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.State(); got != st {
		t.Fatalf("reopened state %+v, want %+v", got, st)
	}
	if got := read(t, s.Reader(), 4); got != "generation" {
		t.Fatalf("page 4 holds %q", got)
	}
	// Each commit writes over the older copy: generation 4 in page 0, 3 in page 1.
	for i, want := range []uint64{4, 3} {
		var b page.Buf
		if err := s.f.ReadAt(b[:], int64(i)*page.Size); err != nil {
			t.Fatal(err)
		}
		if sb, kind, _ := page.Classify(&b); kind != page.Valid || sb.Generation != want {
			t.Fatalf("superblock %d: generation %d, want %d", i, sb.Generation, want)
		}
	}
}

// Writable copies on the first write of each epoch; a restore brings back
// the copies, counts and roots of its savepoint and counts the restore.
func TestSavepoints(t *testing.T) {
	s, err := Create(ctx, vfs.NewMemFS(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin()
	tx.SetPageCount(4)
	overflow(t, tx, 3, "one")
	sp := tx.Savepoint()
	b, err := tx.Writable(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := page.EncodeOverflow(b, 3, []byte("two")); err != nil {
		t.Fatal(err)
	}
	tx.SetPageCount(9)
	tx.SetRoot(1, 3)
	if read(t, tx, 3) != "two" {
		t.Fatal("the transaction does not read its own write")
	}
	if err := tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	if read(t, tx, 3) != "one" || tx.PageCount() != 4 || tx.Root(1) != 0 || tx.Restores() != 1 {
		t.Fatalf("after the restore: %q, %d pages, root %d, %d restores", read(t, tx, 3), tx.PageCount(), tx.Root(1), tx.Restores())
	}
	if err := tx.RollbackTo(5); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("unknown savepoint: %v", err)
	}
	tx.Rollback()
	if _, err := tx.Writable(ctx, 3); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("after Rollback: %v", err)
	}
}

// Finalization refuses a page that is not canonical: the store checks
// what the B+tree must produce.
func TestCommitRefusesNonCanonicalPages(t *testing.T) {
	s, err := Create(ctx, vfs.NewMemFS(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin()
	tx.SetPageCount(4)
	overflow(t, tx, 3, "data")
	b, _ := tx.Writable(ctx, 3)
	b[page.Size-1] = 1 // garbage after the useful bytes
	if err := tx.Commit(ctx); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("non-canonical page: %v", err)
	}
}

func TestOpenRejectsShortFile(t *testing.T) {
	fs := vfs.NewMemFS()
	s, err := Create(ctx, fs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := s.Begin()
	tx.SetPageCount(4)
	overflow(t, tx, 3, "x")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	f, _ := fs.Open(FileName)
	f.Truncate(3 * page.Size)
	f.Close()
	if _, err := Open(ctx, fs, Options{}); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("file shorter than pageCount: %v", err)
	}
}

// Reading an extent checks type, page ID and count of each page.
func TestReadOverflowChecks(t *testing.T) {
	s, err := Create(ctx, vfs.NewMemFS(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.Begin()
	tx.SetPageCount(5)
	overflow(t, tx, 3, string(make([]byte, page.Usable)))
	overflow(t, tx, 4, "tail")
	p := &pins{}
	dst := make([]byte, page.Usable+4)
	if err := tx.ReadOverflow(ctx, p, page.Extent{Start: 3, Pages: 2}, dst); err != nil || string(dst[page.Usable:]) != "tail" {
		t.Fatalf("ReadOverflow: %v", err)
	}
	if err := tx.ReadOverflow(ctx, p, page.Extent{Start: 3, Pages: 2}, dst[:page.Usable+3]); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("wrong count: %v", err)
	}
	if err := tx.ReadOverflow(ctx, p, page.Extent{Start: 3, Pages: 2}, dst[:10]); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("buffer too small for the extent: %v", err)
	}
	if p.used != 0 {
		t.Fatalf("%d bytes still pinned", p.used)
	}
}
