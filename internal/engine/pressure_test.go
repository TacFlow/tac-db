package engine

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// OPS-05, age: a reader past MaxReadTxAge gets SNAPSHOT_EXPIRED on its
// next operation, an idle one is revoked by the maintenance, an event
// reports both, and the segments it held go.
func TestReadTxAge(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	c := newClock().options(&o)
	o.MaxReadTxAge = time.Minute
	var mu sync.Mutex
	var events []Event
	o.OnEvent = func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	db := open(t, fs, o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 10)
	active, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := db.BeginRead(ctx)
	fill(t, db, model, 10, 200)
	c.add(30 * time.Second)
	if _, _, err := active.Get(ctx, 0, []byte("k000")); err != nil {
		t.Fatalf("a young reader: %v", err)
	}
	c.add(31 * time.Second)
	if _, _, err := active.Get(ctx, 0, []byte("k000")); !isCode(err, errs.CodeSnapshotExpired) {
		t.Fatalf("past MaxReadTxAge: %v", err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := idle.Range(ctx, 0, nil, nil, func(k, v []byte) error { return nil }); !isCode(err, errs.CodeSnapshotExpired) {
		t.Fatalf("an idle reader past MaxReadTxAge: %v", err)
	}
	if st := db.Stats(); st.SnapshotsExpired != 2 || st.SegmentsRemoved == 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := len(segs(t, fs)); n > 2 {
		t.Fatalf("%d segments after the revocation", n)
	}
	mu.Lock()
	if len(events) != 2 || events[0].Kind != EventSnapshotExpired || events[0].Count != 1 ||
		events[0].OldestAge < time.Minute || events[0].Origin != "read" {
		t.Fatalf("events %+v", events)
	}
	mu.Unlock()
	if active.Close() != nil || idle.Close() != nil || db.active.Load() != 0 {
		t.Fatal("closing revoked transactions")
	}

	// Unlimited never revokes.
	db.opts.MaxReadTxAge = Unlimited
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	c.add(1000 * time.Hour)
	db.Checkpoint(ctx)
	if _, _, err := r.Get(ctx, 0, []byte("k000")); err != nil {
		t.Fatalf("Unlimited: %v", err)
	}
}

// The revocation is cooperative: the operation in progress finishes with
// all its results, and the snapshot keeps its segments until it ends.
func TestCooperativeRevocation(t *testing.T) {
	o := quiet()
	c := newClock().options(&o)
	o.MaxReadTxAge = time.Minute
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 60)
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	got := 0
	err := r.Range(ctx, 0, nil, nil, func(k, v []byte) error {
		if got == 0 {
			c.add(2 * time.Minute)
			db.Checkpoint(ctx)
			if !r.snap.revoked || r.snap.gone {
				t.Fatal("revoked during an operation: it must stay registered until the operation ends")
			}
		}
		got++
		return nil
	})
	if err != nil || got != len(model) {
		t.Fatalf("the operation in progress: %v, %d of %d keys", err, got, len(model))
	}
	if !r.snap.gone {
		t.Fatal("the snapshot stays registered after its operation ended")
	}
	if _, _, err := r.Get(ctx, 0, []byte("k000")); !isCode(err, errs.CodeSnapshotExpired) {
		t.Fatalf("the next operation: %v", err)
	}
}

// OPS-05, pressure: ExpireOldest revokes the old readers that pin the log,
// from the oldest on, and only those old enough.
func TestPressureExpireOldest(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	c := newClock().options(&o)
	o.MaxReadTxAge = Unlimited
	o.WALPinnedSoftBytes = 2 * o.WALSegmentBytes
	o.MinSnapshotAgeForEviction = time.Minute
	var events []Event
	o.OnEvent = func(e Event) { events = append(events, e) }
	db := open(t, fs, o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 10)
	old, _ := db.BeginRead(ctx)
	defer old.Close()
	fill(t, db, model, 10, 300)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if db.Stats().SnapshotsEvicted != 0 || db.mt.pinned.Load() < o.WALPinnedSoftBytes {
		t.Fatalf("a young reader was revoked, or the pinned log %d is wrong", db.mt.pinned.Load())
	}
	young, _ := db.BeginRead(ctx)
	defer young.Close()
	c.add(2 * time.Minute)
	recent, _ := db.BeginRead(ctx) // younger than MinSnapshotAgeForEviction
	defer recent.Close()
	fill(t, db, model, 310, 100)
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.Get(ctx, 0, []byte("k000")); !isCode(err, errs.CodeSnapshotExpired) {
		t.Fatalf("the oldest reader: %v", err)
	}
	if len(events) == 0 || events[0].Kind != EventSnapshotEvicted || events[0].Count < 1 {
		t.Fatalf("events %+v", events)
	}
	// young is old enough, but recent pins the same segment and is not:
	// revoking young alone would free nothing.
	if _, _, err := young.Get(ctx, 0, []byte("k000")); err != nil {
		t.Fatalf("a reader that shares the floor of a young one: %v", err)
	}
	if _, _, err := recent.Get(ctx, 0, []byte("k000")); err != nil {
		t.Fatalf("a reader younger than MinSnapshotAgeForEviction: %v", err)
	}
	// The base moved up to the reader left, and the log below it went.
	if db.b.Base() != recent.State().CommitID || segs(t, fs)[0] != db.sb.WALFloorSegment {
		t.Fatalf("base %d, reader %d, segments %v, floor %d", db.b.Base(), recent.State().CommitID, segs(t, fs), db.sb.WALFloorSegment)
	}
}

// OPS-05, pressure: RejectWrites keeps the readers and refuses commits with
// WAL_LIMIT at MaxWALBytes, naming the snapshots that keep the log.
func TestPressureRejectWrites(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	newClock().options(&o)
	o.MaxReadTxAge = Unlimited
	o.SnapshotPressurePolicy = RejectWrites
	o.WALPinnedSoftBytes = o.WALSegmentBytes
	o.MinSnapshotAgeForEviction = 0 // ExpireOldest would revoke the reader
	o.MaxWALBytes = 6 * o.WALSegmentBytes
	var rejected atomic.Int64
	o.OnEvent = func(e Event) {
		if e.Kind == EventWritesRejected && e.Count == 1 && e.Origin == "read" {
			rejected.Add(1)
		}
	}
	db := open(t, fs, o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 10)
	r, _ := db.BeginRead(ctx)
	want := contents(t, r)
	var err error
	for i := 0; i < 1000 && err == nil; i++ {
		var tx *WriteTx
		tx, err = db.BeginWrite(ctx)
		if err != nil {
			break
		}
		tx.Put(ctx, 0, []byte(fmt.Sprintf("k%03d", i%60)), bytes.Repeat([]byte("x"), 3000))
		_, err = tx.Commit(ctx)
		if err != nil {
			tx.Rollback()
		}
	}
	if !isCode(err, errs.CodeWALLimit) || !strings.Contains(err.Error(), "1 snapshots keep the log") {
		t.Fatalf("at MaxWALBytes: %v", err)
	}
	if rejected.Load() == 0 || db.Stats().WALLimits == 0 || db.Stats().SnapshotsEvicted != 0 {
		t.Fatalf("events %d, stats %+v", rejected.Load(), db.Stats())
	}
	same(t, contents(t, r), want)
	r.Close()
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	put(t, db, "after", "close")
}

// OPS-04: readers that overlap without a quiescent instant, and a writer;
// the background checkpoint progresses, the log stays bounded and no
// commit gets WAL_LIMIT. Every snapshot stays consistent.
func TestCheckpointUnderLoad(t *testing.T) {
	fs := vfs.NewMemFS()
	o := testOptions()
	o.MaintenanceInterval = time.Hour // only the growth of the log wakes it
	o.CheckpointBytes = 512 << 10
	o.MaxWALBytes = 32 * o.WALSegmentBytes
	db := open(t, fs, o)
	defer db.Close()
	const keys = 20
	write := func(round int) {
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		for k := 0; k < keys; k++ {
			v := fmt.Sprintf("%06d", round) + strings.Repeat("v", 500+k*50)
			if err := tx.Put(ctx, 0, []byte(fmt.Sprintf("k%02d", k)), []byte(v)); err != nil {
				t.Error(err)
			}
		}
		if _, err := tx.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
	write(0)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var maxSegs atomic.Int64
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, _ := db.BeginRead(ctx)
			for {
				cur := next
				var err error
				next, err = db.BeginRead(ctx) // overlaps cur: never a quiescent instant
				if err != nil {
					t.Error(err)
					cur.Close()
					return
				}
				round := ""
				err = cur.Range(ctx, 0, nil, nil, func(k, v []byte) error {
					if round == "" {
						round = string(v[:6])
					} else if string(v[:6]) != round {
						return fmt.Errorf("snapshot mixes rounds %s and %s", round, v[:6])
					}
					return nil
				})
				cur.Close()
				if err != nil {
					t.Error(err)
				}
				if n := int64(len(segs(t, fs))); n > maxSegs.Load() {
					maxSegs.Store(n)
				}
				select {
				case <-stop:
					next.Close()
					return
				default:
				}
			}
		}()
	}
	for round := 1; round <= 300; round++ {
		write(round)
	}
	close(stop)
	wg.Wait()
	if st := db.Stats(); st.Checkpoints == 0 || st.WALLimits != 0 {
		t.Fatalf("stats %+v", st)
	}
	if maxSegs.Load() > 16 {
		t.Fatalf("the log reached %d segments", maxSegs.Load())
	}
}

// At MaxWALBytes, ExpireOldest revokes the old reader that pins the log
// before refusing a commit, and the commit goes through.
func TestWALLimitRelievedByEviction(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	newClock().options(&o)
	o.MaxReadTxAge = Unlimited
	o.WALPinnedSoftBytes = 2 * o.WALSegmentBytes
	o.MinSnapshotAgeForEviction = 0
	o.MaxWALBytes = 8 * o.WALSegmentBytes
	db := open(t, fs, o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 10)
	r, _ := db.BeginRead(ctx)
	defer r.Close()
	fill(t, db, model, 10, 400)
	if _, _, err := r.Get(ctx, 0, []byte("k000")); !isCode(err, errs.CodeSnapshotExpired) {
		t.Fatalf("the reader that pinned the log: %v", err)
	}
	if st := db.Stats(); st.WALLimits != 0 || st.SnapshotsEvicted != 1 {
		t.Fatalf("stats %+v", st)
	}
	same(t, read(t, db), model)
}
