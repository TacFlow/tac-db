// Package wal implements the G2 write-ahead log (SPEC §9.1, §11.2): the
// segment and record formats, a Writer that appends commit groups and
// identifier reservations, and a Scanner that validates the log and
// classifies its tail (R13-23).
package wal

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
)

// Sizes and constants of the format (SPEC §9.1).
const (
	SegmentHeaderSize = 4096
	RecordHeaderSize  = 64
	MaxRecord         = 8192
	Version           = 2
	FormatRevision    = 13
	StateSize         = 384
	BeginSize         = 48 // R13-23: durable_lsn added
	CommitSize        = 4 + 4 + StateSize + 32
	ReserveSize       = 16

	recordMagic  = 0x32524354
	segmentMagic = "TACWAL2\x00"
	crcOff       = 60
)

var (
	le         = binary.LittleEndian
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
)

// Type is the type of a record.
type Type uint8

// Record types (SPEC §9.1).
const (
	TypeBegin      Type = 1
	TypePage       Type = 2
	TypeCommit     Type = 3
	TypeAbort      Type = 4
	TypeReserveIDs Type = 5
	TypeApplied    Type = 6 // cluster (M11); refused in a standalone log
)

func corrupt(format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, "wal", format, args...)
}

// SegmentHeader is the first 4096 bytes of a segment.
type SegmentHeader struct {
	UUID              [16]byte
	Seq               uint64
	FirstLSN          uint64
	BaseCheckpointLSN uint64
	CreatedUnixNs     int64
	// PrevValidEnd is the offset where the valid data of the previous
	// segment ends; zero in segment 1 (R13-23).
	PrevValidEnd uint64
}

// Encode writes the header into b, which must hold SegmentHeaderSize bytes.
func (h *SegmentHeader) Encode(b []byte) {
	clear(b[:SegmentHeaderSize])
	copy(b, segmentMagic)
	le.PutUint16(b[8:], Version)
	le.PutUint16(b[10:], SegmentHeaderSize)
	le.PutUint32(b[12:], page.Size)
	copy(b[16:32], h.UUID[:])
	le.PutUint64(b[32:], h.Seq)
	le.PutUint64(b[40:], h.FirstLSN)
	le.PutUint64(b[48:], h.BaseCheckpointLSN)
	le.PutUint64(b[56:], uint64(h.CreatedUnixNs))
	le.PutUint16(b[64:], FormatRevision)
	le.PutUint64(b[68:], h.PrevValidEnd)
	le.PutUint32(b[4092:], crc32.Checksum(b[:4092], castagnoli))
}

// DecodeSegmentHeader validates and decodes a segment header: magic, CRC,
// version fields, then the other fields. A different format or revision
// is UNSUPPORTED_FORMAT; anything else invalid is CORRUPT.
func DecodeSegmentHeader(b []byte) (SegmentHeader, error) {
	var h SegmentHeader
	if len(b) < SegmentHeaderSize {
		return h, corrupt("segment header of %d bytes", len(b))
	}
	if string(b[:8]) != segmentMagic {
		return h, corrupt("segment without the WAL magic")
	}
	if crc32.Checksum(b[:4092], castagnoli) != le.Uint32(b[4092:]) {
		return h, corrupt("segment header CRC mismatch")
	}
	if v, r := le.Uint16(b[8:]), le.Uint16(b[64:]); v != Version || r != FormatRevision {
		return h, errs.Newf(errs.CodeUnsupportedFormat, "wal", "segment version %d, revision %d", v, r)
	}
	if le.Uint16(b[10:]) != SegmentHeaderSize || le.Uint32(b[12:]) != page.Size {
		return h, corrupt("segment header with header_bytes %d, page_size %d", le.Uint16(b[10:]), le.Uint32(b[12:]))
	}
	for _, c := range append(b[66:68:68], b[76:4092]...) {
		if c != 0 {
			return h, corrupt("segment header with non-zero reserved bytes")
		}
	}
	copy(h.UUID[:], b[16:32])
	h.Seq = le.Uint64(b[32:])
	h.FirstLSN = le.Uint64(b[40:])
	h.BaseCheckpointLSN = le.Uint64(b[48:])
	h.CreatedUnixNs = int64(le.Uint64(b[56:]))
	h.PrevValidEnd = le.Uint64(b[68:])
	if h.Seq == 0 || h.FirstLSN == 0 {
		return h, corrupt("segment with seq %d, first_lsn %d", h.Seq, h.FirstLSN)
	}
	return h, nil
}

