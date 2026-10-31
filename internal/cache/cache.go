// Package cache keeps verified page images in memory (SPEC §8.2): a CLOCK
// with second chance, split in shards, bounded in bytes and debited from
// the process Governor. Concurrent misses on one page share a single load,
// done outside every lock. Entries are immutable; a version is part of the
// key.
package cache

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
)

// Key identifies one version of one page of one database.
type Key struct {
	DB      [16]byte
	Page    page.ID
	Version uint64 // the CommitID in the page header
}

// Loader fills dst with the page and verifies it, under the context of the
// caller that loads; an error is returned to every caller waiting for that
// page, and nothing is cached.
type Loader func(ctx context.Context, dst *page.Buf) error

// EntryCost is what a cached page costs against the budget: the image plus
// bookkeeping.
const EntryCost = page.Size + 128

// MinPages is the quota of every cache, in pages. New reserves it from the
// Governor, so that other handles cannot take it (SPEC §8.2).
const MinPages = 16

const quota = MinPages * EntryCost

// Options configures a Cache.
type Options struct {
	Budget   int64            // bytes; default 64 MiB, at least MinPages entries
	Shards   int              // default: largest power of 2 <= 2*GOMAXPROCS, at least 8
	Governor *govern.Governor // default: govern.Default()
}

// Cache is a page cache. It is safe for concurrent use.
type Cache struct {
	shards []*shard
	budget int64
	next   atomic.Uint32 // shard where the next eviction scan starts
	closed atomic.Bool

	mu      sync.Mutex
	used    int64               // bytes of the pages cached or loading
	res     *govern.Reservation // holds max(used, quota) until Close
	release func()              // removes the reclaimer from the Governor

	hits, misses, loads, evictions atomic.Int64
}

type shard struct {
	mu   sync.Mutex
	m    map[Key]*entry
	hand *entry // CLOCK hand on a circular list of the loaded entries
	n    int    // entries in the list
}

// entry is a page cached or loading. The shard lock guards pins, ref, prev
// and next; buf, err and retry are set once, before ready is closed.
type entry struct {
	key        Key
	buf        *page.Buf
	pins       int
	ref        bool
	prev, next *entry
	ready      chan struct{}
	err        error
	retry      bool // the load ended with its caller's context
}

// New returns a Cache. It reserves the quota of MinPages pages from the
// Governor, waiting like any reservation.
func New(ctx context.Context, opts Options) (*Cache, error) {
	if opts.Budget == 0 {
		opts.Budget = 64 << 20
	}
	if opts.Budget < quota {
		return nil, errs.Newf(errs.CodeInvalidArgument, "cache.New", "budget of %d bytes is below %d pages", opts.Budget, MinPages)
	}
	if opts.Shards <= 0 {
		opts.Shards = 8
		for opts.Shards*2 <= 2*runtime.GOMAXPROCS(0) {
			opts.Shards *= 2
		}
	}
	if opts.Governor == nil {
		opts.Governor = govern.Default()
	}
	res, err := opts.Governor.Reserve(ctx, govern.Cache, quota)
	if err != nil {
		return nil, err
	}
	c := &Cache{budget: opts.Budget, res: res, shards: make([]*shard, opts.Shards)}
	for i := range c.shards {
		c.shards[i] = &shard{m: make(map[Key]*entry)}
	}
	c.release = opts.Governor.AddReclaimer(c.reclaim)
	return c, nil
}

// Close drops the cached pages and returns the cache's memory to the
// Governor; later calls to Get fail with CLOSED. Handles still held stay
// readable.
func (c *Cache) Close() {
	if c.closed.Swap(true) {
		return
	}
	c.release()
	for _, sh := range c.shards {
		sh.mu.Lock()
		sh.m, sh.hand, sh.n = make(map[Key]*entry), nil, 0
		sh.mu.Unlock()
	}
	c.res.Close()
}

// reclaim gives the Governor back unpinned pages above the quota, until
// bytes are freed or none is left.
func (c *Cache) reclaim(bytes int64) int64 {
	var freed int64
	for freed < bytes {
		c.mu.Lock()
		above := c.used > quota
		c.mu.Unlock()
		if !above || c.evictOne() == nil {
			break
		}
		c.shrink()
		freed += EntryCost
	}
	return freed
}

