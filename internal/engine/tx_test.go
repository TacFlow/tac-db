package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// TX-01 and TX-10: FIFO admission, fail-fast Try, a full queue, timeout,
// cancel and deadline, each with its own error and no ticket left behind.
func TestAdmission(t *testing.T) {
	o := testOptions()
	o.MaxWriterQueue = 2
	o.BusyTimeout = 50 * time.Millisecond
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.TryBeginWrite(ctx); !isCode(err, errs.CodeDBBusy) {
		t.Fatalf("Try with the slot taken: %v", err)
	}
	start := time.Now()
	if _, err := db.BeginWrite(ctx); !isCode(err, errs.CodeBusyTimeout) {
		t.Fatalf("BusyTimeout: %v", err)
	} else if time.Since(start) < 50*time.Millisecond {
		t.Fatal("BUSY_TIMEOUT before BusyTimeout")
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := db.BeginWrite(dctx); !isCode(err, errs.CodeDeadline) {
		t.Fatalf("an earlier deadline: %v", err)
	}
	cctx, cancel2 := context.WithCancel(ctx)
	go func() { time.Sleep(5 * time.Millisecond); cancel2() }()
	if _, err := db.BeginWrite(cctx); !isCode(err, errs.CodeCanceled) {
		t.Fatalf("cancel: %v", err)
	}
	if db.waiters.len() != 0 {
		t.Fatal("a ticket was left in the queue")
	}

	// FIFO: two candidates are admitted in order as the slot is freed.
	db.opts.BusyTimeout = 5 * time.Second
	order := make(chan int, 2)
	got := make(chan *WriteTx, 2)
	for i := 1; i <= 2; i++ {
		go func() {
			w, err := db.BeginWrite(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			order <- i
			got <- w
		}()
		waitFor(t, "the candidate to queue", func() bool {
			db.mu.Lock()
			defer db.mu.Unlock()
			return db.waiters.len() == i
		})
	}
	if _, err := db.BeginWrite(ctx); !isCode(err, errs.CodeWriterQueueFull) {
		t.Fatalf("a full queue: %v", err)
	}
	tx.Rollback()
	(<-got).Rollback()
	(<-got).Rollback()
	if a, b := <-order, <-order; a != 1 || b != 2 {
		t.Fatalf("admitted %d then %d", a, b)
	}
	if n := db.active.Load(); n != 0 {
		t.Fatalf("%d transactions counted after all ended", n)
	}
}

// TX-09: MaxActiveTransactions counts readers and the writer.
func TestMaxActiveTransactions(t *testing.T) {
	o := testOptions()
	o.MaxActiveTransactions = 3
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	var rs []*ReadTx
	for i := 0; i < 3; i++ {
		r, err := db.BeginRead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	if _, err := db.BeginRead(ctx); !isCode(err, errs.CodeTooManyTransactions) {
		t.Fatalf("a fourth reader: %v", err)
	}
	if _, err := db.BeginWrite(ctx); !isCode(err, errs.CodeTooManyTransactions) {
		t.Fatalf("a writer over the limit: %v", err)
	}
	rs[0].Close()
	rs[0].Close() // idempotent: counted once
	tx, err := db.BeginWrite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := db.active.Load(); n != 2 {
		t.Fatalf("%d open, want 2", n)
	}
}

// TX-02: an old snapshot stays constant; a new one sees whole commits.
func TestSnapshotIsolation(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	put(t, db, "a", "1")
	old, _ := db.BeginRead(ctx)
	defer old.Close()
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("a"), []byte("2"))
	tx.Put(ctx, 0, []byte("b"), []byte("2"))
	if got := read(t, db); got["a"] != "1" || len(got) != 1 {
		t.Fatalf("an uncommitted write is visible: %v", got)
	}
	if v, _, _ := tx.Get(ctx, 0, []byte("a")); string(v) != "2" {
		t.Fatal("a transaction does not read its own write")
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := contents(t, old); got["a"] != "1" || len(got) != 1 {
		t.Fatalf("the old snapshot changed: %v", got)
	}
	if got := read(t, db); got["a"] != "2" || got["b"] != "2" {
		t.Fatalf("the new snapshot: %v", got)
	}
}

// TX-04 and TX-20: rollback leaves nothing; a failed statement restores
// only itself; savepoints are O(1) over many statements.
func TestRollbackAndStatements(t *testing.T) {
	o := testOptions()
	o.MaxTxBytes = 64 << 10
	db := open(t, vfs.NewMemFS(), o)
	defer db.Close()
	put(t, db, "keep", "1")
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("gone"), []byte("x"))
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("Rollback is idempotent")
	}
	if err := tx.Put(ctx, 0, []byte("k"), []byte("v")); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("a write after Rollback: %v", err)
	}

	tx, _ = db.BeginWrite(ctx)
	if err := tx.Put(ctx, 0, []byte("first"), []byte("ok")); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 200<<10) // over MaxTxBytes: the statement fails alone
	if err := tx.Put(ctx, 0, []byte("second"), big); !isCode(err, errs.CodeLimitExceeded) {
		t.Fatalf("a statement over MaxTxBytes: %v", err)
	}
	for i := 0; i < 10000; i++ { // TX-20
		if err := tx.Put(ctx, 0, []byte(fmt.Sprintf("s%05d", i%50)), []byte("y")); err != nil {
			t.Fatal(err)
		}
		if i%7 == 0 {
			if err := tx.Put(ctx, 0, []byte("bad"), big); err == nil {
				t.Fatal("an oversized statement passed")
			}
		}
	}
	sp, _ := tx.Savepoint()
	tx.Put(ctx, 0, []byte("later"), []byte("z"))
	tx.Put(ctx, 1, []byte("slot1"), []byte("z")) // a tree first opened after the savepoint
	if err := tx.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := tx.Get(ctx, 1, []byte("slot1")); ok {
		t.Fatal("RollbackTo kept a write of a tree opened after the savepoint")
	}
	tx.Release(sp)
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := read(t, db)
	if got["keep"] != "1" || got["first"] != "ok" || got["second"] != "" || got["later"] != "" || got["gone"] != "" || len(got) != 52 {
		t.Fatalf("after statements: %d keys %v", len(got), got["first"])
	}
	if r, _ := db.BeginRead(ctx); r.State().Roots[1] != 0 {
		t.Fatal("the rolled-back tree left a root")
	} else {
		r.Close()
	}
	structural(t, db)
}

