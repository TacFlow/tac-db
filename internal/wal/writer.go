package wal

import (
	"slices"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// DefaultSegmentBytes is the default target size of a segment
// (WALSegmentBytes, SPEC §9.1).
const DefaultSegmentBytes = 64 << 20

// Options configures a Writer.
type Options struct {
	// SegmentBytes is the target size of a segment, header included; a
	// group never splits, so a segment may exceed it to finish one.
	SegmentBytes int64
	// Now is the clock of created_unix_ns (R13-12); tests fix it.
	Now func() time.Time
	// MaxSegmentBytes bounds a segment, the same bound recovery reads with
	// (ScanInput.MaxSegmentBytes); a group that would not fit a new
	// segment under it is refused. Zero means DefaultMaxSegmentBytes.
	MaxSegmentBytes int64
}

func (o *Options) defaults() {
	if o.SegmentBytes == 0 {
		o.SegmentBytes = DefaultSegmentBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxSegmentBytes == 0 {
		o.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
}

func (o *Options) check() error {
	if o.SegmentBytes < 2*SegmentHeaderSize || o.SegmentBytes > o.MaxSegmentBytes {
		return errs.Newf(errs.CodeInvalidArgument, "wal", "segment of %d bytes, bound %d", o.SegmentBytes, o.MaxSegmentBytes)
	}
	return nil
}

// Position is a place in the log: a segment, an offset in it, and the LSN
// of the record that goes there.
type Position struct {
	Seq     uint64
	Offset  int64
	NextLSN uint64
}

// Locator gives where the payload of a PAGE record lives.
type Locator struct {
	Seq    uint64
	Offset int64
}

// Group is a commit group to append (SPEC §9.2 step 5): the BEGINs of its
// intents in chain order, the final version of each page in PageID order,
// with CommitID and CRC already set, and the COMMIT. The Writer fills the
// durable_lsn of each BEGIN.
type Group struct {
	ID     uint64
	Begins []Begin
	Pages  []*page.Buf
	Commit Commit
}

// Appended describes a group written to the log.
type Appended struct {
	Pages     []Locator // one per page, in the order of Group.Pages
	CommitLSN uint64
}

// Writer appends to the log in directory dir of fs. One goroutine uses it
// (the I/O goroutine of the coordinator); the next segment is prepared by
// a goroutine of its own, outside the commit path.
type Writer struct {
	fs   vfs.FS
	dir  string
	uuid [16]byte
	opts Options

	seg     vfs.File
	seq     uint64
	off     int64
	size    int64 // size of the segment file at its last Sync
	nextLSN uint64
	written uint64 // last LSN whose write completed
	durable uint64 // R13-23: last LSN known durable
	base    uint64 // checkpoint_lsn in force, for new segment headers

	lastGroup  uint64 // GroupID of the last group appended
	lastCommit uint64 // CommitID of the last intent appended

	spare  chan spareResult
	err    error // sticky: after a failed write, roll or Sync the log is unknown
	closed bool

	fullSyncs, dataSyncs int // how Sync synced the segment; tests read them
}

// Resume says where a Writer continues after recovery, from a ScanResult.
type Resume struct {
	End           Position
	Discarded     bool // the Scanner discarded a tail
	CheckpointLSN uint64
	LastGroupID   uint64
	LastCommitID  uint64
}

type spareResult struct {
	f   vfs.File
	err error
}

func (w *Writer) path(name string) string { return w.dir + "/" + name }

// Create creates the log of a new database: the directory dir with
// segment 1 synced, and both directories synced (SPEC §11.1).
func Create(fs vfs.FS, dir string, uuid [16]byte, opts Options) (*Writer, error) {
	opts.defaults()
	if err := opts.check(); err != nil {
		return nil, err
	}
	w := &Writer{fs: fs, dir: dir, uuid: uuid, opts: opts, seq: 1, nextLSN: 1}
	if err := fs.Mkdir(dir); err != nil {
		return nil, err
	}
	f, err := fs.Create(w.path(SegmentName(1)))
	if err != nil {
		return nil, err
	}
	h := SegmentHeader{UUID: uuid, Seq: 1, FirstLSN: 1, CreatedUnixNs: opts.Now().UnixNano()}
	if err := w.initSegment(f, &h); err != nil {
		f.Close()
		return nil, err
	}
	if err := fs.SyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	if err := fs.SyncDir("."); err != nil {
		f.Close()
		return nil, err
	}
	w.seg, w.off, w.size = f, SegmentHeaderSize, opts.SegmentBytes
	w.startSpare()
	return w, nil
}

// initSegment sizes a segment to its target, writes its header and syncs.
func (w *Writer) initSegment(f vfs.File, h *SegmentHeader) error {
	if err := f.Truncate(w.opts.SegmentBytes); err != nil {
		return err
	}
	f.Preallocate(w.opts.SegmentBytes) // best effort: a failure only means the file grows on write
	var b [SegmentHeaderSize]byte
	h.Encode(b[:])
	if err := f.WriteAt(b[:], 0); err != nil {
		return err
	}
	return f.Sync()
}

// Open continues the log after recovery. end is the end of the valid
// prefix the Scanner established; the prefix is synced first, so that
// what recovery read from the operating system cache is durable before
// anyone relies on it. When the Scanner discarded a tail, appends go to a
// new segment whose prev_valid_end marks where the old one ends, and the
// discarded bytes are never read again (R13-23).
func Open(fs vfs.FS, dir string, uuid [16]byte, r Resume, opts Options) (*Writer, error) {
	opts.defaults()
	if err := opts.check(); err != nil {
		return nil, err
	}
	end := r.End
	w := &Writer{fs: fs, dir: dir, uuid: uuid, opts: opts, seq: end.Seq, off: end.Offset,
		nextLSN: end.NextLSN, written: end.NextLSN - 1, base: r.CheckpointLSN,
		lastGroup: r.LastGroupID, lastCommit: r.LastCommitID}
	f, err := fs.Open(w.path(SegmentName(end.Seq)))
	if err != nil {
		return nil, err
	}
	w.seg = f
	if w.size, err = f.Size(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := fs.SyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	w.durable = w.written
	// A spare left by a crash is discarded and prepared again.
	if ok, err := fs.Exists(w.path(SpareName)); err != nil {
		f.Close()
		return nil, err
	} else if ok {
		if err := fs.Remove(w.path(SpareName)); err != nil {
			f.Close()
			return nil, err
		}
	}
	w.startSpare()
	if r.Discarded {
		if err := w.roll(); err != nil {
			w.Close()
			return nil, err
		}
	}
	return w, nil
}

// startSpare prepares the next segment in the background.
func (w *Writer) startSpare() {
	ch := make(chan spareResult, 1)
	w.spare = ch
	fs, name, size := w.fs, w.path(SpareName), w.opts.SegmentBytes
	go func() {
		f, err := fs.Create(name)
		if err == nil {
			if err = f.Truncate(size); err == nil {
				f.Preallocate(size) // best effort
				err = f.Sync()
			}
			if err != nil {
				f.Close()
				f = nil
			}
		}
		ch <- spareResult{f, err}
	}()
}

// roll moves appends to the next segment (SPEC §9.1, R13-23): the current
// segment is synced, the spare gets its real header and is synced, then
// renamed to its final name, and the directory is synced. A segment that
// is not the last is therefore complete on disk.
func (w *Writer) roll() error {
	if err := w.Sync(); err != nil {
		return err
	}
	res := <-w.spare
	w.spare = nil
	if res.err != nil {
		return res.err
	}
	f := res.f
	h := SegmentHeader{UUID: w.uuid, Seq: w.seq + 1, FirstLSN: w.nextLSN, BaseCheckpointLSN: w.base,
		CreatedUnixNs: w.opts.Now().UnixNano(), PrevValidEnd: uint64(w.off)}
	var b [SegmentHeaderSize]byte
	h.Encode(b[:])
	if err := f.WriteAt(b[:], 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := w.fs.Rename(w.path(SpareName), w.path(SegmentName(h.Seq))); err != nil {
		f.Close()
		return err
	}
	if err := w.fs.SyncDir(w.dir); err != nil {
		f.Close()
		return err
	}
	w.seg.Close()
	w.seg, w.seq, w.off, w.size = f, h.Seq, SegmentHeaderSize, w.opts.SegmentBytes
	w.startSpare()
	return nil
}

// write appends encoded records, rolling first if they do not fit the
// current segment, and returns the offset where they start.
func (w *Writer) write(b []byte) (int64, error) {
	if w.off > SegmentHeaderSize && w.off+int64(len(b)) > w.opts.SegmentBytes {
		if err := w.roll(); err != nil {
			return 0, w.fail(err)
		}
	}
	at := w.off
	if err := w.seg.WriteAt(b, at); err != nil {
		return 0, err
	}
	w.off += int64(len(b))
	return at, nil
}

// fail records a fatal error: the log is in an unknown state.
func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return err
}

// AppendGroup writes a group with one WriteAt and returns where its pages
// are. A failed write leaves the log in an unknown state: the caller must
// stop using the Writer (RECOVERY_REQUIRED).
func (w *Writer) AppendGroup(g *Group) (Appended, error) {
	if w.err != nil {
		return Appended{}, w.err
	}
	if err := w.check(g); err != nil {
		return Appended{}, err
	}
	var buf []byte
	lsn, prev := w.nextLSN, uint64(0)
	add := func(r Record) int {
		r.LSN, r.GroupID, r.PrevLSN = lsn, g.ID, prev
		start := len(buf)
		buf = AppendRecord(buf, &r)
		prev, lsn = lsn, lsn+1
		return start
	}
	for i := range g.Begins {
		b := g.Begins[i]
		b.DurableLSN = w.durable
		add(Record{Type: TypeBegin, CommitID: b.CommitID, Payload: b.Encode()})
	}
	starts := make([]int, len(g.Pages))
	for i, img := range g.Pages {
		h := page.HeaderOf(img)
		starts[i] = add(Record{Type: TypePage, CommitID: h.CommitID(), PageID: h.ID(), Payload: img[:]})
	}
	last := g.Begins[len(g.Begins)-1].CommitID
	commitLSN := lsn
	add(Record{Type: TypeCommit, CommitID: last, Payload: g.Commit.Encode()})
	if SegmentHeaderSize+int64(len(buf)) > w.opts.MaxSegmentBytes {
		return Appended{}, errs.Newf(errs.CodeLimitExceeded, "wal.AppendGroup",
			"a group of %d bytes does not fit a segment of at most %d bytes", len(buf), w.opts.MaxSegmentBytes)
	}
	at, err := w.write(buf)
	if err != nil {
		return Appended{}, w.fail(err)
	}
	w.lastGroup, w.lastCommit = g.ID, last
	out := Appended{Pages: make([]Locator, len(g.Pages)), CommitLSN: commitLSN}
	for i, s := range starts {
		out.Pages[i] = Locator{Seq: w.seq, Offset: at + int64(s) + RecordHeaderSize}
	}
	w.nextLSN, w.written = lsn, lsn-1
	return out, nil
}

// AppendReserve writes a RESERVE_IDS record between groups and returns
// its LSN; it becomes durable with the next Sync.
func (w *Writer) AppendReserve(r Reserve) (uint64, error) {
	if w.err != nil {
		return 0, w.err
	}
	lsn := w.nextLSN
	b := AppendRecord(nil, &Record{Type: TypeReserveIDs, LSN: lsn, Payload: r.Encode()})
	if _, err := w.write(b); err != nil {
		return 0, w.fail(err)
	}
	w.nextLSN, w.written = lsn+1, lsn
	return lsn, nil
}

// Sync makes every completed write durable. It syncs metadata as well
// when the segment grew past its size. On success, durable_lsn advances
// to what was written before the call (R13-23); a failed Sync must never
// be retried to declare success.
func (w *Writer) Sync() error {
	if w.err != nil {
		return w.err
	}
	issued := w.written
	var err error
	if w.off > w.size { // the file grew: its size is metadata to sync
		w.fullSyncs++
		err = w.seg.Sync()
	} else {
		w.dataSyncs++
		err = w.seg.SyncData()
	}
	if err != nil {
		return w.fail(err)
	}
	w.size = max(w.size, w.off)
	w.durable = max(w.durable, issued)
	return nil
}

// check refuses a group that recovery would not recognize, with the rules
// the Scanner applies: the chain of parents from the last group appended,
// each BEGIN, the counts and State of the COMMIT, the manifest digest, a
// growing GroupID, and pages in strictly increasing order, each a valid
// image of a PageID in [2, pageCount) and of an intent of the group. A
// group the Writer accepts and syncs is never dropped by recovery.
func (w *Writer) check(g *Group) error {
	invalid := func(format string, args ...any) error {
		return errs.Newf(errs.CodeInvalidArgument, "wal.AppendGroup", format, args...)
	}
	if len(g.Begins) == 0 || g.ID <= w.lastGroup {
		return invalid("group %d after group %d, with %d intents", g.ID, w.lastGroup, len(g.Begins))
	}
	var m Manifest
	parent := w.lastCommit
	for i := range g.Begins {
		b := &g.Begins[i]
		if err := b.validate(); err != nil || b.Parent != parent {
			return invalid("intent %d over parent %d, expected parent %d: %v", b.CommitID, b.Parent, parent, err)
		}
		parent = b.CommitID
		m.AddBegin(b)
	}
	last := g.Begins[len(g.Begins)-1]
	c := &g.Commit
	if int(c.IntentCount) != len(g.Begins) || int(c.PageCount) != len(g.Pages) ||
		c.State.CommitID != last.CommitID || c.State.Parent != last.Parent {
		return invalid("COMMIT does not match the intents and pages of group %d", g.ID)
	}
	if err := c.State.validate(); err != nil {
		return invalid("State of group %d: %v", g.ID, err)
	}
	for i, img := range g.Pages {
		h := page.HeaderOf(img)
		id := h.ID()
		if i > 0 && page.HeaderOf(g.Pages[i-1]).ID() >= id {
			return invalid("pages not in strictly increasing PageID order")
		}
		if id < page.FirstBitmap || uint64(id) >= c.State.PageCount {
			return invalid("page %d outside the %d pages of the state", id, c.State.PageCount)
		}
		if !slices.ContainsFunc(g.Begins, func(b Begin) bool { return b.CommitID == h.CommitID() }) {
			return invalid("page %d of commit %d, not an intent of the group", id, h.CommitID())
		}
		if err := page.Verify(img, id, c.State.PageCount); err != nil {
			return invalid("page %d: %v", id, err)
		}
		m.AddPage(id, img)
	}
	if m.Digest() != c.Digest {
		return invalid("manifest digest of group %d does not match", g.ID)
	}
	return nil
}

// DurableLSN returns the last LSN known durable.
func (w *Writer) DurableLSN() uint64 { return w.durable }

// Position returns where the next record goes.
func (w *Writer) Position() Position { return Position{Seq: w.seq, Offset: w.off, NextLSN: w.nextLSN} }

// SetCheckpointLSN records the checkpoint_lsn in force, written into the
// headers of new segments.
func (w *Writer) SetCheckpointLSN(lsn uint64) { w.base = lsn }

// Close waits for the spare in preparation and closes the files; the
// spare stays for the next open, which discards it.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.spare != nil {
		if res := <-w.spare; res.f != nil {
			res.f.Close()
		}
		w.spare = nil
	}
	return w.seg.Close()
}
