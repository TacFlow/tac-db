package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// dirGateFS holds the next SyncDir of the log directory once armed.
type dirGateFS struct {
	*vfs.MemFS
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (g *dirGateFS) SyncDir(n string) error {
	g.mu.Lock()
	hold := g.armed && n == name+"-wal"
	if hold {
		g.armed = false
	}
	g.mu.Unlock()
	if hold {
		close(g.entered)
		<-g.release
	}
	return g.MemFS.SyncDir(n)
}

// A checkpoint that runs while the writer rolls, after the rename of the
// new segment and before its SyncDir, may take that segment as floor: it
// makes the name durable before the superblock refers to it, so a power
// loss right after the superblock leaves a log that opens.
func TestFloorOfARollInProgress(t *testing.T) {
	mem := vfs.NewMemFS()
	g := &dirGateFS{MemFS: mem, entered: make(chan struct{}), release: make(chan struct{})}
	o := quiet()
	o.Durability = Sync
	db := open(t, g, o)
	model := map[string]string{}
	fill(t, db, model, 0, 5)
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 5; i < 500; i++ {
			tx, err := db.BeginWrite(ctx)
			if err != nil {
				return
			}
			tx.Put(ctx, 0, []byte(fmt.Sprintf("k%03d", i%60)), []byte(strings.Repeat("z", 3000)))
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err = tx.Commit(cctx)
			cancel()
			if err != nil {
				return
			}
		}
	}()
	<-g.entered // the new segment is renamed; the rename is not durable
	want := read(t, db)
	mem.InjectCrash(vfs.CrashPoint{Op: vfs.OpSync, Name: name, Nth: 2, After: true}) // the superblock
	db.Checkpoint(ctx)
	close(g.release)
	<-done
	db.Close()
	mem.Restart()
	db, err := Open(ctx, mem, name, o)
	if err != nil {
		t.Fatalf("reopen after a crash right after the superblock: %v (segments %v)", err, segs(t, mem))
	}
	defer db.Close()
	same(t, read(t, db), want)
}

// syncFailFS fails the next Sync of the main file once armed.
type syncFailFS struct {
	*vfs.MemFS
	arm atomic.Bool
}

type syncFailFile struct {
	vfs.File
	fs *syncFailFS
}

func (f *syncFailFile) Sync() error {
	if f.fs.arm.CompareAndSwap(true, false) {
		return errs.New(errs.CodeIO, "test", "injected fsync failure")
	}
	return f.File.Sync()
}

func (s *syncFailFS) Open(n string) (vfs.File, error) {
	f, err := s.MemFS.Open(n)
	if err != nil || n != name {
		return f, err
	}
	return &syncFailFile{f, s}, nil
}

// A failed Sync of the main file fails the handle at once, not when the
// coordinator gets to it: no other pass may retry the Sync meanwhile.
func TestCheckpointFailureIsImmediate(t *testing.T) {
	fs := &syncFailFS{MemFS: vfs.NewMemFS()}
	o := quiet()
	o.Durability = Sync
	db := open(t, fs, o)
	defer db.Close()
	model := map[string]string{}
	fill(t, db, model, 0, 40)
	var superblocks atomic.Int64
	db.hooks.checkpoint = func(stage string) {
		if stage == "superblock" {
			superblocks.Add(1)
		}
	}
	db.mu.Lock() // the coordinator, which fails the handle too, waits here
	fs.arm.Store(true)
	err1 := db.Checkpoint(ctx)
	err2 := db.Checkpoint(ctx)
	db.mu.Unlock()
	if !isCode(err1, errs.CodeRecoveryRequired) || !isCode(err2, errs.CodeRecoveryRequired) || superblocks.Load() != 0 {
		t.Fatalf("after a failed Sync: %v, then %v, %d superblocks", err1, err2, superblocks.Load())
	}
}

// Readers that keep their snapshots across many operations stay
// consistent while the writer commits and the maintenance checkpoints
// and removes segments as often as it can.
func TestReadersAgainstFrequentCheckpoints(t *testing.T) {
	for _, mode := range []Durability{Sync, GroupSync, Async} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			o := testOptions()
			o.Durability = mode
			o.MaintenanceInterval = time.Millisecond
			o.CheckpointBytes = 64 << 10
			o.CheckpointSegments = 1
			db := open(t, vfs.NewMemFS(), o)
			const keys = 12
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var failed atomic.Value
			for r := 0; r < 4; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						rt, err := db.BeginRead(ctx)
						if err != nil {
							failed.Store(err.Error())
							return
						}
						var first string
						for pass := 0; pass < 5; pass++ {
							for i := 0; i < keys; i++ {
								v, ok, err := rt.Get(ctx, 0, []byte(fmt.Sprintf("k%02d", i)))
								if err != nil {
									failed.Store("get: " + err.Error())
									rt.Close()
									return
								}
								if !ok {
									continue
								}
								if first == "" {
									first = string(v)
								} else if string(v) != first {
									failed.Store(fmt.Sprintf("snapshot %d: key %d has %.8q, want %.8q", rt.State().CommitID, i, v, first))
									rt.Close()
									return
								}
							}
							time.Sleep(50 * time.Microsecond)
						}
						rt.Close()
					}
				}()
			}
			for n := 0; n < 400 && failed.Load() == nil; n++ {
				tx, err := db.BeginWrite(ctx)
				if err != nil {
					t.Fatal(err)
				}
				val := fmt.Sprintf("%06d", n) + strings.Repeat("x", 1500)
				for i := 0; i < keys; i++ {
					tx.Put(ctx, 0, []byte(fmt.Sprintf("k%02d", i)), []byte(val))
				}
				if _, err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			close(stop)
			wg.Wait()
			if f := failed.Load(); f != nil {
				t.Fatal(f)
			}
			if st := db.Stats(); st.Checkpoints == 0 || st.SegmentsRemoved == 0 {
				t.Fatalf("stats %+v", st)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A group whose append fails is finished like the others in the failure:
// its commit gets COMMIT_UNKNOWN and its token is Unknown.
func TestFailedAppendFinishesItsCommits(t *testing.T) {
	fs := vfs.NewMemFS()
	o := quiet()
	o.Durability = Sync
	db := open(t, fs, o)
	defer db.Close()
	put(t, db, "a", "1")
	fs.Inject(vfs.Fault{Op: vfs.OpWrite, Name: name + "-wal/*"})
	tx, _ := db.BeginWrite(ctx)
	tx.Put(ctx, 0, []byte("b"), []byte("2"))
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := tx.Commit(cctx)
	if !isCode(err, errs.CodeCommitUnknown) || db.CommitStatus(r.Token) != Unknown {
		t.Fatalf("a commit whose append failed: %v, status %v", err, db.CommitStatus(r.Token))
	}
}
