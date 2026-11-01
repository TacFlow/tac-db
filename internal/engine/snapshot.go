package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/versions"
	"github.com/TacFlow/tacdb/internal/wal"
)

// snapshot is a registered reader or writer (design §4.3). It registers
// first, with a provisional floor, the base T in force; then it loads its
// head and records the CommitID. A checkpointer loads durableHead first
// and then scans the registry: a registration the scan missed happened
// after it, and the head that reader loads is at or beyond the durable
// head the checkpointer read.
type snapshot struct {
	base   uint64        // the base T when it registered: the provisional floor
	commit atomic.Uint64 // CommitID+1 of its head; zero while provisional
	writer bool          // a writer does not count for MaxReadTxAge
	began  time.Time
	shard  int
}

// floor is the oldest commit the snapshot may read through.
func (s *snapshot) floor() uint64 {
	if c := s.commit.Load(); c != 0 {
		return c - 1
	}
	return s.base
}

const regShards = 16

// registry holds the snapshots in shards, so that Begin and the end of a
// read take no global lock.
type registry struct {
	next   atomic.Uint32
	shards [regShards]struct {
		mu sync.Mutex
		m  map[*snapshot]struct{}
		_  [40]byte // keep shards on separate cache lines
	}
}

func (r *registry) register(s *snapshot) {
	s.shard = int(r.next.Add(1) % regShards)
	sh := &r.shards[s.shard]
	sh.mu.Lock()
	if sh.m == nil {
		sh.m = map[*snapshot]struct{}{}
	}
	sh.m[s] = struct{}{}
	sh.mu.Unlock()
}

func (r *registry) remove(s *snapshot) {
	sh := &r.shards[s.shard]
	sh.mu.Lock()
	delete(sh.m, s)
	sh.mu.Unlock()
}

// oldest returns the lowest floor among the registered snapshots.
func (r *registry) oldest() (uint64, bool) {
	var m uint64
	found := false
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		for s := range sh.m {
			if f := s.floor(); !found || f < m {
				m, found = f, true
			}
		}
		sh.mu.Unlock()
	}
	return m, found
}

// checkpointTarget is the commit a checkpoint may write (SPEC §11.1):
// the durable head, or the oldest snapshot if older. The durable head is
// loaded before the registry is scanned.
func (db *DB) checkpointTarget() uint64 {
	t := db.durable.Load().State.CommitID
	if o, ok := db.reg.oldest(); ok {
		t = min(t, o)
	}
	return t
}

// enter counts a new transaction against MaxActiveTransactions.
func (db *DB) enter(op string) error {
	for {
		n := db.active.Load()
		if n >= int64(db.opts.MaxActiveTransactions) {
			return errs.Newf(errs.CodeTooManyTransactions, op, "%d transactions open", n).
				With("limit", int64(db.opts.MaxActiveTransactions))
		}
		if db.active.CompareAndSwap(n, n+1) {
			return nil
		}
	}
}

// ReadTx is a read-only transaction over one snapshot. It never appends
// to the log, reserves IDs or syncs (SPEC §10.1). A ReadTx is used by one
// goroutine at a time.
type ReadTx struct {
	db      *DB
	snap    *snapshot
	dir     *versions.Directory
	view    *versions.View
	pins    *cache.Pins
	durable bool
	closed  bool
}

// BeginRead opens a snapshot of the visible head.
func (db *DB) BeginRead(ctx context.Context) (*ReadTx, error) {
	return db.beginRead(ctx, &db.visible, "engine.BeginRead")
}

// BeginReadDurable opens a snapshot of the durable head.
func (db *DB) BeginReadDurable(ctx context.Context) (*ReadTx, error) {
	return db.beginRead(ctx, &db.durable, "engine.BeginReadDurable")
}

func (db *DB) beginRead(ctx context.Context, head *atomic.Pointer[versions.Directory], op string) (*ReadTx, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctxErr(err, op)
	}
	if err := db.usable(op); err != nil {
		return nil, err
	}
	if err := db.enter(op); err != nil {
		return nil, err
	}
	s := &snapshot{base: db.b.Base(), began: db.opts.Now()}
	db.reg.register(s)
	if h := db.hooks.afterRegister; h != nil {
		h()
	}
	d := head.Load()
	s.commit.Store(d.State.CommitID + 1)
	return &ReadTx{db: db, snap: s, dir: d, view: db.b.Snapshot(d), pins: cache.NewPins(0),
		durable: d.State.CommitID <= db.durable.Load().State.CommitID}, nil
}

func (t *ReadTx) check(op string) error {
	if t.closed {
		return errs.New(errs.CodeTxClosed, op, "transaction closed")
	}
	return nil
}

// State returns the state of the snapshot.
func (t *ReadTx) State() wal.State { return t.dir.State }

// Durable reports whether the snapshot was durable when it began.
func (t *ReadTx) Durable() bool { return t.durable }

// Get reads key in the tree of root slot.
func (t *ReadTx) Get(ctx context.Context, slot int, key []byte) ([]byte, bool, error) {
	if err := t.check("engine.Get"); err != nil {
		return nil, false, err
	}
	if err := checkSlot(slot); err != nil {
		return nil, false, err
	}
	return btree.Get(ctx, t.view, t.pins, t.dir.State.Roots[slot], key)
}

// Range calls fn for the keys in [lo, hi) of the tree of root slot; nil
// bounds are open.
func (t *ReadTx) Range(ctx context.Context, slot int, lo, hi []byte, fn func(k, v []byte) error) error {
	if err := t.check("engine.Range"); err != nil {
		return err
	}
	if err := checkSlot(slot); err != nil {
		return err
	}
	return btree.Range(ctx, t.view, t.pins, t.dir.State.Roots[slot], lo, hi, fn)
}

// Close ends the transaction; it is idempotent and does no I/O.
func (t *ReadTx) Close() error {
	if !t.closed {
		t.closed = true
		t.db.reg.remove(t.snap)
		t.db.active.Add(-1)
	}
	return nil
}

func checkSlot(slot int) error {
	if slot < 0 || slot >= page.UsedRoots {
		return errs.Newf(errs.CodeInvalidArgument, "engine", "root slot %d outside [0, %d)", slot, page.UsedRoots)
	}
	return nil
}
