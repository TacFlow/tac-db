package govern

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/codec"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

func small(t *testing.T, wait time.Duration) *Governor {
	t.Helper()
	cfg := Config{Limit: 1000, Wait: wait}
	cfg.Floor[Execution] = 300
	cfg.Floor[Sealed] = 200
	cfg.NoWait[Sealed] = true
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestNewValidates(t *testing.T) {
	cfg := Config{Limit: 100}
	cfg.Floor[Cache] = 101
	if _, err := New(cfg); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("floors above the limit: %v", err)
	}
	if _, err := New(Config{Limit: 0}); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("zero limit: %v", err)
	}
	huge := Config{Limit: 100}
	huge.Floor[Cache], huge.Floor[Execution], huge.Floor[Overlay] = 1<<63-1, 1<<63-1, 2 // the sum wraps to 0
	if _, err := New(huge); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("floors whose sum overflows: %v", err)
	}
	if Default() != Default() {
		t.Fatal("Default must return the process Governor")
	}
	g := small(t, 0)
	for _, c := range []Category{-1, NumCategories} {
		if _, err := g.Reserve(context.Background(), c, 1); !isCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("category %d: %v", c, err)
		}
	}
	if _, err := g.Reserve(context.Background(), Cache, -1); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("negative reservation: %v", err)
	}
}

// Floors are reserved for their category; the rest is shared.
func TestFloorsAndSharedPool(t *testing.T) {
	g := small(t, 0)
	ctx := context.Background()
	// Shared pool: 1000 - 300 - 200 = 500.
	cache, err := g.Reserve(ctx, Cache, 500)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reserve(ctx, Cache, 1); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("cache beyond the shared pool: %v", err)
	}
	exec, err := g.Reserve(ctx, Execution, 300) // its floor is still free
	if err != nil {
		t.Fatalf("execution within its floor: %v", err)
	}
	if _, err := g.Reserve(ctx, Execution, 1); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("execution beyond its floor with no shared memory: %v", err)
	}
	cache.Shrink(100)
	if err := exec.Grow(ctx, 100); err != nil {
		t.Fatalf("execution into the released shared memory: %v", err)
	}
	if got := g.Stats().Used; got[Cache] != 400 || got[Execution] != 400 {
		t.Fatalf("used = %v", got)
	}
	cache.Close()
	exec.Close()
	exec.Close() // idempotent
	if got := g.Stats().Used; got[Cache] != 0 || got[Execution] != 0 {
		t.Fatalf("after Close used = %v", got)
	}
}

func TestRefusalDetails(t *testing.T) {
	g := small(t, 0)
	_, err := g.Reserve(context.Background(), Cache, 600)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeResourceLimit {
		t.Fatalf("err = %v", err)
	}
	if e.Details["requested"] != 600 || e.Details["available"] != 500 || e.Details["limit"] != 1000 {
		t.Fatalf("details = %v", e.Details)
	}
	if g.Stats().Refusals[Cache] != 1 {
		t.Fatal("refusal not counted")
	}
}

func TestWaitsForRelease(t *testing.T) {
	g := small(t, 5*time.Second)
	ctx := context.Background()
	held, _ := g.Reserve(ctx, Cache, 500)
	done := make(chan error)
	go func() {
		r, err := g.Reserve(ctx, Cache, 200)
		if r != nil {
			r.Close()
		}
		done <- err
	}()
	for g.Stats().Waits[Cache] == 0 {
		time.Sleep(time.Millisecond)
	}
	held.Shrink(250)
	if err := <-done; err != nil {
		t.Fatalf("waiting reservation: %v", err)
	}
	if g.Stats().Waits[Cache] != 1 {
		t.Fatal("wait not counted")
	}
	held.Close()
}

