package alloc

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/TacFlow/tacdb/internal/btree/btreetest"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var ctx = context.Background()

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

func newTxn(t *testing.T) (*btreetest.Store, *btreetest.Txn) {
	t.Helper()
	s, err := btreetest.Create(ctx, vfs.NewMemFS(), btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	return s, tx
}

func mustAlloc(t *testing.T, a *Allocator, hint page.ID) page.ID {
	t.Helper()
	id, err := a.Alloc(ctx, hint)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// freeBits counts the free data pages below pageCount from the bitmaps.
func freeBits(t *testing.T, w page.Writer) uint64 {
	t.Helper()
	var free uint64
	pc := w.PageCount()
	for g := uint64(0); g < groups(pc); g++ {
		ref, err := w.Page(ctx, nopins{}, page.BitmapPage(g))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < groupLen(g, pc); i++ {
			if !page.BitmapBit(ref.Bytes(), i) {
				free++
			}
		}
		ref.Release()
	}
	return free
}

func TestLowestAddressFirst(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	for want := page.ID(3); want < 13; want++ { // page 2 is the first bitmap
		if got := mustAlloc(t, a, 0); got != want {
			t.Fatalf("Alloc = %d, want %d", got, want)
		}
	}
	if tx.PageCount() != 13 || tx.FreePageCount() != 0 {
		t.Fatalf("pageCount %d, free %d", tx.PageCount(), tx.FreePageCount())
	}
	for _, id := range []page.ID{9, 5} {
		if err := a.Free(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if tx.FreePageCount() != 2 || freeBits(t, tx) != 2 {
		t.Fatalf("free %d, bits %d", tx.FreePageCount(), freeBits(t, tx))
	}
	for _, want := range []page.ID{5, 9, 13} {
		if got := mustAlloc(t, a, 0); got != want {
			t.Fatalf("Alloc = %d, want %d", got, want)
		}
	}
	if err := a.Free(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(ctx, 5); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("double free: %v", err)
	}
	if err := a.Free(ctx, page.FirstBitmap); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("freeing a bitmap page: %v", err)
	}
	if err := a.Free(ctx, 500); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("freeing beyond pageCount: %v", err)
	}
}

// The hint picks the group of a sibling or parent (SPEC §8.4).
func TestLocalityHint(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	if _, err := a.AllocExtents(ctx, page.GroupPages+10, 0); err != nil {
		t.Fatal(err)
	}
	g1 := page.BitmapPage(1)
	for _, id := range []page.ID{100, g1 + 5} {
		if err := a.Free(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustAlloc(t, a, g1+2); got != g1+5 {
		t.Fatalf("Alloc with a hint in group 1 = %d, want %d", got, g1+5)
	}
	if got := mustAlloc(t, a, g1+2); got != 100 {
		t.Fatalf("Alloc after group 1 filled = %d, want 100", got)
	}
}

// An extent never covers a bitmap page; the end of the file creates the
// bitmap of the next group.
func TestExtentsSkipBitmaps(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	ext, err := a.AllocExtents(ctx, page.GroupPages+10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []page.Extent{{Start: 3, Pages: page.GroupPages}, {Start: page.BitmapPage(1) + 1, Pages: 10}}
	if len(ext) != 2 || ext[0] != want[0] || ext[1] != want[1] {
		t.Fatalf("extents %v, want %v", ext, want)
	}
	if err := page.CheckExtents(ext, uint64(page.GroupPages+10)*page.Usable, tx.PageCount()); err != nil {
		t.Fatal(err)
	}
	ref, err := tx.Page(ctx, nopins{}, page.BitmapPage(1))
	if err != nil || page.HeaderOf(ref.Bytes()).Type() != page.TypeBitmap {
		t.Fatalf("bitmap of group 1: %v", err)
	}
	ref.Release()
}

// More than MaxExtents holes would be needed: the value goes to the end
// of the file instead (R13-02).
func TestExtentLimitFallsBackToTail(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	if _, err := a.AllocExtents(ctx, 800, 0); err != nil {
		t.Fatal(err)
	}
	for id := page.ID(3); id < 803; id += 2 { // 400 holes of one page
		if err := a.Free(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	pc := tx.PageCount()
	ext, err := a.AllocExtents(ctx, 400, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 1 || ext[0] != (page.Extent{Start: page.ID(pc), Pages: 400}) || tx.FreePageCount() != 400 {
		t.Fatalf("extents %v, free %d", ext, tx.FreePageCount())
	}
	ext, err = a.AllocExtents(ctx, 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 300 || ext[0].Start != 3 || tx.FreePageCount() != 100 {
		t.Fatalf("%d extents from %d, free %d", len(ext), ext[0].Start, tx.FreePageCount())
	}
	if err := page.CheckExtents(ext, 300*page.Usable, tx.PageCount()); err != nil {
		t.Fatal(err)
	}
	// 100 holes and the end of the file.
	ext, err = a.AllocExtents(ctx, 150, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 101 || tx.FreePageCount() != 0 {
		t.Fatalf("%d extents, free %d", len(ext), tx.FreePageCount())
	}
	if err := page.CheckExtents(ext, 150*page.Usable, tx.PageCount()); err != nil {
		t.Fatal(err)
	}
}

// A free run that ends where the file ends joins the pages added at the
// end into one extent.
func TestRunJoinsTheTail(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	if _, err := a.AllocExtents(ctx, 20, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeExtents(ctx, []page.Extent{{Start: 18, Pages: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(ctx, 4); err != nil {
		t.Fatal(err)
	}
	ext, err := a.AllocExtents(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []page.Extent{{Start: 4, Pages: 1}, {Start: 18, Pages: 9}}
	if len(ext) != 2 || ext[0] != want[0] || ext[1] != want[1] {
		t.Fatalf("extents %v, want %v", ext, want)
	}
}

// A savepoint restore brings back the old bitmap; the summary follows.
func TestSummaryFollowsRestore(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	for i := 0; i < 5; i++ {
		mustAlloc(t, a, 0)
	}
	sp := tx.Savepoint()
	if err := a.Free(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	if got := mustAlloc(t, a, 0); got != 8 {
		t.Fatalf("Alloc after the restore = %d, want 8 (page 4 is allocated again)", got)
	}
}

// BT-05 without scrub: random allocations and frees, committed and
// reopened, keep free_page_count equal to the bitmaps.
func TestRandomAllocFreeWithReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	s, err := btreetest.Create(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(5, 5))
	live := map[page.ID]bool{}
	for round := 0; round < 20; round++ {
		tx, err := s.Begin()
		if err != nil {
			t.Fatal(err)
		}
		a := New(tx)
		for op := 0; op < 200; op++ {
			if len(live) > 0 && r.IntN(3) == 0 {
				for id := range live {
					if err := a.Free(ctx, id); err != nil {
						t.Fatal(err)
					}
					delete(live, id)
					break
				}
				continue
			}
			ext, err := a.AllocExtents(ctx, 1+r.IntN(6), 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ext {
				for p := e.Start; p < e.Start+page.ID(e.Pages); p++ {
					if live[p] {
						t.Fatalf("page %d allocated twice", p)
					}
					live[p] = true
					buf, err := tx.Fresh(p)
					if err != nil {
						t.Fatal(err)
					}
					if err := page.EncodeOverflow(buf, p, []byte("x")); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if got := freeBits(t, tx); got != tx.FreePageCount() {
			t.Fatalf("round %d: free_page_count %d, bitmaps %d", round, tx.FreePageCount(), got)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if round%5 == 4 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if s, err = btreetest.Open(ctx, fs, btreetest.Options{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer s.Close()
	st := s.State()
	if want := st.PageCount - 3 - uint64(len(live)); st.FreePageCount != want {
		t.Fatalf("free_page_count %d, want %d", st.FreePageCount, want)
	}
}

// One free run long enough wins over smaller holes at lower addresses.
func TestContiguousRunPreferred(t *testing.T) {
	_, tx := newTxn(t)
	a := New(tx)
	if _, err := a.AllocExtents(ctx, 20, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.Free(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := a.FreeExtents(ctx, []page.Extent{{Start: 10, Pages: 5}}); err != nil {
		t.Fatal(err)
	}
	ext, err := a.AllocExtents(ctx, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ext) != 1 || ext[0] != (page.Extent{Start: 10, Pages: 5}) {
		t.Fatalf("extents %v, want the run at 10", ext)
	}
}
