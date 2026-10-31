package page

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

type goldenPage struct {
	name string
	id   ID
	b    *Buf
}

// goldenPages builds one image of each page type (FORMAT.md §6).
func goldenPages() []goldenPage {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var out []goldenPage
	add := func(name string, id ID, fill func(*Buf)) {
		b := new(Buf)
		fill(b)
		HeaderOf(b).SetCommitID(7)
		Seal(b)
		out = append(out, goldenPage{name, id, b})
	}
	add("leaf-prefix", 10, func(b *Buf) {
		must(EncodeLeaf(b, 10, []LeafCell{
			{Key: []byte("user/0001"), Value: []byte("alice")},
			{Key: []byte("user/0002"), Value: []byte("bob")},
		}, 11))
	})
	add("leaf-plain", 11, func(b *Buf) {
		must(EncodeLeaf(b, 11, []LeafCell{
			{Key: []byte("a"), Value: []byte{0x00, 0xff}},
			{Key: []byte("b"), Value: nil},
		}, 0))
	})
	add("leaf-out", 12, func(b *Buf) {
		must(EncodeLeaf(b, 12, []LeafCell{
			{Key: []byte("big"), Out: &OutRef{Len: 5000, Extents: []Extent{{Start: 20, Pages: 2}}}},
			{Key: []byte("huge"), Out: &OutRef{Len: 9 * Usable, Descriptor: 30}},
			{Key: []byte("small"), Value: []byte(strings.Repeat("v", 1300))},
		}, 0))
	})
	add("internal", 13, func(b *Buf) {
		must(EncodeInternal(b, 13, 1, 10, []InternalCell{{Key: []byte("user/0002"), Right: 11}, {Key: []byte("user/0100"), Right: 12}}))
	})
	add("overflow", 20, func(b *Buf) { must(EncodeOverflow(b, 20, []byte("first page of a value"))) })
	add("descriptor", 30, func(b *Buf) {
		must(EncodeDescriptor(b, 30, []Extent{{40, 1}, {42, 2}, {45, 1}, {47, 3}, {51, 2}}))
	})
	add("bitmap", BitmapPage(0), func(b *Buf) {
		must(EncodeBitmap(b, BitmapPage(0)))
		for _, i := range []int{0, 1, 7, 8, 32383} {
			SetBitmapBit(b, i, true)
		}
	})
	add("free", 60, func(b *Buf) { EncodeFree(b, 60) })
	s := sampleSuperblock(5)
	sb := new(Buf)
	s.Encode(sb)
	out = append(out, goldenPage{"superblock", 0, sb})
	return out
}

func TestGoldenPages(t *testing.T) {
	for _, g := range goldenPages() {
		path := filepath.Join("testdata", "golden", g.name+".bin")
		if *update {
			if err := os.WriteFile(path, g.b[:], 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run `go test ./internal/page -run TestGoldenPages -update` once and review the files)", err)
		}
		if !bytes.Equal(g.b[:], want) {
			t.Errorf("%s differs from %s", g.name, path)
		}
		if g.name == "superblock" {
			if _, kind, why := Classify(g.b); kind != Valid {
				t.Errorf("superblock golden: %v (%s)", kind, why)
			}
			continue
		}
		if err := Verify(g.b, g.id, 1<<20); err != nil {
			t.Errorf("%s: %v", g.name, err)
		}
		if err := Canonical(g.b); err != nil {
			t.Errorf("%s: %v", g.name, err)
		}
	}
}
