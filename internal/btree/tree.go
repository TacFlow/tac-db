// Package btree is the B+tree of tacdb over G2 pages (SPEC §8.1): pages
// with a common prefix, truncated separators, values out of the page in
// extents, cursors and a structural Verify.
//
// A read tree is a page.Reader and a root. A write tree (Tree) belongs to
// one transaction and keeps its own mutation counter: changing the tree, or
// restoring a savepoint of the transaction, invalidates its cursors.
//
// Every page change in M1 rewrites the whole page in canonical form from
// its decoded cells (design §4.9), so a private copy is always canonical
// and every fit decision uses the canonical size of the resulting set
// (R13-19).
package btree

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/TacFlow/tacdb/internal/alloc"
	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/page"
)

// Txn is the transaction of a write tree: its pages, and a counter of
// savepoint restores that only grows.
type Txn interface {
	page.Writer
	Restores() uint64
}

// Tree is a write tree. It is not safe for concurrent use; the creator of
// the trees (M2) gives each root one Tree per transaction.
type Tree struct {
	tx    Txn
	alloc *alloc.Allocator
	root  page.ID
	muts  uint64
}

// New returns the write tree with the given root (0 for an empty tree).
func New(tx Txn, a *alloc.Allocator, root page.ID) *Tree {
	return &Tree{tx: tx, alloc: a, root: root}
}

// Root returns the current root.
func (t *Tree) Root() page.ID { return t.root }

// Reset sets the root after a savepoint restore. The mutation counter
// never goes back, so a cursor of the abandoned state stays invalid.
func (t *Tree) Reset(root page.ID) {
	t.root = root
	t.muts++
}

func corrupt(id page.ID, format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, "btree", "page %d: "+format, append([]any{id}, args...)...)
}

func ctxErr(ctx context.Context, op string) error {
	switch err := ctx.Err(); {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeDeadline, op, err, "context ended")
	default:
		return errs.Wrap(errs.CodeCanceled, op, err, "context ended")
	}
}

// frame is an internal page of a descent and the child taken from it.
type frame struct {
	id    page.ID
	child int
}

// descend walks from root to the leaf whose range holds key, recording
// the internal pages in path when it is not nil. The walk is at most
// MaxTreeHeight deep, levels strictly decrease by one, no page repeats
// and every pointer is checked (SPEC §8.1).
func descend(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte, path *[]frame) (page.Ref, error) {
	pc := r.PageCount()
	id := root
	want := -1 // the level the next page must have; unknown at the root
	var seen []page.ID
	for depth := 0; ; depth++ {
		if depth >= page.MaxTreeHeight {
			return nil, corrupt(id, "the tree is deeper than %d levels", page.MaxTreeHeight)
		}
		if err := page.CheckPointer(id, pc); err != nil {
			return nil, err
		}
		if slices.Contains(seen, id) {
			return nil, corrupt(id, "the page repeats on the path from the root")
		}
		seen = append(seen, id)
		ref, err := r.Page(ctx, pins, id)
		if err != nil {
			return nil, err
		}
		h := page.HeaderOf(ref.Bytes())
		level := int(h.Level())
		switch {
		case h.ID() != id:
			err = corrupt(id, "the page claims to be page %d", h.ID())
		case h.Type() != page.TypeLeaf && h.Type() != page.TypeInternal:
			err = corrupt(id, "a tree page of type %d", h.Type())
		case want >= 0 && level != want:
			err = corrupt(id, "level %d below a page of level %d", level, want+1)
		}
		if err != nil {
			ref.Release()
			return nil, err
		}
		if h.Type() == page.TypeLeaf {
			return ref, nil
		}
		i, child := page.NodeOf(ref.Bytes()).ChildFor(key)
		ref.Release()
		if path != nil {
			*path = append(*path, frame{id: id, child: i})
		}
		id, want = child, level-1
	}
}

// Get returns a copy of the value of key in the tree rooted at root.
func Get(ctx context.Context, r page.Reader, pins page.Pins, root page.ID, key []byte) ([]byte, bool, error) {
	if root == 0 {
		return nil, false, nil
	}
	ref, err := descend(ctx, r, pins, root, key, nil)
	if err != nil {
		return nil, false, err
	}
	n := page.NodeOf(ref.Bytes())
	i, found := n.Search(key)
	if !found {
		ref.Release()
		return nil, false, nil
	}
	v := n.Value(i)
	if !v.Out() {
		out := bytes.Clone(v.Inline)
		ref.Release()
		if out == nil {
			out = []byte{}
		}
		return out, true, nil
	}
	ref.Release()
	out, err := readValue(ctx, r, pins, &v)
	return out, err == nil, err
}

