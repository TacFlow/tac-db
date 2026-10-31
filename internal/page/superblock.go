package page

import (
	"bytes"
	"hash/crc32"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Superblock versions this reader supports (SPEC §7.2).
const (
	FormatVersion    = 2
	ReaderVersion    = 2
	FormatRevision   = 13
	NumRoots         = 32
	UsedRoots        = 19 // roots 19..31 are reserved and must be zero
	FlagCluster      = 1  // superblock flag bit 0
	superblockMagic  = "TACDBG2\x00"
	superblockCRCOff = Size - 4
)

// Superblock is the content of page 0 or 1 (SPEC §7.2). The version
// fields are constants of the format: Encode writes them and Classify
// checks them.
type Superblock struct {
	UUID             [16]byte
	Generation       uint64
	CheckpointLSN    uint64
	CommitID         uint64
	SchemaEpoch      uint64
	NodeCount        uint64
	EdgeCount        uint64
	NodeHWM          uint64
	EdgeHWM          uint64
	SymbolHWM        uint64
	PageCount        uint64
	WALFloorSegment  uint64
	Flags            uint64
	Roots            [NumRoots]ID
	FreePageCount    uint64
	AppliedRaftIndex uint64
	AppliedRaftTerm  uint64
}

// Encode writes the superblock with format version 2, min_reader_version 2
// and revision 13 and seals it with its CRC32C (bytes 0..4091).
func (s *Superblock) Encode(b *Buf) {
	*b = Buf{}
	copy(b[0:8], superblockMagic)
	le.PutUint16(b[8:], FormatVersion)
	le.PutUint16(b[10:], ReaderVersion)
	le.PutUint32(b[12:], Size)
	copy(b[16:32], s.UUID[:])
	for i, v := range []uint64{s.Generation, s.CheckpointLSN, s.CommitID, s.SchemaEpoch,
		s.NodeCount, s.EdgeCount, s.NodeHWM, s.EdgeHWM, s.SymbolHWM, s.PageCount,
		s.WALFloorSegment, s.Flags} {
		le.PutUint64(b[32+8*i:], v)
	}
	for i, r := range s.Roots {
		le.PutUint64(b[128+8*i:], uint64(r))
	}
	le.PutUint64(b[384:], s.FreePageCount)
	le.PutUint64(b[392:], s.AppliedRaftIndex)
	le.PutUint64(b[400:], s.AppliedRaftTerm)
	le.PutUint16(b[408:], FormatRevision)
	le.PutUint32(b[superblockCRCOff:], crc32.Checksum(b[:superblockCRCOff], castagnoli))
}

// Kind classifies a superblock copy (SPEC §7.2, R13-20).
type Kind int

// Superblock kinds.
const (
	Foreign     Kind = iota // not a G2 superblock
	Damaged                 // a G2 superblock with a bad CRC or field
	Unsupported             // a valid superblock of another version
	Valid
)

// Classify decodes and classifies one superblock copy, in the order of
// SPEC §7.2: magic, CRC, version fields, then the fields that depend on
// the revision. The reason explains any kind other than Valid.
func Classify(b *Buf) (Superblock, Kind, string) {
	var s Superblock
	if string(b[0:8]) != superblockMagic {
		return s, Foreign, "unknown magic"
	}
	if crc32.Checksum(b[:superblockCRCOff], castagnoli) != le.Uint32(b[superblockCRCOff:]) {
		return s, Damaged, "CRC mismatch"
	}
	if v := le.Uint16(b[8:]); v != FormatVersion {
		return s, Unsupported, "format_version is not 2"
	}
	if r := le.Uint16(b[408:]); r != FormatRevision {
		return s, Unsupported, "format_revision is not 13"
	}
	minReader := le.Uint16(b[10:])
	if minReader > ReaderVersion {
		return s, Unsupported, "min_reader_version above this reader"
	}
	if minReader != ReaderVersion {
		return s, Damaged, "min_reader_version is not 2"
	}
	if le.Uint32(b[12:]) != Size {
		return s, Damaged, "page_size is not 4096"
	}
	copy(s.UUID[:], b[16:32])
	fields := []*uint64{&s.Generation, &s.CheckpointLSN, &s.CommitID, &s.SchemaEpoch,
		&s.NodeCount, &s.EdgeCount, &s.NodeHWM, &s.EdgeHWM, &s.SymbolHWM, &s.PageCount,
		&s.WALFloorSegment, &s.Flags}
	for i, f := range fields {
		*f = le.Uint64(b[32+8*i:])
	}
	for i := range s.Roots {
		s.Roots[i] = ID(le.Uint64(b[128+8*i:]))
	}
	s.FreePageCount = le.Uint64(b[384:])
	s.AppliedRaftIndex = le.Uint64(b[392:])
	s.AppliedRaftTerm = le.Uint64(b[400:])
	switch {
	case s.Flags&^FlagCluster != 0:
		return s, Damaged, "unknown flags"
	case !allZero(b[410:superblockCRCOff]):
		return s, Damaged, "reserved bytes are not zero"
	case s.PageCount < 2 || s.PageCount > MaxPageCount:
		return s, Damaged, "pageCount out of range"
	case s.FreePageCount > s.PageCount:
		return s, Damaged, "free_page_count above pageCount"
	}
	for i, r := range s.Roots {
		if i >= UsedRoots && r != 0 {
			return s, Damaged, "reserved root is not zero"
		}
		if r != 0 && CheckPointer(r, s.PageCount) != nil {
			return s, Damaged, "root is not a valid data pointer"
		}
	}
	return s, Valid, ""
}

// ChooseSuperblock picks the superblock to open from the copies in pages 0
// and 1 (SPEC §7.2): any unsupported copy, or two foreign copies, make the
// file UNSUPPORTED_FORMAT; among valid copies the higher generation wins,
// and equal generations with different content are CORRUPT; with no valid
// copy the file is CORRUPT. It returns the chosen copy and its page.
func ChooseSuperblock(a, b *Buf) (Superblock, ID, error) {
	const op = "page.ChooseSuperblock"
	sa, ka, ra := Classify(a)
	sb, kb, rb := Classify(b)
	switch {
	case ka == Unsupported:
		return Superblock{}, 0, errs.Newf(errs.CodeUnsupportedFormat, op, "superblock 0: %s", ra)
	case kb == Unsupported:
		return Superblock{}, 0, errs.Newf(errs.CodeUnsupportedFormat, op, "superblock 1: %s", rb)
	case ka == Foreign && kb == Foreign:
		return Superblock{}, 0, errs.New(errs.CodeUnsupportedFormat, op, "not a G2 database")
	case ka == Valid && kb == Valid:
		switch {
		case sa.Generation > sb.Generation:
			return sa, 0, nil
		case sb.Generation > sa.Generation:
			return sb, 1, nil
		case bytes.Equal(a[:], b[:]):
			return sa, 0, nil
		}
		return Superblock{}, 0, errs.Newf(errs.CodeCorrupt, op, "both superblocks have generation %d with different content", sa.Generation)
	case ka == Valid:
		return sa, 0, nil
	case kb == Valid:
		return sb, 1, nil
	}
	return Superblock{}, 0, errs.Newf(errs.CodeCorrupt, op, "no valid superblock (0: %s; 1: %s)", ra, rb)
}
