package engine

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/vfs"
)

// BenchmarkConcurrency is the concurrency row of SPEC §26.1 (REV-26): 1, 8,
// 32 and 100 readers against four writers queued for the writer slot, on
// files on disk in GroupSync. b.N is the number of reads. It reports the
// read and commit rates, their p99 latencies, BUSY_TIMEOUTs, the fairness
// of the queue (the fewest commits of a writer over the most) and the
// Syncs per commit.
func BenchmarkConcurrency(b *testing.B) {
	for _, readers := range []int{1, 8, 32, 100} {
		b.Run(fmt.Sprintf("readers=%d", readers), func(b *testing.B) {
			fs, err := vfs.OpenDir(b.TempDir(), vfs.Options{})
			if err != nil {
				b.Fatal(err)
			}
			o := DefaultOptions()
			o.Create = true
			db, err := Open(ctx, fs, name, o)
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			const keys = 2000
			tx, _ := db.BeginWrite(ctx)
			for k := range keys {
				tx.Put(ctx, 0, []byte(fmt.Sprintf("k%05d", k)), []byte(strings.Repeat("v", 200)))
			}
			if _, err := tx.Commit(ctx); err != nil {
				b.Fatal(err)
			}

			stop := make(chan struct{})
			var writers sync.WaitGroup
			var commits [4]atomic.Int64
			var timeouts atomic.Int64
			var cmu sync.Mutex
			var commitLat []time.Duration
			for w := range commits {
				writers.Add(1)
				go func() {
					defer writers.Done()
					rng := rand.New(rand.NewPCG(uint64(w), 7))
					for {
						select {
						case <-stop:
							return
						default:
						}
						start := time.Now()
						tx, err := db.BeginWrite(ctx)
						if err != nil {
							if isCode(err, "BUSY_TIMEOUT") {
								timeouts.Add(1)
								continue
							}
							b.Error(err)
							return
						}
						tx.Put(ctx, 0, []byte(fmt.Sprintf("k%05d", rng.IntN(keys))), []byte(strings.Repeat("w", 200)))
						if _, err := tx.Commit(ctx); err != nil {
							b.Error(err)
							return
						}
						d := time.Since(start)
						commits[w].Add(1)
						cmu.Lock()
						commitLat = append(commitLat, d)
						cmu.Unlock()
					}
				}()
			}

			per := max(1, b.N/readers)
			lat := make([][]time.Duration, readers)
			syncs0 := db.Stats().Syncs
			b.ResetTimer()
			start := time.Now()
			var rd sync.WaitGroup
			for r := range readers {
				rd.Add(1)
				go func() {
					defer rd.Done()
					rng := rand.New(rand.NewPCG(uint64(r), 11))
					lat[r] = make([]time.Duration, 0, per)
					for range per {
						t0 := time.Now()
						rt, err := db.BeginRead(ctx)
						if err != nil {
							b.Error(err)
							return
						}
						if _, _, err := rt.Get(ctx, 0, []byte(fmt.Sprintf("k%05d", rng.IntN(keys)))); err != nil {
							b.Error(err)
						}
						rt.Close()
						lat[r] = append(lat[r], time.Since(t0))
					}
				}()
			}
			rd.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			var counted [4]int64 // the commits inside the measured window
			for i := range commits {
				counted[i] = commits[i].Load()
			}
			close(stop)
			writers.Wait()

			reads := slices.Concat(lat...)
			var total int64
			least, most := counted[0], counted[0]
			for _, n := range counted {
				total += n
				least, most = min(least, n), max(most, n)
			}
			b.ReportMetric(float64(len(reads))/elapsed.Seconds(), "reads/s")
			b.ReportMetric(float64(p99(reads).Microseconds()), "read-p99-µs")
			b.ReportMetric(float64(total)/elapsed.Seconds(), "commits/s")
			cmu.Lock()
			b.ReportMetric(float64(p99(commitLat).Microseconds()), "commit-p99-µs")
			cmu.Unlock()
			b.ReportMetric(float64(timeouts.Load()), "busy-timeouts")
			if most > 0 {
				b.ReportMetric(float64(least)/float64(most), "fairness")
			}
			if total > 0 {
				b.ReportMetric(float64(db.Stats().Syncs-syncs0)/float64(total), "syncs/commit")
			}
		})
	}
}

func p99(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[len(s)*99/100]
}

