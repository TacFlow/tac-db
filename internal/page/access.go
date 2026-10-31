package page

import "context"

// Pins is the pin budget of one execution (MaxPinnedBytesPerExecution,
// SPEC §8.2). A Reader charges it for every page it hands out.
type Pins interface {
	Acquire(bytes int) error
	Release(bytes int)
}

// Ref is a page handed out by a Reader. The image of a published page is
// immutable while the Ref is held. The image of a private copy stays
// valid only until the next mutation of the tree that owns the page or
// the next savepoint restore (design §4.7).
type Ref interface {
	Bytes() *Buf
	Release()
}

// Reader reads the pages of one transaction. Implementations are the test
// store of internal/btree (M1) and the version directory (M2).
type Reader interface {
	// Page returns the private copy of a page dirtied by the transaction,
	// or else the published version, verified.
	Page(ctx context.Context, pins Pins, id ID) (Ref, error)
	// ReadOverflow reads the useful bytes of the overflow pages of an
	// extent, in order, verifying type, PageID, CRC and count.
	ReadOverflow(ctx context.Context, pins Pins, e Extent, dst []byte) error
	// PageCount returns the number of pages of the transaction's state.
	PageCount() uint64
}

// Writer changes the pages of one transaction.
type Writer interface {
	Reader
	// Writable returns the private copy of a page for the current savepoint
	// epoch, copying it on first write.
	Writable(ctx context.Context, id ID) (*Buf, error)
	// Fresh returns a zeroed private copy of a page just allocated, without
	// reading its old content.
	Fresh(id ID) (*Buf, error)
	// Discard forgets the private copy of a page that was freed.
	Discard(id ID)
	SetPageCount(n uint64)
	FreePageCount() uint64
	SetFreePageCount(n uint64)
}
