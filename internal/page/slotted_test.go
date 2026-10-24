package page

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

const testPageCount = 1 << 20

func inlineCell(key string, value string) LeafCell {
	return LeafCell{Key: []byte(key), Value: []byte(value)}
}

func mustLeaf(t *testing.T, id ID, cells []LeafCell, next ID) *Buf {
	t.Helper()
	b := new(Buf)
	if err := EncodeLeaf(b, id, cells, next); err != nil {
		t.Fatal(err)
	}
	Seal(b)
	if err := Verify(b, id, testPageCount); err != nil {
		t.Fatalf("Verify of an encoded leaf: %v", err)
	}
	if err := Canonical(b); err != nil {
		t.Fatalf("Canonical of an encoded leaf: %v", err)
	}
	return b
}

// usedBytes is what a page really occupies: header and slots up to lower,
// cells from upper to the end.
func usedBytes(b *Buf) int {
	h := HeaderOf(b)
	return h.Lower() + Size - h.Upper()
}

func TestInlineRuleBoundary(t *testing.T) {
	key := strings.Repeat("k", 100)
	// 1 + uvlen(100) + uvlen(v) + 100 + v; with v >= 128, uvlen(v) = 2.
	if !Inline(len(key), 1344-1-1-2-100) {
		t.Error("a full cell of exactly 1344 bytes must be inline")
	}
	if Inline(len(key), 1344-1-1-2-100+1) {
		t.Error("a full cell of 1345 bytes must go out of the page")
	}
	if got := FullCellSize(1024, 0); got != 1+2+1+1024 {
		t.Errorf("FullCellSize(1024, 0) = %d", got)
	}
}

func TestLeafRoundTrip(t *testing.T) {
	cells := []LeafCell{
		inlineCell("user/0001", "alice"),
		inlineCell("user/0002", ""),
		{Key: []byte("user/0003"), Out: &OutRef{Len: 5000, Extents: []Extent{{Start: 7, Pages: 2}}}},
		{Key: []byte("user/0004"), Out: &OutRef{Len: 9 * Usable, Descriptor: 9}},
	}
	b := mustLeaf(t, 42, cells, 43)
	n := NodeOf(b)
	if !n.Leaf() || n.Len() != 4 || string(n.Prefix()) != "user/000" {
		t.Fatalf("leaf: %v cells, prefix %q", n.Len(), n.Prefix())
	}
	if HeaderOf(b).Sibling() != 43 {
		t.Fatal("next leaf lost")
	}
	got := n.LeafCells()
	for i := range cells {
		if !bytes.Equal(got[i].Key, cells[i].Key) || !bytes.Equal(got[i].Value, cells[i].Value) {
			t.Fatalf("cell %d: %q=%q, want %q=%q", i, got[i].Key, got[i].Value, cells[i].Key, cells[i].Value)
		}
	}
	v := n.Value(2)
	if !v.Out() || v.Len != 5000 || v.NExtents != 1 || v.Extents[0] != (Extent{7, 2}) {
		t.Fatalf("out-of-page value: %+v", v)
	}
	if v := n.Value(3); v.Descriptor != 9 || v.Len != 9*Usable {
		t.Fatalf("descriptor value: %+v", v)
	}
	if v := n.Value(1); v.Out() || len(v.Inline) != 0 {
		t.Fatalf("empty inline value: %+v", v)
	}
	if usedBytes(b) != LeafPageSize(cells) {
		t.Fatalf("LeafPageSize = %d, page uses %d", LeafPageSize(cells), usedBytes(b))
	}
}

// R13-19: the prefix is used only when it makes the page strictly
// smaller.
func TestCanonicalPrefixChoice(t *testing.T) {
	v := strings.Repeat("v", 1000)
	keys := []string{"kaaa", "kbbb", "kccc", "kddd"}
	var cells []LeafCell
	for _, k := range keys {
		cells = append(cells, inlineCell(k, v))
	}
	if got := LeafPageSize(cells); got != Size {
		t.Fatalf("kaaa..kddd with 1000-byte values: size %d, want exactly %d", got, Size)
	}
	b := mustLeaf(t, 5, cells, 0)
	if NodeOf(b).Prefix() != nil {
		t.Fatal(`prefix "k" saves 4 bytes and costs 5; it must not be used`)
	}

	long := strings.Repeat("p", 50)
	cells = []LeafCell{inlineCell(long+"a", "1"), inlineCell(long+"b", "2")}
	if b := mustLeaf(t, 6, cells, 0); string(NodeOf(b).Prefix()) != long {
		t.Fatalf("a 50-byte shared prefix must be used, got %q", NodeOf(b).Prefix())
	}
	if b := mustLeaf(t, 7, cells[:1], 0); NodeOf(b).Prefix() != nil {
		t.Fatal("a page with one key has no prefix")
	}
}

