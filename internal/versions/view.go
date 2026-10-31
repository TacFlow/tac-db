package versions

import (
	"context"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/wal"
)

// private is a private copy of a page and the savepoint epoch it was made
// in; it is never pruned.
type private struct {
	buf   *page.Buf
	epoch uint64
}

func (*private) Commit() uint64 { return 0 }

// save is a savepoint: O(1) to take and to restore (SPEC §10.4).
type save struct {
	pages    Tree[*private]
	state    wal.State
	epoch    uint64
	charged  int64
	replaced int // length of View.replaced when taken
}

// View reads the pages of one commit, and, for a write transaction, keeps
// its overlay of private copies. It implements page.Reader, and for a
// write transaction page.Writer and the Restores counter that the B+tree
// and the allocator use (design §4.2 of M2). A View is not safe for
// concurrent use.
type View struct {
	b   *Backing
	dir *Directory

	// Write transactions only.
	write    bool
	pages    Tree[*private]
	state    wal.State // the working state: page counts, roots, watermarks
	epoch    uint64
	saves    []save
	restores uint64
	res      *govern.Reservation // overlay memory; nil in tests without a Governor
	charged  int64               // bytes of the private copies alive
	replaced []*private          // copies out of the current tree that savepoints may still hold
	sealed   bool                // Finalize ran: the View is closed
}

// Writer returns a write view over d, whose private copies are charged to
// res (which may be nil).
func (b *Backing) Writer(d *Directory, res *govern.Reservation) *View {
	return &View{b: b, dir: d, write: true, state: d.State, res: res}
}

// Directory returns the directory the view reads.
func (v *View) Directory() *Directory { return v.dir }

func (v *View) closed(op string) error {
	if v.sealed {
		return errs.New(errs.CodeTxClosed, op, "the transaction was finalized")
	}
	return nil
}

// writable fails unless the view is an open write view.
func (v *View) writable(op string) error {
	if !v.write {
		return errs.New(errs.CodeInvalidArgument, op, "a read-only view does not change pages")
	}
	return v.closed(op)
}

// mutable reports whether the setters may change the view; on a closed or
// read-only view they do nothing.
func (v *View) mutable() bool { return v.write && !v.sealed }

// Page returns the private copy of id, or else its version in the
// directory, verified.
func (v *View) Page(ctx context.Context, pins page.Pins, id page.ID) (page.Ref, error) {
	if err := v.closed("versions.Page"); err != nil {
		return nil, err
	}
	if p, ok := v.pages.Get(uint64(id)); ok {
		if err := pins.Acquire(page.Size); err != nil {
			return nil, err
		}
		return &imageRef{img: p.buf, pins: pins}, nil
	}
	if v.write && (id < page.FirstBitmap || uint64(id) >= v.state.PageCount) {
		return nil, corrupt(id, "outside the %d pages of the transaction", v.state.PageCount)
	}
	if v.write && uint64(id) >= v.dir.State.PageCount {
		return nil, corrupt(id, "allocated by the transaction but never written")
	}
	return v.b.page(ctx, pins, v.dir, id)
}

// ReadOverflow reads the useful bytes of the overflow pages of e into dst,
// checking type, page ID and count. An extent larger than
// LargeValueBypassBytes without private copies skips the cache.
func (v *View) ReadOverflow(ctx context.Context, pins page.Pins, e page.Extent, dst []byte) error {
	const op = "versions.ReadOverflow"
	if err := v.closed(op); err != nil {
		return err
	}
	n := uint64(e.Pages)
	if n == 0 || uint64(len(dst)) > n*page.Usable || uint64(len(dst)) <= (n-1)*page.Usable {
		return errs.Newf(errs.CodeInvalidArgument, op, "%d bytes for an extent of %d pages", len(dst), e.Pages)
	}
	bypass := len(dst) > LargeValueBypassBytes
	for id := e.Start; bypass && id < e.Start+page.ID(e.Pages); id++ {
		if _, ok := v.pages.Get(uint64(id)); ok {
			bypass = false
		}
	}
	var direct page.Buf
	for j := uint64(0); j < n; j++ {
		if j%64 == 0 {
			if err := ctx.Err(); err != nil {
				return errs.Wrap(errs.CodeCanceled, op, err, "reading an overflow extent")
			}
		}
		id := e.Start + page.ID(j)
		chunk := dst[j*page.Usable : min(uint64(len(dst)), (j+1)*page.Usable)]
		var b *page.Buf
		var ref page.Ref
		if bypass {
			if uint64(id) >= v.dir.State.PageCount {
				return corrupt(id, "outside the %d pages of commit %d", v.dir.State.PageCount, v.dir.State.CommitID)
			}
			if err := v.b.direct(&direct, v.dir, id); err != nil {
				return err
			}
			b = &direct
		} else {
			r, err := v.Page(ctx, pins, id)
			if err != nil {
				return err
			}
			ref, b = r, r.Bytes()
		}
		h := page.HeaderOf(b)
		ok := h.Type() == page.TypeOverflow && h.ID() == id && h.Count() == len(chunk)
		if ok {
			copy(chunk, page.OverflowData(b))
		}
		if ref != nil {
			ref.Release()
		}
		if !ok {
			return corrupt(id, "is not the overflow page %d of %d useful bytes", j, len(chunk))
		}
	}
	return nil
}