func (c *Cache) shardOf(k Key) *shard {
	h := uint64(k.Page)*0x9E3779B97F4A7C15 ^ k.Version*0xBF58476D1CE4E5B9
	for _, b := range k.DB {
		h = (h ^ uint64(b)) * 0x100000001B3
	}
	h ^= h >> 32
	return c.shards[h%uint64(len(c.shards))]
}

// Handle is a pinned cached page; it implements page.Ref.
type Handle struct {
	sh   *shard
	e    *entry
	pins page.Pins
	once sync.Once
}

// Bytes returns the page image, immutable while the handle is held.
func (h *Handle) Bytes() *page.Buf { return h.e.buf }

// Release unpins the page; it is idempotent.
func (h *Handle) Release() {
	h.once.Do(func() {
		h.sh.unpin(h.e)
		h.pins.Release(page.Size)
	})
}

// Get returns the page of key, loading it with load on a miss. The page
// counts against pins until the handle is released. A caller that waits
// for the load of another caller gets that load's error, unless the load
// ended with the other caller's context: then it loads the page itself.
func (c *Cache) Get(ctx context.Context, pins page.Pins, key Key, load Loader) (*Handle, error) {
	if c.closed.Load() {
		return nil, errs.New(errs.CodeClosed, "cache.Get", "cache closed")
	}
	if err := pins.Acquire(page.Size); err != nil {
		return nil, err
	}
	sh := c.shardOf(key)
	for {
		sh.mu.Lock()
		e, hit := sh.m[key]
		if !hit {
			e = &entry{key: key, ready: make(chan struct{})}
			sh.m[key] = e
		}
		e.pins++
		e.ref = true
		sh.mu.Unlock()
		if !hit {
			c.misses.Add(1)
			return c.fill(ctx, sh, e, pins, load)
		}
		c.hits.Add(1)
		select {
		case <-e.ready:
		case <-ctx.Done():
			sh.unpin(e)
			pins.Release(page.Size)
			code := errs.CodeCanceled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = errs.CodeDeadline
			}
			return nil, errs.Wrap(code, "cache.Get", ctx.Err(), "waiting for a page load")
		}
		if e.err == nil {
			return &Handle{sh: sh, e: e, pins: pins}, nil
		}
		sh.unpin(e)
		if !e.retry {
			pins.Release(page.Size)
			return nil, e.err
		}
	}
}

// fill loads the page of e, a new entry, and hands it to the callers
// waiting for it. A Loader that panics leaves no trace in the cache, and
// the waiters load the page themselves.
func (c *Cache) fill(ctx context.Context, sh *shard, e *entry, pins page.Pins, load Loader) (h *Handle, err error) {
	admitted, done := false, false
	defer func() {
		if done {
			return
		}
		if admitted {
			c.shrink() // the page is not cached: its room goes back
		}
		retry := ctx.Err() != nil
		if err == nil { // the Loader panicked
			err, retry = errs.New(errs.CodeIO, "cache.Get", "page load panicked"), true
		}
		sh.mu.Lock()
		if sh.m[e.key] == e {
			delete(sh.m, e.key)
		}
		e.pins--
		e.err, e.retry = err, retry
		sh.mu.Unlock()
		close(e.ready)
		pins.Release(page.Size)
	}()
	buf, err := c.admit()
	if err != nil {
		return nil, err
	}
	admitted = true
	if err = load(ctx, buf); err != nil {
		return nil, err
	}
	c.loads.Add(1)
	sh.mu.Lock()
	e.buf = buf
	if !c.closed.Load() && sh.m[e.key] == e {
		sh.insert(e)
	}
	sh.mu.Unlock()
	done = true
	close(e.ready)
	return &Handle{sh: sh, e: e, pins: pins}, nil
}

// admit returns a buffer for one more page: a new one while the cache has
// room in its budget and memory in its quota or the Governor, otherwise the
// buffer of an evicted page, whose room passes to the new page. It never
// waits: when every page is pinned it fails with RESOURCE_LIMIT (SPEC §8.2).
func (c *Cache) admit() (*page.Buf, error) {
	ok, short := c.grow()
	if ok {
		return new(page.Buf), nil
	}
	if buf := c.evictOne(); buf != nil {
		return buf, nil
	}
	if short != nil {
		return nil, short // the Governor has no memory and every page is pinned
	}
	return nil, errs.New(errs.CodeResourceLimit, "cache.Get", "every cached page is pinned").
		With("requested", EntryCost).With("available", 0).With("limit", c.budget)
}