// The canonical size never exceeds the size with any other prefix choice.
func TestCanonicalSizeIsMinimal(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	for i := 0; i < 2000; i++ {
		cells := randomCells(r, 1+r.IntN(30))
		p, size := leafLayout(cells)
		limit := lcp(cells[0].Key, cells[len(cells)-1].Key)
		for q := 0; q <= limit; q++ {
			other := HeaderSize
			if q > 0 {
				other += SlotSize + q
			}
			for j := range cells {
				other += SlotSize + cells[j].cellSize(q)
			}
			if size > other {
				t.Fatalf("canonical size %d (prefix %d) exceeds %d with prefix %d", size, p, other, q)
			}
		}
	}
}

// randomCells returns sorted cells with long shared prefixes, keys around
// the 128-byte uvarint boundary and values around the inline limit.
func randomCells(r *rand.Rand, n int) []LeafCell {
	base := strings.Repeat("x", []int{0, 1, 10, 120, 500, 900}[r.IntN(6)])
	seen := map[string]bool{}
	var cells []LeafCell
	for len(cells) < n {
		k := base + fmt.Sprintf("%0*d", 1+r.IntN(130), r.IntN(1000))
		if len(k) > MaxKeyLen {
			k = k[:MaxKeyLen]
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		vlen := []int{0, 1, 100, 127, 128, 300, 1200, 1300, 5000}[r.IntN(9)]
		c := LeafCell{Key: []byte(k)}
		if Inline(len(k), uint64(vlen)) {
			c.Value = bytes.Repeat([]byte{'v'}, vlen)
		} else {
			c.Out = &OutRef{Len: uint64(vlen), Extents: []Extent{{Start: 10, Pages: uint32((vlen + Usable - 1) / Usable)}}}
		}
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Key, cells[j].Key) < 0 })
	return cells
}

// LeafPageSize equals the bytes an encoded page really uses.
func TestLeafPageSizeMatchesEncoding(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	for i := 0; i < 2000; i++ {
		cells := randomCells(r, 1+r.IntN(20))
		size := LeafPageSize(cells)
		if size > Size {
			continue
		}
		b := new(Buf)
		if err := EncodeLeaf(b, 9, cells, 0); err != nil {
			t.Fatal(err)
		}
		if got := usedBytes(b); got != size {
			t.Fatalf("LeafPageSize = %d, page uses %d", size, got)
		}
	}
}

// R13-01/R13-16: a full page plus one more cell always splits into two
// pages, measuring each side with its own canonical size.
func TestTwoWaySplitAlwaysExists(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 4))
	for events := 0; events < 3000; {
		page := randomCells(r, 1)
		for {
			next := randomCells(r, 1)[0]
			cand := append(append([]LeafCell(nil), page...), next)
			sort.Slice(cand, func(i, j int) bool { return bytes.Compare(cand[i].Key, cand[j].Key) < 0 })
			if dup := len(cand) > 1 && hasDup(cand); dup {
				continue
			}
			if LeafPageSize(cand) <= Size {
				page = cand
				continue
			}
			events++
			ok := false
			for k := 1; k < len(cand) && !ok; k++ {
				ok = LeafPageSize(cand[:k]) <= Size && LeafPageSize(cand[k:]) <= Size
			}
			if !ok {
				t.Fatalf("no two-way split for %d cells", len(cand))
			}
			break
		}
	}
}

func hasDup(cells []LeafCell) bool {
	for i := 1; i < len(cells); i++ {
		if bytes.Equal(cells[i-1].Key, cells[i].Key) {
			return true
		}
	}
	return false
}

// The case from the design review: 160 short suffixes under a 1000-byte
// prefix, plus the key "b" with a 1300-byte value.
func TestSplitWithForeignKey(t *testing.T) {
	prefix := strings.Repeat("a", 1000)
	var cells []LeafCell
	for i := 0; i < 160; i++ {
		cells = append(cells, inlineCell(prefix+fmt.Sprintf("%c%c", 'a'+i/26, 'a'+i%26), ""))
	}
	if LeafPageSize(cells) > Size {
		t.Fatalf("the original page does not fit: %d", LeafPageSize(cells))
	}
	cells = append(cells, inlineCell("b", strings.Repeat("v", 1300)))
	var valid []int
	for k := 1; k < len(cells); k++ {
		if LeafPageSize(cells[:k]) <= Size && LeafPageSize(cells[k:]) <= Size {
			valid = append(valid, k)
		}
	}
	if fmt.Sprint(valid) != "[158 159 160]" {
		t.Fatalf("valid split points %v, want [158 159 160]", valid)
	}
}

