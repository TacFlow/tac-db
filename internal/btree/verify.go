package btree

import (
	"bytes"
	"context"

	"github.com/TacFlow/tacdb/internal/page"
)

// Report describes a tree checked by Verify.
type Report struct {
	Pages     []page.ID // every page the tree owns: tree pages, overflow pages and descriptors
	Height    int
	Leaves    int
	Internals int
	Keys      int
	// MinFanout is the smallest number of children of an internal page
	// other than the root; 0 when there is none.
	MinFanout int
}

// Verify checks the structure of the tree rooted at root (SPEC §8.1): the
// order of the keys, the ranges of the children and the separator rule
// max(L) < s <= min(R), levels, the leaf chain against the traversal,
// canonical forms, the extents of every value and the pages they cover,
// and that no page appears twice. It returns the pages the tree owns, for
// the global checker.
func Verify(ctx context.Context, r page.Reader, pins page.Pins, root page.ID) (Report, error) {
	v := &verifier{r: r, pins: pins, pc: r.PageCount(), seen: make(map[page.ID]bool)}
	if root == 0 {
		return v.rep, nil
	}
	height, err := v.walk(ctx, root, -1, nil, nil, 0, true)
	if err != nil {
		return v.rep, err
	}
	v.rep.Height = height
	for i, l := range v.leaves {
		want := page.ID(0)
		if i+1 < len(v.leaves) {
			want = v.leaves[i+1].id
		}
		if l.next != want {
			return v.rep, corrupt(l.id, "the next leaf is %d, the traversal gives %d", l.next, want)
		}
	}
	return v.rep, nil
}

type verifier struct {
	r       page.Reader
	pins    page.Pins
	pc      uint64
	seen    map[page.ID]bool
	visited int // pages read, for cancellation checks
	leaves  []leafLink
	rep     Report
}

type leafLink struct{ id, next page.ID }

// page reads page id and checks it with page.Verify and page.Canonical. A
// private copy has no CRC yet, so the check runs on a sealed copy.
func (v *verifier) page(ctx context.Context, id page.ID) (*page.Buf, error) {
	if v.visited++; v.visited%256 == 0 {
		if err := ctxErr(ctx, "btree.Verify"); err != nil {
			return nil, err
		}
	}
	ref, err := v.r.Page(ctx, v.pins, id)
	if err != nil {
		return nil, err
	}
	b := *ref.Bytes()
	ref.Release()
	page.Seal(&b)
	if err := page.Verify(&b, id, v.pc); err != nil {
		return nil, err
	}
	if err := page.Canonical(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (v *verifier) own(id page.ID) error {
	if v.seen[id] {
		return corrupt(id, "the page appears twice in the tree")
	}
	v.seen[id] = true
	v.rep.Pages = append(v.rep.Pages, id)
	return nil
}

// walk checks the subtree of id, whose keys lie in [lo, hi) (nil bounds are
// open), and returns its height.
func (v *verifier) walk(ctx context.Context, id page.ID, level int, lo, hi []byte, depth int, root bool) (int, error) {
	if depth >= page.MaxTreeHeight {
		return 0, corrupt(id, "the tree is deeper than %d levels", page.MaxTreeHeight)
	}
	if err := page.CheckPointer(id, v.pc); err != nil {
		return 0, err
	}
	if err := v.own(id); err != nil {
		return 0, err
	}
	b, err := v.page(ctx, id)
	if err != nil {
		return 0, err
	}
	h := page.HeaderOf(b)
	if h.Type() != page.TypeLeaf && h.Type() != page.TypeInternal {
		return 0, corrupt(id, "a tree page of type %d", h.Type())
	}
	if level >= 0 && int(h.Level()) != level {
		return 0, corrupt(id, "level %d where %d was expected", h.Level(), level)
	}
	n := page.NodeOf(b)
	keys := make([][]byte, n.Len())
	for i := range keys {
		keys[i] = n.AppendKey(nil, i)
		if i > 0 && bytes.Compare(keys[i-1], keys[i]) >= 0 {
			return 0, corrupt(id, "keys out of order at cell %d", i)
		}
	}
	if n.Len() > 0 && (lo != nil && bytes.Compare(keys[0], lo) < 0 || hi != nil && bytes.Compare(keys[n.Len()-1], hi) >= 0) {
		return 0, corrupt(id, "a key outside the range given by the parent")
	}
	if h.Type() == page.TypeLeaf {
		v.rep.Leaves++
		v.rep.Keys += n.Len()
		v.leaves = append(v.leaves, leafLink{id: id, next: h.Sibling()})
		for i := 0; i < n.Len(); i++ {
			if err := v.value(ctx, n.Value(i)); err != nil {
				return 0, err
			}
		}
		return 1, nil
	}
	v.rep.Internals++
	if fan := n.Len() + 1; !root && (v.rep.MinFanout == 0 || fan < v.rep.MinFanout) {
		v.rep.MinFanout = fan
	}
	height := 0
	for i := 0; i <= n.Len(); i++ {
		clo, chi := lo, hi
		if i > 0 {
			clo = keys[i-1]
		}
		if i < n.Len() {
			chi = keys[i]
		}
		hgt, err := v.walk(ctx, n.Child(i), int(h.Level())-1, clo, chi, depth+1, false)
		if err != nil {
			return 0, err
		}
		height = hgt + 1
	}
	return height, nil
}

// value checks a value stored out of the page: its extents, and each
// overflow page and descriptor, which must be canonical and belong to this
// value only.
func (v *verifier) value(ctx context.Context, lv page.LeafValue) error {
	if !lv.Out() {
		return nil
	}
	if lv.Descriptor != 0 {
		if err := v.own(lv.Descriptor); err != nil {
			return err
		}
		if _, err := v.page(ctx, lv.Descriptor); err != nil {
			return err
		}
	}
	exts, err := extentsOf(ctx, v.r, v.pins, &lv)
	if err != nil {
		return err
	}
	if lv.Descriptor != 0 && len(exts) <= page.MaxInline {
		return corrupt(lv.Descriptor, "a descriptor for %d extents", len(exts))
	}
	left := lv.Len
	for _, e := range exts {
		for p := e.Start; p < e.Start+page.ID(e.Pages); p++ {
			if err := v.own(p); err != nil {
				return err
			}
			b, err := v.page(ctx, p)
			if err != nil {
				return err
			}
			want := min(left, page.Usable)
			if h := page.HeaderOf(b); h.Type() != page.TypeOverflow || uint64(h.Count()) != want {
				return corrupt(p, "not an overflow page of %d useful bytes", want)
			}
			left -= want
		}
	}
	return nil
}