// TX-19: a writer that holds the slot past MaxWriteTxDuration is rolled
// back, and the queue moves.
func TestWriteTxTimeout(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	put(t, db, "sealed", "1")
	setLimit := func(d time.Duration) { // read at each admission, under db.mu
		db.mu.Lock()
		db.opts.MaxWriteTxDuration = d
		db.mu.Unlock()
	}
	setLimit(30 * time.Millisecond)
	slow, _ := db.BeginWrite(ctx)
	setLimit(time.Minute)
	slow.Put(ctx, 0, []byte("slow"), []byte("x"))
	next, err := db.BeginWrite(ctx) // admitted once the slow one expires
	if err != nil {
		t.Fatal(err)
	}
	if err := slow.Put(ctx, 0, []byte("more"), []byte("y")); !isCode(err, errs.CodeWriteTxTimeout) {
		t.Fatalf("after the timeout: %v", err)
	}
	if _, err := slow.Commit(ctx); !isCode(err, errs.CodeWriteTxTimeout) {
		t.Fatalf("Commit after the timeout: %v", err)
	}
	next.Put(ctx, 0, []byte("next"), []byte("1"))
	if _, err := next.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := read(t, db); got["slow"] != "" || got["sealed"] != "1" || got["next"] != "1" {
		t.Fatalf("after the timeout: %v", got)
	}
}