// Get returns a copy of the value of key.
func (t *Tree) Get(ctx context.Context, pins page.Pins, key []byte) ([]byte, bool, error) {
	return Get(ctx, t.tx, pins, t.root, key)
}

// extentsOf returns the checked extents of a value stored out of the page.
func extentsOf(ctx context.Context, r page.Reader, pins page.Pins, v *page.LeafValue) ([]page.Extent, error) {
	pc := r.PageCount()
	if v.Len > page.MaxValue {
		return nil, corrupt(0, "value of %d bytes", v.Len)
	}
	exts := slices.Clone(v.Extents[:v.NExtents])
	if v.Descriptor != 0 {
		if err := page.CheckPointer(v.Descriptor, pc); err != nil {
			return nil, err
		}
		ref, err := r.Page(ctx, pins, v.Descriptor)
		if err != nil {
			return nil, err
		}
		if h := page.HeaderOf(ref.Bytes()); h.ID() != v.Descriptor {
			ref.Release()
			return nil, corrupt(v.Descriptor, "descriptor claims to be page %d", h.ID())
		}
		exts, err = page.DescriptorExtents(ref.Bytes())
		ref.Release()
		if err != nil {
			return nil, err
		}
	}
	if err := page.CheckExtents(exts, v.Len, pc); err != nil {
		return nil, err
	}
	return exts, nil
}

// readValue reads a value stored out of the page.
func readValue(ctx context.Context, r page.Reader, pins page.Pins, v *page.LeafValue) ([]byte, error) {
	exts, err := extentsOf(ctx, r, pins, v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, v.Len)
	off := 0
	for _, e := range exts {
		n := min(len(out)-off, int(e.Pages)*page.Usable)
		if err := r.ReadOverflow(ctx, pins, e, out[off:off+n]); err != nil {
			return nil, err
		}
		off += n
	}
	return out, nil
}

// node is a decoded tree page being edited.
type node struct {
	id    page.ID
	level uint16
	next  page.ID // leaf: the next leaf
	left  page.ID // internal: the first child
	cells []page.LeafCell
	seps  []page.InternalCell
}

func (n *node) leaf() bool { return n.level == 0 }

func (n *node) count() int {
	if n.leaf() {
		return len(n.cells)
	}
	return len(n.seps)
}

// size is the canonical size of the page that would hold the node.
func (n *node) size() int {
	if n.leaf() {
		return page.LeafPageSize(n.cells)
	}
	return page.InternalPageSize(n.seps)
}

// underfull reports an occupancy below 25% of the usable bytes.
func (n *node) underfull() bool { return n.size()-page.HeaderSize < page.Usable/4 }

// child returns child i of an internal node.
func (n *node) child(i int) page.ID {
	if i == 0 {
		return n.left
	}
	return n.seps[i-1].Right
}

// load decodes a tree page for editing.
func (t *Tree) load(ctx context.Context, pins page.Pins, id page.ID) (*node, error) {
	if err := page.CheckPointer(id, t.tx.PageCount()); err != nil {
		return nil, err
	}
	ref, err := t.tx.Page(ctx, pins, id)
	if err != nil {
		return nil, err
	}
	defer ref.Release()
	b := ref.Bytes()
	h := page.HeaderOf(b)
	n := &node{id: id, level: h.Level()}
	switch h.Type() {
	case page.TypeLeaf:
		n.next, n.cells = h.Sibling(), page.NodeOf(b).LeafCells()
	case page.TypeInternal:
		n.left, n.seps = h.Aux(), page.NodeOf(b).InternalCells()
	default:
		return nil, corrupt(id, "a tree page of type %d", h.Type())
	}
	return n, nil
}

// store writes a node over its page; a fresh page is not read first.
func (t *Tree) store(ctx context.Context, n *node, fresh bool) error {
	var b *page.Buf
	var err error
	if fresh {
		b, err = t.tx.Fresh(n.id)
	} else {
		b, err = t.tx.Writable(ctx, n.id)
	}
	if err != nil {
		return err
	}
	if n.leaf() {
		return page.EncodeLeaf(b, n.id, n.cells, n.next)
	}
	return page.EncodeInternal(b, n.id, n.level, n.left, n.seps)
}

