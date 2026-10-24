// Package govern implements the process memory governor (SPEC §8.5). Every
// handle debits its cache, executions, overlays, sealed commits, transport
// buffers and spool from one Governor, so that the process stays within a
// global budget.
package govern

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Category is a kind of memory use.
type Category int

// Categories (SPEC §8.5).
const (
	Cache Category = iota
	Execution
	Overlay
	Sealed
	Transport
	Spool
	NumCategories
)

var categoryNames = [NumCategories]string{"cache", "execution", "overlay", "sealed", "transport", "spool"}

func (c Category) String() string {
	if c < 0 || c >= NumCategories {
		return "unknown"
	}
	return categoryNames[c]
}

// Config sets the budget of a Governor.
type Config struct {
	// Limit is the total budget; the default is 1 GiB.
	Limit int64
	// Floor is the part of Limit reserved for each category; what is left
	// is shared by every category above its floor.
	Floor [NumCategories]int64
	// NoWait marks categories that never wait for memory: a reservation
	// above what is available fails at once. Sealed commits and the WAL
	// coordinator never wait for queries (SPEC §8.5).
	NoWait [NumCategories]bool
	// Wait bounds how long a reservation waits for memory; the default is
	// 1 s (GovernorWait).
	Wait time.Duration
}

// DefaultConfig returns the defaults of SPEC §24.1: 1 GiB in total, with
// floors of 64 MiB for executions, 128 MiB for sealed commits (the default
// MaxPendingCommitBytes) and 32 MiB for transport, and a 1 s wait. The
// cache has no floor here: each cache reserves its own quota of 16 pages
// when it is created.
func DefaultConfig() Config {
	c := Config{Limit: 1 << 30, Wait: time.Second}
	c.Floor[Execution] = 64 << 20
	c.Floor[Sealed] = 128 << 20
	c.Floor[Transport] = 32 << 20
	c.NoWait[Sealed] = true
	return c
}

// Governor accounts the memory of a process by category.
type Governor struct {
	cfg    Config
	shared int64 // Limit minus the floors

	mu      sync.Mutex
	used    [NumCategories]int64
	changed chan struct{} // closed and replaced whenever memory is released

	waits, refusals [NumCategories]atomic.Int64

	rmu        sync.Mutex
	reclaimers map[int]func(bytes int64) int64
	nextID     int
}

// New returns a Governor; it fails with INVALID_ARGUMENT if the floors
// exceed the limit or a value is negative.
func New(cfg Config) (*Governor, error) {
	const op = "govern.New"
	if cfg.Limit <= 0 || cfg.Wait < 0 {
		return nil, errs.New(errs.CodeInvalidArgument, op, "limit must be positive and wait non-negative")
	}
	var floors int64
	for _, f := range cfg.Floor {
		if f < 0 {
			return nil, errs.New(errs.CodeInvalidArgument, op, "negative floor")
		}
		if f > cfg.Limit-floors { // checked before adding, so the sum cannot overflow
			return nil, errs.New(errs.CodeInvalidArgument, op, "floors exceed the limit").With("limit", cfg.Limit)
		}
		floors += f
	}
	return &Governor{cfg: cfg, shared: cfg.Limit - floors, changed: make(chan struct{})}, nil
}

var (
	defaultOnce sync.Once
	defaultGov  *Governor
)

// Default returns the process Governor, created with DefaultConfig on
// first use.
func Default() *Governor {
	defaultOnce.Do(func() {
		g, err := New(DefaultConfig())
		if err != nil {
			panic(err) // DefaultConfig is valid
		}
		defaultGov = g
	})
	return defaultGov
}

// available returns what category c can still take. g.mu is held.
func (g *Governor) available(c Category) int64 {
	var over int64
	for i, u := range g.used {
		over += max(0, u-g.cfg.Floor[i])
	}
	return max(0, g.cfg.Floor[c]-g.used[c]) + max(0, g.shared-over)
}

// AddReclaimer registers a function that frees memory kept only as a
// cache, such as unpinned pages of a page cache above its quota. Before a
// reservation waits or is refused, the Governor asks the reclaimers for
// the missing bytes; each returns what it freed. A reclaimer is called
// with no lock of the Governor held, and only by reservations that may
// wait, never by TryGrow, so it may shrink its own reservations. The
// returned function removes it.
func (g *Governor) AddReclaimer(reclaim func(bytes int64) int64) (remove func()) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	if g.reclaimers == nil {
		g.reclaimers = make(map[int]func(int64) int64)
	}
	id := g.nextID
	g.nextID++
	g.reclaimers[id] = reclaim
	return func() {
		g.rmu.Lock()
		defer g.rmu.Unlock()
		delete(g.reclaimers, id)
	}
}

// reclaim asks the reclaimers for bytes and reports whether any freed
// memory.
func (g *Governor) reclaim(bytes int64) bool {
	g.rmu.Lock()
	fns := make([]func(int64) int64, 0, len(g.reclaimers))
	for _, f := range g.reclaimers {
		fns = append(fns, f)
	}
	g.rmu.Unlock()
	var freed int64
	for _, f := range fns {
		if freed >= bytes {
			break
		}
		freed += f(bytes - freed)
	}
	return freed > 0
}

