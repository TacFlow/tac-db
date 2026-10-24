// Package alloc allocates pages with the bitmaps of the allocation groups
// (SPEC §8.4). It reads and writes the bitmap pages through the page.Writer
// of the transaction, since the bitmap is versioned like any page, and
// keeps pageCount and free_page_count there. The physical growth of the
// file belongs to the layer that writes it.
package alloc

import (
	"context"
	"encoding/binary"
	"math/bits"
	"slices"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
)

// Allocator allocates the pages of one transaction. It is not safe for
// concurrent use.
type Allocator struct {
	w page.Writer
	// free counts the free pages of each group below pageCount. It is a
	// cache, not format: rebuilt from the bitmap on demand, kept by deltas
	// and dropped when the transaction restores a savepoint.
	free     map[uint64]int
	restores uint64
}

// New returns the allocator of the transaction of w. If w has a
// Restores() uint64 method, a savepoint restore drops the cached summary.
func New(w page.Writer) *Allocator { return &Allocator{w: w} }

type restorer interface{ Restores() uint64 }

func (a *Allocator) sync() {
	if r, ok := a.w.(restorer); ok && r.Restores() != a.restores {
		a.free, a.restores = nil, r.Restores()
	}
	if a.free == nil {
		a.free = make(map[uint64]int)
	}
}

// nopins is the pin budget of the allocator, which holds one bitmap page
// at a time.
type nopins struct{}

func (nopins) Acquire(int) error { return nil }
func (nopins) Release(int)       {}

// groups returns the number of groups whose bitmap page is below pageCount.
func groups(pageCount uint64) uint64 {
	if pageCount <= page.FirstBitmap {
		return 0
	}
	return (pageCount-page.FirstBitmap-1)/page.GroupStride + 1
}

// groupLen returns how many data pages of group g lie below pageCount.
func groupLen(g, pageCount uint64) int {
	first := uint64(page.BitmapPage(g)) + 1
	if pageCount <= first {
		return 0
	}
	return int(min(page.GroupPages, pageCount-first))
}

// bitmap reads the bitmap page of group g, checking its type.
func (a *Allocator) bitmap(ctx context.Context, g uint64) (page.Ref, error) {
	id := page.BitmapPage(g)
	ref, err := a.w.Page(ctx, nopins{}, id)
	if err != nil {
		return nil, err
	}
	if h := page.HeaderOf(ref.Bytes()); h.Type() != page.TypeBitmap || h.ID() != id {
		ref.Release()
		return nil, errs.Newf(errs.CodeCorrupt, "alloc", "page %d is not the bitmap of group %d", id, g)
	}
	return ref, nil
}

// freeIn returns the number of free pages of group g below pageCount.
func (a *Allocator) freeIn(ctx context.Context, g uint64) (int, error) {
	if n, ok := a.free[g]; ok {
		return n, nil
	}
	ref, err := a.bitmap(ctx, g)
	if err != nil {
		return 0, err
	}
	defer ref.Release()
	n := groupLen(g, a.w.PageCount())
	used := 0
	b := ref.Bytes()
	for i := 0; i < n; i += 64 {
		w := binary.LittleEndian.Uint64(b[page.HeaderSize+i/8:])
		if left := n - i; left < 64 {
			w &= 1<<left - 1
		}
		used += bits.OnesCount64(w)
	}
	a.free[g] = n - used
	return n - used, nil
}

// nextBit returns the first index in [from, n) whose bit equals set, or n.
func nextBit(b *page.Buf, from, n int, set bool) int {
	for i := from; i < n; {
		if i%64 == 0 && i+64 <= n {
			w := binary.LittleEndian.Uint64(b[page.HeaderSize+i/8:])
			if !set {
				w = ^w
			}
			if w == 0 {
				i += 64
				continue
			}
			return i + bits.TrailingZeros64(w)
		}
		if page.BitmapBit(b, i) == set {
			return i
		}
		i++
	}
	return n
}

// order returns the groups to search: the group of hint first, then the
// others from the lowest address (locality hint, SPEC §8.4).
func order(hint page.ID, pageCount uint64) []uint64 {
	n := groups(pageCount)
	out := make([]uint64, 0, n)
	first, _, ok := page.GroupOf(hint)
	if ok && first < n {
		out = append(out, first)
	}
	for g := uint64(0); g < n; g++ {
		if !ok || g != first {
			out = append(out, g)
		}
	}
	return out
}