// PageCount returns the page count of the view.
func (v *View) PageCount() uint64 {
	if v.write {
		return v.state.PageCount
	}
	return v.dir.State.PageCount
}

// charge debits one private copy from the overlay reservation.
func (v *View) charge(ctx context.Context) error {
	if v.res != nil {
		if err := v.res.Grow(ctx, page.Size); err != nil {
			return err
		}
	}
	v.charged += page.Size
	return nil
}

// shared reports whether an active savepoint holds the copy p: it was made
// at or before the epoch of the last savepoint. Savepoints form a stack
// with growing epochs, so the last one decides.
func (v *View) shared(p *private) bool {
	return len(v.saves) > 0 && v.saves[len(v.saves)-1].epoch >= p.epoch
}

// Writable returns the private copy of id, copying the page on its first
// write, and again on the first write after a savepoint that holds the
// current copy.
func (v *View) Writable(ctx context.Context, id page.ID) (*page.Buf, error) {
	if err := v.writable("versions.Writable"); err != nil {
		return nil, err
	}
	p, ok := v.pages.Get(uint64(id))
	if ok && !v.shared(p) {
		// No savepoint holds this copy: write it in place, so a statement
		// per savepoint does not pile up copies of the same page.
		p.epoch = v.epoch
		return p.buf, nil
	}
	if !ok && (id < page.FirstBitmap || uint64(id) >= v.dir.State.PageCount) {
		return nil, corrupt(id, "has no version to copy in commit %d", v.dir.State.CommitID)
	}
	buf := new(page.Buf)
	if ok {
		*buf = *p.buf
	} else {
		ref, err := v.b.page(ctx, nopins{}, v.dir, id)
		if err != nil {
			return nil, err // nothing charged yet
		}
		*buf = *ref.Bytes()
		ref.Release()
	}
	if err := v.charge(ctx); err != nil {
		return nil, err
	}
	if ok {
		v.replaced = append(v.replaced, p) // a savepoint holds it
	}
	v.pages = v.pages.Set(uint64(id), &private{buf: buf, epoch: v.epoch})
	return buf, nil
}

// Fresh returns a zeroed private copy of a page just allocated.
func (v *View) Fresh(id page.ID) (*page.Buf, error) {
	if err := v.writable("versions.Fresh"); err != nil {
		return nil, err
	}
	if id < page.FirstBitmap || uint64(id) >= v.state.PageCount {
		return nil, corrupt(id, "outside the %d pages of the transaction", v.state.PageCount)
	}
	p, ok := v.pages.Get(uint64(id))
	if ok && !v.shared(p) { // reuse the copy no savepoint holds
		*p.buf = page.Buf{}
		p.epoch = v.epoch
		return p.buf, nil
	}
	if err := v.charge(context.Background()); err != nil {
		return nil, err
	}
	if ok {
		v.replaced = append(v.replaced, p)
	}
	buf := new(page.Buf)
	v.pages = v.pages.Set(uint64(id), &private{buf: buf, epoch: v.epoch})
	return buf, nil
}

// Discard forgets the private copy of a page that was freed.
func (v *View) Discard(id page.ID) {
	if !v.mutable() {
		return
	}
	p, ok := v.pages.Get(uint64(id))
	if !ok {
		return
	}
	v.pages = v.pages.Delete(uint64(id))
	if v.shared(p) {
		v.replaced = append(v.replaced, p)
	} else {
		v.uncharge(1)
	}
}

// uncharge returns n private copies to the Governor.
func (v *View) uncharge(n int) {
	bytes := int64(n) * page.Size
	if v.res != nil {
		v.res.Shrink(bytes)
	}
	v.charged -= bytes
}

// collect returns to the Governor the replaced copies that no savepoint
// holds any more. With a savepoint per statement, the list empties at
// every Release, so the cost is O(1) per copy.
func (v *View) collect() {
	keep, freed := v.replaced[:0], 0
	for _, p := range v.replaced {
		if v.shared(p) {
			keep = append(keep, p)
		} else {
			freed++
		}
	}
	clear(v.replaced[len(keep):])
	v.replaced = keep
	v.uncharge(freed)
}

