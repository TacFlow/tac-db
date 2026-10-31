package wal

import (
	"context"
	"errors"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

var ctx = context.Background()

const dir = "db.tacdb-wal"

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

// chain builds consecutive groups of one intent each over a base commit.
type chain struct {
	commit uint64
	group  uint64
}

// next returns a group of one intent writing the given pages, each an
// overflow page holding its ID and the commit.
func (c *chain) next(t *testing.T, ids ...page.ID) *Group {
	t.Helper()
	c.group++
	parent := c.commit
	c.commit++
	g := &Group{ID: c.group, Begins: []Begin{{Token: [16]byte{byte(c.commit)}, Parent: parent, CommitID: c.commit}}}
	var m Manifest
	m.AddBegin(&g.Begins[0])
	for _, id := range ids {
		img := new(page.Buf)
		if err := page.EncodeOverflow(img, id, []byte{byte(id), byte(c.commit)}); err != nil {
			t.Fatal(err)
		}
		page.HeaderOf(img).SetCommitID(c.commit)
		page.Seal(img)
		g.Pages = append(g.Pages, img)
		m.AddPage(id, img)
	}
	g.Commit = Commit{IntentCount: 1, PageCount: uint32(len(ids)), Digest: m.Digest(),
		State: State{CommitID: c.commit, Parent: parent, PageCount: 1000}}
	return g
}

func newWriter(t *testing.T, fs vfs.FS, segBytes int64) *Writer {
	t.Helper()
	w, err := Create(fs, dir, testUUID, Options{SegmentBytes: segBytes, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func mustAppend(t *testing.T, w *Writer, g *Group) Appended {
	t.Helper()
	a, err := w.AppendGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustSync(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
}

func scanFS(fs vfs.FS) (*ScanResult, error) {
	return Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1})
}

func mustScan(t *testing.T, fs vfs.FS) *ScanResult {
	t.Helper()
	res, err := scanFS(fs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// commits lists the CommitIDs of the recovered groups.
func commits(res *ScanResult) []uint64 {
	var out []uint64
	for _, g := range res.Groups {
		out = append(out, g.Commit.State.CommitID)
	}
	return out
}

// poke changes one byte of a file and syncs it, as corruption on the
// medium would.
func poke(t *testing.T, fs vfs.FS, name string, off int64, xor byte) {
	t.Helper()
	f, err := fs.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= xor
	if err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// resume continues a log from a scan.
func resume(res *ScanResult) Resume {
	return Resume{End: res.End, Discarded: res.TailDiscarded, LastGroupID: res.LastGroupID, LastCommitID: res.LastCommitID}
}

// rawRecords encodes a group as AppendGroup would, from LSN lsn, without
// the checks of the Writer, so that tests can forge it.
func rawRecords(g *Group, lsn, durable uint64) []Record {
	var out []Record
	prev := uint64(0)
	add := func(r Record) {
		r.LSN, r.GroupID, r.PrevLSN = lsn, g.ID, prev
		out = append(out, r)
		prev, lsn = lsn, lsn+1
	}
	for _, b := range g.Begins {
		b.DurableLSN = durable
		add(Record{Type: TypeBegin, CommitID: b.CommitID, Payload: b.Encode()})
	}
	for _, img := range g.Pages {
		h := page.HeaderOf(img)
		add(Record{Type: TypePage, CommitID: h.CommitID(), PageID: h.ID(), Payload: img[:]})
	}
	add(Record{Type: TypeCommit, CommitID: g.Begins[len(g.Begins)-1].CommitID, Payload: g.Commit.Encode()})
	return out
}

// writeRaw writes records at a position of the log and syncs them.
func writeRaw(t *testing.T, fs vfs.FS, at Position, recs []Record) {
	t.Helper()
	var b []byte
	for i := range recs {
		b = AppendRecord(b, &recs[i])
	}
	f, err := fs.Open(dir + "/" + SegmentName(at.Seq))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.WriteAt(b, at.Offset); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// fixDigest recomputes the manifest digest of forged group records, so
// that a test reaches the checks behind it.
func fixDigest(recs []Record) []Record {
	var m Manifest
	for _, r := range recs {
		switch r.Type {
		case TypeBegin:
			b, _ := DecodeBegin(r.Payload)
			m.AddBegin(&b)
		case TypePage:
			img := new(page.Buf)
			copy(img[:], r.Payload)
			m.AddPage(r.PageID, img)
		case TypeCommit:
			cm, _ := DecodeCommit(r.Payload)
			cm.Digest = m.Digest()
			r := &recs[len(recs)-1]
			r.Payload = cm.Encode()
		}
	}
	return recs
}
