package engine

import (
	"container/list"
	"context"
	"errors"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
)

// waiter is a candidate in the FIFO queue of the writer slot.
type waiter struct {
	ch    chan admitted // receives exactly once
	elem  *list.Element
	taken bool // removed from the queue, by a handoff or by its own exit
}

type admitted struct {
	tx  *WriteTx
	err error
}

type waitQueue struct{ l list.List }

func (q *waitQueue) len() int { return q.l.Len() }

func (q *waitQueue) push(w *waiter) { w.elem = q.l.PushBack(w) }

func (q *waitQueue) pop() *waiter {
	w := q.l.Remove(q.l.Front()).(*waiter)
	w.taken = true
	return w
}

func (q *waitQueue) remove(w *waiter) {
	q.l.Remove(w.elem)
	w.taken = true
}

// failAll answers every candidate with err.
func (q *waitQueue) failAll(err error) {
	for q.l.Len() > 0 {
		q.pop().ch <- admitted{err: err}
	}
}

// BeginWrite waits in the FIFO queue for the writer slot (SPEC §10.2), at
// most BusyTimeout or until ctx ends.
func (db *DB) BeginWrite(ctx context.Context) (*WriteTx, error) {
	return db.beginWrite(ctx, true)
}

// TryBeginWrite takes the writer slot only if it is free now; otherwise it
// returns DB_BUSY.
func (db *DB) TryBeginWrite(ctx context.Context) (*WriteTx, error) {
	return db.beginWrite(ctx, false)
}

func (db *DB) beginWrite(ctx context.Context, wait bool) (*WriteTx, error) {
	const op = "engine.BeginWrite"
	if db.opts.ReadOnly {
		return nil, errs.New(errs.CodeReadOnly, op, "database opened read-only")
	}
	if err := ctx.Err(); err != nil {
		return nil, ctxErr(err, op)
	}
	db.mu.Lock()
	if err := db.usable(op); err != nil {
		db.mu.Unlock()
		return nil, err
	}
	if db.writer == nil && db.waiters.len() == 0 {
		tx, err := db.admitLocked()
		db.mu.Unlock()
		return tx, err
	}
	if !wait || db.opts.BusyTimeout == 0 {
		db.mu.Unlock()
		return nil, errs.New(errs.CodeDBBusy, op, "the writer slot is taken")
	}
	if db.waiters.len() >= db.opts.MaxWriterQueue {
		db.mu.Unlock()
		return nil, errs.New(errs.CodeWriterQueueFull, op, "the writer queue is full").
			With("limit", int64(db.opts.MaxWriterQueue))
	}
	w := &waiter{ch: make(chan admitted, 1)}
	db.waiters.push(w)
	db.mu.Unlock()

	start := time.Now()
	timer := time.NewTimer(db.opts.BusyTimeout)
	defer timer.Stop()
	var cause error
	select {
	case a := <-w.ch:
		return a.tx, a.err
	case <-timer.C:
		cause = errs.New(errs.CodeBusyTimeout, op, "waited BusyTimeout for the writer slot").
			With("waited_ms", time.Since(start).Milliseconds())
	case <-ctx.Done():
		cause = ctxErr(ctx.Err(), op)
	}
	db.mu.Lock()
	if !w.taken {
		db.waiters.remove(w)
		db.mu.Unlock()
		return nil, cause
	}
	db.mu.Unlock()
	// The slot was handed over while giving up: give it back.
	if a := <-w.ch; a.tx != nil {
		a.tx.Rollback()
	}
	return nil, cause
}

// admitLocked gives the writer slot to a new transaction: the counter, the
// slot and the registration change together, under db.mu.
func (db *DB) admitLocked() (*WriteTx, error) {
	if err := db.enter("engine.BeginWrite"); err != nil {
		return nil, err
	}
	tx := db.newWriteTx()
	db.writer = tx
	return tx, nil
}

// releaseWriterLocked frees the slot of tx and hands it to the next
// candidate. When the head cannot be admitted because MaxActiveTransactions
// is full, the next one is tried, and so on: a candidate left waiting with
// a free slot would wait for nothing, since readers end without db.mu and
// never wake the queue.
func (db *DB) releaseWriterLocked(tx *WriteTx) {
	if db.writer != tx {
		return
	}
	db.writer = nil
	db.active.Add(-1)
	db.reg.remove(tx.snap)
	for db.writer == nil && db.waiters.len() > 0 {
		w := db.waiters.pop()
		if err := db.usable("engine.BeginWrite"); err != nil {
			w.ch <- admitted{err: err}
			continue
		}
		tx, err := db.admitLocked()
		w.ch <- admitted{tx: tx, err: err}
	}
}

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }
