package cache

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
)

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

func newGovernor(t *testing.T, limit int64) *govern.Governor {
	t.Helper()
	g, err := govern.New(govern.Config{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newCache(t *testing.T, pages, shards int, gov *govern.Governor) *Cache {
	t.Helper()
	if gov == nil {
		gov = newGovernor(t, 1<<30)
	}
	c, err := New(context.Background(), Options{Budget: int64(pages) * EntryCost, Shards: shards, Governor: gov})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func key(p uint64) Key { return Key{Page: page.ID(p), Version: 1} }

// loader writes the page number into the image, so readers can check
// they got the right page.
func loader(calls *atomic.Int64, p uint64) Loader {
	return func(_ context.Context, dst *page.Buf) error {
		if calls != nil {
			calls.Add(1)
		}
		binary.LittleEndian.PutUint64(dst[100:], p)
		return nil
	}
}

func get(t *testing.T, c *Cache, pins page.Pins, p uint64) *Handle {
	t.Helper()
	h, err := c.Get(context.Background(), pins, key(p), loader(nil, p))
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(h.Bytes()[100:]); got != p {
		t.Fatalf("page %d holds %d", p, got)
	}
	return h
}

// cached reports whether page p is loaded, without touching its CLOCK bit.
func cached(c *Cache, p uint64) bool {
	sh := c.shardOf(key(p))
	sh.mu.Lock()
	defer sh.mu.Unlock()
	e, ok := sh.m[key(p)]
	return ok && e.buf != nil
}

// pinsOf returns the pin count of page p's entry, or -1 without one.
func pinsOf(c *Cache, p uint64) int {
	sh := c.shardOf(key(p))
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if e, ok := sh.m[key(p)]; ok {
		return e.pins
	}
	return -1
}

// eventually waits until cond holds.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
	}
}

func TestHitsAndMisses(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	pins := NewPins(0)
	var calls atomic.Int64
	for i := 0; i < 3; i++ {
		h, err := c.Get(context.Background(), pins, key(5), loader(&calls, 5))
		if err != nil {
			t.Fatal(err)
		}
		h.Release()
		h.Release() // idempotent
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want 1", calls.Load())
	}
	if s := c.Stats(); s.Hits != 2 || s.Misses != 1 || s.Loads != 1 || s.Bytes != EntryCost {
		t.Fatalf("stats %+v", s)
	}
	if pins.Used() != 0 {
		t.Fatalf("pins still held: %d", pins.Used())
	}
	// Another version of the same page is another entry.
	h, err := c.Get(context.Background(), pins, Key{Page: 5, Version: 2}, loader(&calls, 5))
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
	if calls.Load() != 2 {
		t.Fatal("a new version must be loaded")
	}
}

// Concurrent misses on one page share a single load.
func TestSingleLoadForConcurrentMisses(t *testing.T) {
	c := newCache(t, MinPages, 4, nil)
	var calls atomic.Int64
	slow := func(_ context.Context, dst *page.Buf) error {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		binary.LittleEndian.PutUint64(dst[100:], 9)
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := c.Get(context.Background(), NewPins(0), key(9), slow)
			if err != nil {
				t.Error(err)
				return
			}
			if binary.LittleEndian.Uint64(h.Bytes()[100:]) != 9 {
				t.Error("wrong page")
			}
			h.Release()
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want 1", calls.Load())
	}
}

// A caller waiting for the load of another caller does not inherit that
// caller's cancellation: it loads the page itself.
func TestWaiterRetriesAfterCanceledLoad(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, NewPins(0), key(7), func(ctx context.Context, _ *page.Buf) error {
			<-ctx.Done()
			return ctx.Err()
		})
		first <- err
	}()
	eventually(t, func() bool { return c.Stats().Misses == 1 })
	second := make(chan error, 1)
	go func() {
		h, err := c.Get(context.Background(), NewPins(0), key(7), loader(nil, 7))
		if err == nil {
			if binary.LittleEndian.Uint64(h.Bytes()[100:]) != 7 {
				err = errors.New("wrong page")
			}
			h.Release()
		}
		second <- err
	}()
	eventually(t, func() bool { return c.Stats().Hits == 1 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the waiter inherited the cancellation: %v", err)
	}
	if s := c.Stats(); s.Misses != 2 || s.Loads != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// A caller that stops waiting for a load releases its pin.
func TestWaitEndsWithContext(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		h, err := c.Get(context.Background(), NewPins(0), key(5), func(context.Context, *page.Buf) error {
			<-release
			return nil
		})
		if err == nil {
			h.Release()
		}
	}()
	eventually(t, func() bool { return c.Stats().Misses == 1 })
	pins := NewPins(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, pins, key(5), loader(nil, 5)); !isCode(err, errs.CodeDeadline) {
		t.Fatalf("err = %v, want DEADLINE", err)
	}
	if pins.Used() != 0 {
		t.Fatal("pins leaked")
	}
	close(release)
	<-done
	if n := pinsOf(c, 5); n != 0 {
		t.Fatalf("the page keeps %d pins after every caller left", n)
	}
}

// A waiter gets the error of the load it waited for, without loading again.
func TestWaiterGetsLoadError(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	var calls atomic.Int64
	release := make(chan struct{})
	bad := errs.New(errs.CodeCorrupt, "test", "bad page")
	failing := func(context.Context, *page.Buf) error {
		calls.Add(1)
		<-release
		return bad
	}
	first := make(chan error, 1)
	firstPins := NewPins(0)
	go func() {
		_, err := c.Get(context.Background(), firstPins, key(3), failing)
		first <- err
	}()
	eventually(t, func() bool { return c.Stats().Misses == 1 })
	second := make(chan error, 1)
	secondPins := NewPins(0)
	go func() {
		_, err := c.Get(context.Background(), secondPins, key(3), failing)
		second <- err
	}()
	eventually(t, func() bool { return c.Stats().Hits == 1 })
	close(release)
	for _, ch := range []chan error{first, second} {
		if err := <-ch; !errors.Is(err, bad) {
			t.Fatalf("err = %v, want the load error", err)
		}
	}
	if calls.Load() != 1 || firstPins.Used() != 0 || secondPins.Used() != 0 || pinsOf(c, 3) != -1 {
		t.Fatalf("calls %d, pins %d and %d, entry pins %d", calls.Load(), firstPins.Used(), secondPins.Used(), pinsOf(c, 3))
	}
}

// A Loader that panics leaves nothing behind.
func TestLoaderPanic(t *testing.T) {
	gov := newGovernor(t, 1<<30)
	c := newCache(t, MinPages+4, 1, gov)
	pins := NewPins(0)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		c.Get(context.Background(), pins, key(1), func(context.Context, *page.Buf) error { panic("boom") })
	}()
	if s := c.Stats(); s.Bytes != 0 || pins.Used() != 0 || pinsOf(c, 1) != -1 {
		t.Fatalf("after a panic: %+v, pins %d", s, pins.Used())
	}
	get(t, c, pins, 1).Release()
}