// Record is one record of the log.
type Record struct {
	Type     Type
	LSN      uint64
	GroupID  uint64 // zero for a reservation
	CommitID uint64
	PrevLSN  uint64 // the previous record of the same group, or zero
	PageID   page.ID
	Payload  []byte
}

// encodedLen is the total length of a record with n payload bytes,
// padded to a multiple of 8.
func encodedLen(n int) int { return (RecordHeaderSize + n + 7) &^ 7 }

// AppendRecord appends the encoding of r to dst.
func AppendRecord(dst []byte, r *Record) []byte {
	total := encodedLen(len(r.Payload))
	start := len(dst)
	dst = append(dst, make([]byte, total)...)
	b := dst[start:]
	le.PutUint32(b[0:], recordMagic)
	b[4] = byte(r.Type)
	le.PutUint16(b[6:], RecordHeaderSize)
	le.PutUint32(b[8:], uint32(total))
	le.PutUint32(b[12:], uint32(len(r.Payload)))
	le.PutUint64(b[16:], r.LSN)
	le.PutUint64(b[24:], r.GroupID)
	le.PutUint64(b[32:], r.CommitID)
	le.PutUint64(b[40:], r.PrevLSN)
	le.PutUint64(b[48:], uint64(r.PageID))
	copy(b[RecordHeaderSize:], r.Payload)
	le.PutUint32(b[crcOff:], crc32.Checksum(b[:total], castagnoli))
	return dst
}

// plausible checks the fixed fields of a record header without the CRC:
// it is cheap, so the forward scan of R13-23 runs it at every aligned
// position. It returns the total length.
func plausible(b []byte) (int, bool) {
	if len(b) < RecordHeaderSize || le.Uint32(b) != recordMagic {
		return 0, false
	}
	t, total, n := Type(b[4]), int(le.Uint32(b[8:])), int(le.Uint32(b[12:]))
	if t < TypeBegin || t > TypeApplied || b[5] != 0 || le.Uint16(b[6:]) != RecordHeaderSize ||
		total < RecordHeaderSize || total > MaxRecord || total%8 != 0 || n > total-RecordHeaderSize ||
		total != encodedLen(n) || total > len(b) {
		return 0, false
	}
	return total, true
}

// DecodeRecord decodes the record at the start of b and returns it with
// its total length. Every field and the CRC are checked; the payload
// aliases b.
func DecodeRecord(b []byte) (Record, int, error) {
	total, ok := plausible(b)
	if !ok {
		return Record{}, 0, corrupt("invalid record header")
	}
	if le.Uint32(b[56:]) != 0 {
		return Record{}, 0, corrupt("record with non-zero reserved bytes")
	}
	want := le.Uint32(b[crcOff:])
	var head [RecordHeaderSize]byte
	copy(head[:], b[:RecordHeaderSize])
	le.PutUint32(head[crcOff:], 0)
	crc := crc32.Update(crc32.Checksum(head[:], castagnoli), castagnoli, b[RecordHeaderSize:total])
	if crc != want {
		return Record{}, 0, corrupt("record CRC mismatch")
	}
	n := int(le.Uint32(b[12:]))
	for _, c := range b[RecordHeaderSize+n : total] {
		if c != 0 {
			return Record{}, 0, corrupt("record padding is not zero")
		}
	}
	r := Record{
		Type: Type(b[4]), LSN: le.Uint64(b[16:]), GroupID: le.Uint64(b[24:]),
		CommitID: le.Uint64(b[32:]), PrevLSN: le.Uint64(b[40:]), PageID: page.ID(le.Uint64(b[48:])),
		Payload: b[RecordHeaderSize : RecordHeaderSize+n],
	}
	if r.LSN == 0 {
		return Record{}, 0, corrupt("record with LSN 0")
	}
	if r.Type != TypePage && r.PageID != 0 {
		return Record{}, 0, corrupt("type %d record with a PageID", r.Type)
	}
	return r, total, nil
}

