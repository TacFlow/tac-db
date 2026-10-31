// Package page implements the G2 page formats (SPEC §7.2, §7.3, §8.4):
// the 48-byte header, slotted leaf and internal pages with a canonical
// common prefix, overflow and descriptor pages, bitmap pages, the
// superblock, pointer validation and the interfaces through which the
// B+tree reads and writes pages.
package page

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Fixed sizes of the G2 format.
const (
	Size       = 4096                // page size
	HeaderSize = 48                  // page header
	Usable     = Size - 48           // bytes after the header: 4048
	SlotSize   = 4                   // offset u16 + length u16
	MaxKeyLen  = 1024                // full B+tree key, after encoding
	MaxValue   = 16 << 20            // value of a B+tree record
	ExtentSize = 12                  // start u64 + pages u32
	MaxInline  = 4                   // extents kept inside a leaf cell
	MaxExtents = Usable / ExtentSize // 337 extents in a descriptor page

	// MaxInlineCell bounds the full cell of an inline value (R13-01):
	// (Usable - SlotSize) / 3 - SlotSize, so that any page plus one cell
	// always splits into two pages.
	MaxInlineCell = 1344

	// MaxTreeHeight bounds a descent (SPEC §8.1).
	MaxTreeHeight = 40
)

// ID is a page number; the page lives at byte ID*Size of the main file.
type ID uint64

// Type is the page type stored in byte 0 of the header.
type Type uint8

// Page types (SPEC §7.3).
const (
	TypeFree     Type = 0
	TypeInternal Type = 1
	TypeLeaf     Type = 2
	TypeOverflow Type = 3
	TypeBitmap   Type = 4
	TypeFixed    Type = 5
)

// FlagPrefix marks a slotted page whose slot 0 holds the common prefix.
const FlagPrefix = 0x01

// Header offsets (SPEC §7.3).
const (
	offType     = 0
	offFlags    = 1
	offHdrBytes = 2
	offCRC      = 4
	offID       = 8
	offCommit   = 16
	offCount    = 24
	offLower    = 26
	offUpper    = 28
	offLevel    = 30
	offSibling  = 32 // next leaf; group number of a bitmap; index ID of a fixed page
	offAux      = 40 // first child of an internal page
)

var le = binary.LittleEndian

// Buf is one page image.
type Buf = [Size]byte

// Header gives typed access to the header of a page image.
type Header struct{ b *Buf }

// HeaderOf returns the header view of b.
func HeaderOf(b *Buf) Header { return Header{b} }

func (h Header) Type() Type           { return Type(h.b[offType]) }
func (h Header) Flags() uint8         { return h.b[offFlags] }
func (h Header) ID() ID               { return ID(le.Uint64(h.b[offID:])) }
func (h Header) CommitID() uint64     { return le.Uint64(h.b[offCommit:]) }
func (h Header) Count() int           { return int(le.Uint16(h.b[offCount:])) }
func (h Header) Lower() int           { return int(le.Uint16(h.b[offLower:])) }
func (h Header) Upper() int           { return int(le.Uint16(h.b[offUpper:])) }
func (h Header) Level() uint16        { return le.Uint16(h.b[offLevel:]) }
func (h Header) Sibling() ID          { return ID(le.Uint64(h.b[offSibling:])) }
func (h Header) Aux() ID              { return ID(le.Uint64(h.b[offAux:])) }
func (h Header) SetCommitID(c uint64) { le.PutUint64(h.b[offCommit:], c) }

// initHeader clears b and writes the fields shared by every page type.
func initHeader(b *Buf, t Type, id ID) {
	*b = Buf{}
	b[offType] = byte(t)
	le.PutUint16(b[offHdrBytes:], HeaderSize)
	le.PutUint64(b[offID:], uint64(id))
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Seal computes the CRC32C of the page, with the CRC field zeroed during
// the computation, and stores it (SPEC §7.3).
func Seal(b *Buf) {
	le.PutUint32(b[offCRC:], 0)
	le.PutUint32(b[offCRC:], crc32.Checksum(b[:], castagnoli))
}

// checkCRC reports whether the stored CRC matches the page.
func checkCRC(b *Buf) bool {
	stored := le.Uint32(b[offCRC:])
	c := crc32.Update(0, castagnoli, b[:offCRC])
	c = crc32.Update(c, castagnoli, []byte{0, 0, 0, 0})
	c = crc32.Update(c, castagnoli, b[offCRC+4:])
	return c == stored
}

func corrupt(id ID, format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, "page", "page %d: "+format, append([]any{id}, args...)...)
}
