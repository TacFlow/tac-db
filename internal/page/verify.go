package page

import (
	"bytes"
	"sort"
)

// Verify checks a page read from storage before any other code sees it
// (SPEC §7.1, §7.3, §20.11): the CRC, the header, the structure of its
// type and every page reference it holds, against pageCount. It accepts
// any valid layout; Canonical checks the canonical form.
func Verify(b *Buf, id ID, pageCount uint64) error {
	if !checkCRC(b) {
		return corrupt(id, "CRC mismatch")
	}
	h := HeaderOf(b)
	if le.Uint16(b[offHdrBytes:]) != HeaderSize {
		return corrupt(id, "header_bytes %d", le.Uint16(b[offHdrBytes:]))
	}
	if h.ID() != id {
		return corrupt(id, "page claims to be page %d", h.ID())
	}
	switch h.Type() {
	case TypeLeaf, TypeInternal:
		return verifySlotted(b, id, pageCount)
	case TypeOverflow:
		return verifyPlain(b, id, int(le.Uint16(b[offCount:])), false)
	case TypeFixed:
		if h.Sibling() == 0 {
			return corrupt(id, "fixed page without an index ID")
		}
		return verifyPlain(b, id, int(le.Uint16(b[offCount:])), true)
	case TypeBitmap:
		if !IsBitmapPage(id) || uint64(h.Sibling()) != (uint64(id)-FirstBitmap)/GroupStride {
			return corrupt(id, "bitmap page with group %d", h.Sibling())
		}
		if h.Count() != 0 {
			return corrupt(id, "bitmap page with count %d", h.Count())
		}
		return verifyPlain(b, id, 0, true)
	case TypeFree:
		if !allZero(b[offFlags:offHdrBytes]) || !allZero(b[offCommit+8:]) {
			return corrupt(id, "free page with data")
		}
		return nil
	}
	return corrupt(id, "unknown page type %d", h.Type())
}

// verifyPlain checks the header of a page without slots: flags, lower,
// upper, level and aux are zero, the sibling field is zero unless
// keepSibling, and count is at most Usable.
func verifyPlain(b *Buf, id ID, count int, keepSibling bool) error {
	h := HeaderOf(b)
	if h.Flags() != 0 || h.Lower() != 0 || h.Upper() != 0 || h.Level() != 0 || h.Aux() != 0 ||
		(!keepSibling && h.Sibling() != 0) {
		return corrupt(id, "type %d page with stray header fields", h.Type())
	}
	if count > Usable {
		return corrupt(id, "count %d above %d", count, Usable)
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func verifySlotted(b *Buf, id ID, pageCount uint64) error {
	h := HeaderOf(b)
	leaf := h.Type() == TypeLeaf
	if h.Flags()&^FlagPrefix != 0 {
		return corrupt(id, "flags 0x%02x", h.Flags())
	}
	base := 0
	if h.Flags()&FlagPrefix != 0 {
		base = 1
	}
	slots := h.Count()
	if slots-base < 1 {
		return corrupt(id, "slotted page without cells")
	}
	if h.Lower() != HeaderSize+SlotSize*slots || h.Upper() < h.Lower() || h.Upper() > Size {
		return corrupt(id, "lower %d / upper %d for %d slots", h.Lower(), h.Upper(), slots)
	}
	if leaf {
		if h.Level() != 0 || h.Aux() != 0 {
			return corrupt(id, "leaf with level %d or aux %d", h.Level(), h.Aux())
		}
		if next := h.Sibling(); next != 0 {
			if err := CheckPointer(next, pageCount); err != nil {
				return err
			}
		}
	} else {
		if h.Level() < 1 || h.Sibling() != 0 {
			return corrupt(id, "internal page with level %d or sibling %d", h.Level(), h.Sibling())
		}
		if err := CheckPointer(h.Aux(), pageCount); err != nil {
			return err
		}
	}
	type region struct{ off, end int }
	regions := make([]region, slots)
	for s := 0; s < slots; s++ {
		o := HeaderSize + SlotSize*s
		off, l := int(le.Uint16(b[o:])), int(le.Uint16(b[o+2:]))
		if l == 0 || off < h.Upper() || off+l > Size {
			return corrupt(id, "slot %d outside the cell area", s)
		}
		regions[s] = region{off, off + l}
	}
	sorted := append([]region(nil), regions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].off < sorted[j].off })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].off < sorted[i-1].end {
			return corrupt(id, "overlapping cells")
		}
	}
	var prefix []byte
	if base == 1 {
		prefix = b[regions[0].off:regions[0].end]
		if len(prefix) > MaxKeyLen {
			return corrupt(id, "prefix of %d bytes", len(prefix))
		}
	}
	var prev []byte
	for s := base; s < slots; s++ {
		c := b[regions[s].off:regions[s].end]
		var suffix []byte
		if leaf {
			lc, err := parseLeafCell(c)
			if err != nil {
				return corrupt(id, "cell %d: %v", s-base, err)
			}
			suffix = lc.suffix
			v, err := decodeValue(lc)
			if err != nil {
				return corrupt(id, "cell %d: %v", s-base, err)
			}
			if err := verifyValue(id, s-base, len(prefix)+len(suffix), &v, pageCount); err != nil {
				return err
			}
		} else {
			var right ID
			var err error
			suffix, right, err = parseInternalCell(c)
			if err != nil {
				return corrupt(id, "cell %d: %v", s-base, err)
			}
			if err := CheckPointer(right, pageCount); err != nil {
				return err
			}
		}
		if len(prefix)+len(suffix) > MaxKeyLen {
			return corrupt(id, "cell %d: key of %d bytes", s-base, len(prefix)+len(suffix))
		}
		if s > base && bytes.Compare(prev, suffix) >= 0 {
			return corrupt(id, "cell %d: keys are not strictly increasing", s-base)
		}
		prev = suffix
	}
	return nil
}

