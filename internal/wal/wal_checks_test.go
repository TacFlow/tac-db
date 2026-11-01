package wal

import (
	"slices"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// Checks behind the digest: with a recomputed digest, a forged group is
// still refused for its pages.
func TestForgedGroupsWithValidDigest(t *testing.T) {
	cases := map[string]func(recs []Record) []Record{
		"page 1 (superblock)": func(recs []Record) []Record {
			img := new(page.Buf)
			page.EncodeOverflow(img, 1, []byte{1})
			page.HeaderOf(img).SetCommitID(2)
			page.Seal(img)
			recs[1].Payload, recs[1].PageID = img[:], 1
			return recs
		},
		"pages out of order": func(recs []Record) []Record {
			recs[1].Payload, recs[2].Payload = recs[2].Payload, recs[1].Payload
			recs[1].PageID, recs[2].PageID = recs[2].PageID, recs[1].PageID
			return recs
		},
		"page of another commit": func(recs []Record) []Record {
			img := new(page.Buf)
			copy(img[:], recs[1].Payload)
			page.HeaderOf(img).SetCommitID(99)
			page.Seal(img)
			recs[1].Payload, recs[1].CommitID = img[:], 99
			return recs
		},
	}
	for name, forge := range cases {
		t.Run(name, func(t *testing.T) {
			fs := vfs.NewMemFS()
			var c chain
			end := logOf(t, fs, &c, 1)
			recs := fixDigest(forge(rawRecords(c.next(t, 4, 5), end.NextLSN, 0)))
			writeRaw(t, fs, end, recs)
			res, err := scanFS(fs)
			if err != nil || !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
				t.Fatalf("commits %v, discarded %v, %v", commits(res), res.TailDiscarded, err)
			}
		})
	}
}

// A segment below the floor is a removable leftover only if it belongs to
// the database.
func TestStaleSegmentOfAnotherDatabase(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 16<<10)
	var c chain
	for i := 0; i < 12; i++ {
		mustAppend(t, w, c.next(t, page.ID(3+i)))
	}
	mustSync(t, w)
	w.Close()
	first := mustScan(t, fs)
	var g ScannedGroup
	for _, gr := range first.Groups {
		if gr.Pages[0].Loc.Seq == 2 {
			g = gr
			break
		}
	}
	f, _ := fs.Open(dir + "/" + SegmentName(1))
	var b [SegmentHeaderSize]byte
	f.ReadAt(b[:], 0)
	h, _ := DecodeSegmentHeader(b[:])
	h.UUID[0] ^= 1
	h.Encode(b[:])
	f.WriteAt(b[:], 0)
	f.Sync()
	f.Close()
	_, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 2,
		CheckpointLSN: g.CommitLSN - 3, BaseCommitID: g.Commit.State.Parent})
	if !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("leftover segment of another database: %v, want CORRUPT", err)
	}
}

// ABORT carries no payload; an APPLIED record in a standalone log is not
// a torn write but another format.
func TestAbortPayloadAndApplied(t *testing.T) {
	fs := vfs.NewMemFS()
	var c chain
	end := logOf(t, fs, &c, 1)
	x := c
	g := x.next(t, 4)
	recs := rawRecords(g, end.NextLSN, 0)[:1]
	recs = append(recs, Record{Type: TypeAbort, LSN: end.NextLSN + 1, GroupID: g.ID, PrevLSN: end.NextLSN, Payload: []byte{1}})
	next := c
	next.group = g.ID
	recs = append(recs, rawRecords(next.next(t, 5), end.NextLSN+2, 0)...)
	writeRaw(t, fs, end, recs)
	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
		t.Fatalf("ABORT with a payload: commits %v, discarded %v", commits(res), res.TailDiscarded)
	}

	fs = vfs.NewMemFS()
	c = chain{}
	end = logOf(t, fs, &c, 1)
	writeRaw(t, fs, end, []Record{{Type: TypeApplied, LSN: end.NextLSN, GroupID: 7, Payload: make([]byte, 16)}})
	if _, err := scanFS(fs); !isCode(err, errs.CodeUnsupportedFormat) {
		t.Fatalf("APPLIED in a standalone log: %v, want UNSUPPORTED_FORMAT", err)
	}
}

