package wal

import (
	"slices"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// logOf writes n synced groups of one page each and returns the end.
func logOf(t *testing.T, fs vfs.FS, c *chain, n int) Position {
	t.Helper()
	w := newWriter(t, fs, 1<<20)
	for i := 0; i < n; i++ {
		mustAppend(t, w, c.next(t, page.ID(3+i)))
		mustSync(t, w)
	}
	end := w.Position()
	w.Close()
	return end
}

// The checkpoint proves its prefix durable: the log must reach it, and a
// defect before it is CORRUPT, never a discarded tail.
func TestCheckpointLSNIsReached(t *testing.T) {
	fs := vfs.NewMemFS()
	var c chain
	logOf(t, fs, &c, 2)
	if _, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, CheckpointLSN: 100}); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("log shorter than checkpoint_lsn: %v, want CORRUPT", err)
	}
	res := mustScan(t, fs)
	g2 := res.Groups[1]
	poke(t, fs, dir+"/"+SegmentName(1), g2.Pages[0].Loc.Offset-RecordHeaderSize+20, 0xff) // the LSN of a PAGE
	_, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, CheckpointLSN: g2.CommitLSN, BaseCommitID: 2})
	if !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("defect before checkpoint_lsn: %v, want CORRUPT", err)
	}
}

// Forged sizes never panic, and a segment above the bound is CORRUPT.
func TestForgedSegmentSizes(t *testing.T) {
	for _, extra := range []int64{1, 2, 3, 7} {
		fs := vfs.NewMemFS()
		var c chain
		end := logOf(t, fs, &c, 1)
		f, _ := fs.Open(dir + "/" + SegmentName(1))
		f.Truncate(end.Offset + extra)
		f.WriteAt([]byte{0xff}, end.Offset)
		f.Sync()
		f.Close()
		if _, err := scanFS(fs); err != nil && !isCode(err, errs.CodeCorrupt) {
			t.Fatalf("extra %d: %v", extra, err)
		}
	}
	fs := vfs.NewMemFS()
	var c chain
	logOf(t, fs, &c, 1)
	_, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, MaxSegmentBytes: 8192})
	if !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("segment above MaxSegmentBytes: %v", err)
	}
}

// A roll syncs the groups appended since the last Sync: a power loss
// right after it keeps them.
func TestRollSyncsPendingGroups(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 16<<10)
	var c chain
	fs.InjectCrash(vfs.CrashPoint{Op: vfs.OpRename, Nth: 1, After: true})
	n := 0
	for fs.Crashes() == 0 {
		if _, err := w.AppendGroup(c.next(t, page.ID(3+n))); err != nil {
			break
		}
		n++
	}
	fs.Restart()
	res := mustScan(t, fs)
	if len(res.Groups) != n-1 && len(res.Groups) != n {
		t.Fatalf("%d groups recovered of %d appended, none synced except by the roll", len(res.Groups), n)
	}
}

// A zero magic followed by data is a defect: CORRUPT with evidence that
// the region was durable, a discarded tail without.
func TestZeroMagicFollowedByData(t *testing.T) {
	for _, evidence := range []bool{true, false} {
		fs := vfs.NewMemFS()
		w := newWriter(t, fs, 1<<20)
		var c chain
		mustAppend(t, w, c.next(t, 3))
		mustSync(t, w)
		a2 := mustAppend(t, w, c.next(t, 4))
		if evidence {
			mustSync(t, w)
		}
		mustAppend(t, w, c.next(t, 5))
		mustSync(t, w)
		w.Close()
		// Zero the magic of the first record of group 2 (its BEGIN).
		begin := a2.Pages[0].Offset - RecordHeaderSize - int64(encodedLen(BeginSize))
		f, _ := fs.Open(dir + "/" + SegmentName(1))
		f.WriteAt(make([]byte, 4), begin)
		f.Sync()
		f.Close()
		res, err := scanFS(fs)
		if evidence {
			if !isCode(err, errs.CodeCorrupt) {
				t.Fatalf("with evidence: %v, want CORRUPT", err)
			}
			continue
		}
		if err != nil || !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
			t.Fatalf("without evidence: %v, %v", commits(res), err)
		}
	}
}