// writeValue makes the cell of key and value: inline when the full cell
// fits the inline rule, else written to overflow pages near hint.
func (t *Tree) writeValue(ctx context.Context, key, value []byte, hint page.ID) (page.LeafCell, error) {
	if page.Inline(len(key), uint64(len(value))) {
		return page.LeafCell{Key: bytes.Clone(key), Value: append([]byte{}, value...)}, nil
	}
	pages := (len(value) + page.Usable - 1) / page.Usable
	exts, err := t.alloc.AllocExtents(ctx, pages, hint)
	if err != nil {
		return page.LeafCell{}, err
	}
	off := 0
	for _, e := range exts {
		for p := e.Start; p < e.Start+page.ID(e.Pages); p++ {
			b, err := t.tx.Fresh(p)
			if err != nil {
				return page.LeafCell{}, err
			}
			end := min(off+page.Usable, len(value))
			if err := page.EncodeOverflow(b, p, value[off:end]); err != nil {
				return page.LeafCell{}, err
			}
			off = end
		}
	}
	ref := &page.OutRef{Len: uint64(len(value))}
	if len(exts) <= page.MaxInline {
		ref.Extents = exts
	} else {
		d, err := t.alloc.Alloc(ctx, hint)
		if err != nil {
			return page.LeafCell{}, err
		}
		b, err := t.tx.Fresh(d)
		if err != nil {
			return page.LeafCell{}, err
		}
		if err := page.EncodeDescriptor(b, d, exts); err != nil {
			return page.LeafCell{}, err
		}
		ref.Descriptor = d
	}
	return page.LeafCell{Key: bytes.Clone(key), Out: ref}, nil
}

// freeValue frees the overflow pages of a value, and its descriptor.
func (t *Tree) freeValue(ctx context.Context, pins page.Pins, ref *page.OutRef) error {
	exts := ref.Extents
	if ref.Descriptor != 0 {
		v := page.LeafValue{Len: ref.Len, Descriptor: ref.Descriptor}
		var err error
		if exts, err = extentsOf(ctx, t.tx, pins, &v); err != nil {
			return err
		}
	}
	if err := t.alloc.FreeExtents(ctx, exts); err != nil {
		return err
	}
	if ref.Descriptor != 0 {
		return t.alloc.Free(ctx, ref.Descriptor)
	}
	return nil
}

// path loads the internal pages on the way to the leaf of key, and the
// leaf.
func (t *Tree) path(ctx context.Context, pins page.Pins, key []byte) ([]frame, []*node, *node, error) {
	var frames []frame
	ref, err := descend(ctx, t.tx, pins, t.root, key, &frames)
	if err != nil {
		return nil, nil, nil, err
	}
	leafID := page.HeaderOf(ref.Bytes()).ID()
	ref.Release()
	nodes := make([]*node, len(frames))
	for i, f := range frames {
		if nodes[i], err = t.load(ctx, pins, f.id); err != nil {
			return nil, nil, nil, err
		}
	}
	leaf, err := t.load(ctx, pins, leafID)
	return frames, nodes, leaf, err
}

func search(cells []page.LeafCell, key []byte) (int, bool) {
	return slices.BinarySearchFunc(cells, key, func(c page.LeafCell, k []byte) int { return bytes.Compare(c.Key, k) })
}

// Put sets the value of key and returns the new root. A replaced value
// out of the page is freed only after the new one is written (SPEC §8.1).
// After an error the transaction must be rolled back.
func (t *Tree) Put(ctx context.Context, pins page.Pins, key, value []byte) (page.ID, error) {
	const op = "btree.Put"
	if len(key) > page.MaxKeyLen || len(value) > page.MaxValue {
		return t.root, errs.Newf(errs.CodeLimitExceeded, op, "key of %d bytes or value of %d bytes", len(key), len(value))
	}
	t.muts++
	if t.root == 0 {
		id, err := t.alloc.Alloc(ctx, 0)
		if err != nil {
			return t.root, err
		}
		cell, err := t.writeValue(ctx, key, value, id)
		if err != nil {
			return t.root, err
		}
		if err := t.store(ctx, &node{id: id, cells: []page.LeafCell{cell}}, true); err != nil {
			return t.root, err
		}
		t.root = id
		return t.root, nil
	}
	frames, nodes, leaf, err := t.path(ctx, pins, key)
	if err != nil {
		return t.root, err
	}
	cell, err := t.writeValue(ctx, key, value, leaf.id)
	if err != nil {
		return t.root, err
	}
	var old *page.OutRef
	if i, found := search(leaf.cells, key); found {
		old = leaf.cells[i].Out
		leaf.cells[i] = cell
	} else {
		leaf.cells = slices.Insert(leaf.cells, i, cell)
	}
	if err := t.fixup(ctx, pins, frames, nodes, leaf); err != nil {
		return t.root, err
	}
	if old != nil {
		return t.root, t.freeValue(ctx, pins, old)
	}
	return t.root, nil
}