// BenchmarkIsolatedCommit is gate 12 of SPEC §26.2 (TX-17): a writer alone
// in GroupSync waits for no window, so its median commit is at most the
// median Sync of a page in the same directory plus 0.2 ms. It reports
// both medians and their difference.
func BenchmarkIsolatedCommit(b *testing.B) {
	fs, err := vfs.OpenDir(b.TempDir(), vfs.Options{})
	if err != nil {
		b.Fatal(err)
	}
	f, err := fs.Create("probe")
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	o := DefaultOptions()
	o.Create = true
	db, err := Open(ctx, fs, name, o)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	var buf [4096]byte
	syncs := make([]time.Duration, 0, b.N)
	commits := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		t0 := time.Now()
		f.WriteAt(buf[:], int64(i%64)*4096)
		if err := f.SyncData(); err != nil {
			b.Fatal(err)
		}
		syncs = append(syncs, time.Since(t0))
		t0 = time.Now()
		tx, err := db.BeginWrite(ctx)
		if err != nil {
			b.Fatal(err)
		}
		tx.Put(ctx, 0, []byte(fmt.Sprintf("k%03d", i%500)), buf[:200])
		if _, err := tx.Commit(ctx); err != nil {
			b.Fatal(err)
		}
		commits = append(commits, time.Since(t0))
	}
	b.StopTimer()
	median := func(d []time.Duration) time.Duration {
		s := slices.Clone(d)
		slices.Sort(s)
		return s[len(s)/2]
	}
	sm, cm := median(syncs), median(commits)
	b.ReportMetric(float64(sm.Microseconds()), "sync-median-µs")
	b.ReportMetric(float64(cm.Microseconds()), "commit-median-µs")
	b.ReportMetric(float64((cm - sm).Microseconds()), "over-sync-µs")
}

// BenchmarkMaintenance is gate 10 of SPEC §26.2 (REV-02): with the
// checkpoint running at its defaults, the p99 of reads and of
// commits stays within 1.2 times the p99 with the checkpoint suspended.
// b.N is the number of commits of each run (about 2000 make 64 MiB of log);
// eight readers read all along.
// It reports both p99s of each run and the ratios.
func BenchmarkMaintenance(b *testing.B) {
	measure := func(active bool) (read, commit time.Duration) {
		fs, err := vfs.OpenDir(b.TempDir(), vfs.Options{})
		if err != nil {
			b.Fatal(err)
		}
		o := DefaultOptions() // the checkpoint every 64 MiB of log, as shipped
		o.Create = true
		db, err := Open(ctx, fs, name, o)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		db.mt.suspended.Store(!active)
		const keys = 2000
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var reads []time.Duration
		for r := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(uint64(r), 3))
				var mine []time.Duration
				defer func() { mu.Lock(); reads = append(reads, mine...); mu.Unlock() }()
				for {
					select {
					case <-stop:
						return
					default:
					}
					t0 := time.Now()
					rt, err := db.BeginRead(ctx)
					if err != nil {
						b.Error(err)
						return
					}
					rt.Get(ctx, 0, []byte(fmt.Sprintf("k%05d", rng.IntN(keys))))
					rt.Close()
					mine = append(mine, time.Since(t0))
				}
			}()
		}
		rng := rand.New(rand.NewPCG(9, 9))
		commits := make([]time.Duration, 0, b.N)
		for range b.N {
			t0 := time.Now()
			tx, err := db.BeginWrite(ctx)
			if err != nil {
				b.Fatal(err)
			}
			for range 8 {
				tx.Put(ctx, 0, []byte(fmt.Sprintf("k%05d", rng.IntN(keys))), []byte(strings.Repeat("m", 1500)))
			}
			if _, err := tx.Commit(ctx); err != nil {
				b.Fatal(err)
			}
			commits = append(commits, time.Since(t0))
		}
		close(stop)
		wg.Wait()
		if active && b.N >= 200 && db.Stats().Checkpoints == 0 { // the first run of a benchmark has b.N = 1
			b.Fatal("no checkpoint ran")
		}
		return p99(reads), p99(commits)
	}
	b.ResetTimer()
	r0, c0 := measure(false)
	r1, c1 := measure(true)
	b.StopTimer()
	b.ReportMetric(float64(r0.Microseconds()), "read-p99-off-µs")
	b.ReportMetric(float64(r1.Microseconds()), "read-p99-on-µs")
	b.ReportMetric(float64(c0.Microseconds()), "commit-p99-off-µs")
	b.ReportMetric(float64(c1.Microseconds()), "commit-p99-on-µs")
	b.ReportMetric(float64(r1)/float64(max(r0, 1)), "read-ratio")
	b.ReportMetric(float64(c1)/float64(max(c0, 1)), "commit-ratio")
}
