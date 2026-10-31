package versions

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/wal"
)

// Random sequences of Writable, Fresh, Discard, Savepoint, RollbackTo and
// Release against a model: the contents of the current tree and of every
// savepoint, and the accounting — never below the copies alive, never
// above them plus the replaced copies waiting for a Release.
func TestViewAgainstModel(t *testing.T) {
	for seed := uint64(0); seed < 2000; seed++ {
		r := rand.New(rand.NewPCG(seed, 9))
		gov, _ := govern.New(govern.Config{Limit: 1 << 30})
		res, _ := gov.Reserve(ctx, govern.Overlay, 0)
		v := (&Backing{}).Writer(NewDirectory(wal.State{PageCount: 2}), res)
		v.SetPageCount(40)
		type model map[page.ID]byte
		cur := model{}
		var saves []model
		clone := func(m model) model {
			c := model{}
			for k, x := range m {
				c[k] = x
			}
			return c
		}
		stamp := byte(1)
		for step := 0; step < 300; step++ {
			id := page.ID(2 + r.IntN(6))
			switch op := r.IntN(10); {
			case op < 3:
				var buf *page.Buf
				var err error
				if _, ok := cur[id]; ok && r.IntN(2) == 0 {
					buf, err = v.Writable(ctx, id)
				} else {
					buf, err = v.Fresh(id)
				}
				if err != nil {
					t.Fatal(err)
				}
				stamp++
				buf[100] = stamp
				cur[id] = stamp
			case op < 4:
				v.Discard(id)
				delete(cur, id)
			case op < 6:
				v.Savepoint()
				saves = append(saves, clone(cur))
			case op < 8:
				if len(saves) > 0 {
					sp := r.IntN(len(saves))
					if err := v.RollbackTo(sp); err != nil {
						t.Fatal(err)
					}
					saves = saves[:sp+1]
					cur = clone(saves[sp])
				}
			default:
				if len(saves) > 0 {
					sp := r.IntN(len(saves))
					v.Release(sp)
					saves = saves[:sp]
				}
			}
			n := 0
			v.pages.All(func(k uint64, p *private) bool {
				n++
				if cur[page.ID(k)] != p.buf[100] {
					t.Fatalf("seed %d step %d: page %d holds %d, model %d", seed, step, k, p.buf[100], cur[page.ID(k)])
				}
				return true
			})
			if n != len(cur) {
				t.Fatalf("seed %d step %d: %d pages, model %d", seed, step, n, len(cur))
			}
			live := map[*page.Buf]bool{}
			v.pages.All(func(_ uint64, p *private) bool { live[p.buf] = true; return true })
			for i, s := range v.saves {
				s.pages.All(func(k uint64, p *private) bool {
					live[p.buf] = true
					if saves[i][page.ID(k)] != p.buf[100] {
						t.Fatalf("seed %d step %d: savepoint %d page %d holds %d, model %d", seed, step, i, k, p.buf[100], saves[i][page.ID(k)])
					}
					return true
				})
			}
			pending := 0
			for _, p := range v.replaced {
				if !live[p.buf] {
					pending++
				}
			}
			switch {
			case res.Bytes() != v.charged:
				t.Fatalf("seed %d step %d: reservation %d, charged %d", seed, step, res.Bytes(), v.charged)
			case v.charged < int64(len(live))*page.Size:
				t.Fatalf("seed %d step %d: charged %d copies, %d alive", seed, step, v.charged/page.Size, len(live))
			case v.charged > int64(len(live)+pending)*page.Size:
				t.Fatalf("seed %d step %d: leak: charged %d copies, %d alive, %d pending", seed, step, v.charged/page.Size, len(live), pending)
			}
		}
		res.Close()
	}
}

// Fresh over a copy reuses it when no savepoint holds it; a Writable that
// cannot read keeps no charge.
func TestChargesOfFreshAndFailedWritable(t *testing.T) {
	gov, _ := govern.New(govern.Config{Limit: 1 << 30})
	res, _ := gov.Reserve(ctx, govern.Overlay, 0)
	defer res.Close()
	v := (&Backing{}).Writer(NewDirectory(wal.State{PageCount: 2}), res)
	v.SetPageCount(10)
	v.Fresh(5)
	v.Fresh(5)
	if v.charged != page.Size {
		t.Fatalf("Fresh twice: %d copies charged for one", v.charged/page.Size)
	}
	sp := v.Savepoint()
	v.Fresh(5)
	v.Release(sp)
	if v.charged != page.Size || res.Bytes() != page.Size {
		t.Fatalf("one live copy, %d bytes charged", v.charged)
	}

	h := newHarness(t)
	res2, _ := gov.Reserve(ctx, govern.Overlay, 0)
	defer res2.Close()
	w := h.b.Writer(NewDirectory(wal.State{PageCount: 10}), res2)
	if _, err := w.Writable(ctx, 5); err == nil { // the main file is empty
		t.Fatal("reading a missing page succeeded")
	}
	if w.charged != 0 || res2.Bytes() != 0 {
		t.Fatalf("a failed Writable kept %d bytes charged", w.charged)
	}
}

