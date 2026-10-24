package page

import (
	"bytes"
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
)

// Leaf cell flags (SPEC §7.3).
const (
	cellOut        = 0x01 // the value is out of the page
	cellDescriptor = 0x02 // the extents live in a descriptor page
)

// LeafCell is a key with its value, as given to EncodeLeaf.
type LeafCell struct {
	Key   []byte
	Value []byte  // inline value; must be nil when Out is set
	Out   *OutRef // value stored in overflow pages
}

// OutRef locates a value stored out of the page.
type OutRef struct {
	Len        uint64   // logical length of the value
	Extents    []Extent // 1..MaxInline extents, when Descriptor is 0
	Descriptor ID       // descriptor page listing more than MaxInline extents
}

// InternalCell is a separator and the child to its right.
type InternalCell struct {
	Key   []byte
	Right ID
}

// FullCellSize is the size of a leaf cell holding key and an inline value
// of valueLen bytes, without removing any prefix. A value is inline if and
// only if this is at most MaxInlineCell (R13-01, R13-15); the measure does
// not depend on the page prefix, so splits and merges never change it.
func FullCellSize(keyLen int, valueLen uint64) uint64 {
	return 1 + uint64(codec.UvarintLen(uint64(keyLen))) + uint64(codec.UvarintLen(valueLen)) +
		uint64(keyLen) + valueLen
}

// Inline reports whether a value of valueLen bytes stays in the leaf cell.
func Inline(keyLen int, valueLen uint64) bool {
	return FullCellSize(keyLen, valueLen) <= MaxInlineCell
}

func (c *LeafCell) valueLen() uint64 {
	if c.Out != nil {
		return c.Out.Len
	}
	return uint64(len(c.Value))
}

func (c *LeafCell) payloadLen() int {
	switch {
	case c.Out == nil:
		return len(c.Value)
	case c.Out.Descriptor != 0:
		return 8
	default:
		return 1 + ExtentSize*len(c.Out.Extents)
	}
}

// cellSize is the size of the cell with p prefix bytes removed from key.
func (c *LeafCell) cellSize(p int) int {
	s := len(c.Key) - p
	return 1 + codec.UvarintLen(uint64(s)) + codec.UvarintLen(c.valueLen()) + s + c.payloadLen()
}

func (c *InternalCell) cellSize(p int) int {
	s := len(c.Key) - p
	return codec.UvarintLen(uint64(s)) + 8 + s
}

