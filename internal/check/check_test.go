package check

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/btree/btreetest"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var ctx = context.Background()

// build publishes two trees: root 0 with keys user/00..user/39 (one value
// out of the page) and root 3 with a few keys, then frees some pages.
func build(t *testing.T) *vfs.MemFS {
	t.Helper()
	fs := vfs.NewMemFS()
	s, err := btreetest.Create(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	a := alloc.New(tx)
	pins := cache.NewPins(0)
	users, other := btree.New(tx, a, 0), btree.New(tx, a, 0)
	for i := 0; i < 40; i++ {
		v := []byte(strings.Repeat("v", 200))
		if i == 7 {
			v = []byte(strings.Repeat("big", 3000))
		}
		if _, err := users.Put(ctx, pins, []byte(fmt.Sprintf("user/%02d", i)), v); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := other.Put(ctx, pins, []byte{byte('a' + i)}, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 10; i < 30; i++ {
		if _, _, err := users.Delete(ctx, pins, []byte(fmt.Sprintf("user/%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	tx.SetRoot(0, users.Root())
	tx.SetRoot(3, other.Root())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return fs
}

// structural reopens the store of fs and checks it.
func structural(t *testing.T, fs vfs.FS) Report {
	t.Helper()
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := s.State()
	rep, err := Structural(ctx, s.Reader(), st.Roots, st.PageCount, st.FreePageCount)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func readPage(t *testing.T, fs vfs.FS, id page.ID) *page.Buf {
	t.Helper()
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := new(page.Buf)
	if err := f.ReadAt(b[:], int64(id)*page.Size); err != nil {
		t.Fatal(err)
	}
	return b
}

// forgePage writes b over page id with a recomputed CRC, as a forged file
// would (SPEC §20.11).
func forgePage(t *testing.T, fs vfs.FS, id page.ID, b *page.Buf) {
	t.Helper()
	page.Seal(b)
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.WriteAt(b[:], int64(id)*page.Size); err != nil {
		t.Fatal(err)
	}
}

// forgeState rewrites both superblocks with a changed state.
func forgeState(t *testing.T, fs vfs.FS, change func(*page.Superblock)) {
	t.Helper()
	sb, _, err := page.ChooseSuperblock(readPage(t, fs, 0), readPage(t, fs, 1))
	if err != nil {
		t.Fatal(err)
	}
	change(&sb)
	sb.Generation += 2
	var b page.Buf
	sb.Encode(&b)
	f, err := fs.Open(btreetest.FileName)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, off := range []int64{0, page.Size} {
		if err := f.WriteAt(b[:], off); err != nil {
			t.Fatal(err)
		}
	}
}

func state(t *testing.T, fs vfs.FS) page.Superblock {
	t.Helper()
	sb, _, err := page.ChooseSuperblock(readPage(t, fs, 0), readPage(t, fs, 1))
	if err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestValidState(t *testing.T) {
	fs := build(t)
	rep := structural(t, fs)
	if !rep.OK() || rep.Trees != 2 || rep.BitmapPages != 1 {
		t.Fatalf("report %+v: %v", rep, rep.Err())
	}
	st := state(t, fs)
	if rep.OwnedPages+rep.FreePages+3 != st.PageCount || rep.FreePages == 0 {
		t.Fatalf("%d owned + %d free + 3 != %d pages", rep.OwnedPages, rep.FreePages, st.PageCount)
	}
}

func expect(t *testing.T, rep Report, c Category) {
	t.Helper()
	if rep.Problems[c] == nil {
		t.Fatalf("no %s problem in %v", c, rep.Err())
	}
	t.Logf("%s: %v", c, rep.Problems[c])
}

func TestPageWithTwoOwners(t *testing.T) {
	fs := build(t)
	forgeState(t, fs, func(sb *page.Superblock) { sb.Roots[5] = sb.Roots[3] })
	expect(t, structural(t, fs), Owner)
}

func TestWrongBitmapBit(t *testing.T) {
	fs := build(t)
	b := readPage(t, fs, page.FirstBitmap)
	free := -1
	for i := 0; page.FirstBitmap+1+page.ID(i) < page.ID(state(t, fs).PageCount); i++ {
		if !page.BitmapBit(b, i) {
			free = i
			break
		}
	}
	if free < 0 {
		t.Fatal("no free page to mark")
	}
	page.SetBitmapBit(b, free, true)
	forgePage(t, fs, page.FirstBitmap, b)
	expect(t, structural(t, fs), Bitmap)
}

func TestWrongFreeCount(t *testing.T) {
	fs := build(t)
	forgeState(t, fs, func(sb *page.Superblock) { sb.FreePageCount++ })
	expect(t, structural(t, fs), FreeCount)
}

// A value whose extent runs past pageCount.
func TestExtentBeyondPageCount(t *testing.T) {
	fs := build(t)
	pc := state(t, fs).PageCount
	for id := page.ID(3); uint64(id) < pc; id++ {
		b := readPage(t, fs, id)
		h := page.HeaderOf(b)
		if h.Type() != page.TypeLeaf {
			continue
		}
		cells := page.NodeOf(b).LeafCells()
		for i := range cells {
			if out := cells[i].Out; out != nil {
				out.Extents = []page.Extent{{Start: page.ID(pc), Pages: out.Extents[0].Pages}}
				commit := h.CommitID()
				if err := page.EncodeLeaf(b, id, cells, h.Sibling()); err != nil {
					t.Fatal(err)
				}
				page.HeaderOf(b).SetCommitID(commit)
				forgePage(t, fs, id, b)
				expect(t, structural(t, fs), Tree)
				return
			}
		}
	}
	t.Fatal("no value out of the page")
}

// A leaf whose keys share a prefix, written without it: valid, but not
// canonical.
func TestPrefixNotMaximal(t *testing.T) {
	fs := build(t)
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var leaf page.ID
	var cells []page.LeafCell
	var next page.ID
	for id := page.ID(3); uint64(id) < s.State().PageCount && leaf == 0; id++ {
		b := readPage(t, fs, id)
		h := page.HeaderOf(b)
		if h.Type() != page.TypeLeaf || h.Flags()&page.FlagPrefix == 0 {
			continue
		}
		if cs := page.NodeOf(b).LeafCells(); !slices.ContainsFunc(cs, func(c page.LeafCell) bool { return c.Out != nil }) {
			leaf, cells, next = id, cs, h.Sibling()
		}
	}
	commit := s.State().CommitID
	s.Close()
	if leaf == 0 {
		t.Fatal("no leaf with a prefix")
	}
	forgePage(t, fs, leaf, leafWithoutPrefix(leaf, cells, next, commit))
	expect(t, structural(t, fs), Tree)
}

// leafWithoutPrefix lays out a leaf of inline cells by hand, without the
// prefix the canonical form requires (header layout of FORMAT.md §6).
func leafWithoutPrefix(id page.ID, cells []page.LeafCell, next page.ID, commit uint64) *page.Buf {
	b := new(page.Buf)
	le := binary.LittleEndian
	b[0] = byte(page.TypeLeaf)
	le.PutUint16(b[2:], page.HeaderSize)
	le.PutUint64(b[8:], uint64(id))
	le.PutUint64(b[16:], commit)
	le.PutUint16(b[24:], uint16(len(cells)))
	le.PutUint64(b[32:], uint64(next))
	upper := page.Size
	var all [][]byte
	for _, c := range cells {
		cell := []byte{0, byte(len(c.Key))}
		cell = binary.AppendUvarint(cell, uint64(len(c.Value)))
		all = append(all, append(append(cell, c.Key...), c.Value...))
	}
	for i := len(all) - 1; i >= 0; i-- {
		upper -= len(all[i])
		copy(b[upper:], all[i])
		le.PutUint16(b[page.HeaderSize+4*i:], uint16(upper))
		le.PutUint16(b[page.HeaderSize+4*i+2:], uint16(len(all[i])))
	}
	le.PutUint16(b[26:], uint16(page.HeaderSize+4*len(cells)))
	le.PutUint16(b[28:], uint16(upper))
	return b
}

func TestCanceled(t *testing.T) {
	fs := build(t)
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := s.State()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Structural(cctx, s.Reader(), st.Roots, st.PageCount, st.FreePageCount); err == nil {
		t.Fatal("a canceled check returned no error")
	}
}

// findPage returns the first page from 3 for which match holds.
func findPage(t *testing.T, fs vfs.FS, match func(*page.Buf) bool) (page.ID, *page.Buf) {
	t.Helper()
	pc := state(t, fs).PageCount
	for id := page.ID(3); uint64(id) < pc; id++ {
		if b := readPage(t, fs, id); match(b) {
			return id, b
		}
	}
	t.Fatal("no such page")
	return 0, nil
}

// An overflow page with bytes after its useful data is valid but not
// canonical (FORMAT §8).
func TestOverflowGarbage(t *testing.T) {
	fs := build(t)
	id, b := findPage(t, fs, func(b *page.Buf) bool {
		h := page.HeaderOf(b)
		return h.Type() == page.TypeOverflow && h.Count() < page.Usable
	})
	b[page.Size-1] = 0x5a
	forgePage(t, fs, id, b)
	expect(t, structural(t, fs), Tree)
}

func TestBitBeyondPageCount(t *testing.T) {
	fs := build(t)
	b := readPage(t, fs, page.FirstBitmap)
	page.SetBitmapBit(b, int(state(t, fs).PageCount-page.FirstBitmap-1)+5, true)
	forgePage(t, fs, page.FirstBitmap, b)
	expect(t, structural(t, fs), Bitmap)
}

func TestOwnedPageMarkedFree(t *testing.T) {
	fs := build(t)
	b := readPage(t, fs, page.FirstBitmap)
	root := state(t, fs).Roots[3]
	page.SetBitmapBit(b, int(root-page.FirstBitmap-1), false)
	forgePage(t, fs, page.FirstBitmap, b)
	rep := structural(t, fs)
	expect(t, rep, Bitmap)
	expect(t, rep, FreeCount)
}

// Where a bitmap page belongs, the checker requires one.
func TestBitmapPositionHoldsAnotherPage(t *testing.T) {
	fs := build(t)
	var b page.Buf
	if err := page.EncodeOverflow(&b, page.FirstBitmap, []byte("not a bitmap")); err != nil {
		t.Fatal(err)
	}
	forgePage(t, fs, page.FirstBitmap, &b)
	expect(t, structural(t, fs), Bitmap)
}

// A check whose deadline passed ends with DEADLINE, not CANCELED.
func TestDeadline(t *testing.T) {
	fs := build(t)
	s, err := btreetest.Open(ctx, fs, btreetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := s.State()
	dctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if _, err := Structural(dctx, s.Reader(), st.Roots, st.PageCount, st.FreePageCount); errs.CodeOf(err) != errs.CodeDeadline {
		t.Fatalf("expired deadline: %v", err)
	}
}