// A group cut short at the end of the data is a discarded tail, and the
// end of the log is the end of the last complete group.
func TestIncompleteGroupAtTheEnd(t *testing.T) {
	fs := vfs.NewMemFS()
	var c chain
	end := logOf(t, fs, &c, 1)
	recs := rawRecords(c.next(t, 4, 5), end.NextLSN, 0)
	writeRaw(t, fs, end, recs[:2]) // BEGIN and one PAGE, no COMMIT
	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded || res.End != end {
		t.Fatalf("commits %v, discarded %v, end %+v (want %+v)", commits(res), res.TailDiscarded, res.End, end)
	}
}

// Every segment but the last must end exactly at the prev_valid_end of
// the next, with first_lsn continuing its LSNs.
func TestForgedSegmentHeaders(t *testing.T) {
	for name, change := range map[string]func(h *SegmentHeader){
		"prev_valid_end": func(h *SegmentHeader) { h.PrevValidEnd -= 8 },
		"first_lsn":      func(h *SegmentHeader) { h.FirstLSN++ },
	} {
		fs := vfs.NewMemFS()
		w := newWriter(t, fs, 16<<10)
		var c chain
		for i := 0; i < 12; i++ {
			mustAppend(t, w, c.next(t, page.ID(3+i)))
		}
		mustSync(t, w)
		w.Close()
		f, _ := fs.Open(dir + "/" + SegmentName(2))
		var b [SegmentHeaderSize]byte
		f.ReadAt(b[:], 0)
		h, _ := DecodeSegmentHeader(b[:])
		change(&h)
		h.Encode(b[:])
		f.WriteAt(b[:], 0)
		f.Sync()
		f.Close()
		if _, err := scanFS(fs); !isCode(err, errs.CodeCorrupt) {
			t.Fatalf("%s: %v, want CORRUPT", name, err)
		}
	}
}

// Each check of a group: a forged group at the end of the log is never
// recovered (it has no later evidence, so it is a discarded tail).
func TestForgedGroups(t *testing.T) {
	cases := map[string]func(t *testing.T, g *Group, recs []Record) []Record{
		"wrong digest": func(t *testing.T, g *Group, recs []Record) []Record {
			cm := g.Commit
			cm.Digest[0] ^= 1
			recs[len(recs)-1].Payload = cm.Encode()
			return recs
		},
		"wrong page count": func(t *testing.T, g *Group, recs []Record) []Record {
			cm := g.Commit
			cm.PageCount++
			recs[len(recs)-1].Payload = cm.Encode()
			return recs
		},
		"pages out of order": func(t *testing.T, g *Group, recs []Record) []Record {
			recs[1].Payload, recs[2].Payload = recs[2].Payload, recs[1].Payload
			recs[1].PageID, recs[2].PageID = recs[2].PageID, recs[1].PageID
			return recs
		},
		"page of another commit": func(t *testing.T, g *Group, recs []Record) []Record {
			img := new(page.Buf)
			copy(img[:], recs[1].Payload)
			page.HeaderOf(img).SetCommitID(99)
			page.Seal(img)
			recs[1].Payload, recs[1].CommitID = img[:], 99
			return recs
		},
		"short page payload": func(t *testing.T, g *Group, recs []Record) []Record {
			recs[1].Payload = recs[1].Payload[:100]
			return recs
		},
		"invalid page image": func(t *testing.T, g *Group, recs []Record) []Record {
			img := append([]byte(nil), recs[1].Payload...)
			img[100] ^= 1 // the page CRC no longer matches
			recs[1].Payload = img
			return recs
		},
		"page beyond pageCount": func(t *testing.T, g *Group, recs []Record) []Record {
			img := new(page.Buf)
			page.EncodeOverflow(img, 1500, []byte{1})
			page.HeaderOf(img).SetCommitID(2)
			page.Seal(img)
			recs[2].Payload, recs[2].PageID = img[:], 1500
			return recs
		},
		"group ID going back": func(t *testing.T, g *Group, recs []Record) []Record {
			for i := range recs {
				recs[i].GroupID = 1
			}
			return recs
		},
	}
	for name, forge := range cases {
		t.Run(name, func(t *testing.T) {
			fs := vfs.NewMemFS()
			var c chain
			end := logOf(t, fs, &c, 1)
			g := c.next(t, 4, 5)
			recs := forge(t, g, rawRecords(g, end.NextLSN, 0))
			writeRaw(t, fs, end, recs)
			res, err := scanFS(fs)
			if err != nil || !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
				t.Fatalf("commits %v, discarded %v, %v", commits(res), res.TailDiscarded, err)
			}
		})
	}
}