// The Writer refuses each group that recovery would refuse, so that it
// never acknowledges a commit recovery would drop.
func TestWriterRefusesWhatRecoveryRefuses(t *testing.T) {
	cases := map[string]func(g *Group){
		"wrong digest":           func(g *Group) { g.Commit.Digest[0] ^= 1 },
		"free count above pages": func(g *Group) { g.Commit.State.FreePageCount = 2000 },
		"root beyond pageCount":  func(g *Group) { g.Commit.State.Roots[0] = 5000 },
		"Raft fields":            func(g *Group) { g.Commit.State.AppliedRaftTerm = 1 },
		"BEGIN flags":            func(g *Group) { g.Begins[0].Flags = 1 },
		"page beyond pageCount":  func(g *Group) { g.Commit.State.PageCount = 4 },
		"invalid page image":     func(g *Group) { g.Pages[0][100] ^= 1 },
		"page 1 (superblock)": func(g *Group) {
			img := new(page.Buf)
			page.EncodeOverflow(img, 1, []byte{1})
			page.HeaderOf(img).SetCommitID(g.Begins[0].CommitID)
			page.Seal(img)
			g.Pages[0] = img
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			fs := vfs.NewMemFS()
			w := newWriter(t, fs, 1<<20)
			var c chain
			g := c.next(t, 4)
			spoil(g)
			if _, err := w.AppendGroup(g); !isCode(err, errs.CodeInvalidArgument) {
				t.Fatalf("%v, want INVALID_ARGUMENT", err)
			}
			w.Close()
		})
	}
}

// A group that would not fit a new segment under the scan bound is
// refused: recovery could not read it back.
func TestGroupAboveTheSegmentBound(t *testing.T) {
	fs := vfs.NewMemFS()
	w, err := Create(fs, dir, testUUID, Options{SegmentBytes: 16 << 10, MaxSegmentBytes: 32 << 10, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var c chain
	ids := make([]page.ID, 10) // 10 pages: about 41 KiB
	for i := range ids {
		ids[i] = page.ID(3 + i)
	}
	if _, err := w.AppendGroup(c.next(t, ids...)); !isCode(err, errs.CodeLimitExceeded) {
		t.Fatalf("group above the bound: %v, want LIMIT_EXCEEDED", err)
	}
	if _, err := Create(vfs.NewMemFS(), dir, testUUID, Options{SegmentBytes: 64 << 10, MaxSegmentBytes: 32 << 10}); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("segment above the bound: %v", err)
	}
}

// GroupIDs keep growing across the checkpoint: a writer resumed from a
// scan where every group is checkpointed does not restart them, so a scan
// from an older checkpoint still reads the log.
func TestGroupIDsAcrossTheCheckpoint(t *testing.T) {
	fs := vfs.NewMemFS()
	var c chain
	logOf(t, fs, &c, 3)
	all := mustScan(t, fs)
	last := all.Groups[2]
	res, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, CheckpointLSN: last.CommitLSN, BaseCommitID: 3})
	if err != nil || res.LastGroupID != 3 || len(res.Groups) != 0 {
		t.Fatalf("all checkpointed: last group %d, %d groups, %v", res.LastGroupID, len(res.Groups), err)
	}
	w, err := Open(fs, dir, testUUID, Resume{End: res.End, LastGroupID: res.LastGroupID, LastCommitID: 3, CheckpointLSN: last.CommitLSN},
		Options{SegmentBytes: 1 << 20, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	c.group = res.LastGroupID
	mustAppend(t, w, c.next(t, 9))
	mustSync(t, w)
	w.Close()
	if res := mustScan(t, fs); !slices.Equal(commits(res), []uint64{1, 2, 3, 4}) {
		t.Fatalf("from the older checkpoint: %v", commits(res))
	}
}

// Sync syncs metadata only when the segment grew past its size.
func TestSyncChoosesDataOrFull(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 16<<10)
	defer w.Close()
	var c chain
	mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	if w.fullSyncs != 0 || w.dataSyncs != 1 {
		t.Fatalf("inside the segment: %d full syncs, %d data syncs", w.fullSyncs, w.dataSyncs)
	}
	// A group larger than a segment grows the new segment it goes to.
	ids := make([]page.ID, 5) // about 20 KiB
	for i := range ids {
		ids[i] = page.ID(10 + i)
	}
	mustAppend(t, w, c.next(t, ids...))
	full := w.fullSyncs
	mustSync(t, w)
	if w.fullSyncs != full+1 {
		t.Fatalf("after the segment grew: %d full syncs, want %d", w.fullSyncs, full+1)
	}
	mustAppend(t, w, c.next(t, 4)) // the size is now known: data syncs again
	data := w.dataSyncs
	mustSync(t, w)
	if w.dataSyncs != data+1 {
		t.Fatal("a sync after the growth was synced did not use SyncData")
	}
}