// Delete removes key and returns whether it was there and the new root.
// A delete can allocate pages: when an empty internal page cannot merge
// with its sibling, the redistribution may split the parent and grow the
// tree, so Delete can fail with NO_SPACE or LIMIT_EXCEEDED. After an error
// the transaction must be rolled back.
func (t *Tree) Delete(ctx context.Context, pins page.Pins, key []byte) (bool, page.ID, error) {
	if t.root == 0 {
		return false, 0, nil
	}
	frames, nodes, leaf, err := t.path(ctx, pins, key)
	if err != nil {
		return false, t.root, err
	}
	i, found := search(leaf.cells, key)
	if !found {
		return false, t.root, nil
	}
	t.muts++
	old := leaf.cells[i].Out
	leaf.cells = slices.Delete(leaf.cells, i, i+1)
	if err := t.fixup(ctx, pins, frames, nodes, leaf); err != nil {
		return true, t.root, err
	}
	if old != nil {
		return true, t.root, t.freeValue(ctx, pins, old)
	}
	return true, t.root, nil
}

// fixup writes the changed leaf and settles each level up to the root:
// an oversized node splits, an underfull one merges or redistributes, and
// the climb stops at the first parent that did not change.
func (t *Tree) fixup(ctx context.Context, pins page.Pins, frames []frame, nodes []*node, cur *node) error {
	for d := len(nodes); d > 0; d-- {
		parent, ci := nodes[d-1], frames[d-1].child
		changed, err := t.settle(ctx, pins, cur, parent, ci)
		if err != nil || !changed {
			return err
		}
		cur = parent
	}
	return t.settleRoot(ctx, cur)
}

func (t *Tree) settle(ctx context.Context, pins page.Pins, cur, parent *node, ci int) (bool, error) {
	switch {
	case cur.size() > page.Size:
		right, sep, err := t.split(ctx, cur)
		if err != nil {
			return false, err
		}
		parent.seps = slices.Insert(parent.seps, ci, page.InternalCell{Key: sep, Right: right})
		return true, nil
	case cur.count() == 0 || cur.underfull():
		return t.rebalance(ctx, pins, cur, parent, ci)
	}
	return false, t.store(ctx, cur, false)
}

func (t *Tree) settleRoot(ctx context.Context, root *node) error {
	switch {
	case root.size() > page.Size:
		if int(root.level)+1 >= page.MaxTreeHeight {
			return errs.Newf(errs.CodeLimitExceeded, "btree", "the tree would exceed %d levels", page.MaxTreeHeight)
		}
		right, sep, err := t.split(ctx, root)
		if err != nil {
			return err
		}
		id, err := t.alloc.Alloc(ctx, root.id)
		if err != nil {
			return err
		}
		top := &node{id: id, level: root.level + 1, left: root.id, seps: []page.InternalCell{{Key: sep, Right: right}}}
		if err := t.store(ctx, top, true); err != nil {
			return err
		}
		t.root = id
	case root.count() == 0 && root.leaf():
		t.root = 0 // the tree is empty
		return t.alloc.Free(ctx, root.id)
	case root.count() == 0:
		t.root = root.left // a root with one child collapses
		return t.alloc.Free(ctx, root.id)
	default:
		return t.store(ctx, root, false)
	}
	return nil
}

// bestSplit returns the cut k closest to half of the bytes such that
// both sides fit; side gives the canonical sizes of the two sides of k.
// Every cut with a non-empty side is tried, each measured exactly (R13-16).
func bestSplit(n int, side func(k int) (int, int)) int {
	best, bestDiff := -1, 0
	for k := 1; k < n; k++ {
		l, r := side(k)
		if l > page.Size || r > page.Size {
			continue
		}
		if d := max(l-r, r-l); best < 0 || d < bestDiff {
			best, bestDiff = k, d
		}
	}
	return best
}

// separator returns the shortest s with maxL < s <= minR (SPEC §7.3).
func separator(maxL, minR []byte) []byte {
	p := 0
	for p < len(maxL) && p < len(minR) && maxL[p] == minR[p] {
		p++
	}
	return bytes.Clone(minR[:p+1])
}

// leafCut returns the best cut of leaf cells into two pages.
func leafCut(cells []page.LeafCell) int {
	return bestSplit(len(cells), func(k int) (int, int) {
		return page.LeafPageSize(cells[:k]), page.LeafPageSize(cells[k:])
	})
}

