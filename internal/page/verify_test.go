package page

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
)

// forge applies a change to a valid page and reseals it, the way a forged
// file with a recomputed CRC would look (SPEC §20.11).
func forge(b *Buf, change func(*Buf)) *Buf {
	f := *b
	change(&f)
	Seal(&f)
	return &f
}

func validLeaf(t *testing.T) *Buf {
	return mustLeaf(t, 50, []LeafCell{
		inlineCell("key/a", "1"), inlineCell("key/b", "2"), inlineCell("key/c", "3"),
	}, 51)
}

func TestVerifyRejectsForgedPages(t *testing.T) {
	leaf := validLeaf(t)
	slot := func(i int) int { return HeaderSize + SlotSize*i }
	cases := map[string]*Buf{
		"wrong page ID":     forge(leaf, func(b *Buf) { le.PutUint64(b[offID:], 49) }),
		"unknown type":      forge(leaf, func(b *Buf) { b[offType] = 9 }),
		"unknown flag":      forge(leaf, func(b *Buf) { b[offFlags] |= 0x80 }),
		"header bytes":      forge(leaf, func(b *Buf) { le.PutUint16(b[offHdrBytes:], 40) }),
		"lower":             forge(leaf, func(b *Buf) { le.PutUint16(b[offLower:], 100) }),
		"leaf with level":   forge(leaf, func(b *Buf) { le.PutUint16(b[offLevel:], 1) }),
		"sibling too far":   forge(leaf, func(b *Buf) { le.PutUint64(b[offSibling:], testPageCount) }),
		"sibling is bitmap": forge(leaf, func(b *Buf) { le.PutUint64(b[offSibling:], uint64(BitmapPage(0))) }),
		"slot past end":     forge(leaf, func(b *Buf) { le.PutUint16(b[slot(1)+2:], 4000) }),
		"overlapping slots": forge(leaf, func(b *Buf) { copy(b[slot(2):slot(2)+4], b[slot(1):slot(1)+4]) }),
		"keys out of order": forge(leaf, func(b *Buf) {
			s1, s2 := b[slot(1):slot(1)+4], b[slot(2):slot(2)+4]
			var tmp [4]byte
			copy(tmp[:], s1)
			copy(s1, s2)
			copy(s2, tmp[:])
		}),
		"no cells": forge(leaf, func(b *Buf) { le.PutUint16(b[offCount:], 1) }),
	}
	for name, b := range cases {
		if err := Verify(b, 50, testPageCount); !isCorrupt(err) {
			t.Errorf("%s: %v, want CORRUPT", name, err)
		}
	}
	flipped := *leaf
	flipped[100] ^= 1
	if err := Verify(&flipped, 50, testPageCount); !isCorrupt(err) {
		t.Errorf("bit flip without a new CRC: %v, want CORRUPT", err)
	}
}

func TestVerifyRejectsBadValues(t *testing.T) {
	ext := &OutRef{Len: 3 * Usable, Extents: []Extent{{Start: 10, Pages: 3}}}
	leaf := mustLeaf(t, 50, []LeafCell{{Key: []byte("k"), Out: ext}}, 0)
	if err := Verify(leaf, 50, 12); !isCorrupt(err) {
		t.Errorf("extent beyond pageCount: %v, want CORRUPT", err)
	}
	cross := new(Buf)
	if err := EncodeLeaf(cross, 50, []LeafCell{{Key: []byte("k"),
		Out: &OutRef{Len: 3 * Usable, Extents: []Extent{{Start: BitmapPage(1) - 1, Pages: 3}}}}}, 0); err != nil {
		t.Fatal(err)
	}
	Seal(cross)
	if err := Verify(cross, 50, testPageCount); !isCorrupt(err) {
		t.Errorf("extent across a bitmap page: %v, want CORRUPT", err)
	}
	// An inline value above the inline rule, written by hand.
	big := strings.Repeat("v", 1400)
	w := newWriter(new(Buf), TypeLeaf, 50, 1, 0, nil)
	cell := append(w.begin(), 0, 1, 0xf8, 0x0a) // flags, suffix 1, value 1400 (uvarint f8 0a)
	cell = append(append(cell, 'k'), big...)
	w.end(cell)
	w.finish()
	Seal(w.b)
	if err := Verify(w.b, 50, testPageCount); !isCorrupt(err) {
		t.Errorf("inline value of 1400 bytes: %v, want CORRUPT", err)
	}
}

// A page that is valid but not canonical passes Verify and fails
// Canonical.
func TestCanonicalDetectsMissingPrefix(t *testing.T) {
	long := strings.Repeat("p", 40)
	cells := []LeafCell{inlineCell(long+"a", "1"), inlineCell(long+"b", "2")}
	b := new(Buf)
	w := newWriter(b, TypeLeaf, 60, len(cells), 0, nil) // no prefix, against the rule
	for i := range cells {
		c := w.begin()
		c = append(c, 0, byte(len(cells[i].Key)), byte(len(cells[i].Value)))
		c = append(append(c, cells[i].Key...), cells[i].Value...)
		w.end(c)
	}
	w.finish()
	Seal(b)
	if err := Verify(b, 60, testPageCount); err != nil {
		t.Fatalf("valid non-canonical page: %v", err)
	}
	if err := Canonical(b); !isCorrupt(err) {
		t.Fatalf("Canonical: %v, want CORRUPT", err)
	}
}