// Alloc allocates one page: the lowest free page of the group of hint, or
// else the lowest free page, or else a page at the end of the file.
func (a *Allocator) Alloc(ctx context.Context, hint page.ID) (page.ID, error) {
	a.sync()
	pc := a.w.PageCount()
	for _, g := range order(hint, pc) {
		n, err := a.freeIn(ctx, g)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		ref, err := a.bitmap(ctx, g)
		if err != nil {
			return 0, err
		}
		i := nextBit(ref.Bytes(), 0, groupLen(g, pc), false)
		ref.Release()
		if i == groupLen(g, pc) {
			return 0, errs.Newf(errs.CodeCorrupt, "alloc.Alloc", "group %d has no free page, its summary %d", g, n)
		}
		id := page.BitmapPage(g) + 1 + page.ID(i)
		if err := a.mark(ctx, []page.Extent{{Start: id, Pages: 1}}, true, true); err != nil {
			return 0, err
		}
		return id, nil
	}
	ext, err := a.grow(ctx, 1)
	if err != nil {
		return 0, err
	}
	return ext[0].Start, nil
}

// AllocExtents allocates n pages as at most page.MaxExtents extents, in
// the order the value fills them. It prefers one contiguous run below
// pageCount; else free runs from the lowest address, completed at the end
// of the file; and when that would take more than MaxExtents extents, a
// run at the end of the file, broken only by bitmap pages (R13-02).
func (a *Allocator) AllocExtents(ctx context.Context, n int, hint page.ID) ([]page.Extent, error) {
	if n < 1 {
		return nil, errs.Newf(errs.CodeInvalidArgument, "alloc.AllocExtents", "%d pages", n)
	}
	a.sync()
	pc := a.w.PageCount()
	var runs []page.Extent
	have := 0
	for _, g := range order(hint, pc) {
		free, err := a.freeIn(ctx, g)
		if err != nil {
			return nil, err
		}
		if free == 0 {
			continue
		}
		groupRuns, err := a.runs(ctx, g, pc)
		if err != nil {
			return nil, err
		}
		for _, r := range groupRuns {
			if int(r.Pages) >= n { // one contiguous run is enough
				one := []page.Extent{{Start: r.Start, Pages: uint32(n)}}
				return one, a.mark(ctx, one, true, true)
			}
			if have < n && len(runs) < page.MaxExtents {
				take := min(int(r.Pages), n-have)
				runs = append(runs, page.Extent{Start: r.Start, Pages: uint32(take)})
				have += take
			}
		}
	}
	if have == n && len(runs) <= page.MaxExtents {
		return runs, a.mark(ctx, runs, true, true)
	}
	// Free runs plus the end of the file, if that stays within the limit.
	tail := tailExtents(pc, n-have)
	if have < n && len(runs)+tail <= page.MaxExtents {
		if err := a.mark(ctx, runs, true, true); err != nil {
			return nil, err
		}
		more, err := a.grow(ctx, n-have)
		if err != nil {
			return nil, err
		}
		return merge(append(runs, more...)), nil
	}
	if tailExtents(pc, n) > page.MaxExtents {
		return nil, errs.Newf(errs.CodeLimitExceeded, "alloc.AllocExtents", "%d pages need more than %d extents", n, page.MaxExtents)
	}
	return a.grow(ctx, n)
}

// tailExtents returns how many extents n pages take at the end of a file
// of pageCount pages.
func tailExtents(pageCount uint64, n int) int {
	count := 0
	for pc := pageCount; n > 0; {
		if page.IsBitmapPage(page.ID(pc)) {
			pc++
			continue
		}
		toBitmap := page.GroupStride - (pc-page.FirstBitmap)%page.GroupStride
		k := min(uint64(n), toBitmap)
		count++
		n -= int(k)
		pc += k
	}
	return count
}

// merge joins consecutive extents that are physically contiguous: a free
// run that ends where the end of the file began.
func merge(ext []page.Extent) []page.Extent {
	out := ext[:1]
	for _, e := range ext[1:] {
		last := &out[len(out)-1]
		if last.Start+page.ID(last.Pages) == e.Start {
			last.Pages += e.Pages
			continue
		}
		out = append(out, e)
	}
	return out
}