// grow takes room for one more page within the budget, from the quota or
// else from the Governor, without waiting. When the Governor is what is
// short, it also returns the Governor's refusal.
func (c *Cache) grow() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used+EntryCost > c.budget {
		return false, nil
	}
	if c.used+EntryCost > quota {
		if err := c.res.TryGrow(EntryCost); err != nil {
			return false, err
		}
	}
	c.used += EntryCost
	return true, nil
}

// shrink returns the room of one page.
func (c *Cache) shrink() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used > quota {
		c.res.Shrink(EntryCost)
	}
	c.used -= EntryCost
}

// evictOne evicts one unpinned page, running the CLOCK of one shard at a
// time, and returns its buffer; it returns nil if every page is pinned.
func (c *Cache) evictOne() *page.Buf {
	n := uint32(len(c.shards))
	start := c.next.Add(1)
	for i := range n {
		sh := c.shards[(start+i)%n]
		sh.mu.Lock()
		buf := sh.evict()
		sh.mu.Unlock()
		if buf != nil {
			c.evictions.Add(1)
			return buf
		}
	}
	return nil
}

// evict moves the hand at most two turns: it skips pinned pages, clears
// the reference bit of the others and evicts the first page found without
// it. sh.mu is held.
func (sh *shard) evict() *page.Buf {
	for steps := 2 * sh.n; steps > 0; steps-- {
		e := sh.hand
		sh.hand = e.next
		if e.pins > 0 {
			continue
		}
		if e.ref {
			e.ref = false
			continue
		}
		sh.unlink(e)
		delete(sh.m, e.key)
		return e.buf
	}
	return nil
}

// insert puts e just behind the hand, the last place the hand reaches.
// sh.mu is held.
func (sh *shard) insert(e *entry) {
	if sh.hand == nil {
		e.prev, e.next = e, e
		sh.hand = e
	} else {
		e.prev, e.next = sh.hand.prev, sh.hand
		e.prev.next = e
		sh.hand.prev = e
	}
	sh.n++
}

// unlink takes e, which the hand has passed, out of the list. sh.mu is
// held.
func (sh *shard) unlink(e *entry) {
	if e.next == e {
		sh.hand = nil
	} else {
		e.prev.next, e.next.prev = e.next, e.prev
	}
	e.prev, e.next = nil, nil
	sh.n--
}

func (sh *shard) unpin(e *entry) {
	sh.mu.Lock()
	e.pins--
	sh.mu.Unlock()
}

// Stats is a snapshot of a Cache.
type Stats struct {
	Hits, Misses, Loads, Evictions int64
	Bytes, Budget                  int64
}

// Stats returns the counters and the bytes in use.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	used := c.used
	c.mu.Unlock()
	return Stats{
		Hits: c.hits.Load(), Misses: c.misses.Load(), Loads: c.loads.Load(),
		Evictions: c.evictions.Load(), Bytes: used, Budget: c.budget,
	}
}

// Pins is the pin budget of one execution (MaxPinnedBytesPerExecution,
// SPEC §8.2); it implements page.Pins and is safe for concurrent use.
type Pins struct {
	limit int64
	used  atomic.Int64
}

// NewPins returns a pin budget of limit bytes; 0 means the 8 MiB default.
func NewPins(limit int64) *Pins {
	if limit == 0 {
		limit = 8 << 20
	}
	return &Pins{limit: limit}
}

// Acquire takes bytes or fails with RESOURCE_LIMIT.
func (p *Pins) Acquire(bytes int) error {
	for {
		u := p.used.Load()
		if u+int64(bytes) > p.limit {
			return errs.New(errs.CodeResourceLimit, "cache.Pins", "too many pinned pages in one execution").
				With("requested", int64(bytes)).With("available", p.limit-u).With("limit", p.limit)
		}
		if p.used.CompareAndSwap(u, u+int64(bytes)) {
			return nil
		}
	}
}

// Release returns bytes.
func (p *Pins) Release(bytes int) { p.used.Add(-int64(bytes)) }

// Used returns the pinned bytes.
func (p *Pins) Used() int64 { return p.used.Load() }