func TestOtherPages(t *testing.T) {
	var b Buf
	if err := EncodeOverflow(&b, 70, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	Seal(&b)
	if err := Verify(&b, 70, testPageCount); err != nil || string(OverflowData(&b)) != "payload" {
		t.Fatalf("overflow: %v, %q", err, OverflowData(&b))
	}
	tail := forge(&b, func(b *Buf) { b[Size-1] = 1 })
	if err := Verify(tail, 70, testPageCount); err != nil {
		t.Fatalf("overflow with a dirty tail is valid: %v", err)
	}
	if err := Canonical(tail); !isCorrupt(err) {
		t.Fatalf("overflow with a dirty tail is not canonical: %v", err)
	}

	extents := []Extent{{3, 1}, {5, 1}, {7, 1}, {9, 1}, {11, 1}}
	if err := EncodeDescriptor(&b, 71, extents); err != nil {
		t.Fatal(err)
	}
	Seal(&b)
	got, err := DescriptorExtents(&b)
	if err != nil || len(got) != 5 || got[4] != (Extent{11, 1}) {
		t.Fatalf("descriptor: %v, %v", got, err)
	}
	if err := EncodeDescriptor(&b, 71, extents[:4]); err == nil {
		t.Fatal("a descriptor with four extents must be refused")
	}

	if err := EncodeBitmap(&b, BitmapPage(1)); err != nil {
		t.Fatal(err)
	}
	SetBitmapBit(&b, 0, true)
	SetBitmapBit(&b, 9, true)
	Seal(&b)
	if err := Verify(&b, BitmapPage(1), testPageCount); err != nil {
		t.Fatal(err)
	}
	if b[HeaderSize] != 0x01 || b[HeaderSize+1] != 0x02 || !BitmapBit(&b, 9) || BitmapBit(&b, 8) {
		t.Fatalf("bit order: % x", b[HeaderSize:HeaderSize+2])
	}
	if err := EncodeBitmap(&b, 7); err == nil {
		t.Fatal("page 7 is not a bitmap page")
	}

	EncodeFree(&b, 80)
	Seal(&b)
	if err := Verify(&b, 80, testPageCount); err != nil {
		t.Fatal(err)
	}
	if err := Verify(forge(&b, func(b *Buf) { b[200] = 1 }), 80, testPageCount); !isCorrupt(err) {
		t.Fatalf("free page with data: %v, want CORRUPT", err)
	}
}

func TestCheckExtentsCanonical(t *testing.T) {
	const pc = 10000
	ok := []Extent{{Start: 10, Pages: 2}, {Start: 20, Pages: 1}}
	if err := CheckExtents(ok, 3*Usable, pc); err != nil {
		t.Fatalf("valid extents: %v", err)
	}
	if err := CheckExtents(ok, 3*Usable+1, pc); !isCorrupt(err) {
		t.Errorf("one page short: %v, want CORRUPT", err)
	}
	contiguous := []Extent{{Start: 10, Pages: 2}, {Start: 12, Pages: 1}}
	if err := CheckExtents(contiguous, 3*Usable, pc); !isCorrupt(err) {
		t.Errorf("contiguous extents: %v, want CORRUPT", err)
	}
}

// A separator length near 2^64 must not overflow the cell length check
// (found by review: Verify panicked on such a forged page).
func TestVerifyHugeSeparatorLength(t *testing.T) {
	b := new(Buf)
	if err := EncodeInternal(b, 60, 1, 61, []InternalCell{{Key: []byte("k"), Right: 62}}); err != nil {
		t.Fatal(err)
	}
	off := int(le.Uint16(b[HeaderSize:])) // the only cell: 1 + 8 + 1 bytes
	for x := 0; x < 8; x++ {
		forged := forge(b, func(b *Buf) {
			// The minimal uvarint of 2^64-8+x takes the 10 bytes of the cell.
			copy(b[off:off+10], codec.AppendUvarint(nil, 1<<64-8+uint64(x)))
		})
		if err := Verify(forged, 60, testPageCount); !isCorrupt(err) {
			t.Fatalf("x=%d: %v, want CORRUPT", x, err)
		}
	}
}

// A length written as a non-minimal uvarint is CORRUPT (SPEC §7.4).
func TestVerifyNonMinimalUvarintInCell(t *testing.T) {
	leaf := mustLeaf(t, 50, []LeafCell{inlineCell("ab", "")}, 0)
	off := int(le.Uint16(leaf[HeaderSize:]))
	if !bytes.Equal(leaf[off:off+5], []byte{0, 2, 0, 'a', 'b'}) {
		t.Fatalf("unexpected cell % x", leaf[off:off+5])
	}
	// Suffix length 1 as 0x81 0x00, value length 0, suffix "b".
	forged := forge(leaf, func(b *Buf) { copy(b[off:], []byte{0, 0x81, 0x00, 0, 'b'}) })
	if err := Verify(forged, 50, testPageCount); !isCorrupt(err) {
		t.Fatalf("non-minimal uvarint: %v, want CORRUPT", err)
	}
}

// The header of a fixed page (its payload belongs to M8).
func TestVerifyFixedHeader(t *testing.T) {
	fixed := func(change func(*Buf)) *Buf {
		b := new(Buf)
		initHeader(b, TypeFixed, 90)
		le.PutUint64(b[offSibling:], 5) // the index ID
		le.PutUint16(b[offCount:], 100)
		change(b)
		Seal(b)
		return b
	}
	if err := Verify(fixed(func(*Buf) {}), 90, testPageCount); err != nil {
		t.Fatalf("valid fixed page: %v", err)
	}
	for name, change := range map[string]func(*Buf){
		"no index ID":    func(b *Buf) { le.PutUint64(b[offSibling:], 0) },
		"count too big":  func(b *Buf) { le.PutUint16(b[offCount:], Usable+1) },
		"level":          func(b *Buf) { le.PutUint16(b[offLevel:], 1) },
		"first child":    func(b *Buf) { le.PutUint64(b[offAux:], 3) },
		"flags":          func(b *Buf) { b[offFlags] = FlagPrefix },
		"lower or upper": func(b *Buf) { le.PutUint16(b[offLower:], 60) },
	} {
		if err := Verify(fixed(change), 90, testPageCount); !isCorrupt(err) {
			t.Errorf("%s: %v, want CORRUPT", name, err)
		}
	}
}

func TestDescriptorExtentsRejects(t *testing.T) {
	var b Buf
	extents := []Extent{{3, 1}, {5, 1}, {7, 1}, {9, 1}, {11, 1}}
	if err := EncodeDescriptor(&b, 71, extents); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Buf){
		"not an overflow page": func(b *Buf) { b[offType] = byte(TypeLeaf) },
		"partial extent":       func(b *Buf) { le.PutUint16(b[offCount:], 5*ExtentSize+1) },
		"four extents":         func(b *Buf) { le.PutUint16(b[offCount:], 4*ExtentSize) },
	} {
		f := b
		change(&f)
		if _, err := DescriptorExtents(&f); !isCorrupt(err) {
			t.Errorf("%s: %v, want CORRUPT", name, err)
		}
	}
}

