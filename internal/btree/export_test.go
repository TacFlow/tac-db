package btree

import (
	"context"

	"github.com/TacFlow/tacdb/internal/page"
)

// Hooks for the external tests, which also use internal/check.

// LeafOf returns a copy of the leaf whose range holds key.
func LeafOf(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte) (page.Node, error) {
	ref, err := descend(ctx, r, pins, root, key, nil)
	if err != nil {
		return page.Node{}, err
	}
	b := *ref.Bytes()
	ref.Release()
	return page.NodeOf(&b), nil
}

// StoreLeaf writes a leaf on a page just allocated, to build shapes by hand.
func (t *Tree) StoreLeaf(ctx context.Context, id page.ID, cells []page.LeafCell, next page.ID) error {
	return t.store(ctx, &node{id: id, cells: cells, next: next}, true)
}

// StoreInternal writes an internal page on a page just allocated.
func (t *Tree) StoreInternal(ctx context.Context, id page.ID, level uint16, left page.ID, seps []page.InternalCell) error {
	return t.store(ctx, &node{id: id, level: level, left: left, seps: seps}, true)
}

// SetRoot replaces the root without counting a mutation.
func (t *Tree) SetRoot(id page.ID) { t.root = id }
