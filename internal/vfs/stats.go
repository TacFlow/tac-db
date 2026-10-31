package vfs

import "sync/atomic"

// fullSyncFallbacks counts syncs that fell back from F_FULLFSYNC to plain
// fsync (macOS).
var fullSyncFallbacks atomic.Int64

// Stats are counters of the OS file systems of the process (SPEC §8.3).
type Stats struct {
	// FullSyncFallbacks counts file and directory syncs on macOS that fell
	// back to plain fsync, which may lose recent writes or renames on
	// power loss.
	FullSyncFallbacks int64
}

// ReadStats returns the counters of the OS file systems.
func ReadStats() Stats { return Stats{FullSyncFallbacks: fullSyncFallbacks.Load()} }