// SetPageCount sets the page count; the allocator grows it.
func (v *View) SetPageCount(n uint64) {
	if v.mutable() {
		v.state.PageCount = n
	}
}

// FreePageCount returns free_page_count.
func (v *View) FreePageCount() uint64 { return v.state.FreePageCount }

// SetFreePageCount sets free_page_count.
func (v *View) SetFreePageCount(n uint64) {
	if v.mutable() {
		v.state.FreePageCount = n
	}
}

// Restores counts the savepoint restores of the transaction; it only grows.
func (v *View) Restores() uint64 { return v.restores }

// Root returns root i of the transaction.
func (v *View) Root(i int) page.ID { return v.state.Roots[i] }

// SetRoot sets root i.
func (v *View) SetRoot(i int, id page.ID) {
	if v.mutable() {
		v.state.Roots[i] = id
	}
}

// State returns the working state, for the fields the engine keeps there.
func (v *View) State() *wal.State { return &v.state }

// Savepoint marks the current state in O(1); RollbackTo returns to it. It
// returns -1 on a closed or read-only view.
func (v *View) Savepoint() int {
	if !v.mutable() {
		return -1
	}
	v.saves = append(v.saves, save{pages: v.pages, state: v.state, epoch: v.epoch, charged: v.charged, replaced: len(v.replaced)})
	v.epoch++
	return len(v.saves) - 1
}

// RollbackTo restores savepoint sp in O(1), which stays usable, and drops
// the later ones. The private copies made since are returned to the
// Governor, and every buffer and cursor of the transaction is invalid.
func (v *View) RollbackTo(sp int) error {
	if err := v.writable("versions.RollbackTo"); err != nil {
		return err
	}
	if sp < 0 || sp >= len(v.saves) {
		return errs.Newf(errs.CodeInvalidArgument, "versions.RollbackTo", "no savepoint %d", sp)
	}
	s := v.saves[sp]
	v.pages, v.state = s.pages, s.state
	if extra := v.charged - s.charged; extra > 0 && v.res != nil {
		v.res.Shrink(extra)
	}
	v.charged = s.charged
	clear(v.replaced[s.replaced:])
	v.replaced = v.replaced[:s.replaced] // back in the restored tree, or gone with it
	v.saves = v.saves[:sp+1]
	v.epoch++
	v.restores++
	return nil
}

// Release forgets savepoint sp and the later ones, after the statement
// they protected succeeded; its copies stay in the transaction.
func (v *View) Release(sp int) {
	if v.mutable() && sp >= 0 && sp < len(v.saves) {
		v.saves = v.saves[:sp]
		v.collect()
	}
}

// Final is a finalized transaction, ready to seal.
type Final struct {
	Dir    *Directory  // the directory of the new commit, with sealed entries
	Pages  []*page.Buf // the images, in PageID order, with CommitID and CRC
	Sealed []*Sealed   // one per page, in the same order
}

// Finalize seals the private copies under commit, a child of parent: each
// page gets the CommitID and its CRC, must pass page.Verify and be in
// canonical form (the B+tree of M1 writes canonical pages), and becomes a
// sealed version of the new directory. Entries at or below the base T are
// pruned on the way, up to pruneBudget nodes. The View is closed after,
// also when Finalize fails, since the private copies are already stamped.
// The cost is O(D·log N) for D private copies. The engine builds the
// manifest from Final.Pages, whose CRC fields are set.
func (v *View) Finalize(commit, parent uint64, pruneBudget int) (*Final, error) {
	const op = "versions.Finalize"
	if err := v.writable(op); err != nil {
		return nil, err
	}
	v.sealed = true
	state := v.state
	state.CommitID, state.Parent = commit, parent
	f := &Final{}
	var versions []Version
	var err error
	v.pages.All(func(k uint64, p *private) bool {
		id := page.ID(k)
		page.HeaderOf(p.buf).SetCommitID(commit)
		page.Seal(p.buf)
		if err = page.Verify(p.buf, id, state.PageCount); err != nil {
			return false
		}
		if err = page.Canonical(p.buf); err != nil {
			return false
		}
		s := NewSealed(commit, p.buf)
		f.Pages, f.Sealed = append(f.Pages, p.buf), append(f.Sealed, s)
		versions = append(versions, Version{ID: id, Loc: SealedLoc(s)})
		return true
	})
	if err != nil {
		return nil, err
	}
	f.Dir = v.dir.Apply(state, versions).Prune(v.b.Base(), pruneBudget)
	return f, nil
}

// nopins is the pin budget of the view's own reads, which hold one page
// at a time.
type nopins struct{}

func (nopins) Acquire(int) error { return nil }
func (nopins) Release(int)       {}