func TestLoadErrorIsNotCached(t *testing.T) {
	gov := newGovernor(t, 1<<30)
	c := newCache(t, MinPages+4, 1, gov)
	pins := NewPins(0)
	for p := uint64(1); p <= MinPages; p++ {
		get(t, c, pins, p).Release()
	}
	bad := errs.New(errs.CodeCorrupt, "test", "bad page")
	if _, err := c.Get(context.Background(), pins, key(99), func(context.Context, *page.Buf) error { return bad }); !errors.Is(err, bad) {
		t.Fatalf("err = %v", err)
	}
	// The failed page, beyond the quota, returned its room to the Governor.
	if s := c.Stats(); s.Bytes != MinPages*EntryCost || gov.Stats().Used[govern.Cache] != MinPages*EntryCost {
		t.Fatalf("a failed load kept its room: %+v, governor %d", s, gov.Stats().Used[govern.Cache])
	}
	get(t, c, pins, 99).Release() // the error was not cached
	if pins.Used() != 0 {
		t.Fatal("pins leaked")
	}
	// Below the quota a failed load leaves the reservation at the quota.
	empty := newCache(t, MinPages, 1, gov)
	if _, err := empty.Get(context.Background(), pins, key(1), func(context.Context, *page.Buf) error { return bad }); !errors.Is(err, bad) {
		t.Fatal(err)
	}
	if got := gov.Stats().Used[govern.Cache]; got != (2*MinPages+1)*EntryCost {
		t.Fatalf("governor cache usage %d, want two quotas and one page", got)
	}
}

