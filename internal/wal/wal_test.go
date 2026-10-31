package wal

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

func TestRoundTrip(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	a1 := mustAppend(t, w, c.next(t, 3, 7))
	if _, err := w.AppendReserve(Reserve{Kind: KindNode, HighWatermark: 2048}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, w, c.next(t))
	g3 := c.next(t, 5)
	mustAppend(t, w, g3)
	mustSync(t, w)
	end := w.Position()
	w.Close()

	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1, 2, 3}) || res.TailDiscarded || res.End != end {
		t.Fatalf("commits %v, discarded %v, end %+v (writer %+v)", commits(res), res.TailDiscarded, res.End, end)
	}
	if len(res.Reserves) != 1 || res.Reserves[0].HighWatermark != 2048 || res.Reserves[0].LSN != 5 {
		t.Fatalf("reserves %+v", res.Reserves)
	}
	g := res.Groups[0]
	if len(g.Pages) != 2 || g.Pages[0].Loc != a1.Pages[0] || g.Pages[1].ID != 7 {
		t.Fatalf("pages %+v, appended %+v", g.Pages, a1.Pages)
	}
	// A locator points at the page image.
	f, _ := fs.Open(dir + "/" + SegmentName(1))
	var img page.Buf
	if err := f.ReadAt(img[:], g.Pages[1].Loc.Offset); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if page.HeaderOf(&img).ID() != 7 || page.Verify(&img, 7, 1000) != nil {
		t.Fatal("the locator does not point at page 7")
	}
	if res.Groups[2].Begins[0].DurableLSN != 0 {
		t.Fatalf("nothing was synced before group 3, durable_lsn %d", res.Groups[2].Begins[0].DurableLSN)
	}
}

// durable_lsn reports what a Sync covered before the group was formed.
func TestDurableLSN(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	a := mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	mustAppend(t, w, c.next(t, 4))
	mustAppend(t, w, c.next(t, 5)) // formed before any new Sync
	mustSync(t, w)
	w.Close()
	res := mustScan(t, fs)
	for i, want := range []uint64{0, a.CommitLSN, a.CommitLSN} {
		if got := res.Groups[i].Begins[0].DurableLSN; got != want {
			t.Fatalf("group %d: durable_lsn %d, want %d", i+1, got, want)
		}
	}
}

// Segments roll at their target size, through the spare: every segment
// but the last ends at the prev_valid_end of the next one.
func TestSegmentRoll(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 16<<10)
	var c chain
	for i := 0; i < 20; i++ {
		mustAppend(t, w, c.next(t, page.ID(3+i), page.ID(100+i)))
	}
	mustSync(t, w)
	w.Close()
	res := mustScan(t, fs)
	if len(res.Groups) != 20 || len(res.Segments) < 3 {
		t.Fatalf("%d groups in %d segments", len(res.Groups), len(res.Segments))
	}
	if !slices.Contains(res.Stale, SpareName) {
		t.Fatalf("stale %v: the spare left by Close is removable", res.Stale)
	}
	// Reopening continues in the last segment.
	w, err := Open(fs, dir, testUUID, resume(res), Options{SegmentBytes: 16 << 10, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if w.Position() != res.End {
		t.Fatalf("reopened at %+v, scan ended at %+v", w.Position(), res.End)
	}
	mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	w.Close()
	if res := mustScan(t, fs); len(res.Groups) != 21 {
		t.Fatalf("%d groups after reopening", len(res.Groups))
	}
}

// REC-02 and R13-23: an unsynced group torn by a power loss, with sectors
// kept in any order, is a discarded tail, never CORRUPT; synced groups
// always survive.
func TestTornTailIsDiscarded(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		fs := vfs.NewMemFS()
		w := newWriter(t, fs, 1<<20)
		var c chain
		mustAppend(t, w, c.next(t, 3, 4))
		mustSync(t, w)
		mustAppend(t, w, c.next(t, 5, 6, 7, 8))
		mustAppend(t, w, c.next(t, 9))
		fs.Crash(vfs.CrashOptions{Seed: seed, TornWrites: seed%2 == 0, SectorSubset: seed%2 == 1, Reorder: true})
		res, err := scanFS(fs)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		got := commits(res)
		if len(got) == 0 || got[0] != 1 || len(got) > 3 {
			t.Fatalf("seed %d: recovered %v", seed, got)
		}
	}
}