// lcp returns the length of the longest common prefix of a and b.
func lcp(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// canonical picks the canonical prefix length of a page with n keys whose
// first and last keys are given (SPEC §7.3, R13-19): the longest common
// prefix p of the first and last key is used only if the page has at
// least two keys, p >= 1 and the encoding with it is strictly smaller.
// It returns p (0 for no prefix) and the total page size.
func canonical(n int, first, last []byte, size func(p int) int) (int, int) {
	plain := HeaderSize + size(0)
	if n < 2 {
		return 0, plain
	}
	p := lcp(first, last)
	if p == 0 {
		return 0, plain
	}
	if with := HeaderSize + SlotSize + p + size(p); with < plain {
		return p, with
	}
	return 0, plain
}

func leafLayout(cells []LeafCell) (p, size int) {
	if len(cells) == 0 {
		return 0, HeaderSize
	}
	return canonical(len(cells), cells[0].Key, cells[len(cells)-1].Key, func(p int) int {
		t := 0
		for i := range cells {
			t += SlotSize + cells[i].cellSize(p)
		}
		return t
	})
}

func internalLayout(cells []InternalCell) (p, size int) {
	if len(cells) == 0 {
		return 0, HeaderSize
	}
	return canonical(len(cells), cells[0].Key, cells[len(cells)-1].Key, func(p int) int {
		t := 0
		for i := range cells {
			t += SlotSize + cells[i].cellSize(p)
		}
		return t
	})
}

// LeafPageSize returns the canonical size in bytes of a leaf page holding
// cells, which must be sorted. A page fits if the size is at most Size.
// Every fit decision uses this measure (R13-19).
func LeafPageSize(cells []LeafCell) int {
	_, size := leafLayout(cells)
	return size
}

// InternalPageSize returns the canonical size in bytes of an internal page
// holding cells, which must be sorted.
func InternalPageSize(cells []InternalCell) int {
	_, size := internalLayout(cells)
	return size
}

func invalid(format string, args ...any) error {
	return errs.Newf(errs.CodeInvalidArgument, "page.Encode", format, args...)
}

// EncodeLeaf writes the canonical leaf page id holding cells, linked to
// the next leaf. Cells must have strictly increasing keys, values must
// follow the inline rule, and the page must fit; otherwise it returns
// INVALID_ARGUMENT and b is unspecified.
func EncodeLeaf(b *Buf, id ID, cells []LeafCell, next ID) error {
	for i := range cells {
		c := &cells[i]
		if len(c.Key) > MaxKeyLen {
			return invalid("key of %d bytes", len(c.Key))
		}
		if c.valueLen() > MaxValue {
			return invalid("cell %d: value of %d bytes above %d", i, c.valueLen(), MaxValue)
		}
		if i > 0 && bytes.Compare(cells[i-1].Key, c.Key) >= 0 {
			return invalid("keys are not strictly increasing at cell %d", i)
		}
		inline := Inline(len(c.Key), c.valueLen())
		switch {
		case c.Out == nil && !inline:
			return invalid("cell %d: a value of %d bytes must go out of the page", i, len(c.Value))
		case c.Out != nil && inline:
			return invalid("cell %d: a value of %d bytes must stay inline", i, c.Out.Len)
		case c.Out != nil && c.Value != nil:
			return invalid("cell %d: both an inline value and an out-of-page reference", i)
		case c.Out != nil && c.Out.Descriptor == 0 && (len(c.Out.Extents) < 1 || len(c.Out.Extents) > MaxInline):
			return invalid("cell %d: %d inline extents", i, len(c.Out.Extents))
		case c.Out != nil && c.Out.Descriptor != 0 && len(c.Out.Extents) != 0:
			return invalid("cell %d: extents given with a descriptor page", i)
		}
	}
	p, size := leafLayout(cells)
	if size > Size {
		return invalid("leaf of %d bytes does not fit", size)
	}
	if len(cells) == 0 {
		return invalid("leaf without cells")
	}
	w := newWriter(b, TypeLeaf, id, len(cells), p, cells[0].Key)
	for i := range cells {
		c := &cells[i]
		cell := w.begin()
		flags := byte(0)
		if c.Out != nil {
			flags = cellOut
			if c.Out.Descriptor != 0 {
				flags |= cellDescriptor
			}
		}
		cell = append(cell, flags)
		cell = codec.AppendUvarint(cell, uint64(len(c.Key)-p))
		cell = codec.AppendUvarint(cell, c.valueLen())
		cell = append(cell, c.Key[p:]...)
		switch {
		case c.Out == nil:
			cell = append(cell, c.Value...)
		case c.Out.Descriptor != 0:
			cell = le.AppendUint64(cell, uint64(c.Out.Descriptor))
		default:
			cell = append(cell, byte(len(c.Out.Extents)))
			for _, e := range c.Out.Extents {
				cell = le.AppendUint64(cell, uint64(e.Start))
				cell = le.AppendUint32(cell, e.Pages)
			}
		}
		w.end(cell)
	}
	w.finish()
	le.PutUint64(b[offSibling:], uint64(next))
	return nil
}

// EncodeInternal writes the canonical internal page id at the given level
// (>= 1), whose first child is left and whose separators are cells.
func EncodeInternal(b *Buf, id ID, level uint16, left ID, cells []InternalCell) error {
	if level < 1 {
		return invalid("internal page at level %d", level)
	}
	for i := range cells {
		if len(cells[i].Key) > MaxKeyLen {
			return invalid("separator of %d bytes", len(cells[i].Key))
		}
		if i > 0 && bytes.Compare(cells[i-1].Key, cells[i].Key) >= 0 {
			return invalid("separators are not strictly increasing at cell %d", i)
		}
	}
	p, size := internalLayout(cells)
	if size > Size {
		return invalid("internal page of %d bytes does not fit", size)
	}
	if len(cells) == 0 {
		return invalid("internal page without separators")
	}
	w := newWriter(b, TypeInternal, id, len(cells), p, cells[0].Key)
	for i := range cells {
		c := &cells[i]
		cell := w.begin()
		cell = codec.AppendUvarint(cell, uint64(len(c.Key)-p))
		cell = le.AppendUint64(cell, uint64(c.Right))
		cell = append(cell, c.Key[p:]...)
		w.end(cell)
	}
	w.finish()
	le.PutUint16(b[offLevel:], level)
	le.PutUint64(b[offAux:], uint64(left))
	return nil
}

// writer lays out a canonical slotted page: slots after the header, cells
// packed in slot order so that the last one ends at the end of the page.
type writer struct {
	b       *Buf
	slots   int // number of slots, prefix slot included
	slot    int // next slot to fill
	cells   []byte
	offsets []int
}

func newWriter(b *Buf, t Type, id ID, n, p int, first []byte) *writer {
	initHeader(b, t, id)
	w := &writer{b: b, slots: n}
	if p > 0 {
		b[offFlags] = FlagPrefix
		w.slots++
		cell := w.begin()
		w.end(append(cell, first[:p]...))
	}
	return w
}

func (w *writer) begin() []byte { return w.cells }

func (w *writer) end(cells []byte) {
	w.offsets = append(w.offsets, len(w.cells))
	w.cells = cells
}

func (w *writer) finish() {
	upper := Size - len(w.cells)
	copy(w.b[upper:], w.cells)
	for i, off := range w.offsets {
		var length int
		if i+1 < len(w.offsets) {
			length = w.offsets[i+1] - off
		} else {
			length = len(w.cells) - off
		}
		s := HeaderSize + SlotSize*i
		le.PutUint16(w.b[s:], uint16(upper+off))
		le.PutUint16(w.b[s+2:], uint16(length))
	}
	le.PutUint16(w.b[offCount:], uint16(w.slots))
	le.PutUint16(w.b[offLower:], uint16(HeaderSize+SlotSize*w.slots))
	le.PutUint16(w.b[offUpper:], uint16(upper))
}

// Node reads a leaf or internal page that passed Verify or was written by
// EncodeLeaf or EncodeInternal. Its accessors trust that structure; an
// unverified page must never reach them.
type Node struct {
	b      *Buf
	leaf   bool
	base   int // 1 when slot 0 holds the prefix
	n      int // number of cells
	prefix []byte
}

// NodeOf returns the reader of a slotted page.
func NodeOf(b *Buf) Node {
	h := HeaderOf(b)
	n := Node{b: b, leaf: h.Type() == TypeLeaf, n: h.Count()}
	if h.Flags()&FlagPrefix != 0 {
		n.base, n.n = 1, n.n-1
		n.prefix = n.slot(0)
	}
	return n
}

// Leaf reports whether the page is a leaf.
func (n Node) Leaf() bool { return n.leaf }

// Len returns the number of cells.
func (n Node) Len() int { return n.n }

// Prefix returns the common prefix of the keys, or nil.
func (n Node) Prefix() []byte { return n.prefix }

func (n Node) slot(s int) []byte {
	o := HeaderSize + SlotSize*s
	off, l := int(le.Uint16(n.b[o:])), int(le.Uint16(n.b[o+2:]))
	return n.b[off : off+l]
}

// leafCell is a parsed leaf cell.
type leafCell struct {
	flags   byte
	suffix  []byte
	valLen  uint64
	payload []byte
}

func parseLeafCell(c []byte) (leafCell, error) {
	var lc leafCell
	if len(c) < 3 {
		return lc, errs.New(errs.CodeCorrupt, "page.leafCell", "cell too short")
	}
	lc.flags = c[0]
	if lc.flags&^(cellOut|cellDescriptor) != 0 || lc.flags == cellDescriptor {
		return lc, errs.Newf(errs.CodeCorrupt, "page.leafCell", "flags 0x%02x", lc.flags)
	}
	sl, n1, err := codec.ReadUvarint(c[1:])
	if err != nil {
		return lc, err
	}
	vl, n2, err := codec.ReadUvarint(c[1+n1:])
	if err != nil {
		return lc, err
	}
	pos := 1 + n1 + n2
	if sl > uint64(len(c)-pos) {
		return lc, errs.New(errs.CodeCorrupt, "page.leafCell", "suffix past the cell")
	}
	lc.suffix = c[pos : pos+int(sl)]
	lc.valLen = vl
	lc.payload = c[pos+int(sl):]
	return lc, nil
}

func parseInternalCell(c []byte) (suffix []byte, right ID, err error) {
	sl, n1, err := codec.ReadUvarint(c)
	if err != nil {
		return nil, 0, err
	}
	if rest := len(c) - n1; rest < 8 || uint64(rest-8) != sl {
		return nil, 0, errs.New(errs.CodeCorrupt, "page.internalCell", "cell length does not match the separator")
	}
	return c[n1+8:], ID(le.Uint64(c[n1:])), nil
}

func (n Node) suffix(i int) []byte {
	c := n.slot(i + n.base)
	if n.leaf {
		lc, err := parseLeafCell(c)
		if err != nil {
			panic("page: Node used on an unverified page: " + err.Error())
		}
		return lc.suffix
	}
	s, _, err := parseInternalCell(c)
	if err != nil {
		panic("page: Node used on an unverified page: " + err.Error())
	}
	return s
}

// AppendKey appends the full key of cell i to dst.
func (n Node) AppendKey(dst []byte, i int) []byte {
	return append(append(dst, n.prefix...), n.suffix(i)...)
}

// CompareKey compares the key of cell i with key, without materializing
// the stored key.
func (n Node) CompareKey(i int, key []byte) int {
	p := len(n.prefix)
	// A key shorter than the prefix compares as smaller than it here, so
	// key[p:] below is in range.
	if c := bytes.Compare(n.prefix, key[:min(p, len(key))]); c != 0 {
		return c
	}
	return bytes.Compare(n.suffix(i), key[p:])
}

// Search returns the index of the first cell whose key is >= key, and
// whether that key equals key.
func (n Node) Search(key []byte) (int, bool) {
	lo, hi := 0, n.n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if n.CompareKey(mid, key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < n.n && n.CompareKey(lo, key) == 0
}

// Child returns child i of an internal page: child 0 is the first child,
// and child i > 0 is the right child of separator i-1.
func (n Node) Child(i int) ID {
	if i == 0 {
		return HeaderOf(n.b).Aux()
	}
	_, right, err := parseInternalCell(n.slot(i - 1 + n.base))
	if err != nil {
		panic("page: Node used on an unverified page: " + err.Error())
	}
	return right
}

// ChildFor returns the index and page of the child of an internal page
// whose range contains key: the child after the last separator <= key.
func (n Node) ChildFor(key []byte) (int, ID) {
	i, found := n.Search(key)
	if found {
		i++
	}
	return i, n.Child(i)
}

// LeafValue describes the value of a leaf cell.
type LeafValue struct {
	Len        uint64
	Inline     []byte // the value itself, aliasing the page, when inline
	NExtents   int
	Extents    [MaxInline]Extent
	Descriptor ID
}

// Out reports whether the value is stored out of the page.
func (v *LeafValue) Out() bool { return v.Inline == nil }

// Value returns the value of leaf cell i.
func (n Node) Value(i int) LeafValue {
	lc, err := parseLeafCell(n.slot(i + n.base))
	if err != nil {
		panic("page: Node used on an unverified page: " + err.Error())
	}
	v, err := decodeValue(lc)
	if err != nil {
		panic("page: Node used on an unverified page: " + err.Error())
	}
	return v
}

func decodeValue(lc leafCell) (LeafValue, error) {
	v := LeafValue{Len: lc.valLen}
	switch {
	case lc.flags&cellOut == 0:
		if uint64(len(lc.payload)) != lc.valLen {
			return v, errs.New(errs.CodeCorrupt, "page.leafCell", "inline value length mismatch")
		}
		v.Inline = lc.payload
		if v.Inline == nil {
			v.Inline = []byte{}
		}
	case lc.flags&cellDescriptor != 0:
		if len(lc.payload) != 8 {
			return v, errs.New(errs.CodeCorrupt, "page.leafCell", "descriptor reference length")
		}
		v.Descriptor = ID(le.Uint64(lc.payload))
	default:
		if len(lc.payload) < 1 {
			return v, errs.New(errs.CodeCorrupt, "page.leafCell", "missing extent count")
		}
		k := int(lc.payload[0])
		if k < 1 || k > MaxInline || len(lc.payload) != 1+ExtentSize*k {
			return v, errs.New(errs.CodeCorrupt, "page.leafCell", "invalid inline extents")
		}
		v.NExtents = k
		for j := 0; j < k; j++ {
			e := lc.payload[1+ExtentSize*j:]
			v.Extents[j] = Extent{Start: ID(le.Uint64(e)), Pages: binary.LittleEndian.Uint32(e[8:])}
		}
	}
	return v, nil
}

// LeafCells decodes every cell of a leaf page for editing. Keys and inline
// values are copies; the result does not alias the page.
func (n Node) LeafCells() []LeafCell {
	out := make([]LeafCell, n.n)
	for i := range out {
		out[i].Key = n.AppendKey(nil, i)
		v := n.Value(i)
		if v.Inline != nil {
			out[i].Value = append([]byte{}, v.Inline...)
			continue
		}
		ref := &OutRef{Len: v.Len, Descriptor: v.Descriptor}
		ref.Extents = append([]Extent(nil), v.Extents[:v.NExtents]...)
		out[i].Out = ref
	}
	return out
}

// InternalCells decodes every separator of an internal page for editing.
func (n Node) InternalCells() []InternalCell {
	out := make([]InternalCell, n.n)
	for i := range out {
		out[i] = InternalCell{Key: n.AppendKey(nil, i), Right: n.Child(i + 1)}
	}
	return out
}