// InternalPageSize is the size the encoder really uses.
func TestInternalPageSizeMatchesEncoding(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	for i := 0; i < 2000; i++ {
		cells := make([]InternalCell, 1+r.IntN(40))
		for j := range cells {
			cells[j] = InternalCell{Key: []byte(fmt.Sprintf("%s%06d", strings.Repeat("p", r.IntN(30)), j)), Right: ID(10 + j)}
		}
		sort.Slice(cells, func(a, b int) bool { return bytes.Compare(cells[a].Key, cells[b].Key) < 0 })
		if hasDupKeys(cells) {
			continue
		}
		size := InternalPageSize(cells)
		if size > Size {
			continue
		}
		b := new(Buf)
		if err := EncodeInternal(b, 9, 1, 8, cells); err != nil {
			t.Fatal(err)
		}
		if got := usedBytes(b); got != size {
			t.Fatalf("InternalPageSize = %d, page uses %d", size, got)
		}
	}
}

func hasDupKeys(cells []InternalCell) bool {
	for i := 1; i < len(cells); i++ {
		if bytes.Equal(cells[i-1].Key, cells[i].Key) {
			return true
		}
	}
	return false
}

// CheckExtent never overflows, even with a pageCount no superblock allows.
func TestCheckExtentHugePageCount(t *testing.T) {
	const pc = 1<<64 - 1
	start := ID(pc - 1)
	if IsBitmapPage(start) {
		start--
	}
	if err := CheckExtent(Extent{Start: start, Pages: 3}, pc); !isCorrupt(err) {
		t.Fatalf("extent past 2^64: %v, want CORRUPT", err)
	}
}

// EncodeLeaf refuses a value above MaxValue instead of overflowing the
// inline rule.
func TestEncodeLeafRejectsHugeValue(t *testing.T) {
	for _, n := range []uint64{MaxValue + 1, 1<<64 - 1} {
		cell := LeafCell{Key: []byte("k"), Out: &OutRef{Len: n, Extents: []Extent{{Start: 10, Pages: 1}}}}
		err := EncodeLeaf(new(Buf), 9, []LeafCell{cell}, 0)
		if !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) || !strings.Contains(err.Error(), "above") {
			t.Fatalf("value of %d bytes: %v", n, err)
		}
	}
}