// internalCut returns the separator k to move up so that seps[:k] and
// seps[k+1:] fill two pages; both sides keep at least one separator.
func internalCut(seps []page.InternalCell) int {
	k := bestSplit(len(seps)-1, func(k int) (int, int) {
		return page.InternalPageSize(seps[:k]), page.InternalPageSize(seps[k+1:])
	})
	return k
}

// split moves the upper half of an oversized node to a new page at its
// right and returns that page and the separator for the parent. The 1344
// byte inline rule guarantees a cut (R13-01); finding none is a bug.
func (t *Tree) split(ctx context.Context, n *node) (page.ID, []byte, error) {
	id, err := t.alloc.Alloc(ctx, n.id)
	if err != nil {
		return 0, nil, err
	}
	right := &node{id: id, level: n.level}
	var sep []byte
	if n.leaf() {
		k := leafCut(n.cells)
		if k < 0 {
			panic("btree: no two-way split of a leaf")
		}
		right.cells, right.next = slices.Clone(n.cells[k:]), n.next
		n.cells, n.next = n.cells[:k], id
		sep = separator(n.cells[k-1].Key, right.cells[0].Key)
	} else {
		k := internalCut(n.seps)
		if k < 0 {
			panic("btree: no two-way split of an internal page")
		}
		sep = n.seps[k].Key
		right.left, right.seps = n.seps[k].Right, slices.Clone(n.seps[k+1:])
		n.seps = n.seps[:k]
	}
	if err := t.store(ctx, n, false); err != nil {
		return 0, nil, err
	}
	return id, sep, t.store(ctx, right, true)
}

// rebalance handles a node that is empty or below 25% (SPEC §8.1). It
// takes the sibling under the same parent, merges the right one into the
// left one when both fit in a page, and otherwise redistributes the
// cells; a redistribution whose separator does not fit in the parent is
// not made, unless the node is empty and cannot stay. It reports whether
// the parent changed.
func (t *Tree) rebalance(ctx context.Context, pins page.Pins, cur, parent *node, ci int) (bool, error) {
	l, r, si := cur, (*node)(nil), 0
	var err error
	if ci > 0 {
		si = ci - 1
		l, r = nil, cur
		if l, err = t.load(ctx, pins, parent.child(ci-1)); err != nil {
			return false, err
		}
	} else if r, err = t.load(ctx, pins, parent.child(1)); err != nil {
		return false, err
	}
	if l.level != cur.level || r.level != cur.level {
		return false, corrupt(parent.id, "children at different levels")
	}
	empty := cur.count() == 0
	if cur.leaf() {
		all := append(slices.Clone(l.cells), r.cells...)
		if page.LeafPageSize(all) <= page.Size {
			l.cells, l.next = all, r.next
			return true, t.merged(ctx, l, r, parent, si)
		}
		k := leafCut(all)
		if k < 0 {
			panic("btree: no redistribution of two leaves")
		}
		if !t.setSeparator(parent, si, separator(all[k-1].Key, all[k].Key), empty) {
			return false, t.store(ctx, cur, false)
		}
		l.cells, r.cells = all[:k], all[k:]
	} else {
		down := page.InternalCell{Key: parent.seps[si].Key, Right: r.left}
		all := append(append(slices.Clone(l.seps), down), r.seps...)
		if page.InternalPageSize(all) <= page.Size {
			l.seps = all
			return true, t.merged(ctx, l, r, parent, si)
		}
		k := internalCut(all)
		if k < 0 {
			panic("btree: no redistribution of two internal pages")
		}
		if !t.setSeparator(parent, si, all[k].Key, empty) {
			return false, t.store(ctx, cur, false)
		}
		l.seps, r.left, r.seps = all[:k], all[k].Right, all[k+1:]
	}
	if err := t.store(ctx, l, false); err != nil {
		return false, err
	}
	return true, t.store(ctx, r, false)
}

// setSeparator replaces separator si of the parent. Unless forced, it
// refuses a separator with which the parent would not fit (R13-16).
func (t *Tree) setSeparator(parent *node, si int, key []byte, force bool) bool {
	old := parent.seps[si].Key
	parent.seps[si].Key = key
	if !force && parent.size() > page.Size {
		parent.seps[si].Key = old
		return false
	}
	return true
}

// merged writes l, which absorbed r, frees r and removes their separator.
func (t *Tree) merged(ctx context.Context, l, r, parent *node, si int) error {
	if err := t.store(ctx, l, false); err != nil {
		return err
	}
	parent.seps = slices.Delete(parent.seps, si, si+1)
	return t.alloc.Free(ctx, r.id)
}