// Reserve takes bytes for category c, waiting up to the configured time
// for other reservations to release memory; caches are asked to give back
// memory first. It returns RESOURCE_LIMIT
// with requested, available and limit details when the memory does not
// come in time, CANCELED or DEADLINE when ctx ends first.
func (g *Governor) Reserve(ctx context.Context, c Category, bytes int64) (*Reservation, error) {
	if err := g.take(ctx, c, bytes, true); err != nil {
		return nil, err
	}
	return &Reservation{g: g, cat: c, bytes: bytes}, nil
}

// take moves bytes into category c. Without wait, or for a category that
// never waits, memory that is not available now is refused at once.
func (g *Governor) take(ctx context.Context, c Category, bytes int64, wait bool) error {
	const op = "govern.Reserve"
	if c < 0 || c >= NumCategories || bytes < 0 {
		return errs.New(errs.CodeInvalidArgument, op, "unknown category or negative reservation")
	}
	var timer *time.Timer
	for {
		g.mu.Lock()
		avail := g.available(c)
		if bytes <= avail {
			g.used[c] += bytes
			g.mu.Unlock()
			if timer != nil {
				timer.Stop()
			}
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		if !wait {
			return g.short(c, bytes, avail) // a probe, not counted as a refusal
		}
		if bytes <= g.cfg.Floor[c]+g.shared && g.reclaim(bytes-avail) {
			continue
		}
		if g.cfg.NoWait[c] || g.cfg.Wait == 0 || bytes > g.cfg.Floor[c]+g.shared {
			g.refusals[c].Add(1)
			return g.short(c, bytes, avail)
		}
		if timer == nil {
			g.waits[c].Add(1)
			timer = time.NewTimer(g.cfg.Wait)
		}
		select {
		case <-changed:
		case <-timer.C:
			g.refusals[c].Add(1)
			return g.short(c, bytes, avail)
		case <-ctx.Done():
			timer.Stop()
			code := errs.CodeCanceled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = errs.CodeDeadline
			}
			return errs.Wrap(code, op, ctx.Err(), "waiting for memory")
		}
	}
}

// short is the RESOURCE_LIMIT of a reservation that does not fit.
func (g *Governor) short(c Category, bytes, avail int64) error {
	return errs.Newf(errs.CodeResourceLimit, "govern.Reserve", "%s memory exhausted", c).
		With("requested", bytes).With("available", avail).With("limit", g.cfg.Limit)
}

func (g *Governor) release(c Category, bytes int64) {
	if bytes == 0 {
		return
	}
	g.mu.Lock()
	g.used[c] -= bytes
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

// Stats is a snapshot of a Governor.
type Stats struct {
	Limit    int64
	Used     [NumCategories]int64
	Waits    [NumCategories]int64
	Refusals [NumCategories]int64
}

// Stats returns the current usage and counters.
func (g *Governor) Stats() Stats {
	s := Stats{Limit: g.cfg.Limit}
	g.mu.Lock()
	s.Used = g.used
	g.mu.Unlock()
	for i := range s.Waits {
		s.Waits[i] = g.waits[i].Load()
		s.Refusals[i] = g.refusals[i].Load()
	}
	return s
}

// Reservation is memory held by one resource. It grows and shrinks
// incrementally and returns everything on Close. It is safe for concurrent
// use; a Grow that waits holds no lock of the Reservation.
type Reservation struct {
	g   *Governor
	cat Category

	mu     sync.Mutex
	bytes  int64
	closed bool
}

// Bytes returns the memory held.
func (r *Reservation) Bytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Grow takes more bytes, with the waiting rules of Reserve.
func (r *Reservation) Grow(ctx context.Context, bytes int64) error {
	return r.grow(ctx, bytes, true)
}

// TryGrow takes more bytes only if they are available now. It never waits,
// and its refusals are not counted in Stats.
func (r *Reservation) TryGrow(bytes int64) error {
	return r.grow(context.Background(), bytes, false)
}

func (r *Reservation) grow(ctx context.Context, bytes int64, wait bool) error {
	if err := r.g.take(ctx, r.cat, bytes, wait); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.g.release(r.cat, bytes)
		return errs.New(errs.CodeClosed, "govern.Grow", "reservation closed")
	}
	r.bytes += bytes
	return nil
}

// Shrink returns bytes, at most what is held.
func (r *Reservation) Shrink(bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bytes = max(0, min(bytes, r.bytes))
	r.bytes -= bytes
	r.g.release(r.cat, bytes)
}

// Close returns all the memory; it is idempotent.
func (r *Reservation) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.g.release(r.cat, r.bytes)
		r.bytes = 0
	}
}

// Budget charges value decoding to a Reservation (the Budget of
// internal/codec, SPEC §5.3), with the waiting rules of Reserve.
type Budget struct {
	Ctx context.Context
	Res *Reservation
}

// Debit grows the reservation by bytes; a nil Ctx is context.Background.
func (b Budget) Debit(bytes int64) error {
	ctx := b.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return b.Res.Grow(ctx, bytes)
}