// CLOCK gives a recently used page a second chance.
func TestClockSecondChance(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	pins := NewPins(0)
	for p := uint64(1); p <= MinPages; p++ {
		get(t, c, pins, p).Release()
	}
	get(t, c, pins, 100).Release() // full: the hand clears every bit and evicts page 1
	get(t, c, pins, 2).Release()   // page 2 is referenced again
	get(t, c, pins, 101).Release() // the hand skips page 2 and evicts page 3
	for _, p := range []uint64{1, 3} {
		if cached(c, p) {
			t.Errorf("page %d should have been evicted", p)
		}
	}
	for _, p := range []uint64{2, 4, MinPages, 100, 101} {
		if !cached(c, p) {
			t.Errorf("page %d should still be cached", p)
		}
	}
	if s := c.Stats(); s.Evictions != 2 || s.Bytes != MinPages*EntryCost {
		t.Fatalf("stats %+v", s)
	}
}

func TestAllPinned(t *testing.T) {
	c := newCache(t, MinPages, 2, nil)
	pins := NewPins(1 << 30)
	var held []*Handle
	for p := uint64(1); p <= MinPages; p++ {
		held = append(held, get(t, c, pins, p))
	}
	if _, err := c.Get(context.Background(), pins, key(99), loader(nil, 99)); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("every page pinned: %v, want RESOURCE_LIMIT", err)
	}
	held[0].Release()
	get(t, c, pins, 99).Release()
	for _, h := range held[1:] {
		h.Release()
	}
}

func TestPinsPerExecution(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	pins := NewPins(2 * page.Size)
	a, b := get(t, c, pins, 1), get(t, c, pins, 2)
	if _, err := c.Get(context.Background(), pins, key(3), loader(nil, 3)); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("third pin: %v, want RESOURCE_LIMIT", err)
	}
	a.Release()
	get(t, c, pins, 3).Release()
	b.Release()
}

// Each cache reserves a quota of MinPages pages when it is created and grows
// beyond it only while the Governor has memory; when the Governor is short,
// a new page evicts one of the cache's own pages (SPEC §8.2).
func TestQuotaAndGovernor(t *testing.T) {
	gov := newGovernor(t, 40*EntryCost)
	a := newCache(t, 64, 1, gov)
	b := newCache(t, 64, 1, gov)
	if _, err := New(context.Background(), Options{Budget: 64 * EntryCost, Governor: gov}); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("a third quota does not fit: %v", err)
	}
	pins := NewPins(1 << 30)
	var held []*Handle
	for p := uint64(1); p <= 24; p++ { // the quota and the 8 shared pages
		held = append(held, get(t, a, pins, p))
	}
	_, err := a.Get(context.Background(), pins, key(25), loader(nil, 25))
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeResourceLimit || e.Details["limit"] != 40*EntryCost {
		t.Fatalf("beyond the Governor with every page pinned: %v", err)
	}
	for p := uint64(1); p <= MinPages; p++ { // b keeps its quota
		get(t, b, pins, p)
	}
	if _, err := b.Get(context.Background(), pins, key(17), loader(nil, 17)); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("b beyond its quota: %v", err)
	}
	held[0].Release()
	get(t, a, pins, 25).Release() // evicts page 1 of a instead of waiting
	if !cached(a, 25) || cached(a, 1) {
		t.Fatal("page 25 should have replaced page 1")
	}
	if got := gov.Stats().Used[govern.Cache]; got != 40*EntryCost {
		t.Fatalf("governor cache usage %d, want %d", got, 40*EntryCost)
	}
	for _, h := range held[1:] {
		h.Release()
	}
	a.Close()
	if got := gov.Stats().Used[govern.Cache]; got != MinPages*EntryCost {
		t.Fatalf("after closing a, governor cache usage %d", got)
	}
}

