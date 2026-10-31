package page

// Allocation groups (SPEC §8.4): group g has its bitmap page at
// FirstBitmap + g*GroupStride and covers the GroupPages pages after it.
const (
	FirstBitmap = 2
	GroupPages  = Usable * 8 // 32384 pages per group
	GroupStride = GroupPages + 1
)

// MaxPageCount keeps pageCount*Size within an int64 file offset.
const MaxPageCount = (1<<63 - 1) / Size

// Extent is a run of contiguous pages.
type Extent struct {
	Start ID
	Pages uint32
}

// BitmapPage returns the bitmap page of group g.
func BitmapPage(g uint64) ID { return ID(FirstBitmap + g*GroupStride) }

// IsBitmapPage reports whether id is the bitmap page of some group.
func IsBitmapPage(id ID) bool { return id >= FirstBitmap && (id-FirstBitmap)%GroupStride == 0 }

// GroupOf returns the group of a data page and its bit index in the
// group's bitmap; ok is false for superblocks and bitmap pages.
func GroupOf(id ID) (g uint64, bit int, ok bool) {
	if id <= FirstBitmap || IsBitmapPage(id) {
		return 0, 0, false
	}
	off := uint64(id - FirstBitmap)
	return off / GroupStride, int(off%GroupStride) - 1, true
}

// CheckPointer rejects, as CORRUPT, a reference to a data page that is
// null, a superblock, a bitmap page or beyond pageCount (R13-05).
func CheckPointer(id ID, pageCount uint64) error {
	switch {
	case id == 0:
		return corrupt(id, "null page pointer")
	case id == 1:
		return corrupt(id, "pointer to a superblock")
	case IsBitmapPage(id):
		return corrupt(id, "pointer to a bitmap page")
	case uint64(id) >= pageCount:
		return corrupt(id, "pointer beyond pageCount %d", pageCount)
	}
	return nil
}

// CheckExtent rejects an extent that is empty, crosses pageCount or covers
// a superblock or a bitmap page.
func CheckExtent(e Extent, pageCount uint64) error {
	if e.Pages == 0 {
		return corrupt(e.Start, "empty extent")
	}
	if err := CheckPointer(e.Start, pageCount); err != nil {
		return err
	}
	// Distances from Start, so that no sum can overflow: CheckPointer gave
	// FirstBitmap < Start < pageCount, and Start is not a bitmap page.
	if uint64(e.Pages) > pageCount-uint64(e.Start) {
		return corrupt(e.Start, "extent of %d pages crosses pageCount %d", e.Pages, pageCount)
	}
	if toBitmap := GroupStride - (uint64(e.Start)-FirstBitmap)%GroupStride; uint64(e.Pages) > toBitmap {
		return corrupt(e.Start, "extent of %d pages covers bitmap page %d", e.Pages, uint64(e.Start)+toBitmap)
	}
	return nil
}
