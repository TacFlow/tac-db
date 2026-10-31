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
