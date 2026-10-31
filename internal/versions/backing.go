package versions

import (
	"context"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

// LargeValueBypassBytes is the size above which an overflow extent without
// private copies is read without the cache (SPEC §8.2).
const LargeValueBypassBytes = 1 << 20

// Backing is what the reads of every snapshot of a database share: the
// page cache, the main file, the WAL segments and the base T, the CommitID
// of the state the main file holds since the last checkpoint. A directory
// entry with a CommitID at or below T resolves to the main file.
type Backing struct {
	UUID    [16]byte
	Cache   *cache.Cache
	Main    vfs.File
	Segment func(seq uint64) (vfs.File, error) // the open segment files
	base    atomic.Uint64
}

// SetBase publishes a new base T, after the checkpoint wrote it to the main
// file and invalidated the cached images it rewrote.
func (b *Backing) SetBase(t uint64) { b.base.Store(t) }

// Base returns the base T.
func (b *Backing) Base() uint64 { return b.base.Load() }

// InvalidateBase drops the cached image of a page of the main file.
func (b *Backing) InvalidateBase(id page.ID) {
	b.Cache.Invalidate(cache.Key{DB: b.UUID, Page: id, Version: 0})
}

func corrupt(id page.ID, format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, "versions", "page %d: "+format, append([]any{id}, args...)...)
}

// imageRef hands out a sealed image, immutable.
type imageRef struct {
	img  *page.Buf
	pins page.Pins
}

func (r *imageRef) Bytes() *page.Buf { return r.img }
func (r *imageRef) Release() {
	if r.pins != nil {
		r.pins.Release(page.Size)
		r.pins = nil
	}
}

// source says where a page version is read from.
type source struct {
	img    *page.Buf   // a sealed image still held
	wal    wal.Locator // else a PAGE record, when inWAL
	inWAL  bool
	commit uint64 // the expected CommitID; 0 when unknown (main file without entry)
}

// resolve finds the version of id in d.
func (b *Backing) resolve(d *Directory, id page.ID) (source, error) {
	if id < page.FirstBitmap || uint64(id) >= d.State.PageCount {
		return source{}, corrupt(id, "outside the %d pages of commit %d", d.State.PageCount, d.State.CommitID)
	}
	loc, ok := d.Lookup(id)
	if !ok {
		return source{}, nil
	}
	if loc.commit <= b.Base() {
		return source{commit: loc.commit}, nil // the main file holds this version
	}
	if loc.sealed == nil {
		return source{wal: loc.wal, inWAL: true, commit: loc.commit}, nil
	}
	st := loc.sealed.state.Load()
	if st.img != nil {
		return source{img: st.img, commit: loc.commit}, nil
	}
	return source{wal: st.loc, inWAL: true, commit: loc.commit}, nil
}

// page reads the version of id in d, verified, through the cache.
func (b *Backing) page(ctx context.Context, pins page.Pins, d *Directory, id page.ID) (page.Ref, error) {
	src, err := b.resolve(d, id)
	if err != nil {
		return nil, err
	}
	if src.img != nil {
		if err := pins.Acquire(page.Size); err != nil {
			return nil, err
		}
		return &imageRef{img: src.img, pins: pins}, nil
	}
	version := uint64(0)
	if src.inWAL {
		version = src.commit
	}
	pc := d.State.PageCount
	ref, err := b.Cache.Get(ctx, pins, cache.Key{DB: b.UUID, Page: id, Version: version}, func(_ context.Context, dst *page.Buf) error {
		return b.load(dst, id, src, pc)
	})
	if err != nil {
		return nil, err
	}
	if src.commit != 0 && page.HeaderOf(ref.Bytes()).CommitID() != src.commit {
		c := page.HeaderOf(ref.Bytes()).CommitID()
		ref.Release()
		return nil, corrupt(id, "holds commit %d where the directory has commit %d", c, src.commit)
	}
	return ref, nil
}

// load reads a version from the WAL or the main file and verifies it.
func (b *Backing) load(dst *page.Buf, id page.ID, src source, pageCount uint64) error {
	if src.inWAL {
		f, err := b.Segment(src.wal.Seq)
		if err != nil {
			return err
		}
		if err := f.ReadAt(dst[:], src.wal.Offset); err != nil {
			return err
		}
	} else if err := b.Main.ReadAt(dst[:], int64(id)*page.Size); err != nil {
		return err
	}
	return page.Verify(dst, id, pageCount)
}

// direct reads a version without the cache, for the large-value bypass.
func (b *Backing) direct(dst *page.Buf, d *Directory, id page.ID) error {
	src, err := b.resolve(d, id)
	if err != nil {
		return err
	}
	if src.img != nil {
		*dst = *src.img
		return nil
	}
	if err := b.load(dst, id, src, d.State.PageCount); err != nil {
		return err
	}
	if src.commit != 0 && page.HeaderOf(dst).CommitID() != src.commit {
		return corrupt(id, "holds commit %d where the directory has commit %d", page.HeaderOf(dst).CommitID(), src.commit)
	}
	return nil
}

// Snapshot returns a read-only view of the directory d.
func (b *Backing) Snapshot(d *Directory) *View { return &View{b: b, dir: d} }