// Begin is the payload of a BEGIN record (R13-23: 48 bytes).
type Begin struct {
	Token    [16]byte
	Parent   uint64
	CommitID uint64
	Flags    uint32
	// DurableLSN is a lower bound of the log known durable when the group
	// was formed: the highest LSN of any record whose write completed
	// before a Sync that then returned success.
	DurableLSN uint64
}

// Encode returns the payload.
func (b *Begin) Encode() []byte {
	p := make([]byte, BeginSize)
	copy(p, b.Token[:])
	le.PutUint64(p[16:], b.Parent)
	le.PutUint64(p[24:], b.CommitID)
	le.PutUint32(p[32:], b.Flags)
	le.PutUint64(p[40:], b.DurableLSN)
	return p
}

// DecodeBegin decodes a BEGIN payload.
func DecodeBegin(p []byte) (Begin, error) {
	var b Begin
	if len(p) != BeginSize || le.Uint32(p[36:]) != 0 {
		return b, corrupt("BEGIN payload of %d bytes", len(p))
	}
	copy(b.Token[:], p)
	b.Parent, b.CommitID = le.Uint64(p[16:]), le.Uint64(p[24:])
	b.Flags, b.DurableLSN = le.Uint32(p[32:]), le.Uint64(p[40:])
	return b, b.validate()
}

// validate applies the rules of a BEGIN; the Writer and the Scanner share
// them, so that the Writer never acknowledges what recovery would refuse.
func (b *Begin) validate() error {
	if b.Flags != 0 {
		return corrupt("BEGIN flags 0x%x", b.Flags)
	}
	if b.CommitID <= b.Parent {
		return corrupt("BEGIN of commit %d over parent %d", b.CommitID, b.Parent)
	}
	return nil
}

// State is the published state of a commit (SPEC §9.1, 384 bytes).
type State struct {
	CommitID, SchemaEpoch, NodeCount, EdgeCount uint64
	NodeHWM, EdgeHWM, SymbolHWM                 uint64
	PageCount, FreePageCount                    uint64
	Roots                                       [page.NumRoots]page.ID
	Parent, AppliedRaftIndex, AppliedRaftTerm   uint64
}

// Encode writes the state into b (StateSize bytes).
func (s *State) Encode(b []byte) {
	clear(b[:StateSize])
	for i, v := range []uint64{s.CommitID, s.SchemaEpoch, s.NodeCount, s.EdgeCount,
		s.NodeHWM, s.EdgeHWM, s.SymbolHWM, s.PageCount, s.FreePageCount} {
		le.PutUint64(b[8*i:], v)
	}
	for i, r := range s.Roots {
		le.PutUint64(b[72+8*i:], uint64(r))
	}
	le.PutUint64(b[328:], s.Parent)
	le.PutUint64(b[336:], s.AppliedRaftIndex)
	le.PutUint64(b[344:], s.AppliedRaftTerm)
}

// DecodeState decodes a state; the 32 trailing bytes must be zero, and a
// standalone log has no Raft fields.
func DecodeState(b []byte) (State, error) {
	var s State
	fields := []*uint64{&s.CommitID, &s.SchemaEpoch, &s.NodeCount, &s.EdgeCount,
		&s.NodeHWM, &s.EdgeHWM, &s.SymbolHWM, &s.PageCount, &s.FreePageCount}
	for i, f := range fields {
		*f = le.Uint64(b[8*i:])
	}
	for i := range s.Roots {
		s.Roots[i] = page.ID(le.Uint64(b[72+8*i:]))
	}
	s.Parent, s.AppliedRaftIndex, s.AppliedRaftTerm = le.Uint64(b[328:]), le.Uint64(b[336:]), le.Uint64(b[344:])
	for _, c := range b[352:StateSize] {
		if c != 0 {
			return s, corrupt("state with non-zero reserved bytes")
		}
	}
	return s, s.validate()
}