// R13-23: a synced group corrupted on the medium is CORRUPT when a later
// BEGIN shows it was durable.
func TestCorruptionAfterSync(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	a2 := mustAppend(t, w, c.next(t, 4))
	mustSync(t, w)
	mustAppend(t, w, c.next(t, 5))
	mustSync(t, w)
	w.Close()
	poke(t, fs, dir+"/"+SegmentName(1), a2.Pages[0].Offset+100, 0xff)
	if _, err := scanFS(fs); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("corrupted synced group: %v, want CORRUPT", err)
	}
}

// The residual risk of R13-23: the last synced group, with no later
// evidence, is discarded as a tail when it is corrupted.
func TestLastSyncedGroupWithoutEvidence(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	a2 := mustAppend(t, w, c.next(t, 4))
	mustSync(t, w)
	w.Close()
	poke(t, fs, dir+"/"+SegmentName(1), a2.Pages[0].Offset+100, 0xff)
	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
		t.Fatalf("commits %v, discarded %v", commits(res), res.TailDiscarded)
	}
}

// REC-03: a defect in a segment that is not the last is CORRUPT.
func TestCorruptionInAnEarlierSegment(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 16<<10)
	var c chain
	var first Appended
	for i := 0; i < 12; i++ {
		a := mustAppend(t, w, c.next(t, page.ID(3+i)))
		if i == 0 {
			first = a
		}
	}
	mustSync(t, w)
	w.Close()
	poke(t, fs, dir+"/"+SegmentName(1), first.Pages[0].Offset+10, 0x01)
	_, err := scanFS(fs)
	if !isCode(err, errs.CodeCorrupt) || !strings.Contains(err.Error(), "segment 1, offset") {
		t.Fatalf("defect in segment 1 of several: %v, want CORRUPT at segment 1", err)
	}
}

// A discarded tail never comes back: the next writer starts a new
// segment, and an old group after the discarded point is never read, even
// when the new group has the same size and LSNs.
func TestDiscardedTailDoesNotResurrect(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	mustAppend(t, w, c.next(t, 3))
	mustSync(t, w)
	stale := c
	a2 := mustAppend(t, w, c.next(t, 4))
	mustAppend(t, w, c.next(t, 5)) // formed before a Sync: no evidence
	mustSync(t, w)
	w.Close()
	poke(t, fs, dir+"/"+SegmentName(1), a2.Pages[0].Offset+100, 0xff)
	res := mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1}) || !res.TailDiscarded {
		t.Fatalf("commits %v, discarded %v", commits(res), res.TailDiscarded)
	}
	w, err := Open(fs, dir, testUUID, resume(res), Options{SegmentBytes: 1 << 20, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if w.Position().Seq != 2 {
		t.Fatalf("appends go to segment %d after a discarded tail", w.Position().Seq)
	}
	mustAppend(t, w, stale.next(t, 4)) // commit 2 again, same size
	mustSync(t, w)
	w.Close()
	res = mustScan(t, fs)
	if !slices.Equal(commits(res), []uint64{1, 2}) || res.TailDiscarded {
		t.Fatalf("after the new group: commits %v, discarded %v", commits(res), res.TailDiscarded)
	}
}

// REC-10: the set of segments.
func TestSegmentSetErrors(t *testing.T) {
	build := func(t *testing.T) *vfs.MemFS {
		fs := vfs.NewMemFS()
		w := newWriter(t, fs, 16<<10)
		var c chain
		for i := 0; i < 12; i++ {
			mustAppend(t, w, c.next(t, page.ID(3+i)))
		}
		mustSync(t, w)
		w.Close()
		return fs
	}
	cases := map[string]func(t *testing.T, fs *vfs.MemFS) ScanInput{
		"hole in the sequence": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			fs.Remove(dir + "/" + SegmentName(2))
			return ScanInput{FloorSeq: 1}
		},
		"floor missing": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			return ScanInput{FloorSeq: 9}
		},
		"another database": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			return ScanInput{FloorSeq: 1, UUID: [16]byte{9}}
		},
		"repeated seq": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			// Segment 3 holds a copy of segment 2.
			b, _ := fs.ReadDurable(dir + "/" + SegmentName(2))
			f, _ := fs.Open(dir + "/" + SegmentName(3))
			f.WriteAt(b, 0)
			f.Sync()
			f.Close()
			return ScanInput{FloorSeq: 1}
		},
		"unexpected file": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			f, _ := fs.Create(dir + "/notes.txt")
			f.Close()
			return ScanInput{FloorSeq: 1}
		},
		"floor starts after checkpoint_lsn": func(t *testing.T, fs *vfs.MemFS) ScanInput {
			for _, n := range []string{SegmentName(1), SegmentName(2)} {
				fs.Remove(dir + "/" + n)
			}
			return ScanInput{FloorSeq: 3, CheckpointLSN: 1}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fs := build(t)
			in := setup(t, fs)
			in.FS, in.Dir = fs, dir
			if in.UUID == ([16]byte{}) {
				in.UUID = testUUID
			}
			if _, err := Scan(ctx, in); !isCode(err, errs.CodeCorrupt) {
				t.Fatalf("%v, want CORRUPT", err)
			}
		})
	}
	// Segments below the floor are left over and removable, not an error.
	fs := build(t)
	first, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	g := first.Groups[0] // the first group of segment 2 starts after its checkpoint
	for _, gr := range first.Groups {
		if gr.Pages[0].Loc.Seq == 2 {
			g = gr
			break
		}
	}
	res, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 2,
		CheckpointLSN: g.CommitLSN - 3, BaseCommitID: g.Commit.State.Parent})
	if err != nil || !slices.Contains(res.Stale, SegmentName(1)) {
		t.Fatalf("stale %v, %v", res.Stale, err)
	}
}