// ABORT ends the group in progress without recognizing it; the next group
// builds on the last recognized commit. A RESERVE_IDS with a valid CRC in
// a discarded tail is kept.
func TestAbortAndTailReservations(t *testing.T) {
	fs := vfs.NewMemFS()
	var c chain
	end := logOf(t, fs, &c, 1)
	aborted := c
	g := aborted.next(t, 4)
	recs := rawRecords(g, end.NextLSN, 0)[:1] // its BEGIN
	recs = append(recs, Record{Type: TypeAbort, LSN: end.NextLSN + 1, GroupID: g.ID, PrevLSN: end.NextLSN})
	next := c
	next.group = g.ID
	g2 := next.next(t, 5) // commit 2 again, over commit 1
	recs = append(recs, rawRecords(g2, end.NextLSN+2, 0)...)
	bad := Record{Type: TypeBegin, LSN: end.NextLSN + 5, GroupID: 9, CommitID: 7, Payload: make([]byte, 3)}
	recs = append(recs, bad, Record{Type: TypeReserveIDs, LSN: end.NextLSN + 6, Payload: (&Reserve{Kind: KindNode, HighWatermark: 9000}).Encode()})
	var b []byte
	for i := range recs {
		b = AppendRecord(b, &recs[i])
	}
	b[len(b)-encodedLen(ReserveSize)-encodedLen(3)] ^= 0xff // break the record before the reservation
	f, _ := fs.Open(dir + "/" + SegmentName(1))
	f.WriteAt(b, end.Offset)
	f.Sync()
	f.Close()
	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1, 2}) || !res.TailDiscarded {
		t.Fatalf("commits %v, discarded %v", commits(res), res.TailDiscarded)
	}
	if n := len(res.Reserves); n != 1 || !res.Reserves[0].Discarded || res.Reserves[0].HighWatermark != 9000 {
		t.Fatalf("reserves %+v", res.Reserves)
	}
}

// Opening after a process death syncs what recovery read from the cache:
// a power loss afterwards keeps it. The first new BEGIN declares it
// durable.
func TestOpenSyncsTheRecoveredPrefix(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	mustAppend(t, w, c.next(t, 3))
	a2 := mustAppend(t, w, c.next(t, 4)) // never synced; the process dies
	res := mustScan(t, fs)
	w2, err := Open(fs, dir, testUUID, resume(res), Options{SegmentBytes: 1 << 20, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	fs.Crash(vfs.CrashOptions{})
	res = mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1, 2}) {
		t.Fatalf("after Open and a power loss: %v", commits(res))
	}
	w2, err = Open(fs, dir, testUUID, resume(res), Options{SegmentBytes: 1 << 20, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, w2, c.next(t, 5))
	mustSync(t, w2)
	w2.Close()
	if got := mustScan(t, fs).Groups[2].Begins[0].DurableLSN; got != a2.CommitLSN {
		t.Fatalf("durable_lsn %d after Open, want %d", got, a2.CommitLSN)
	}
}

// The Writer refuses groups that recovery would not recognize, and after
// a failed write every call fails.
func TestWriterChecks(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	mustAppend(t, w, c.next(t, 3))
	for name, g := range map[string]*Group{
		"stale group ID":  func() *Group { x := c; g := x.next(t, 4); g.ID = 1; return g }(),
		"wrong parent":    func() *Group { x := c; x.commit = 7; return x.next(t, 4) }(),
		"COMMIT mismatch": func() *Group { x := c; g := x.next(t, 4); g.Commit.IntentCount = 2; return g }(),
	} {
		if _, err := w.AppendGroup(g); !isCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	fs.Inject(vfs.Fault{Op: vfs.OpWrite, Name: dir + "/*", Nth: 0})
	if _, err := w.AppendGroup(c.next(t, 4)); err == nil {
		t.Fatal("an injected write fault was ignored")
	}
	if err := w.Sync(); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Sync after a failed write: %v", err)
	}
	w.Close()
	if err := w.Close(); err != nil {
		t.Fatal("a second Close failed")
	}
}
