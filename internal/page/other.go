package page

// EncodeOverflow writes overflow page id holding data (at most Usable
// bytes); count records the useful bytes.
func EncodeOverflow(b *Buf, id ID, data []byte) error {
	if len(data) > Usable {
		return invalid("overflow page of %d bytes", len(data))
	}
	initHeader(b, TypeOverflow, id)
	le.PutUint16(b[offCount:], uint16(len(data)))
	copy(b[HeaderSize:], data)
	return nil
}

// OverflowData returns the useful bytes of an overflow page.
func OverflowData(b *Buf) []byte { return b[HeaderSize : HeaderSize+HeaderOf(b).Count()] }

// EncodeDescriptor writes a descriptor page: an overflow page whose useful
// bytes list more than MaxInline extents (SPEC §7.3).
func EncodeDescriptor(b *Buf, id ID, extents []Extent) error {
	if len(extents) <= MaxInline || len(extents) > MaxExtents {
		return invalid("descriptor with %d extents", len(extents))
	}
	initHeader(b, TypeOverflow, id)
	le.PutUint16(b[offCount:], uint16(ExtentSize*len(extents)))
	for i, e := range extents {
		o := HeaderSize + ExtentSize*i
		le.PutUint64(b[o:], uint64(e.Start))
		le.PutUint32(b[o+8:], e.Pages)
	}
	return nil
}

// DescriptorExtents reads the extents of a verified descriptor page.
func DescriptorExtents(b *Buf) ([]Extent, error) {
	h := HeaderOf(b)
	if h.Type() != TypeOverflow {
		return nil, corrupt(h.ID(), "descriptor has type %d", h.Type())
	}
	count := h.Count()
	if count%ExtentSize != 0 || count/ExtentSize <= MaxInline {
		return nil, corrupt(h.ID(), "descriptor of %d bytes", count)
	}
	out := make([]Extent, count/ExtentSize)
	for i := range out {
		o := HeaderSize + ExtentSize*i
		out[i] = Extent{Start: ID(le.Uint64(b[o:])), Pages: le.Uint32(b[o+8:])}
	}
	return out, nil
}

// EncodeBitmap writes an empty bitmap page; id must be a bitmap page.
func EncodeBitmap(b *Buf, id ID) error {
	if !IsBitmapPage(id) {
		return invalid("page %d is not a bitmap page", id)
	}
	initHeader(b, TypeBitmap, id)
	le.PutUint64(b[offSibling:], (uint64(id)-FirstBitmap)/GroupStride)
	return nil
}

// BitmapBit reports whether bit i of a bitmap page is set (the page is
// allocated). Bit i is bit i%8, counted from the least significant, of
// byte HeaderSize + i/8 (SPEC §8.4).
func BitmapBit(b *Buf, i int) bool { return b[HeaderSize+i/8]&(1<<(i%8)) != 0 }

// SetBitmapBit sets or clears bit i of a bitmap page.
func SetBitmapBit(b *Buf, i int, v bool) {
	if v {
		b[HeaderSize+i/8] |= 1 << (i % 8)
	} else {
		b[HeaderSize+i/8] &^= 1 << (i % 8)
	}
}

// EncodeFree writes the zeroed free page that the scrub leaves behind.
func EncodeFree(b *Buf, id ID) { initHeader(b, TypeFree, id) }