// runs returns the free runs of group g below pageCount, in address order.
func (a *Allocator) runs(ctx context.Context, g, pageCount uint64) ([]page.Extent, error) {
	ref, err := a.bitmap(ctx, g)
	if err != nil {
		return nil, err
	}
	defer ref.Release()
	b := ref.Bytes()
	n := groupLen(g, pageCount)
	base := page.BitmapPage(g) + 1
	var out []page.Extent
	for i := nextBit(b, 0, n, false); i < n; {
		j := nextBit(b, i, n, true)
		out = append(out, page.Extent{Start: base + page.ID(i), Pages: uint32(j - i)})
		i = nextBit(b, j, n, false)
	}
	return out, nil
}

// grow allocates n pages at the end of the file, creating the bitmap page
// of each group it reaches.
func (a *Allocator) grow(ctx context.Context, n int) ([]page.Extent, error) {
	const op = "alloc.grow"
	pc := a.w.PageCount()
	if pc < page.FirstBitmap {
		return nil, errs.Newf(errs.CodeCorrupt, op, "pageCount %d below the superblocks", pc)
	}
	var out []page.Extent
	for n > 0 {
		if pc >= page.MaxPageCount {
			return nil, errs.Newf(errs.CodeNoSpace, op, "the file reached %d pages", pc)
		}
		if id := page.ID(pc); page.IsBitmapPage(id) {
			a.w.SetPageCount(pc + 1)
			buf, err := a.w.Fresh(id)
			if err != nil {
				return nil, err
			}
			if err := page.EncodeBitmap(buf, id); err != nil {
				return nil, err
			}
			pc++
			a.free[uint64(id-page.FirstBitmap)/page.GroupStride] = 0
			continue
		}
		toBitmap := page.GroupStride - (pc-page.FirstBitmap)%page.GroupStride
		k := min(uint64(n), toBitmap, page.MaxPageCount-pc)
		e := page.Extent{Start: page.ID(pc), Pages: uint32(k)}
		a.w.SetPageCount(pc + k)
		if err := a.mark(ctx, []page.Extent{e}, true, false); err != nil {
			return nil, err
		}
		out = append(out, e)
		pc += k
		n -= int(k)
	}
	return out, nil
}

// mark sets or clears the bits of the pages of ext. When counted, the
// pages were free (or become free) below pageCount, and free_page_count
// and the summary change; pages new at the end of the file were never
// free.
func (a *Allocator) mark(ctx context.Context, ext []page.Extent, used, counted bool) error {
	const op = "alloc"
	for _, e := range ext {
		for p := e.Start; p < e.Start+page.ID(e.Pages); {
			g, bit, ok := page.GroupOf(p)
			if !ok {
				return errs.Newf(errs.CodeCorrupt, op, "page %d is not a data page", p)
			}
			buf, err := a.w.Writable(ctx, page.BitmapPage(g))
			if err != nil {
				return err
			}
			// The rest of the extent within this group.
			k := min(int(e.Start+page.ID(e.Pages)-p), page.GroupPages-bit)
			for i := bit; i < bit+k; i++ {
				if page.BitmapBit(buf, i) == used {
					state := "free"
					if used {
						state = "allocated"
					}
					return errs.Newf(errs.CodeCorrupt, op, "page %d is already %s", page.BitmapPage(g)+1+page.ID(i), state)
				}
				page.SetBitmapBit(buf, i, used)
			}
			if counted {
				delta := k
				if used {
					delta = -k
					if a.w.FreePageCount() < uint64(k) {
						return errs.Newf(errs.CodeCorrupt, op, "free_page_count %d below the %d pages taken", a.w.FreePageCount(), k)
					}
				}
				if f, ok := a.free[g]; ok {
					a.free[g] = f + delta
				}
				a.w.SetFreePageCount(uint64(int64(a.w.FreePageCount()) + int64(delta)))
			}
			p += page.ID(k)
		}
	}
	return nil
}

// Free frees one page and forgets its private copy.
func (a *Allocator) Free(ctx context.Context, id page.ID) error {
	return a.FreeExtents(ctx, []page.Extent{{Start: id, Pages: 1}})
}

// FreeExtents frees the pages of ext and forgets their private copies.
// Freeing only changes the bitmap; the old content stays readable by
// earlier snapshots (SPEC §8.4).
func (a *Allocator) FreeExtents(ctx context.Context, ext []page.Extent) error {
	a.sync()
	pc := a.w.PageCount()
	for _, e := range ext {
		if err := page.CheckExtent(e, pc); err != nil {
			return err
		}
	}
	if err := a.mark(ctx, slices.Clone(ext), false, true); err != nil {
		return err
	}
	for _, e := range ext {
		for p := e.Start; p < e.Start+page.ID(e.Pages); p++ {
			a.w.Discard(p)
		}
	}
	return nil
}