func TestWaitEndsInTimeoutOrCancel(t *testing.T) {
	g := small(t, 30*time.Millisecond)
	ctx := context.Background()
	held, _ := g.Reserve(ctx, Cache, 500)
	defer held.Close()
	start := time.Now()
	if _, err := g.Reserve(ctx, Cache, 1); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("after GovernorWait: %v, want RESOURCE_LIMIT", err)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("refused before waiting")
	}
	if s := g.Stats(); s.Waits[Cache] != 1 || s.Refusals[Cache] != 1 {
		t.Fatalf("a wait that timed out: %+v", s)
	}

	slow := small(t, time.Hour)
	held2, _ := slow.Reserve(ctx, Cache, 500)
	defer held2.Close()
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := slow.Reserve(cctx, Cache, 1); !isCode(err, errs.CodeCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	dctx, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	if _, err := slow.Reserve(dctx, Cache, 1); !isCode(err, errs.CodeDeadline) {
		t.Fatalf("deadline: %v", err)
	}
}

// Sealed commits never wait for queries (SPEC §8.5), and a request larger
// than the whole category capacity is refused at once.
func TestNoWaitAndImpossibleRequests(t *testing.T) {
	g := small(t, time.Hour)
	ctx := context.Background()
	held, _ := g.Reserve(ctx, Sealed, 700) // floor 200 + shared 500
	defer held.Close()
	start := time.Now()
	if _, err := g.Reserve(ctx, Sealed, 1); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("sealed beyond capacity: %v", err)
	}
	if _, err := g.Reserve(ctx, Cache, 10_000); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("impossible request: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a no-wait or impossible request waited")
	}
}

// Concurrent reservations never take more than the limit.
func TestConcurrentReservationsStayWithinLimit(t *testing.T) {
	g := small(t, 50*time.Millisecond)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var peak int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cat := []Category{Cache, Execution, Overlay}[i%3]
			for j := 0; j < 200; j++ {
				r, err := g.Reserve(ctx, cat, int64(10+j%90))
				if err != nil {
					continue
				}
				s := g.Stats()
				var total int64
				for _, u := range s.Used {
					total += u
				}
				mu.Lock()
				peak = max(peak, total)
				mu.Unlock()
				r.Close()
			}
		}(i)
	}
	wg.Wait()
	if peak > 1000 {
		t.Fatalf("peak usage %d above the limit", peak)
	}
	for c, u := range g.Stats().Used {
		if u != 0 {
			t.Fatalf("category %v still holds %d", Category(c), u)
		}
	}
}

// TryGrow never waits and is not a refusal; a closed reservation does not
// grow; Shrink never returns more than is held.
func TestTryGrowAndClose(t *testing.T) {
	g := small(t, time.Hour)
	ctx := context.Background()
	r, err := g.Reserve(ctx, Cache, 500)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.TryGrow(1); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("TryGrow beyond the shared pool: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("TryGrow waited")
	}
	if s := g.Stats(); s.Waits[Cache] != 0 || s.Refusals[Cache] != 0 {
		t.Fatalf("a probe was counted: %+v", s)
	}
	r.Shrink(-5)
	r.Shrink(100)
	other, err := g.Reserve(ctx, Overlay, 30)
	if err != nil {
		t.Fatal(err)
	}
	other.Shrink(other.Bytes() + 100)
	if other.Bytes() != 0 || g.Stats().Used[Overlay] != 0 {
		t.Fatalf("Shrink returned more than was held: bytes %d, used %d", other.Bytes(), g.Stats().Used[Overlay])
	}
	if err := r.TryGrow(50); err != nil {
		t.Fatal(err)
	}
	if r.Bytes() != 450 || g.Stats().Used[Cache] != 450 {
		t.Fatalf("bytes %d, used %d", r.Bytes(), g.Stats().Used[Cache])
	}
	r.Close()
	if err := r.Grow(ctx, 1); !isCode(err, errs.CodeClosed) {
		t.Fatalf("Grow after Close: %v", err)
	}
	if r.Bytes() != 0 || g.Stats().Used[Cache] != 0 {
		t.Fatal("Close kept memory")
	}
}

// One Reservation shared by many goroutines stays consistent with the
// Governor.
func TestSharedReservation(t *testing.T) {
	g := small(t, time.Millisecond)
	r, err := g.Reserve(context.Background(), Overlay, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				if r.TryGrow(70) == nil {
					r.Shrink(70)
				}
				if r.Grow(context.Background(), 30) == nil {
					r.Shrink(30)
				}
			}
		}()
	}
	wg.Wait()
	if r.Bytes() != 0 || g.Stats().Used[Overlay] != 0 {
		t.Fatalf("bytes %d, used %d", r.Bytes(), g.Stats().Used[Overlay])
	}
	r.Close()
}

