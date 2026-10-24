package page

import (
	"hash/crc32"
	"testing"
)

// Target names must not be substrings of each other within the package.

// exercise calls every accessor of a page that passed Verify; none may
// panic (design §4.6: Verify is the gate that the accessors rely on).
func exercise(t *testing.T, b *Buf) {
	t.Helper()
	switch HeaderOf(b).Type() {
	case TypeLeaf, TypeInternal:
		n := NodeOf(b)
		for i := 0; i < n.Len(); i++ {
			key := n.AppendKey(nil, i)
			if j, found := n.Search(key); !found || j != i {
				t.Fatalf("cell %d: Search of its own key gave %d, %v", i, j, found)
			}
			if n.Leaf() {
				n.Value(i)
			} else {
				n.Child(i)
				n.ChildFor(key)
			}
		}
		if n.Leaf() {
			n.LeafCells()
		} else {
			n.InternalCells()
			n.Child(n.Len())
		}
	case TypeOverflow:
		OverflowData(b)
		DescriptorExtents(b)
	}
	Canonical(b)
}

// FuzzPage verifies arbitrary bytes as a page.
func FuzzPage(f *testing.F) {
	for _, g := range goldenPages() {
		f.Add(g.b[:], uint64(g.id), uint64(1<<20))
	}
	f.Fuzz(func(t *testing.T, data []byte, id, pageCount uint64) {
		var b Buf
		copy(b[:], data)
		if Verify(&b, ID(id), pageCount) == nil {
			exercise(t, &b)
		}
		// Sealed, the same bytes get past the CRC and reach the structure.
		Seal(&b)
		if Verify(&b, ID(id), pageCount) == nil {
			exercise(t, &b)
		}
	})
}

// FuzzForgedPage changes up to 16 bytes of valid pages and recomputes the
// CRC, as a forged file would (SPEC §20.11, §25.3). Each edit is three
// bytes of edits: a position in two bytes and the new value.
func FuzzForgedPage(f *testing.F) {
	f.Add(uint8(0), []byte{0, 60, 0xff})
	f.Add(uint8(1), []byte{0, 24, 9, 0, 70, 0x80})
	f.Fuzz(func(t *testing.T, which uint8, edits []byte) {
		pages := goldenPages()
		g := pages[int(which)%(len(pages)-1)] // the last one is the superblock
		b := *g.b
		for i := 0; i+3 <= len(edits) && i < 16*3; i += 3 {
			b[(int(edits[i])<<8|int(edits[i+1]))%Size] = edits[i+2]
		}
		Seal(&b)
		if Verify(&b, g.id, 1<<20) == nil {
			exercise(t, &b)
		}
	})
}

// FuzzSuperblock classifies arbitrary bytes as a superblock.
func FuzzSuperblock(f *testing.F) {
	for _, g := range goldenPages() {
		f.Add(g.b[:])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var b Buf
		copy(b[:], data)
		Classify(&b)
		valid := goldenPages()[len(goldenPages())-1].b
		ChooseSuperblock(&b, valid)
	})
}

// FuzzForgedSuperblock changes a valid superblock and recomputes its CRC.
// A copy classified as valid must re-encode to the same bytes.
func FuzzForgedSuperblock(f *testing.F) {
	f.Add(uint16(104), byte(1), uint16(0), byte(0))
	f.Fuzz(func(t *testing.T, pos1 uint16, val1 byte, pos2 uint16, val2 byte) {
		pages := goldenPages()
		b := *pages[len(pages)-1].b
		b[int(pos1)%superblockCRCOff] = val1
		b[int(pos2)%superblockCRCOff] = val2
		le.PutUint32(b[superblockCRCOff:], crc32.Checksum(b[:superblockCRCOff], castagnoli))
		s, kind, _ := Classify(&b)
		if kind != Valid {
			return
		}
		var again Buf
		s.Encode(&again)
		if again != b {
			t.Fatal("a valid superblock does not re-encode to the same bytes")
		}
	})
}

// FuzzBitmapExtents compares CheckExtent with a page-by-page check.
func FuzzBitmapExtents(f *testing.F) {
	f.Add(uint64(GroupStride-1), uint32(5), uint64(3*GroupStride))
	f.Add(uint64(1<<64-2), uint32(3), uint64(1<<64-1))
	f.Fuzz(func(t *testing.T, start uint64, pages uint32, pageCount uint64) {
		if pages > 1<<16 {
			pages %= 1 << 16
		}
		e := Extent{Start: ID(start), Pages: pages}
		want := pages > 0
		for p := uint64(0); want && p < uint64(pages); p++ {
			if start+p < start || CheckPointer(ID(start+p), pageCount) != nil {
				want = false
			}
		}
		if got := CheckExtent(e, pageCount) == nil; got != want {
			t.Fatalf("CheckExtent(%+v, %d) valid = %v, want %v", e, pageCount, got, want)
		}
	})
}
