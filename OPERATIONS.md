# Operations

This document explains how to run tacdb in production: memory, files and, in later milestones, checkpoints, backups and the cluster. It grows with each milestone; this version covers memory and the durability of files on macOS.

## 1. Memory

### 1.1 The process governor

Every database handle in a process debits its memory from one governor (`internal/govern`, §8.5 of the specification). The default governor allows 1 GiB in total and splits it into categories:

| Category | Used for | Guaranteed floor (default) |
|---|---|---|
| cache | Page cache of every handle | 16 pages per handle, reserved when the handle opens |
| execution | Query operators, materialization, value decoding | 64 MiB |
| overlay | Private page copies of write transactions | none |
| sealed | Sealed commits waiting for durability | 128 MiB (`MaxPendingCommitBytes`) |
| transport | IPC and RPC buffers | 32 MiB |
| spool | Reassembly spool | none |

A category can always use its floor. Above it, every category shares what is left of the limit (here 1 GiB - 224 MiB). When a reservation does not fit, it waits for memory up to `GovernorWait` (1 s by default) and then fails with `RESOURCE_LIMIT`, whose details give `requested`, `available` and `limit`; a canceled or expired context ends the wait with `CANCELED` or `DEADLINE`. Sealed commits never wait for queries: a sealed reservation that does not fit fails at once.

In milestone M1 only the cache and value decoding debit the governor; the other categories are wired in by the milestones that create those resources (M2 for overlays and sealed commits, M10 for transport).

### 1.2 GOMEMLIMIT

tacdb never changes `GOGC` or `GOMEMLIMIT` of the host process. Set the governor limit below `GOMEMLIMIT`, leaving room for the rest of the host application and for the garbage collector's overhead. A starting point is a governor limit of at most half of `GOMEMLIMIT` when the host does other work, and at most 70% when tacdb is most of the process; measure the resident set under load and adjust.

### 1.3 Page cache

Each handle has a page cache of 64 MiB by default, debited from the governor's cache category. When the handle opens, its cache reserves a quota of 16 pages that other handles cannot take; if the governor cannot give it within `GovernorWait`, opening the handle fails with `RESOURCE_LIMIT`. Above the quota, the cache grows into the governor's shared memory, up to its own budget. That memory is lent, not owned: before another reservation waits for shared memory, the governor asks every cache to give back unpinned pages above its quota.

The cache keeps verified page images and evicts with a CLOCK (second chance) in shards. It never waits for memory: when its budget or the governor has no room, a new page takes the place of an unpinned page of the same cache, and when every cached page is pinned, the new page fails with `RESOURCE_LIMIT`. One execution can pin at most 8 MiB of pages (`MaxPinnedBytesPerExecution`). Concurrent requests for the same page share one read.

## 2. Files

### 2.1 Durability on macOS

tacdb flushes files and directories with `F_FULLFSYNC`, because plain `fsync` does not make macOS write its caches to the medium. Keep databases on APFS or HFS+, which implement it. When `F_FULLFSYNC` fails on a file or a directory, as on network file systems that do not implement it, tacdb falls back to plain `fsync` and counts the fallback. On such file systems recent writes and renames may not survive a power loss. The counter is internal for now; it becomes visible with the metrics of a later milestone.

## 3. Transactions and durability

### 3.1 Durability modes

The mode is chosen per handle and applies to every commit.

| Mode | A commit returns | After a power loss |
|---|---|---|
| `GroupSync` (default) | once its group is synced, with `Durable=true` | every returned commit survives |
| `Sync` | once it is synced as its own group, with `Durable=true` | every returned commit survives |
| `Async` | once it is appended and published, with `Durable=false` | a suffix of returned commits may be lost; what survives is always a prefix |

`GroupSync` is adaptive: a commit alone pays one Sync and waits for no window, and commits that seal while a Sync runs share the next one (up to 64 commits or 8 MiB per group). `Async` syncs every 100 ms, after 16 MiB of appends, on `Flush`, or as soon as a commit waits for room in the commit queue. `Flush` waits until every commit sealed before the call is durable. Readers that must only see durable data use `BeginReadDurable`.

### 3.2 Admission and limits

One writer holds the writer slot at a time. `BeginWrite` waits in a FIFO queue of 128 candidates for at most `BusyTimeout` (5 s); `TryBeginWrite` never waits. The errors are distinct: `DB_BUSY` (the slot is taken and the caller would not wait), `WRITER_QUEUE_FULL`, `BUSY_TIMEOUT`, `CANCELED` and `DEADLINE`. At most `MaxActiveTransactions` (1024) read and write transactions are open; one more gets `TOO_MANY_TRANSACTIONS`. A write transaction older than `MaxWriteTxDuration` (30 s) is rolled back and its next call gets `WRITE_TX_TIMEOUT`; commits it already sealed are not affected. A transaction holds at most `MaxTxBytes` (64 MiB) of private pages; the statement that passes it fails alone. A group must fit one log segment, so `GroupMaxBytes` may not exceed 128 MiB nor `MaxTxBytes` 64 MiB, and `AsyncSyncBytes` may not exceed `MaxPendingCommitBytes`.

Commits sealed and not yet durable are bounded by `MaxPendingCommits` (256) and `MaxPendingCommitBytes` (128 MiB). `Commit` waits for room under the caller's deadline; `TryCommit` gets `COMMIT_QUEUE_FULL` at once. A commit that would take the log past `MaxWALBytes` (1 GiB) gets `WAL_LIMIT`. In all these cases the transaction is not sealed and stays usable.

### 3.3 Pending commits and failures

A commit is sealed once its writer releases the slot; from then on it cannot be rolled back, because the next writer builds on it. Canceling the wait of a sealed commit returns `COMMIT_PENDING` with the commit's token; the commit still completes. `CommitStatus(token)` says `Pending`, `Volatile` (appended in `Async`), `Durable` or `Unknown`. Tokens of commits recovered from the log are known after a reopen; a token never seen, or lost in a failure, is `Unknown`, and the application then checks its own idempotency key. Never repeat a mutation automatically.

When an append or a Sync of the log fails, the handle becomes `RECOVERY_REQUIRED`: pending commits get `COMMIT_UNKNOWN`, a writer that built on them cannot commit, and no transaction begins. The failed Sync is never retried, since the operating system's cache is then in an unknown state. Close the handle and open the database again: recovery decides which commits survived, and a `COMMIT_UNKNOWN` commit may turn out to be complete.

### 3.4 Read-only opening

A read-only open never changes a file. It recovers the log in memory and tries to make what it read durable with a Sync of a read-only handle. Where the platform refuses that, the recovered commits past the last checkpoint are served but not reported durable, and `BeginReadDurable` gives the checkpointed state.