// Value decoding debits the Governor through Budget (SPEC §5.3, §8.5).
func TestDecodeBudget(t *testing.T) {
	g := small(t, 0)
	ctx := context.Background()
	list := func(n int) []byte {
		v, err := value.MakeList(n, func(i int) (value.Value, error) { return value.Int(int64(i)), nil })
		if err != nil {
			t.Fatal(err)
		}
		b, err := codec.AppendValue(nil, v, value.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	res, err := g.Reserve(ctx, Execution, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	opt := codec.DecodeOptions{Limits: value.DefaultLimits(), Budget: Budget{Res: res}} // nil Ctx: Background
	if _, err := codec.DecodeValue(list(4), opt); err != nil {
		t.Fatal(err)
	}
	if res.Bytes() == 0 || g.Stats().Used[Execution] != res.Bytes() {
		t.Fatalf("decoding debited %d, governor shows %d", res.Bytes(), g.Stats().Used[Execution])
	}
	// The execution category holds at most 800 bytes (floor 300 + shared 500).
	if _, err := codec.DecodeValue(list(20), opt); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("decoding beyond the Governor: %v, want RESOURCE_LIMIT", err)
	}
}

// Before a reservation waits, the Governor asks the reclaimers for the
// missing memory; TryGrow never asks.
func TestReclaimers(t *testing.T) {
	g := small(t, time.Hour)
	ctx := context.Background()
	cached, err := g.Reserve(ctx, Cache, 500) // the whole shared pool
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	remove := g.AddReclaimer(func(bytes int64) int64 {
		calls++
		n := min(bytes, cached.Bytes())
		cached.Shrink(n)
		return n
	})
	big, err := g.Reserve(ctx, Execution, 300+200) // floor 300, 200 shared
	if err != nil || calls != 1 || cached.Bytes() != 300 {
		t.Fatalf("reserve with reclaim: err %v, calls %d, cache keeps %d", err, calls, cached.Bytes())
	}
	if s := g.Stats(); s.Waits[Execution] != 0 {
		t.Fatalf("a reclaimed reservation waited: %+v", s)
	}
	if err := big.TryGrow(10); !isCode(err, errs.CodeResourceLimit) || calls != 1 {
		t.Fatalf("TryGrow: %v, reclaimer calls %d", err, calls)
	}
	remove()
	fast := small(t, 0)
	held, _ := fast.Reserve(ctx, Cache, 500)
	defer held.Close()
	fast.AddReclaimer(func(int64) int64 { return 0 }) // frees nothing
	if _, err := fast.Reserve(ctx, Execution, 301); !isCode(err, errs.CodeResourceLimit) {
		t.Fatalf("nothing reclaimed: %v", err)
	}
	big.Close()
	cached.Close()
}

func TestTransfer(t *testing.T) {
	g, err := New(Config{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.Reserve(context.Background(), Overlay, 60)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Transfer(Sealed)
	if st := g.Stats(); st.Used[Overlay] != 0 || st.Used[Sealed] != 60 || s.Bytes() != 60 || r.Bytes() != 0 {
		t.Fatalf("after Transfer: %+v, held %d and %d", st.Used, s.Bytes(), r.Bytes())
	}
	if err := r.TryGrow(1); !isCode(err, errs.CodeClosed) {
		t.Fatalf("the source must be closed: %v", err)
	}
	r.Close() // idempotent, returns nothing more
	if g.Stats().Used[Sealed] != 60 {
		t.Fatal("closing the source returned memory it no longer holds")
	}
	if e := r.Transfer(Sealed); e.Bytes() != 0 {
		t.Fatal("a closed reservation transferred bytes")
	}
	s.Close()
	if st := g.Stats(); st.Used != [NumCategories]int64{} {
		t.Fatalf("leak: %+v", st.Used)
	}
}