func TestClose(t *testing.T) {
	gov := newGovernor(t, 1<<30)
	c := newCache(t, MinPages, 1, gov)
	if got := gov.Stats().Used[govern.Cache]; got != MinPages*EntryCost {
		t.Fatalf("New reserved %d bytes, want the quota", got)
	}
	pins := NewPins(0)
	h := get(t, c, pins, 1)
	c.Close()
	c.Close() // idempotent
	if got := gov.Stats().Used[govern.Cache]; got != 0 {
		t.Fatalf("Close kept %d bytes", got)
	}
	if _, err := c.Get(context.Background(), pins, key(2), loader(nil, 2)); !isCode(err, errs.CodeClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	if binary.LittleEndian.Uint64(h.Bytes()[100:]) != 1 {
		t.Fatal("a held handle must stay readable")
	}
	if cached(c, 1) {
		t.Fatal("Close kept the pages")
	}
	h.Release()
	if pins.Used() != 0 {
		t.Fatal("pins leaked")
	}
}

func TestOptions(t *testing.T) {
	if _, err := New(context.Background(), Options{Budget: (MinPages - 1) * EntryCost}); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("budget below %d pages: %v", MinPages, err)
	}
	c, err := New(context.Background(), Options{Governor: newGovernor(t, 1<<30)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := len(c.shards); n < 8 || n&(n-1) != 0 || n > max(8, 2*runtime.GOMAXPROCS(0)) {
		t.Fatalf("%d shards for GOMAXPROCS %d", n, runtime.GOMAXPROCS(0))
	}
	if c.Stats().Budget != 64<<20 {
		t.Fatalf("default budget %d", c.Stats().Budget)
	}
}

// When a reservation needs shared memory, the cache gives back unpinned
// pages above its quota instead of keeping them until Close.
func TestGovernorReclaimsPages(t *testing.T) {
	gov, err := govern.New(govern.Config{Limit: 64 * EntryCost, Wait: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := newCache(t, 1024, 1, gov)
	pins := NewPins(1 << 30)
	for p := uint64(1); p <= 200; p++ {
		get(t, c, pins, p).Release()
	}
	if got := gov.Stats().Used[govern.Cache]; got != 64*EntryCost {
		t.Fatalf("the cache took %d bytes, want all 64 pages", got)
	}
	held := get(t, c, pins, 1000) // a pinned page is never reclaimed
	r, err := gov.Reserve(context.Background(), govern.Execution, 47*EntryCost)
	if err != nil {
		t.Fatalf("reservation with every cached page unpinned: %v", err)
	}
	defer r.Close()
	if s := gov.Stats(); s.Waits[govern.Execution] != 0 || s.Used[govern.Cache] != 17*EntryCost {
		t.Fatalf("after reclaiming: %+v", s)
	}
	if !cached(c, 1000) {
		t.Fatal("the pinned page was evicted")
	}
	held.Release()
	if _, err := gov.Reserve(context.Background(), govern.Execution, 0); err != nil {
		t.Fatal(err)
	}
}

// Many goroutines with a tiny budget: the right page always comes back
// and the budget holds.
func TestConcurrentUse(t *testing.T) {
	c := newCache(t, MinPages, 8, nil)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, seed))
			pins := NewPins(0)
			for i := 0; i < 2000; i++ {
				p := r.Uint64N(64)
				h, err := c.Get(context.Background(), pins, key(p), loader(nil, p))
				if err != nil {
					if isCode(err, errs.CodeResourceLimit) {
						continue // momentarily all pinned by other goroutines
					}
					t.Error(err)
					return
				}
				if binary.LittleEndian.Uint64(h.Bytes()[100:]) != p {
					t.Errorf("page %d holds the wrong data", p)
				}
				h.Release()
			}
		}(uint64(g))
	}
	wg.Wait()
	if s := c.Stats(); s.Bytes > MinPages*EntryCost {
		t.Fatalf("budget exceeded: %+v", s)
	}
}