func TestEncodeLeafRejects(t *testing.T) {
	invalid := errs.Sentinel(errs.CodeInvalidArgument)
	big := strings.Repeat("v", 2000)
	cases := map[string][]LeafCell{
		"unsorted":         {inlineCell("b", ""), inlineCell("a", "")},
		"duplicate":        {inlineCell("a", ""), inlineCell("a", "")},
		"inline too large": {inlineCell("a", big)},
		"out but small":    {{Key: []byte("a"), Out: &OutRef{Len: 10, Extents: []Extent{{Start: 3, Pages: 1}}}}},
		"five inline extents": {{Key: []byte("a"), Out: &OutRef{Len: 5 * Usable,
			Extents: []Extent{{3, 1}, {5, 1}, {7, 1}, {9, 1}, {11, 1}}}}},
		"key too long": {inlineCell(strings.Repeat("k", MaxKeyLen+1), "")},
		"no cells":     nil,
	}
	for name, cells := range cases {
		if err := EncodeLeaf(new(Buf), 3, cells, 0); !errors.Is(err, invalid) {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", name, err)
		}
	}
	// Three cells of 1305 bytes always fit (the inline rule guarantees it);
	// four do not.
	var many []LeafCell
	for i := 0; i < 4; i++ {
		many = append(many, inlineCell(fmt.Sprint(i), strings.Repeat("v", 1300)))
	}
	if err := EncodeLeaf(new(Buf), 3, many, 0); !errors.Is(err, invalid) {
		t.Errorf("page that does not fit: %v, want INVALID_ARGUMENT", err)
	}
}

func TestSearchAndCompare(t *testing.T) {
	keys := []string{"app", "apple", "apply", "banana"}
	var cells []LeafCell
	for _, k := range keys {
		cells = append(cells, inlineCell(k, ""))
	}
	n := NodeOf(mustLeaf(t, 3, cells, 0))
	cases := []struct {
		key   string
		i     int
		found bool
	}{
		{"", 0, false}, {"ap", 0, false}, {"app", 0, true}, {"appl", 1, false},
		{"apple", 1, true}, {"applz", 3, false}, {"banana", 3, true}, {"z", 4, false},
	}
	for _, c := range cases {
		i, found := n.Search([]byte(c.key))
		if i != c.i || found != c.found {
			t.Errorf("Search(%q) = %d, %v; want %d, %v", c.key, i, found, c.i, c.found)
		}
	}
	// A shared prefix must not change the order.
	long := strings.Repeat("p", 40)
	cells = []LeafCell{inlineCell(long+"b", ""), inlineCell(long+"d", "")}
	n = NodeOf(mustLeaf(t, 4, cells, 0))
	for key, want := range map[string]int{"": 0, long: 0, long + "a": 0, long + "c": 1, long + "e": 2, "q": 2, "o": 0} {
		if i, _ := n.Search([]byte(key)); i != want {
			t.Errorf("prefixed Search(%q) = %d, want %d", key, i, want)
		}
	}
}

func TestInternalPage(t *testing.T) {
	cells := []InternalCell{{Key: []byte("m"), Right: 20}, {Key: []byte("t"), Right: 30}}
	b := new(Buf)
	if err := EncodeInternal(b, 8, 1, 10, cells); err != nil {
		t.Fatal(err)
	}
	Seal(b)
	if err := Verify(b, 8, testPageCount); err != nil {
		t.Fatal(err)
	}
	n := NodeOf(b)
	for key, want := range map[string]ID{"a": 10, "m": 20, "n": 20, "t": 30, "z": 30} {
		if _, child := n.ChildFor([]byte(key)); child != want {
			t.Errorf("ChildFor(%q) = %d, want %d", key, child, want)
		}
	}
	if got := n.InternalCells(); len(got) != 2 || got[1].Right != 30 || string(got[0].Key) != "m" {
		t.Fatalf("InternalCells = %+v", got)
	}
	if err := EncodeInternal(new(Buf), 8, 0, 10, cells); !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("level 0 internal page: %v", err)
	}
}