// A finalized view, or a read-only one, changes nothing.
func TestClosedAndReadOnlyViews(t *testing.T) {
	h := newHarness(t)
	x := h.begin()
	x.tree.Put(ctx, cache.NewPins(0), []byte("k"), []byte("v"))
	sp := x.v.Savepoint()
	h.commitTx(x, inWAL)
	v := x.v
	if err := v.RollbackTo(sp); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("RollbackTo after Finalize: %v", err)
	}
	if v.Savepoint() != -1 {
		t.Fatal("Savepoint after Finalize")
	}
	root := v.Root(0)
	v.SetRoot(0, 99)
	v.SetPageCount(1)
	v.Discard(root)
	if v.Root(0) != root || v.PageCount() < 3 {
		t.Fatal("a finalized view changed")
	}
	if err := v.ReadOverflow(ctx, cache.NewPins(0), page.Extent{Start: 3, Pages: 1}, make([]byte, 10)); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("ReadOverflow after Finalize: %v", err)
	}
	snap := h.b.Snapshot(h.dir)
	if _, err := snap.Writable(ctx, root); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("Writable on a read-only view: %v", err)
	}
	if _, err := snap.Fresh(root); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("Fresh on a read-only view: %v", err)
	}
}

// A small budget still makes progress, and a value equal to the limit is
// pruned.
func TestPruneBudgetAndLimit(t *testing.T) {
	var tr Tree[val]
	for k := uint64(0); k < 5000; k++ {
		tr = tr.Set(k, val(1+k%3)) // commits 1, 2 and 3
	}
	for _, budget := range []int{1, 2, 3, 4} {
		x, next := tr, uint64(0)
		for calls := 0; ; calls++ {
			if x, next = x.Prune(2, next, budget); next == 0 {
				break
			}
			if calls > 100000 {
				t.Fatalf("budget %d: the sweep does not end", budget)
			}
		}
		if want := 5000 / 3; x.Len() != want { // only commit 3 stays
			t.Fatalf("budget %d: %d values left, want %d", budget, x.Len(), want)
		}
	}
	// A tree whose largest commit equals the limit goes in one step.
	if x, next := tr.Prune(3, 0, 1); next != 0 || x.Len() != 0 {
		t.Fatalf("a whole tree at the limit: %d values left, next %d", x.Len(), next)
	}
}

// A sealed version never appended is read from its image.
func TestSealedImageReads(t *testing.T) {
	h := newHarness(t)
	x := h.begin()
	pins := cache.NewPins(0)
	x.tree.Put(ctx, pins, []byte("k"), bytes.Repeat([]byte{4}, 3000))
	x.v.SetRoot(0, x.tree.Root())
	f, err := x.v.Finalize(1, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Sealed {
		if s.Placed() {
			t.Fatal("a version was placed without an append")
		}
	}
	got, ok, err := btree.Get(ctx, h.b.Snapshot(f.Dir), pins, f.Dir.State.Roots[0], []byte("k"))
	if err != nil || !ok || len(got) != 3000 {
		t.Fatalf("read from the sealed images: %v", err)
	}
}

// Readers verify snapshots while the coordinator commits, places and
// releases versions and the checkpointer moves the base, never past the
// oldest snapshot in use.
func TestConcurrentReaders(t *testing.T) {
	h := newHarness(t)
	type pub struct {
		dir   *Directory
		model map[string][]byte
	}
	var head atomic.Pointer[pub]
	head.Store(&pub{dir: h.dir, model: map[string][]byte{}})
	var mu sync.Mutex
	inUse := map[uint64]int{} // commit -> readers
	oldest := func(def uint64) uint64 {
		mu.Lock()
		defer mu.Unlock()
		m := def
		for c := range inUse {
			m = min(m, c)
		}
		return m
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pins := cache.NewPins(0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				p := head.Load() // register before reading, as the engine will
				inUse[p.dir.State.CommitID]++
				mu.Unlock()
				snap := h.b.Snapshot(p.dir)
				n := 0
				err := btree.Range(ctx, snap, pins, p.dir.State.Roots[0], nil, nil, func(k, v []byte) error {
					if !bytes.Equal(p.model[string(k)], v) {
						return fmt.Errorf("commit %d: key %s differs", p.dir.State.CommitID, k)
					}
					n++
					return nil
				})
				if err == nil && n != len(p.model) {
					err = fmt.Errorf("commit %d: %d keys, model %d", p.dir.State.CommitID, n, len(p.model))
				}
				mu.Lock()
				if inUse[p.dir.State.CommitID]--; inUse[p.dir.State.CommitID] == 0 {
					delete(inUse, p.dir.State.CommitID)
				}
				mu.Unlock()
				if err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	r := rand.New(rand.NewPCG(3, 3))
	model := map[string][]byte{}
	pins := cache.NewPins(0)
	for round := 0; round < 60; round++ {
		x := h.begin()
		for op := 0; op < 20; op++ {
			k := fmt.Sprintf("k%03d", r.IntN(200))
			v := bytes.Repeat([]byte{byte(round)}, r.IntN(2500))
			if _, err := x.tree.Put(ctx, pins, []byte(k), v); err != nil {
				t.Fatal(err)
			}
			model[k] = v
		}
		h.commitTx(x, inWAL)
		cp := make(map[string][]byte, len(model))
		for k, v := range model {
			cp[k] = v
		}
		head.Store(&pub{dir: h.dir, model: cp})
		// The checkpointer moves the base only when no reader holds an
		// older snapshot; a reader that registers later sees this head.
		if round%5 == 4 && oldest(h.dir.State.CommitID) == h.dir.State.CommitID {
			h.checkpoint(h.dir)
		}
	}
	close(stop)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
}