// TX-07: canceling before the seal leaves the transaction active;
// canceling the wait after it gives COMMIT_PENDING and the token, and the
// coordinator still makes the commit durable.
func TestCancelBeforeAndAfterSeal(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	db := open(t, g, testOptions())
	defer db.Close()
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("k"), []byte("v"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := tx.Commit(cctx); !isCode(err, errs.CodeCanceled) {
		t.Fatalf("Commit with a canceled context: %v", err)
	}
	if err := tx.Put(ctx, 0, []byte("k2"), []byte("v2")); err != nil {
		t.Fatalf("the transaction must stay active: %v", err)
	}
	g.hold()
	cctx, cancel = context.WithCancel(ctx)
	go func() { <-g.entered; cancel() }()
	r, err := tx.Commit(cctx)
	if !isCode(err, errs.CodeCommitPending) || r.Token == ([16]byte{}) {
		t.Fatalf("cancel after the seal: %v, %+v", err, r)
	}
	if st := db.CommitStatus(r.Token); st != Pending {
		t.Fatalf("status while the Sync waits: %v", st)
	}
	if err := tx.Rollback(); !isCode(err, errs.CodeCommitPending) {
		t.Fatalf("Rollback of a pending commit: %v", err)
	}
	g.release()
	if err := db.WaitDurable(ctx, r.CommitID); err != nil {
		t.Fatal(err)
	}
	if db.CommitStatus(r.Token) != Durable || read(t, db)["k2"] != "v2" {
		t.Fatal("the pending commit was not completed")
	}
	if err := tx.Rollback(); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("Rollback after the commit: %v", err)
	}
}

// TX-08: reads and their ends never touch the log.
func TestReadsDoNoIO(t *testing.T) {
	g := &gateFS{FS: vfs.NewMemFS()}
	db := open(t, g, testOptions())
	defer db.Close()
	put(t, db, "a", "1")
	before := db.Stats()
	ops := settled(t, g)
	for i := 0; i < 200; i++ {
		r, err := db.BeginRead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch i % 4 {
		case 0:
			r.Get(ctx, 0, []byte("a"))
		case 1:
			r.Range(ctx, 0, nil, nil, func(k, v []byte) error { return errors.New("stop") })
		case 2:
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			r.Range(cctx, 0, nil, nil, func(k, v []byte) error { return nil })
		}
		r.Close()
	}
	tx, _ := db.BeginWrite(ctx) // a write without changes appends nothing
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if after := db.Stats(); after != before {
		t.Fatalf("reads changed the log: %+v then %+v", before, after)
	}
	if n := g.ops.Load() - ops; n != 0 {
		t.Fatalf("reads made %d calls that change files", n)
	}
}

// The registration protocol: a reader that registered before a scan, or
// after it, never reads a commit older than the target the scan found.
func TestSnapshotRegistrationRace(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	put(t, db, "a", "0")
	var target uint64
	db.hooks.afterRegister = func() {
		db.hooks.afterRegister = nil
		for i := 1; i <= 3; i++ { // commits land between registering and loading
			put(t, db, "a", fmt.Sprint(i))
		}
		target = db.checkpointTarget()
	}
	r, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.State().CommitID < target {
		t.Fatalf("the reader loaded commit %d below the checkpoint target %d", r.State().CommitID, target)
	}
	if target != db.b.Base() {
		t.Fatalf("the provisional registration must hold the target at the base: %d", target)
	}
	if got := db.checkpointTarget(); got != r.State().CommitID {
		t.Fatalf("with the reader registered, the target is %d, want %d", got, r.State().CommitID)
	}
}

// visibleHead is stored before durableHead: a reader between the two sees
// the new commit as visible, and the durable head is never ahead of it.
func TestHeadOrder(t *testing.T) {
	db := open(t, vfs.NewMemFS(), testOptions())
	defer db.Close()
	put(t, db, "a", "1")
	var vis, dur uint64
	db.hooks.betweenHeads = func() {
		db.hooks.betweenHeads = nil
		r, _ := db.BeginRead(ctx)
		d, _ := db.BeginReadDurable(ctx)
		vis, dur = r.State().CommitID, d.State().CommitID
		r.Close()
		d.Close()
	}
	r := put(t, db, "a", "2")
	if vis != r.CommitID || dur != r.CommitID-1 {
		t.Fatalf("between the heads: visible %d, durable %d, commit %d", vis, dur, r.CommitID)
	}
}

// settled waits until the background work of the log (the spare segment)
// stops changing files and returns the count of calls.
func settled(t *testing.T, g *gateFS) int64 {
	t.Helper()
	last, still := g.ops.Load(), 0
	for i := 0; i < 5000 && still < 50; i++ {
		time.Sleep(time.Millisecond)
		if n := g.ops.Load(); n == last {
			still++
		} else {
			last, still = n, 0
		}
	}
	return last
}
