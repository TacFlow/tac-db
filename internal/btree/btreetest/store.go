// Package btreetest is the page store of the tests of M1 (design §4.10). It
// implements page.Reader and page.Writer in memory, with simple savepoint
// epochs, and publishes each transaction by writing its pages and a G2
// superblock through a VFS, so that tests can close and reopen a database.
// It has no WAL: a crash in the middle of a commit is not survivable, and
// nothing in production uses it. In M2 the overlay and the version
// directory implement the same interfaces.
package btreetest

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"slices"

	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// FileName is the database file of a store inside its VFS.
const FileName = "test.tacdb"

// LargeValueBypassBytes is the size above which an overflow extent is read
// without the cache (SPEC §8.2).
const LargeValueBypassBytes = 1 << 20

// Options configures a store.
type Options struct {
	CacheBytes int64            // default: the minimum of the cache
	Governor   *govern.Governor // default: a private Governor of 1 GiB
}

// Store is one database file.
type Store struct {
	f     vfs.File
	cache *cache.Cache
	sb    page.Superblock // the published state
	ver   map[page.ID]uint64
	tx    *Txn
}

// Create creates the database file in fs, with an empty state.
func Create(ctx context.Context, fs vfs.FS, opts Options) (*Store, error) {
	f, err := fs.Create(FileName)
	if err != nil {
		return nil, err
	}
	sb := page.Superblock{Generation: 1, PageCount: 2}
	rand.Read(sb.UUID[:])
	var b page.Buf
	sb.Encode(&b)
	for _, off := range []int64{0, page.Size} {
		if err := f.WriteAt(b[:], off); err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return newStore(ctx, f, sb, opts)
}

// Open opens the database file of fs, choosing its superblock (SPEC §7.2).
func Open(ctx context.Context, fs vfs.FS, opts Options) (*Store, error) {
	f, err := fs.Open(FileName)
	if err != nil {
		return nil, err
	}
	sb, err := readSuperblock(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return newStore(ctx, f, sb, opts)
}

func readSuperblock(f vfs.File) (page.Superblock, error) {
	var a, b page.Buf
	if err := f.ReadAt(a[:], 0); err != nil {
		return page.Superblock{}, err
	}
	if err := f.ReadAt(b[:], page.Size); err != nil {
		return page.Superblock{}, err
	}
	sb, _, err := page.ChooseSuperblock(&a, &b)
	if err != nil {
		return sb, err
	}
	size, err := f.Size()
	if err != nil {
		return sb, err
	}
	if uint64(size) < sb.PageCount*page.Size {
		return sb, errs.Newf(errs.CodeCorrupt, "btreetest.Open", "file of %d bytes is shorter than %d pages", size, sb.PageCount)
	}
	return sb, nil
}

func newStore(ctx context.Context, f vfs.File, sb page.Superblock, opts Options) (*Store, error) {
	if opts.Governor == nil {
		g, err := govern.New(govern.Config{Limit: 1 << 30})
		if err != nil {
			f.Close()
			return nil, err
		}
		opts.Governor = g
	}
	if opts.CacheBytes == 0 {
		opts.CacheBytes = cache.MinPages * cache.EntryCost
	}
	c, err := cache.New(ctx, cache.Options{Budget: opts.CacheBytes, Governor: opts.Governor})
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Store{f: f, cache: c, sb: sb, ver: make(map[page.ID]uint64)}, nil
}

// Close rolls back an open transaction and closes the file.
func (s *Store) Close() error {
	if s.tx != nil {
		s.tx.Rollback()
	}
	s.cache.Close()
	return s.f.Close()
}

// State returns the published superblock: roots, pageCount and
// free_page_count.
func (s *Store) State() page.Superblock { return s.sb }

// Cache returns the page cache, for tests that look at its counters.
func (s *Store) Cache() *cache.Cache { return s.cache }

// Reader returns a reader of the published state.
func (s *Store) Reader() page.Reader { return published{s} }

// published reads the published state.
type published struct{ s *Store }

func (p published) Page(ctx context.Context, pins page.Pins, id page.ID) (page.Ref, error) {
	return p.s.page(ctx, pins, id)
}

func (p published) ReadOverflow(ctx context.Context, pins page.Pins, e page.Extent, dst []byte) error {
	return readOverflow(ctx, pins, e, dst, p.s.page, p.s)
}

func (p published) PageCount() uint64 { return p.s.sb.PageCount }

// page reads a published page through the cache, verified.
func (s *Store) page(ctx context.Context, pins page.Pins, id page.ID) (page.Ref, error) {
	if id < page.FirstBitmap || uint64(id) >= s.sb.PageCount {
		return nil, errs.Newf(errs.CodeCorrupt, "btreetest.Page", "page %d outside the %d published pages", id, s.sb.PageCount)
	}
	key := cache.Key{DB: s.sb.UUID, Page: id, Version: s.ver[id]}
	return s.cache.Get(ctx, pins, key, func(_ context.Context, dst *page.Buf) error {
		return s.load(dst, id)
	})
}

// load reads a published page from the file and verifies it.
func (s *Store) load(dst *page.Buf, id page.ID) error {
	if err := s.f.ReadAt(dst[:], int64(id)*page.Size); err != nil {
		return err
	}
	return page.Verify(dst, id, s.sb.PageCount)
}

// readOverflow reads the useful bytes of the overflow pages of e into dst,
// checking type, page ID and count. Extents larger than
// LargeValueBypassBytes skip the cache when s is given.
func readOverflow(ctx context.Context, pins page.Pins, e page.Extent, dst []byte,
	get func(context.Context, page.Pins, page.ID) (page.Ref, error), s *Store) error {
	const op = "btreetest.ReadOverflow"
	n := uint64(e.Pages)
	if n == 0 || uint64(len(dst)) > n*page.Usable || uint64(len(dst)) <= (n-1)*page.Usable {
		return errs.Newf(errs.CodeInvalidArgument, op, "%d bytes for an extent of %d pages", len(dst), e.Pages)
	}
	bypass := s != nil && len(dst) > LargeValueBypassBytes
	var direct page.Buf
	for j := uint64(0); j < n; j++ {
		if j%64 == 0 {
			if err := ctxErr(ctx, op); err != nil {
				return err
			}
		}
		id := e.Start + page.ID(j)
		chunk := dst[j*page.Usable : min(uint64(len(dst)), (j+1)*page.Usable)]
		var b *page.Buf
		var ref page.Ref
		if bypass {
			if err := s.load(&direct, id); err != nil {
				return err
			}
			b = &direct
		} else {
			r, err := get(ctx, pins, id)
			if err != nil {
				return err
			}
			ref, b = r, r.Bytes()
		}
		h := page.HeaderOf(b)
		if h.Type() != page.TypeOverflow || h.ID() != id || h.Count() != len(chunk) {
			if ref != nil {
				ref.Release()
			}
			return errs.Newf(errs.CodeCorrupt, op, "page %d is not the overflow page %d of %d useful bytes", id, j, len(chunk))
		}
		copy(chunk, page.OverflowData(b))
		if ref != nil {
			ref.Release()
		}
	}
	return nil
}

// Txn is the transaction of a store; only one is open at a time. It
// implements page.Writer, and Restores for the cursors of the B+tree.
type Txn struct {
	s         *Store
	dirty     map[page.ID]*private
	epoch     uint64
	saves     []savepoint
	restores  uint64
	pageCount uint64
	freeCount uint64
	roots     [page.NumRoots]page.ID
}

type private struct {
	buf   *page.Buf
	epoch uint64
}

type savepoint struct {
	dirty     map[page.ID]*private
	pageCount uint64
	freeCount uint64
	roots     [page.NumRoots]page.ID
}

// Begin starts the transaction.
func (s *Store) Begin() (*Txn, error) {
	if s.tx != nil {
		return nil, errs.New(errs.CodeTxInUse, "btreetest.Begin", "a transaction is open")
	}
	s.tx = &Txn{s: s, dirty: make(map[page.ID]*private), pageCount: s.sb.PageCount,
		freeCount: s.sb.FreePageCount, roots: s.sb.Roots}
	return s.tx, nil
}

func (t *Txn) check(op string, id page.ID) error {
	if t.s.tx != t {
		return errs.New(errs.CodeTxClosed, op, "transaction ended")
	}
	if id < page.FirstBitmap || uint64(id) >= t.pageCount {
		return errs.Newf(errs.CodeCorrupt, op, "page %d outside the %d pages of the transaction", id, t.pageCount)
	}
	return nil
}

// privateRef hands out a private copy; it is valid until the next change
// of the page or the next restore.
type privateRef struct {
	buf  *page.Buf
	pins page.Pins
}

func (r *privateRef) Bytes() *page.Buf { return r.buf }
func (r *privateRef) Release() {
	if r.pins != nil {
		r.pins.Release(page.Size)
		r.pins = nil
	}
}

// Page returns the private copy of id, or else its published version.
func (t *Txn) Page(ctx context.Context, pins page.Pins, id page.ID) (page.Ref, error) {
	if err := t.check("btreetest.Page", id); err != nil {
		return nil, err
	}
	if p, ok := t.dirty[id]; ok {
		if err := pins.Acquire(page.Size); err != nil {
			return nil, err
		}
		return &privateRef{buf: p.buf, pins: pins}, nil
	}
	return t.s.page(ctx, pins, id)
}

// ReadOverflow reads overflow pages, private copies included.
func (t *Txn) ReadOverflow(ctx context.Context, pins page.Pins, e page.Extent, dst []byte) error {
	if err := t.check("btreetest.ReadOverflow", e.Start); err != nil {
		return err
	}
	s := t.s // the bypass reads the file, so only an extent without private copies
	for id := e.Start; id < e.Start+page.ID(e.Pages); id++ {
		if _, ok := t.dirty[id]; ok {
			s = nil
			break
		}
	}
	return readOverflow(ctx, pins, e, dst, t.Page, s)
}

// PageCount returns the page count of the transaction.
func (t *Txn) PageCount() uint64 { return t.pageCount }

// SetPageCount sets the page count; the allocator grows it.
func (t *Txn) SetPageCount(n uint64) { t.pageCount = n }

// FreePageCount returns free_page_count.
func (t *Txn) FreePageCount() uint64 { return t.freeCount }

// SetFreePageCount sets free_page_count.
func (t *Txn) SetFreePageCount(n uint64) { t.freeCount = n }

// Restores counts the savepoint restores of the transaction.
func (t *Txn) Restores() uint64 { return t.restores }

// Writable returns the private copy of id for the current epoch, copying
// the page on its first write in the epoch.
func (t *Txn) Writable(ctx context.Context, id page.ID) (*page.Buf, error) {
	if err := t.check("btreetest.Writable", id); err != nil {
		return nil, err
	}
	p, ok := t.dirty[id]
	if ok && p.epoch == t.epoch {
		return p.buf, nil
	}
	buf := new(page.Buf)
	if ok {
		*buf = *p.buf
	} else {
		ref, err := t.s.page(ctx, nopins{}, id)
		if err != nil {
			return nil, err
		}
		*buf = *ref.Bytes()
		ref.Release()
	}
	t.dirty[id] = &private{buf: buf, epoch: t.epoch}
	return buf, nil
}

// Fresh returns a zeroed private copy of a page just allocated.
func (t *Txn) Fresh(id page.ID) (*page.Buf, error) {
	if err := t.check("btreetest.Fresh", id); err != nil {
		return nil, err
	}
	buf := new(page.Buf)
	t.dirty[id] = &private{buf: buf, epoch: t.epoch}
	return buf, nil
}

// Discard forgets the private copy of a freed page.
func (t *Txn) Discard(id page.ID) { delete(t.dirty, id) }

// Root returns root i of the transaction.
func (t *Txn) Root(i int) page.ID { return t.roots[i] }

// SetRoot sets root i.
func (t *Txn) SetRoot(i int, id page.ID) { t.roots[i] = id }

// Savepoint marks the current state; RollbackTo returns to it.
func (t *Txn) Savepoint() int {
	t.saves = append(t.saves, savepoint{dirty: maps.Clone(t.dirty), pageCount: t.pageCount,
		freeCount: t.freeCount, roots: t.roots})
	t.epoch++
	return len(t.saves) - 1
}

// RollbackTo restores savepoint sp, which stays usable, and drops the
// later ones. Every buffer and cursor of the transaction becomes invalid.
func (t *Txn) RollbackTo(sp int) error {
	if sp < 0 || sp >= len(t.saves) {
		return errs.Newf(errs.CodeInvalidArgument, "btreetest.RollbackTo", "no savepoint %d", sp)
	}
	s := t.saves[sp]
	t.dirty = maps.Clone(s.dirty)
	t.pageCount, t.freeCount, t.roots = s.pageCount, s.freeCount, s.roots
	t.saves = t.saves[:sp+1]
	t.epoch++
	t.restores++
	return nil
}

// Rollback ends the transaction without publishing it.
func (t *Txn) Rollback() {
	if t.s.tx == t {
		t.s.tx = nil
	}
}

// Commit finalizes the private copies and publishes them. A failed Commit
// leaves the file unusable, since pages are written in place without a
// WAL: each page gets
// the new CommitID and its CRC, must pass page.Verify and be in canonical
// form (the M1 B+tree writes canonical pages by construction, so this
// finalization checks instead of normalizing), and is written once; then
// the file is synced and the next superblock is written over the older
// copy and synced.
func (t *Txn) Commit(ctx context.Context) error {
	const op = "btreetest.Commit"
	s := t.s
	if s.tx != t {
		return errs.New(errs.CodeTxClosed, op, "transaction ended")
	}
	defer t.Rollback()
	commit := s.sb.CommitID + 1
	ids := slices.Sorted(maps.Keys(t.dirty))
	for _, id := range ids {
		if err := ctxErr(ctx, op); err != nil {
			return err
		}
		b := t.dirty[id].buf
		page.HeaderOf(b).SetCommitID(commit)
		page.Seal(b)
		if err := page.Verify(b, id, t.pageCount); err != nil {
			return err
		}
		if err := page.Canonical(b); err != nil {
			return err
		}
		if err := s.f.WriteAt(b[:], int64(id)*page.Size); err != nil {
			return err
		}
	}
	if size, err := s.f.Size(); err != nil {
		return err
	} else if want := int64(t.pageCount) * page.Size; size < want {
		if err := s.f.Truncate(want); err != nil { // free pages never written stay zero
			return err
		}
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	sb := s.sb
	sb.Generation++
	sb.CommitID = commit
	sb.PageCount, sb.FreePageCount, sb.Roots = t.pageCount, t.freeCount, t.roots
	var b page.Buf
	sb.Encode(&b)
	if err := s.f.WriteAt(b[:], int64(sb.Generation%2)*page.Size); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.sb = sb
	for _, id := range ids {
		s.ver[id] = commit
	}
	return nil
}

// ctxErr returns CANCELED or DEADLINE once ctx has ended.
func ctxErr(ctx context.Context, op string) error {
	switch err := ctx.Err(); {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeDeadline, op, err, "context ended")
	default:
		return errs.Wrap(errs.CodeCanceled, op, err, "context ended")
	}
}

// nopins is the pin budget of the store's own reads, which hold one page
// at a time.
type nopins struct{}

func (nopins) Acquire(int) error { return nil }
func (nopins) Release(int)       {}
