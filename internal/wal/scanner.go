package wal

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// ScanInput says where recovery starts: the log directory and the state of
// the chosen superblock.
type ScanInput struct {
	FS            vfs.FS
	Dir           string
	UUID          [16]byte
	FloorSeq      uint64 // wal_floor_segment
	CheckpointLSN uint64
	// BaseCommitID is the CommitID of the checkpointed state, the parent of
	// the first group to recover.
	BaseCommitID uint64
	// MaxSegmentBytes bounds the size of a segment the scan reads into
	// memory; a larger file is CORRUPT. Zero means DefaultMaxSegmentBytes.
	MaxSegmentBytes int64
}

// DefaultMaxSegmentBytes bounds a segment: the default target size plus a
// group of the largest transaction, with room to spare.
const DefaultMaxSegmentBytes = 256 << 20

// PageRef locates the version of a page written by a recovered group.
type PageRef struct {
	ID       page.ID
	CommitID uint64
	Loc      Locator
}

// ScannedGroup is a complete, valid group.
type ScannedGroup struct {
	ID        uint64
	Begins    []Begin
	Pages     []PageRef
	Commit    Commit
	CommitLSN uint64
}

// ScannedReserve is a RESERVE_IDS record. Discarded marks one with a valid
// CRC found in a discarded tail: applying it only skips identifiers, so
// recovery may do it (SPEC §9.3).
type ScannedReserve struct {
	Reserve
	LSN       uint64
	Discarded bool
}

// ScanResult is the recoverable prefix of the log.
type ScanResult struct {
	Groups   []ScannedGroup
	Reserves []ScannedReserve
	// End is where the valid prefix ends: the next append goes there, or
	// to a new segment when TailDiscarded.
	End           Position
	TailDiscarded bool
	// LastGroupID and LastCommitID describe the last recovered group (zero
	// and BaseCommitID when there is none); the Writer continues from them.
	LastGroupID  uint64
	LastCommitID uint64
	Segments     []uint64 // the live segments, from the floor up
	// Stale lists names that can be removed once the state is
	// established: segments below the floor and a leftover spare.
	Stale []string
}