// verifyValue applies the inline rule and the canonical extent rules
// (SPEC §7.3) to the value of a leaf cell.
func verifyValue(id ID, cell, keyLen int, v *LeafValue, pageCount uint64) error {
	inline := Inline(keyLen, v.Len)
	if !v.Out() {
		if !inline {
			return corrupt(id, "cell %d: inline value of %d bytes breaks the inline rule", cell, v.Len)
		}
		return nil
	}
	if inline || v.Len > MaxValue {
		return corrupt(id, "cell %d: out-of-page value of %d bytes", cell, v.Len)
	}
	if v.Descriptor != 0 {
		return CheckPointer(v.Descriptor, pageCount)
	}
	return CheckExtents(v.Extents[:v.NExtents], v.Len, pageCount)
}

// CheckExtents applies the canonical rules to the extents of a value of
// valueLen bytes: each extent is valid, together they hold exactly
// ceil(valueLen/Usable) pages, and two consecutive extents are never
// physically contiguous.
func CheckExtents(extents []Extent, valueLen, pageCount uint64) error {
	var pages uint64
	for i, e := range extents {
		if err := CheckExtent(e, pageCount); err != nil {
			return err
		}
		if i > 0 && uint64(extents[i-1].Start)+uint64(extents[i-1].Pages) == uint64(e.Start) {
			return corrupt(e.Start, "extent continues the previous one")
		}
		pages += uint64(e.Pages)
	}
	if want := (valueLen + Usable - 1) / Usable; pages != want {
		return corrupt(0, "extents hold %d pages for a value that needs %d", pages, want)
	}
	return nil
}

// Canonical reports whether a verified page is in canonical form: a
// slotted page equals the encoding of its own cells, and a page without
// slots has zero bytes after its useful data. Check uses it; Verify does
// not require it.
func Canonical(b *Buf) error {
	h := HeaderOf(b)
	id := h.ID()
	var want Buf
	switch h.Type() {
	case TypeLeaf:
		n := NodeOf(b)
		if err := EncodeLeaf(&want, id, n.LeafCells(), h.Sibling()); err != nil {
			return corrupt(id, "cannot re-encode: %v", err)
		}
	case TypeInternal:
		n := NodeOf(b)
		if err := EncodeInternal(&want, id, h.Level(), h.Aux(), n.InternalCells()); err != nil {
			return corrupt(id, "cannot re-encode: %v", err)
		}
	case TypeOverflow, TypeFixed:
		if !allZero(b[HeaderSize+h.Count():]) {
			return corrupt(id, "bytes after the useful data")
		}
		return nil
	default:
		return nil
	}
	h2 := HeaderOf(&want)
	h2.SetCommitID(h.CommitID())
	Seal(&want)
	if want != *b {
		return corrupt(id, "page is not in canonical form")
	}
	return nil
}
