package page

import (
	"errors"
	"hash/crc32"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func sampleSuperblock(gen uint64) Superblock {
	s := Superblock{
		UUID:          [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Generation:    gen,
		CheckpointLSN: 77,
		CommitID:      12,
		PageCount:     1000,
		FreePageCount: 10,
	}
	s.Roots[0] = 5
	s.Roots[18] = 999
	return s
}

func encodeSB(s Superblock) *Buf {
	b := new(Buf)
	s.Encode(b)
	return b
}

// reseal recomputes the CRC of a superblock after a change.
func reseal(b *Buf, change func(*Buf)) *Buf {
	f := *b
	change(&f)
	le.PutUint32(f[superblockCRCOff:], crc32.Checksum(f[:superblockCRCOff], castagnoli))
	return &f
}

func TestSuperblockRoundTrip(t *testing.T) {
	want := sampleSuperblock(3)
	got, kind, why := Classify(encodeSB(want))
	if kind != Valid || got != want {
		t.Fatalf("Classify = %+v, %v (%s)", got, kind, why)
	}
}

func TestClassify(t *testing.T) {
	base := encodeSB(sampleSuperblock(3))
	cases := map[string]struct {
		b    *Buf
		want Kind
	}{
		"foreign magic":         {reseal(base, func(b *Buf) { copy(b[0:8], "SQLite f") }), Foreign},
		"bad CRC":               {func() *Buf { f := *base; f[100] ^= 1; return &f }(), Damaged},
		"format_version 3":      {reseal(base, func(b *Buf) { le.PutUint16(b[8:], 3) }), Unsupported},
		"format_revision 14":    {reseal(base, func(b *Buf) { le.PutUint16(b[408:], 14) }), Unsupported},
		"min_reader_version 3":  {reseal(base, func(b *Buf) { le.PutUint16(b[10:], 3) }), Unsupported},
		"min_reader_version 1":  {reseal(base, func(b *Buf) { le.PutUint16(b[10:], 1) }), Damaged},
		"page_size 8192":        {reseal(base, func(b *Buf) { le.PutUint32(b[12:], 8192) }), Damaged},
		"unknown flag":          {reseal(base, func(b *Buf) { le.PutUint64(b[120:], 2) }), Damaged},
		"reserved byte":         {reseal(base, func(b *Buf) { b[500] = 1 }), Damaged},
		"reserved root 19":      {reseal(base, func(b *Buf) { le.PutUint64(b[128+8*19:], 5) }), Damaged},
		"root beyond pageCount": {reseal(base, func(b *Buf) { le.PutUint64(b[128:], 1000) }), Damaged},
		"root is a bitmap":      {reseal(base, func(b *Buf) { le.PutUint64(b[128:], 2) }), Damaged},
		"pageCount 1":           {reseal(base, func(b *Buf) { le.PutUint64(b[104:], 1) }), Damaged},
		"free above pageCount":  {reseal(base, func(b *Buf) { le.PutUint64(b[384:], 2000) }), Damaged},
		// A future revision may use reserved fields; the version check comes
		// first, so it is unsupported, never damaged (R13-20).
		"revision 14 with root 19": {reseal(base, func(b *Buf) {
			le.PutUint16(b[408:], 14)
			le.PutUint64(b[128+8*19:], 5)
		}), Unsupported},
	}
	for name, c := range cases {
		if _, kind, why := Classify(c.b); kind != c.want {
			t.Errorf("%s: kind %v (%s), want %v", name, kind, why, c.want)
		}
	}
}

func TestChooseSuperblock(t *testing.T) {
	g3, g4 := encodeSB(sampleSuperblock(3)), encodeSB(sampleSuperblock(4))
	damaged := func() *Buf { f := *g4; f[100] ^= 1; return &f }()
	foreign := reseal(g3, func(b *Buf) { copy(b[0:8], "SQLite f") })
	future := reseal(g4, func(b *Buf) { le.PutUint64(b[32:], 9); le.PutUint16(b[408:], 14) })
	otherG3 := encodeSB(func() Superblock { s := sampleSuperblock(3); s.CommitID = 99; return s }())

	ok := []struct {
		name string
		a, b *Buf
		gen  uint64
		page ID
	}{
		{"higher generation in page 1", g3, g4, 4, 1},
		{"higher generation in page 0", g4, g3, 4, 0},
		{"identical copies", g3, g3, 3, 0},
		{"torn page 1", g3, damaged, 3, 0},
		{"foreign page 0", foreign, g4, 4, 1},
	}
	for _, c := range ok {
		s, page, err := ChooseSuperblock(c.a, c.b)
		if err != nil || s.Generation != c.gen || page != c.page {
			t.Errorf("%s: generation %d, page %d, %v; want %d, %d", c.name, s.Generation, page, err, c.gen, c.page)
		}
	}
	bad := []struct {
		name string
		a, b *Buf
		code errs.Code
	}{
		{"same generation, different content", g3, otherG3, errs.CodeCorrupt},
		{"both damaged", damaged, damaged, errs.CodeCorrupt},
		{"damaged and foreign", damaged, foreign, errs.CodeCorrupt},
		{"both foreign", foreign, foreign, errs.CodeUnsupportedFormat},
		{"newer revision next to a valid copy", g3, future, errs.CodeUnsupportedFormat},
	}
	for _, c := range bad {
		if _, _, err := ChooseSuperblock(c.a, c.b); !errors.Is(err, errs.Sentinel(c.code)) {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
}