// Scan validates the log from the floor segment and returns its
// recoverable prefix (SPEC §11.2, R13-23):
//   - segment names, sequence, UUID and LSN continuity (REC-10);
//   - every segment but the last is valid up to the prev_valid_end that the
//     next one declares, and a defect before that is CORRUPT;
//   - in the last segment, data ends at a zero magic followed only by zeros;
//   - a defect in the last segment is a discarded tail, unless a valid BEGIN
//     after it declares a durable_lsn at or beyond it: then the region had
//     been synced, and the log is CORRUPT.
func Scan(ctx context.Context, in ScanInput) (*ScanResult, error) {
	res := &ScanResult{}
	names, err := in.FS.List(in.Dir)
	if err != nil {
		return nil, err
	}
	var seqs []uint64
	for _, name := range names {
		if name == SpareName {
			res.Stale = append(res.Stale, name)
			continue
		}
		seq, ok := parseSegmentName(name)
		if !ok {
			return nil, corrupt("unexpected file %q in the log directory", name)
		}
		if seq < in.FloorSeq {
			// A leftover below the floor is removable only if it is ours.
			if err := checkStale(in, seq); err != nil {
				return nil, err
			}
			res.Stale = append(res.Stale, name)
			continue
		}
		seqs = append(seqs, seq)
	}
	if in.MaxSegmentBytes == 0 {
		in.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	slices.Sort(seqs)
	if len(seqs) == 0 || seqs[0] != in.FloorSeq {
		return nil, corrupt("the floor segment %d is missing", in.FloorSeq)
	}
	for i, seq := range seqs {
		if seq != in.FloorSeq+uint64(i) {
			return nil, corrupt("segment %d is missing", in.FloorSeq+uint64(i))
		}
	}
	res.Segments = seqs

	headers := make([]SegmentHeader, len(seqs))
	files := make([]vfs.File, len(seqs))
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	for i, seq := range seqs {
		f, err := in.FS.OpenReadOnly(in.Dir + "/" + SegmentName(seq))
		if err != nil {
			return nil, err
		}
		files[i] = f
		var b [SegmentHeaderSize]byte
		if err := f.ReadAt(b[:], 0); err != nil {
			return nil, corrupt("segment %d: header: %v", seq, err)
		}
		h, err := DecodeSegmentHeader(b[:])
		if err != nil {
			return nil, err
		}
		switch {
		case h.UUID != in.UUID:
			return nil, corrupt("segment %d belongs to another database", seq)
		case h.Seq != seq:
			return nil, corrupt("segment file %d claims seq %d", seq, h.Seq)
		case i == 0 && h.FirstLSN > in.CheckpointLSN+1:
			return nil, corrupt("segment %d starts at LSN %d, after checkpoint_lsn %d", seq, h.FirstLSN, in.CheckpointLSN)
		}
		headers[i] = h
	}

	s := &scan{in: in, res: res, lastCommit: in.BaseCommitID, nextLSN: headers[0].FirstLSN, validLSN: headers[0].FirstLSN}
	for i, seq := range seqs {
		if err := ctx.Err(); err != nil {
			return nil, errs.Wrap(errs.CodeCanceled, "wal.Scan", err, "scan")
		}
		size, err := files[i].Size()
		if err != nil {
			return nil, err
		}
		if size > in.MaxSegmentBytes {
			return nil, corrupt("segment %d has %d bytes, above %d", seq, size, in.MaxSegmentBytes)
		}
		data := make([]byte, size)
		if err := files[i].ReadAt(data, 0); err != nil {
			return nil, err
		}
		if i > 0 && headers[i].FirstLSN != s.nextLSN {
			return nil, corrupt("segment %d starts at LSN %d, expected %d", seq, headers[i].FirstLSN, s.nextLSN)
		}
		last := i == len(seqs)-1
		limit := int64(len(data))
		if !last {
			limit = int64(headers[i+1].PrevValidEnd)
			if limit < SegmentHeaderSize || limit > int64(len(data)) || (limit-SegmentHeaderSize)%8 != 0 {
				return nil, corrupt("segment %d declares prev_valid_end %d", headers[i+1].Seq, limit)
			}
		}
		if err := s.segment(seq, data, limit, last); err != nil {
			return nil, err
		}
	}
	// The log must reach checkpoint_lsn: the checkpoint proves it durable.
	if res.End.NextLSN <= in.CheckpointLSN {
		return nil, corrupt("the log ends at LSN %d, before checkpoint_lsn %d", res.End.NextLSN-1, in.CheckpointLSN)
	}
	res.LastGroupID, res.LastCommitID = s.lastGroup, s.lastCommit
	return res, nil
}

// checkStale validates a segment below the floor before it is listed as
// removable: it must be a segment of this database with its own seq.
func checkStale(in ScanInput, seq uint64) error {
	f, err := in.FS.OpenReadOnly(in.Dir + "/" + SegmentName(seq))
	if err != nil {
		return err
	}
	defer f.Close()
	var b [SegmentHeaderSize]byte
	if err := f.ReadAt(b[:], 0); err != nil {
		return corrupt("leftover segment %d: header: %v", seq, err)
	}
	h, err := DecodeSegmentHeader(b[:])
	if err != nil {
		return err
	}
	if h.UUID != in.UUID || h.Seq != seq {
		return corrupt("leftover segment %d is not a segment %d of this database", seq, seq)
	}
	return nil
}

// ParseSegmentName returns the seq of a segment file name.
func ParseSegmentName(name string) (uint64, bool) { return parseSegmentName(name) }

func parseSegmentName(name string) (uint64, bool) {
	hex, ok := strings.CutSuffix(name, ".wal")
	if !ok || len(hex) != 16 || strings.ToLower(hex) != hex {
		return 0, false
	}
	seq, err := strconv.ParseUint(hex, 16, 64)
	return seq, err == nil && seq > 0
}

type scan struct {
	in         ScanInput
	res        *ScanResult
	lastCommit uint64 // CommitID of the last recovered state
	lastGroup  uint64 // ID of the last recovered group
	nextLSN    uint64 // LSN of the next record
	validLSN   uint64 // LSN after the last complete unit
	cur        *pending
}

// pending is a group being read.
type pending struct {
	start   int64 // offset of its first record
	g       ScannedGroup
	prevLSN uint64
	images  []*page.Buf
	man     Manifest
}

// errDefect marks a record that ends the valid prefix; the caller decides
// between a discarded tail and CORRUPT.
type errDefect struct{ why string }

func (e *errDefect) Error() string { return e.why }

func defect(format string, args ...any) error {
	return &errDefect{errs.Newf(errs.CodeCorrupt, "wal", format, args...).Error()}
}

// segment reads the records of one segment up to limit. In a segment that
// is not the last, the data must end exactly at limit.
func (s *scan) segment(seq uint64, data []byte, limit int64, last bool) error {
	off := int64(SegmentHeaderSize)
	valid := off // end of the last complete unit (group or reservation)
	for off < limit {
		if limit-off < 8 || le.Uint32(data[off:]) == 0 {
			if !last {
				return corrupt("segment %d: data ends at %d before prev_valid_end %d", seq, off, limit)
			}
			if allZero(data[off:]) {
				break // an incomplete group before the zeros is handled below
			}
			return s.tail(seq, data, valid, "zero magic followed by data")
		}
		total, err := s.record(seq, data[off:limit], off)
		if err != nil {
			var d *errDefect
			if !errors.As(err, &d) {
				return err
			}
			if !last {
				return corrupt("segment %d, offset %d: %s", seq, off, d.why)
			}
			return s.tail(seq, data, valid, d.why)
		}
		off += int64(total)
		if s.cur == nil {
			valid, s.validLSN = off, s.nextLSN
		}
	}
	if last && s.cur != nil {
		return s.tail(seq, data, valid, "incomplete group at the end of the segment")
	}
	if !last {
		if off != limit || s.cur != nil {
			return corrupt("segment %d does not end on a unit boundary at prev_valid_end %d", seq, limit)
		}
		return nil
	}
	s.res.End = Position{Seq: seq, Offset: valid, NextLSN: s.nextLSN}
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

// record decodes and applies one record; it returns its total length.
func (s *scan) record(seq uint64, b []byte, off int64) (int, error) {
	total, ok := plausible(b)
	if !ok {
		return 0, defect("invalid record header")
	}
	if lsn := le.Uint64(b[16:]); lsn <= s.in.CheckpointLSN && s.cur == nil {
		// Already in the checkpoint: only its length matters.
		if lsn != s.nextLSN {
			return 0, defect("LSN %d where %d was expected", lsn, s.nextLSN)
		}
		// GroupIDs keep growing across the checkpoint, so a writer resumed
		// from this scan never reuses one an older checkpoint would see.
		s.lastGroup = max(s.lastGroup, le.Uint64(b[24:]))
		s.nextLSN++
		return total, nil
	}
	r, _, err := DecodeRecord(b)
	if err != nil {
		return 0, &errDefect{err.Error()}
	}
	if r.LSN != s.nextLSN {
		return 0, defect("LSN %d where %d was expected", r.LSN, s.nextLSN)
	}
	if err := s.apply(&r, seq, off); err != nil {
		return 0, err
	}
	s.nextLSN++
	return total, nil
}

// apply adds a decoded record to the group in progress.
func (s *scan) apply(r *Record, seq uint64, off int64) error {
	c := s.cur
	if c != nil && (r.GroupID != c.g.ID || r.PrevLSN != c.prevLSN) {
		return defect("record %d breaks the chain of group %d", r.LSN, c.g.ID)
	}
	switch r.Type {
	case TypeReserveIDs:
		if c != nil || r.GroupID != 0 || r.CommitID != 0 || r.PrevLSN != 0 {
			return defect("RESERVE_IDS inside a group")
		}
		rv, err := DecodeReserve(r.Payload)
		if err != nil {
			return &errDefect{err.Error()}
		}
		s.res.Reserves = append(s.res.Reserves, ScannedReserve{Reserve: rv, LSN: r.LSN})
	case TypeBegin:
		b, err := DecodeBegin(r.Payload)
		if err != nil {
			return &errDefect{err.Error()}
		}
		parent := s.lastCommit
		if c == nil {
			if r.GroupID <= s.lastGroup || r.PrevLSN != 0 {
				return defect("group %d does not follow group %d", r.GroupID, s.lastGroup)
			}
			c = &pending{start: off, g: ScannedGroup{ID: r.GroupID}}
			s.cur = c
		} else {
			if len(c.g.Pages) > 0 {
				return defect("BEGIN after the pages of group %d", c.g.ID)
			}
			parent = c.g.Begins[len(c.g.Begins)-1].CommitID
		}
		if b.Parent != parent || r.CommitID != b.CommitID {
			return defect("BEGIN of commit %d over parent %d, expected parent %d", b.CommitID, b.Parent, parent)
		}
		c.g.Begins = append(c.g.Begins, b)
		c.man.AddBegin(&b)
	case TypePage:
		if c == nil {
			return defect("PAGE outside a group")
		}
		if len(r.Payload) != page.Size {
			return defect("PAGE payload of %d bytes", len(r.Payload))
		}
		img := new(page.Buf)
		copy(img[:], r.Payload)
		h := page.HeaderOf(img)
		switch {
		case h.ID() != r.PageID || h.CommitID() != r.CommitID:
			return defect("PAGE record of page %d holds page %d, commit %d", r.PageID, h.ID(), h.CommitID())
		case !slices.ContainsFunc(c.g.Begins, func(b Begin) bool { return b.CommitID == r.CommitID }):
			return defect("page %d of commit %d, not an intent of group %d", r.PageID, r.CommitID, c.g.ID)
		case len(c.g.Pages) > 0 && c.g.Pages[len(c.g.Pages)-1].ID >= r.PageID:
			return defect("page %d out of order in group %d", r.PageID, c.g.ID)
		}
		c.g.Pages = append(c.g.Pages, PageRef{ID: r.PageID, CommitID: r.CommitID,
			Loc: Locator{Seq: seq, Offset: off + RecordHeaderSize}})
		c.images = append(c.images, img)
		c.man.AddPage(r.PageID, img)
	case TypeCommit:
		if c == nil {
			return defect("COMMIT outside a group")
		}
		cm, err := DecodeCommit(r.Payload)
		if err != nil {
			return &errDefect{err.Error()}
		}
		last := c.g.Begins[len(c.g.Begins)-1]
		switch {
		case int(cm.IntentCount) != len(c.g.Begins) || int(cm.PageCount) != len(c.g.Pages):
			return defect("COMMIT of group %d counts %d intents and %d pages, the group has %d and %d",
				c.g.ID, cm.IntentCount, cm.PageCount, len(c.g.Begins), len(c.g.Pages))
		case r.CommitID != last.CommitID || cm.State.CommitID != last.CommitID || cm.State.Parent != last.Parent:
			return defect("COMMIT of group %d does not match its last intent", c.g.ID)
		case cm.Digest != c.man.Digest():
			return defect("manifest digest of group %d does not match", c.g.ID)
		}
		for i, img := range c.images {
			if id := c.g.Pages[i].ID; id < page.FirstBitmap || uint64(id) >= cm.State.PageCount {
				return defect("page %d outside the %d pages of the state", id, cm.State.PageCount)
			}
			if err := page.Verify(img, c.g.Pages[i].ID, cm.State.PageCount); err != nil {
				return &errDefect{err.Error()}
			}
		}
		c.g.Commit, c.g.CommitLSN = cm, r.LSN
		s.res.Groups = append(s.res.Groups, c.g)
		s.lastCommit, s.lastGroup, s.cur = cm.State.CommitID, c.g.ID, nil
		return nil
	case TypeAbort:
		if c == nil || len(r.Payload) != 0 {
			return defect("ABORT outside a group or with a payload")
		}
		s.lastGroup, s.cur = c.g.ID, nil // the attempt is not recognized
		return nil
	default: // APPLIED: a valid cluster record is not a torn write
		return errs.Newf(errs.CodeUnsupportedFormat, "wal", "record of type %d in a standalone log", r.Type)
	}
	if s.cur != nil {
		s.cur.prevLSN = r.LSN
	}
	return nil
}

// tail classifies a defect in the last segment (R13-23). valid is the end
// of the last complete unit; the LSN there is the last valid LSN + 1. If a
// valid BEGIN after it declares a durable_lsn at or beyond that LSN, the
// region had been synced, and the log is CORRUPT; otherwise the tail is
// discarded. Valid RESERVE_IDS in the tail are kept, marked Discarded.
func (s *scan) tail(seq uint64, data []byte, valid int64, why string) error {
	lsnP := s.validLSN
	if lsnP <= s.in.CheckpointLSN {
		return corrupt("segment %d, offset %d: %s, before checkpoint_lsn %d, which the checkpoint proves durable",
			seq, valid, why, s.in.CheckpointLSN)
	}
	// A record after P has an LSN in [lsnP, lsnP + records that fit).
	maxLSN := lsnP + uint64(int64(len(data))-valid)/RecordHeaderSize
	for off := valid; off+RecordHeaderSize <= int64(len(data)); off += 8 {
		b := data[off:]
		if _, ok := plausible(b); !ok {
			continue
		}
		t, lsn, n := Type(b[4]), le.Uint64(b[16:]), int(le.Uint32(b[12:]))
		if lsn < lsnP || lsn >= maxLSN ||
			!(t == TypeBegin && n == BeginSize || t == TypeReserveIDs && n == ReserveSize) {
			continue
		}
		r, _, err := DecodeRecord(b)
		if err != nil {
			continue
		}
		switch t {
		case TypeBegin:
			if bg, err := DecodeBegin(r.Payload); err == nil && bg.DurableLSN >= lsnP {
				return corrupt("segment %d, offset %d: %s, and a later BEGIN shows the region was durable (durable_lsn %d >= %d)",
					seq, valid, why, bg.DurableLSN, lsnP)
			}
		case TypeReserveIDs:
			if rv, err := DecodeReserve(r.Payload); err == nil {
				s.res.Reserves = append(s.res.Reserves, ScannedReserve{Reserve: rv, LSN: r.LSN, Discarded: true})
			}
		}
	}
	s.cur = nil
	s.res.End = Position{Seq: seq, Offset: valid, NextLSN: lsnP}
	s.res.TailDiscarded = true
	return nil
}
