package page

import (
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func isCorrupt(err error) bool { return errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) }

func TestBitmapLayout(t *testing.T) {
	if BitmapPage(0) != 2 || BitmapPage(1) != 2+GroupStride {
		t.Fatalf("bitmap pages: %d, %d", BitmapPage(0), BitmapPage(1))
	}
	cases := []struct {
		id  ID
		g   uint64
		bit int
		ok  bool
	}{
		{0, 0, 0, false}, {1, 0, 0, false}, {2, 0, 0, false},
		{3, 0, 0, true},
		{2 + GroupPages, 0, GroupPages - 1, true},
		{2 + GroupStride, 0, 0, false},
		{3 + GroupStride, 1, 0, true},
	}
	for _, c := range cases {
		g, bit, ok := GroupOf(c.id)
		if ok != c.ok || (ok && (g != c.g || bit != c.bit)) {
			t.Errorf("GroupOf(%d) = %d, %d, %v; want %d, %d, %v", c.id, g, bit, ok, c.g, c.bit, c.ok)
		}
	}
}

func TestCheckPointer(t *testing.T) {
	const pc = 100
	for _, id := range []ID{0, 1, 2, pc, pc + 1} {
		if err := CheckPointer(id, pc); !isCorrupt(err) {
			t.Errorf("CheckPointer(%d) = %v, want CORRUPT", id, err)
		}
	}
	if err := CheckPointer(BitmapPage(1), 1<<20); !isCorrupt(err) {
		t.Errorf("pointer to the second bitmap: %v, want CORRUPT", err)
	}
	for _, id := range []ID{3, 50, pc - 1} {
		if err := CheckPointer(id, pc); err != nil {
			t.Errorf("CheckPointer(%d) = %v", id, err)
		}
	}
}

// CheckExtent must agree with a page-by-page check.
func TestCheckExtentMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	const pageCount = 3 * GroupStride
	for i := 0; i < 20000; i++ {
		e := Extent{Start: ID(r.Uint64N(pageCount + 10)), Pages: uint32(r.IntN(70000))}
		if r.IntN(4) == 0 {
			e.Start = ID(FirstBitmap + GroupStride - uint64(r.IntN(50))) // near a bitmap page
		}
		want := e.Pages > 0
		for p := uint64(0); want && p < uint64(e.Pages); p++ {
			if CheckPointer(e.Start+ID(p), pageCount) != nil {
				want = false
			}
		}
		if got := CheckExtent(e, pageCount) == nil; got != want {
			t.Fatalf("CheckExtent(%+v) valid = %v, want %v", e, got, want)
		}
	}
}