// Invalidate drops a page: the next Get loads it again. A handle held
// across the invalidation keeps its buffer, and evicting the old entry
// never removes the new one.
func TestInvalidate(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	pins := NewPins(0)
	var calls atomic.Int64
	h, err := c.Get(context.Background(), pins, key(1), loader(&calls, 1))
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
	c.Invalidate(key(1))
	if s := c.Stats(); s.Bytes != 0 {
		t.Fatalf("an unpinned invalidated page kept %d bytes", s.Bytes)
	}
	held := get(t, c, pins, 1)
	c.Invalidate(key(1)) // pinned: only the mapping goes
	if s := c.Stats(); s.Bytes != EntryCost {
		t.Fatalf("a pinned invalidated page must stay counted: %d bytes", s.Bytes)
	}
	fresh, err := c.Get(context.Background(), pins, key(1), loader(&calls, 1))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || fresh.Bytes() == held.Bytes() {
		t.Fatalf("loads %d; the new entry shares the old buffer: %v", calls.Load(), fresh.Bytes() == held.Bytes())
	}
	held.Release()
	fresh.Release()
	// Evicting the old entry, still in the CLOCK list, must leave the new
	// one mapped.
	for p := uint64(2); p < MinPages; p++ {
		get(t, c, pins, p).Release()
	}
	if !cached(c, 1) {
		t.Fatal("evicting the invalidated entry removed its successor")
	}
	for p := uint64(100); p <= 100+MinPages; p++ { // evict everything
		get(t, c, pins, p).Release()
	}
	if s := c.Stats(); s.Bytes > MinPages*EntryCost {
		t.Fatalf("budget exceeded: %+v", s)
	}
	c.Invalidate(key(999)) // absent: nothing to do
}

// A page invalidated while it loads is handed to its caller but not
// cached, and its room goes back.
func TestInvalidateWhileLoading(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	release := make(chan struct{})
	done := make(chan *Handle, 1)
	go func() {
		h, err := c.Get(context.Background(), NewPins(0), key(5), func(_ context.Context, dst *page.Buf) error {
			<-release
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		done <- h
	}()
	eventually(t, func() bool { return c.Stats().Misses == 1 })
	c.Invalidate(key(5))
	close(release)
	h := <-done
	h.Release()
	if cached(c, 5) || c.Stats().Bytes != 0 {
		t.Fatalf("invalidated while loading: cached %v, %d bytes", cached(c, 5), c.Stats().Bytes)
	}
}

// Invalidating the entry under the CLOCK hand moves the hand: the list
// stays whole and later pages are cached normally.
func TestInvalidateUnderTheHand(t *testing.T) {
	c := newCache(t, MinPages, 1, nil)
	pins := NewPins(0)
	for p := uint64(1); p <= MinPages; p++ {
		get(t, c, pins, p).Release()
	}
	sh := c.shards[0]
	sh.mu.Lock()
	under := sh.hand.key
	sh.mu.Unlock()
	c.Invalidate(under)
	for p := uint64(100); p < 100+3*MinPages; p++ {
		get(t, c, pins, p).Release()
	}
	sh.mu.Lock()
	n, walked := sh.n, 0
	for e := sh.hand; walked < n+1; e = e.next {
		if e == nil || e.next.prev != e {
			sh.mu.Unlock()
			t.Fatalf("the CLOCK list is broken after %d entries", walked)
		}
		walked++
	}
	sh.mu.Unlock()
}