// validate applies the rules of a standalone State, shared by the Writer
// and the Scanner.
func (s *State) validate() error {
	switch {
	case s.AppliedRaftIndex != 0 || s.AppliedRaftTerm != 0:
		return corrupt("Raft fields in a standalone state")
	case s.PageCount < 2 || s.PageCount > page.MaxPageCount || s.FreePageCount > s.PageCount:
		return corrupt("state with pageCount %d and free_page_count %d", s.PageCount, s.FreePageCount)
	}
	for i, r := range s.Roots {
		if i >= page.UsedRoots && r != 0 {
			return corrupt("reserved root %d is not zero", i)
		}
		if r != 0 && page.CheckPointer(r, s.PageCount) != nil {
			return corrupt("root %d is not a valid data pointer", i)
		}
	}
	return nil
}

// Commit is the payload of a COMMIT record.
type Commit struct {
	IntentCount uint32
	PageCount   uint32
	State       State
	Digest      [32]byte // SHA-256 of the manifest of the group
}

// Encode returns the payload.
func (c *Commit) Encode() []byte {
	p := make([]byte, CommitSize)
	le.PutUint32(p, c.IntentCount)
	le.PutUint32(p[4:], c.PageCount)
	c.State.Encode(p[8 : 8+StateSize])
	copy(p[8+StateSize:], c.Digest[:])
	return p
}

// DecodeCommit decodes a COMMIT payload.
func DecodeCommit(p []byte) (Commit, error) {
	var c Commit
	if len(p) != CommitSize {
		return c, corrupt("COMMIT payload of %d bytes", len(p))
	}
	c.IntentCount, c.PageCount = le.Uint32(p), le.Uint32(p[4:])
	var err error
	if c.State, err = DecodeState(p[8 : 8+StateSize]); err != nil {
		return c, err
	}
	copy(c.Digest[:], p[8+StateSize:])
	if c.IntentCount == 0 {
		return c, corrupt("COMMIT of no intent")
	}
	return c, nil
}

// Kinds of identifiers reserved by RESERVE_IDS (SPEC §9.3).
const (
	KindNode = 1
	KindEdge = 2
)

// Reserve is the payload of a RESERVE_IDS record.
type Reserve struct {
	Kind          uint8
	HighWatermark uint64
}

// Encode returns the payload.
func (r *Reserve) Encode() []byte {
	p := make([]byte, ReserveSize)
	p[0] = r.Kind
	le.PutUint64(p[8:], r.HighWatermark)
	return p
}

// DecodeReserve decodes a RESERVE_IDS payload.
func DecodeReserve(p []byte) (Reserve, error) {
	var r Reserve
	if len(p) != ReserveSize {
		return r, corrupt("RESERVE_IDS payload of %d bytes", len(p))
	}
	r.Kind, r.HighWatermark = p[0], le.Uint64(p[8:])
	for _, c := range p[1:8] {
		if c != 0 {
			return r, corrupt("RESERVE_IDS with non-zero reserved bytes")
		}
	}
	if r.Kind != KindNode && r.Kind != KindEdge {
		return r, corrupt("RESERVE_IDS of kind %d", r.Kind)
	}
	return r, nil
}

// Manifest accumulates the canonical manifest of a group (SPEC §9.1): for
// each BEGIN, token, parent and commit ID; for each PAGE, PageID and the
// page's CRC32C, in LSN order.
type Manifest struct{ buf []byte }

// AddBegin adds a BEGIN.
func (m *Manifest) AddBegin(b *Begin) {
	m.buf = append(m.buf, b.Token[:]...)
	m.buf = le.AppendUint64(m.buf, b.Parent)
	m.buf = le.AppendUint64(m.buf, b.CommitID)
}

// AddPage adds a page, whose CRC is already set.
func (m *Manifest) AddPage(id page.ID, img *page.Buf) {
	m.buf = le.AppendUint64(m.buf, uint64(id))
	m.buf = append(m.buf, img[4:8]...) // the page CRC field (FORMAT §6)
}

// Digest returns the SHA-256 of the manifest.
func (m *Manifest) Digest() [32]byte { return sha256.Sum256(m.buf) }

// SegmentName returns the file name of segment seq.
func SegmentName(seq uint64) string { return fmt.Sprintf("%016x.wal", seq) }

// SpareName is the name of the segment prepared in advance.
const SpareName = "spare.tmp"