// Records up to checkpoint_lsn are skipped; recovery resumes after it.
func TestScanFromCheckpoint(t *testing.T) {
	fs := vfs.NewMemFS()
	w := newWriter(t, fs, 1<<20)
	var c chain
	a1 := mustAppend(t, w, c.next(t, 3))
	mustAppend(t, w, c.next(t, 4))
	mustSync(t, w)
	w.Close()
	res, err := Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, CheckpointLSN: a1.CommitLSN, BaseCommitID: 1})
	if err != nil || !slices.Equal(commits(res), []uint64{2}) {
		t.Fatalf("commits %v, %v", commits(res), err)
	}
	// A group whose parent is not the recovered state is never recovered on
	// its own: without evidence of a Sync it is a discarded tail, as after
	// a reordered crash in Async.
	res, err = Scan(ctx, ScanInput{FS: fs, Dir: dir, UUID: testUUID, FloorSeq: 1, CheckpointLSN: a1.CommitLSN, BaseCommitID: 7})
	if err != nil || len(res.Groups) != 0 || !res.TailDiscarded {
		t.Fatalf("a group without its parent: commits %v, discarded %v, %v", commits(res), res.TailDiscarded, err)
	}
}

// Every crash point of a segment roll recovers the synced groups.
func TestCrashDuringRoll(t *testing.T) {
	for _, cp := range []vfs.CrashPoint{
		{Op: vfs.OpSync, Name: dir + "/" + SpareName, Nth: 2},
		{Op: vfs.OpRename, Nth: 1},
		{Op: vfs.OpRename, Nth: 1, After: true},
		{Op: vfs.OpSyncDir, Name: dir, Nth: 1},
		{Op: vfs.OpSyncDir, Name: dir, Nth: 1, After: true},
	} {
		t.Run(fmt.Sprintf("%s after=%v", cp.Op, cp.After), func(t *testing.T) {
			fs := vfs.NewMemFS()
			w := newWriter(t, fs, 16<<10)
			var c chain
			mustAppend(t, w, c.next(t, 3))
			mustSync(t, w)
			fs.InjectCrash(cp)
			synced := 1
			for i := 0; i < 12; i++ {
				if _, err := w.AppendGroup(c.next(t, page.ID(4+i))); err != nil {
					break
				}
				if w.Sync() != nil {
					break
				}
				synced++
			}
			if fs.Crashes() != 1 {
				t.Fatal("the crash point did not fire")
			}
			fs.Restart()
			res, err := scanFS(fs)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(res.Groups); got < synced || got > synced+1 {
				t.Fatalf("%d groups recovered, %d were synced", got, synced)
			}
		})
	}
}
