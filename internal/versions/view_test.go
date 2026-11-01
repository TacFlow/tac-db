package versions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/btree"
	"github.com/TacFlow/tacdb/internal/cache"
	"github.com/TacFlow/tacdb/internal/check"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/govern"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
	"github.com/TacFlow/tacdb/internal/wal"
)

var ctx = context.Background()

func isCode(err error, code errs.Code) bool { return errors.Is(err, errs.Sentinel(code)) }

// harness plays the engine of M2c around a Backing: commits publish
// directories with sealed versions, optionally moved to a real WAL, and
// checkpoints write versions to the main file and publish the base T.
type harness struct {
	t      *testing.T
	fs     *vfs.MemFS
	b      *Backing
	dir    *Directory
	w      *wal.Writer
	segMu  sync.Mutex // readers open segments concurrently
	segs   map[uint64]vfs.File
	commit uint64
	group  uint64
	budget int // prune budget of Finalize
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fs := vfs.NewMemFS()
	main, err := fs.Create("main")
	if err != nil {
		t.Fatal(err)
	}
	gov, err := govern.New(govern.Config{Limit: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(ctx, cache.Options{Budget: 64 * cache.EntryCost, Governor: gov})
	if err != nil {
		t.Fatal(err)
	}
	uuid := [16]byte{7}
	w, err := wal.Create(fs, "main-wal", uuid, wal.Options{SegmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, fs: fs, w: w, segs: map[uint64]vfs.File{}, budget: 64}
	h.b = &Backing{UUID: uuid, Cache: c, Main: main, Segment: h.segment}
	h.dir = NewDirectory(wal.State{PageCount: 2})
	t.Cleanup(func() { w.Close(); c.Close() })
	return h
}

func (h *harness) segment(seq uint64) (vfs.File, error) {
	h.segMu.Lock()
	defer h.segMu.Unlock()
	if f, ok := h.segs[seq]; ok {
		return f, nil
	}
	f, err := h.fs.OpenReadOnly("main-wal/" + wal.SegmentName(seq))
	if err != nil {
		return nil, err
	}
	h.segs[seq] = f
	return f, nil
}

// tx is a write transaction with one tree in root 0.
type tx struct {
	v    *View
	a    *alloc.Allocator
	tree *btree.Tree
}

func (h *harness) begin() *tx {
	v := h.b.Writer(h.dir, nil)
	a := alloc.New(v)
	return &tx{v: v, a: a, tree: btree.New(v, a, v.Root(0))}
}

// mode says where a commit leaves its versions.
type mode int

const (
	sealedOnly mode = iota // images held by the directory
	inWAL                  // appended, synced and released: read from the WAL
)

func (h *harness) commitTx(x *tx, m mode) *Final {
	h.t.Helper()
	x.v.SetRoot(0, x.tree.Root())
	parent := h.commit
	f, err := x.v.Finalize(parent+1, parent, h.budget)
	if err != nil {
		h.t.Fatal(err)
	}
	h.commit++
	if m == inWAL {
		h.group++
		g := &wal.Group{ID: h.group, Begins: []wal.Begin{{Token: [16]byte{byte(h.commit)}, Parent: parent, CommitID: h.commit}}}
		var man wal.Manifest
		man.AddBegin(&g.Begins[0])
		for _, img := range f.Pages {
			man.AddPage(page.HeaderOf(img).ID(), img)
		}
		g.Pages = f.Pages
		g.Commit = wal.Commit{IntentCount: 1, PageCount: uint32(len(f.Pages)), State: f.Dir.State, Digest: man.Digest()}
		a, err := h.w.AppendGroup(g)
		if err != nil {
			h.t.Fatal(err)
		}
		for i, s := range f.Sealed {
			s.Place(a.Pages[i])
		}
		if err := h.w.Sync(); err != nil {
			h.t.Fatal(err)
		}
		for _, s := range f.Sealed {
			s.Release()
		}
	} else {
		// Keep the group chain of the WAL consistent for later appends.
		h.group++
		g := &wal.Group{ID: h.group, Begins: []wal.Begin{{Token: [16]byte{1}, Parent: parent, CommitID: h.commit}}}
		var man wal.Manifest
		man.AddBegin(&g.Begins[0])
		for _, img := range f.Pages {
			man.AddPage(page.HeaderOf(img).ID(), img)
		}
		g.Pages = f.Pages
		g.Commit = wal.Commit{IntentCount: 1, PageCount: uint32(len(f.Pages)), State: f.Dir.State, Digest: man.Digest()}
		a, err := h.w.AppendGroup(g)
		if err != nil {
			h.t.Fatal(err)
		}
		for i, s := range f.Sealed {
			s.Place(a.Pages[i]) // placed but the image is kept: reads use it
		}
	}
	h.dir = f.Dir
	return f
}

// checkpoint makes the main file hold the state of directory d, of commit
// T (SPEC §11.1): for every page with a version newer than the current
// base, the latest version at or below T — the entry of d — goes to the
// main file; then the cached base images are invalidated, the base T is
// published. The entries at or below T are left to the pruning of later
// finalizations. T must not pass the oldest snapshot still in use. Pages
// free in T are written too (the engine skips them, design §4.4): that is
// sound, since a free page is only ever reached through Fresh.
func (h *harness) checkpoint(d *Directory) {
	h.t.Helper()
	if err := h.w.Sync(); err != nil {
		h.t.Fatal(err)
	}
	t, base := d.State.CommitID, h.b.Base()
	var buf page.Buf
	var ids []page.ID
	d.pages.All(func(k uint64, l Loc) bool {
		if l.commit > base {
			ids = append(ids, page.ID(k))
		}
		return true
	})
	for _, id := range ids {
		if err := h.b.direct(&buf, d, id); err != nil {
			h.t.Fatal(err)
		}
		if err := h.b.Main.WriteAt(buf[:], int64(id)*page.Size); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.b.Main.Sync(); err != nil {
		h.t.Fatal(err)
	}
	for _, id := range ids {
		h.b.InvalidateBase(id)
	}
	h.b.SetBase(t)
}

// verify reads a directory through a snapshot: the tree, its contents
// against the model, and the global checker.
func (h *harness) verify(d *Directory, model map[string][]byte) {
	h.t.Helper()
	snap := h.b.Snapshot(d)
	pins := cache.NewPins(0)
	root := d.State.Roots[0]
	if _, err := btree.Verify(ctx, snap, pins, root); err != nil {
		h.t.Fatalf("commit %d: Verify: %v", d.State.CommitID, err)
	}
	keys := slices.Sorted(maps.Keys(model))
	i := 0
	err := btree.Range(ctx, snap, pins, root, nil, nil, func(k, v []byte) error {
		if i >= len(keys) || string(k) != keys[i] || !bytes.Equal(v, model[keys[i]]) {
			return fmt.Errorf("key %d is %q", i, k)
		}
		i++
		return nil
	})
	if err != nil || i != len(keys) {
		h.t.Fatalf("commit %d: Range: %v after %d of %d keys", d.State.CommitID, err, i, len(keys))
	}
	rep, err := check.Structural(ctx, snap, d.State.Roots, d.State.PageCount, d.State.FreePageCount)
	if err != nil || !rep.OK() {
		h.t.Fatalf("commit %d: check: %v %v", d.State.CommitID, err, rep.Err())
	}
	if pins.Used() != 0 {
		h.t.Fatalf("%d bytes still pinned", pins.Used())
	}
}

// The B+tree and the allocator of M1 run over the View, with versions read
// from sealed images, the WAL and the main file; old snapshots keep their
// state while newer commits and checkpoints go on.
func TestTreeOverViews(t *testing.T) {
	h := newHarness(t)
	r := rand.New(rand.NewPCG(7, 7))
	model := map[string][]byte{}
	type snap struct {
		dir   *Directory
		model map[string][]byte
	}
	var held []snap
	for round := 0; round < 40; round++ {
		x := h.begin()
		pins := cache.NewPins(0)
		for op := 0; op < 60; op++ {
			k := fmt.Sprintf("key/%04d", r.IntN(400))
			if r.IntN(4) == 0 {
				if _, _, err := x.tree.Delete(ctx, pins, []byte(k)); err != nil {
					t.Fatal(err)
				}
				delete(model, k)
				continue
			}
			v := bytes.Repeat([]byte{byte(round)}, r.IntN(3000))
			if _, err := x.tree.Put(ctx, pins, []byte(k), v); err != nil {
				t.Fatal(err)
			}
			model[k] = v
		}
		h.commitTx(x, mode(round%2))
		h.verify(h.dir, model)
		if round%10 == 3 {
			held = append(held, snap{h.dir, maps.Clone(model)})
		}
		if round%10 == 9 {
			// Checkpoint up to the oldest snapshot still held, then let it go.
			if len(held) > 0 {
				h.checkpoint(held[0].dir)
				for _, s := range held {
					h.verify(s.dir, s.model)
				}
				held = held[1:]
			} else {
				h.checkpoint(h.dir)
			}
		}
	}
	h.checkpoint(h.dir)
	h.verify(h.dir, model)
	// The next finalization prunes every entry the checkpoint made
	// redundant: only its own pages remain.
	h.budget = 1 << 20
	x := h.begin()
	x.tree.Put(ctx, cache.NewPins(0), []byte("key/0000"), []byte("last"))
	model["key/0000"] = []byte("last")
	f := h.commitTx(x, inWAL)
	if h.dir.Len() != len(f.Pages) {
		t.Fatalf("after a full checkpoint and one commit, %d entries for %d pages", h.dir.Len(), len(f.Pages))
	}
	h.verify(h.dir, model)
}

// TX-20: 10 000 statements with a savepoint each and errors in between:
// linear time, O(1) restore, and copies bounded by the savepoints alive.
func TestStatementSavepoints(t *testing.T) {
	gov, err := govern.New(govern.Config{Limit: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	run := func(n int) (time.Duration, *View, *govern.Reservation) {
		h := newHarness(t)
		x := h.begin()
		pins := cache.NewPins(0)
		for i := 0; i < 20; i++ {
			x.tree.Put(ctx, pins, []byte(fmt.Sprintf("seed/%02d", i)), []byte("v"))
		}
		h.commitTx(x, sealedOnly)
		res, err := gov.Reserve(ctx, govern.Overlay, 0)
		if err != nil {
			t.Fatal(err)
		}
		v := h.b.Writer(h.dir, res)
		tree := btree.New(v, alloc.New(v), v.Root(0))
		start := time.Now()
		for i := 0; i < n; i++ {
			sp := v.Savepoint()
			root := tree.Root()
			if _, err := tree.Put(ctx, pins, []byte(fmt.Sprintf("seed/%02d", i%20)), []byte(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
			if i%3 == 0 { // a failed statement
				if err := v.RollbackTo(sp); err != nil {
					t.Fatal(err)
				}
				tree.Reset(root)
			}
			v.Release(sp)
		}
		return time.Since(start), v, res
	}
	small, _, r1 := run(1000)
	large, v, res := run(10000)
	defer r1.Close()
	defer res.Close()
	if large > 30*small+50*time.Millisecond {
		t.Fatalf("10 000 statements took %v, 1 000 took %v: not linear", large, small)
	}
	if v.charged != res.Bytes() || v.charged > 64*page.Size {
		t.Fatalf("charged %d bytes (reservation %d) for a handful of pages", v.charged, res.Bytes())
	}
}

// A restore brings back the pages, counts and roots, returns the copies to
// the Governor, counts the restore, and never lets a cursor survive.
func TestRollbackTo(t *testing.T) {
	h := newHarness(t)
	gov, _ := govern.New(govern.Config{Limit: 1 << 30})
	res, _ := gov.Reserve(ctx, govern.Overlay, 0)
	defer res.Close()
	v := h.b.Writer(h.dir, res)
	a := alloc.New(v)
	tree := btree.New(v, a, 0)
	pins := cache.NewPins(0)
	tree.Put(ctx, pins, []byte("a"), []byte("1"))
	v.SetRoot(0, tree.Root())
	sp := v.Savepoint()
	before := res.Bytes()
	for i := 0; i < 50; i++ {
		tree.Put(ctx, pins, []byte(fmt.Sprintf("b%03d", i)), bytes.Repeat([]byte{1}, 500))
	}
	v.SetRoot(0, tree.Root())
	c, err := tree.First(ctx, pins)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.RollbackTo(sp); err != nil {
		t.Fatal(err)
	}
	tree.Reset(v.Root(0))
	if res.Bytes() != before || v.Restores() != 1 {
		t.Fatalf("after the restore: %d bytes charged (want %d), %d restores", res.Bytes(), before, v.Restores())
	}
	if err := c.Next(ctx); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("a cursor survived the restore: %v", err)
	}
	if got, ok, _ := tree.Get(ctx, pins, []byte("b000")); ok || got != nil {
		t.Fatal("a key written after the savepoint survived the restore")
	}
	if got, ok, _ := tree.Get(ctx, pins, []byte("a")); !ok || string(got) != "1" {
		t.Fatal("the key written before the savepoint was lost")
	}
	if err := v.RollbackTo(5); !isCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("unknown savepoint: %v", err)
	}
	if _, err := v.Finalize(1, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Writable(ctx, 3); !isCode(err, errs.CodeTxClosed) {
		t.Fatalf("a finalized view kept writing: %v", err)
	}
}

// A sealed version reads the same before and after it moves to the WAL,
// and a version whose main-file image has another CommitID is CORRUPT.
func TestSealedToWALAndBaseGuard(t *testing.T) {
	h := newHarness(t)
	x := h.begin()
	pins := cache.NewPins(0)
	x.tree.Put(ctx, pins, []byte("k"), bytes.Repeat([]byte{9}, 5000))
	f := h.commitTx(x, sealedOnly)
	snap := h.b.Snapshot(h.dir)
	v1, _, err := btree.Get(ctx, snap, pins, h.dir.State.Roots[0], []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.w.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Sealed {
		s.Release()
		if s.Image() != nil {
			t.Fatal("Release kept the image")
		}
	}
	v2, _, err := btree.Get(ctx, snap, pins, h.dir.State.Roots[0], []byte("k"))
	if err != nil || !bytes.Equal(v1, v2) {
		t.Fatalf("after the move to the WAL: %v", err)
	}
	// The main file holds another version of the root page, a valid leaf
	// of commit 77, while the base T says it holds commit 1.
	root := h.dir.State.Roots[0]
	var other page.Buf
	if err := h.b.direct(&other, h.dir, root); err != nil {
		t.Fatal(err)
	}
	page.HeaderOf(&other).SetCommitID(77)
	page.Seal(&other)
	h.b.Main.WriteAt(other[:], int64(root)*page.Size)
	h.b.SetBase(1)
	if _, _, err := btree.Get(ctx, snap, pins, root, []byte("k")); !isCode(err, errs.CodeCorrupt) {
		t.Fatalf("a main-file image of another commit: %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("releasing a version never placed did not panic")
		}
	}()
	NewSealed(5, new(page.Buf)).Release()
}

// Large values skip the cache: reading one does not evict the hot set.
func TestLargeValueBypass(t *testing.T) {
	h := newHarness(t)
	x := h.begin()
	pins := cache.NewPins(0)
	for i := 0; i < 30; i++ {
		x.tree.Put(ctx, pins, []byte(fmt.Sprintf("hot/%02d", i)), []byte("h"))
	}
	x.tree.Put(ctx, pins, []byte("big"), bytes.Repeat([]byte{3}, 3<<20))
	h.commitTx(x, inWAL)
	snap := h.b.Snapshot(h.dir)
	root := h.dir.State.Roots[0]
	readHot := func() {
		for i := 0; i < 30; i++ {
			if _, ok, err := btree.Get(ctx, snap, pins, root, []byte(fmt.Sprintf("hot/%02d", i))); err != nil || !ok {
				t.Fatal(err)
			}
		}
	}
	readHot()
	before := h.b.Cache.Stats()
	if v, ok, err := btree.Get(ctx, snap, pins, root, []byte("big")); err != nil || !ok || len(v) != 3<<20 {
		t.Fatalf("big value: %v", err)
	}
	readHot()
	if after := h.b.Cache.Stats(); after.Misses-before.Misses > 4 {
		t.Fatalf("reading 3 MiB evicted the hot set: %d new misses", after.Misses-before.Misses)
	}
}
