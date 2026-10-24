// Package check is the global structural checker (SPEC §20.11, §27; design
// §4.11). Beyond what btree.Verify checks in each tree, it requires that
// every data page has exactly one owner, that the bitmaps mark exactly the
// owned pages and that free_page_count counts the free pages they cover.
// It runs only on published state: a Reader without private copies.
package check

import (
	"context"
	"errors"
	"fmt"

	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
)

// Category groups the problems the checker reports.
type Category string

// Categories of problems.
const (
	Tree      Category = "tree"       // btree.Verify failed for a root
	Owner     Category = "owner"      // a page owned twice
	Bitmap    Category = "bitmap"     // a bit that disagrees with the owners, or a bad bitmap page
	FreeCount Category = "free_count" // free_page_count differs from the bitmaps
)

// Report is the result of a check: counts, and the first problem of each
// category.
type Report struct {
	Trees       int
	OwnedPages  uint64
	FreePages   uint64 // free data pages below pageCount, from the bitmaps
	BitmapPages uint64
	Problems    map[Category]error
}

// OK reports whether no problem was found.
func (r *Report) OK() bool { return len(r.Problems) == 0 }

// Err returns the problems as one CORRUPT error, or nil.
func (r *Report) Err() error {
	if r.OK() {
		return nil
	}
	var list []error
	for _, c := range []Category{Tree, Owner, Bitmap, FreeCount} {
		if err := r.Problems[c]; err != nil {
			list = append(list, fmt.Errorf("%s: %w", c, err))
		}
	}
	return errs.Wrap(errs.CodeCorrupt, "check.Structural", errors.Join(list...), "structural check failed")
}

func (r *Report) add(c Category, err error) {
	if r.Problems == nil {
		r.Problems = make(map[Category]error)
	}
	if r.Problems[c] == nil {
		r.Problems[c] = err
	}
}

// pins is the pin budget of the checker, which holds one page at a time
// besides the values it reads.
type pins struct{}

func (pins) Acquire(int) error { return nil }
func (pins) Release(int)       {}

// Structural checks the published state of r: the trees of the non-null
// roots, the bitmaps up to pageCount and freeCount. Problems in the
// state go to the report; the returned error is only for cancellation.
func Structural(ctx context.Context, r page.Reader, roots [page.NumRoots]page.ID, pageCount, freeCount uint64) (Report, error) {
	var rep Report
	if r.PageCount() != pageCount {
		rep.add(Tree, fmt.Errorf("the reader has %d pages, the state %d", r.PageCount(), pageCount))
		return rep, nil
	}
	owner := make(map[page.ID]int) // page -> root slot that owns it
	for slot, root := range roots {
		if err := ctxErr(ctx); err != nil {
			return rep, err
		}
		if root == 0 {
			continue
		}
		rep.Trees++
		tr, err := btree.Verify(ctx, r, pins{}, root)
		if err != nil {
			if errs.CodeOf(err) == errs.CodeCanceled || errs.CodeOf(err) == errs.CodeDeadline {
				return rep, err
			}
			rep.add(Tree, fmt.Errorf("root %d (page %d): %w", slot, root, err))
		}
		for _, p := range tr.Pages {
			if prev, ok := owner[p]; ok {
				rep.add(Owner, fmt.Errorf("page %d belongs to roots %d and %d", p, prev, slot))
				continue
			}
			owner[p] = slot
		}
	}
	rep.OwnedPages = uint64(len(owner))
	for g := uint64(0); ; g++ {
		bm := page.BitmapPage(g)
		if uint64(bm) >= pageCount {
			break
		}
		if err := ctxErr(ctx); err != nil {
			return rep, err
		}
		rep.BitmapPages++
		if err := checkGroup(ctx, r, g, pageCount, owner, &rep); err != nil {
			rep.add(Bitmap, err)
		}
	}
	for p := range owner {
		if _, _, ok := page.GroupOf(p); !ok || uint64(p) >= pageCount {
			rep.add(Owner, fmt.Errorf("page %d is owned but is not a data page below pageCount", p))
		}
	}
	if rep.FreePages != freeCount {
		rep.add(FreeCount, fmt.Errorf("free_page_count is %d, the bitmaps leave %d pages free", freeCount, rep.FreePages))
	}
	return rep, nil
}

// ctxErr returns CANCELED or DEADLINE once ctx has ended (design §6).
func ctxErr(ctx context.Context) error {
	switch err := ctx.Err(); {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeDeadline, "check.Structural", err, "check")
	default:
		return errs.Wrap(errs.CodeCanceled, "check.Structural", err, "check")
	}
}

// checkGroup compares the bitmap of group g with the owned pages.
func checkGroup(ctx context.Context, r page.Reader, g, pageCount uint64, owner map[page.ID]int, rep *Report) error {
	bm := page.BitmapPage(g)
	ref, err := r.Page(ctx, pins{}, bm)
	if err != nil {
		return fmt.Errorf("bitmap page %d: %w", bm, err)
	}
	defer ref.Release()
	b := ref.Bytes()
	if h := page.HeaderOf(b); h.Type() != page.TypeBitmap {
		return fmt.Errorf("page %d has type %d instead of a bitmap", bm, h.Type())
	}
	var first error
	for i := 0; i < page.GroupPages; i++ {
		id := bm + 1 + page.ID(i)
		set := page.BitmapBit(b, i)
		if uint64(id) >= pageCount {
			if set && first == nil {
				first = fmt.Errorf("page %d is marked allocated beyond pageCount %d", id, pageCount)
			}
			continue
		}
		if !set {
			rep.FreePages++ // free_page_count counts every clear bit (FORMAT §11)
		}
		_, owned := owner[id]
		switch {
		case set && !owned && first == nil:
			first = fmt.Errorf("page %d is marked allocated but no tree owns it", id)
		case !set && owned && first == nil:
			first = fmt.Errorf("page %d is owned but marked free", id)
		}
	}
	return first
}
