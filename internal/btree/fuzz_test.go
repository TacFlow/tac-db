package btree_test

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/TacFlow/tacdb/internal/btree/btreetest"
	"github.com/TacFlow/tacdb/internal/page"
	"github.com/TacFlow/tacdb/internal/vfs"
)

// Target names must not be substrings of each other within the package.

// FuzzBTreeOps runs operations decoded from the input against the model,
// then verifies the tree and checks the published state.
func FuzzBTreeOps(f *testing.F) {
	f.Add([]byte{0, 3, 1, 0, 9, 6, 3, 2})
	f.Add(bytes.Repeat([]byte{1, 200, 5, 255}, 40))
	prefixes := []string{"", "k/", strings.Repeat("p", 900)}
	f.Fuzz(func(t *testing.T, data []byte) {
		e := newEnv(t)
		for len(data) >= 4 && len(e.model) < 400 {
			op, kb, vb := data[0]%10, data[1], int(data[2])<<8|int(data[3])
			data = data[4:]
			key := fmt.Appendf([]byte(prefixes[int(kb)%len(prefixes)]), "%d", kb)
			switch {
			case op < 6:
				e.put(key, bytes.Repeat([]byte{kb}, vb%6000))
			case op < 9:
				e.del(key)
			default:
				e.get(key)
			}
		}
		e.check()
		e.commit()
		e.structural()
	})
}

var pristine struct {
	once  sync.Once
	image []byte
	pages int
}

// pristineImage is the file of forgedStore, built once.
func pristineImage(t *testing.T) ([]byte, int) {
	pristine.once.Do(func() {
		fs, _ := forgedStore(t)
		f, err := fs.Open(btreetest.FileName)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		size, _ := f.Size()
		pristine.image = make([]byte, size)
		if err := f.ReadAt(pristine.image, 0); err != nil {
			t.Fatal(err)
		}
		pristine.pages = int(size / page.Size)
	})
	return pristine.image, pristine.pages
}

// FuzzForgedTree changes up to 16 bytes of one page of a valid tree and
// recomputes its CRC, as a forged file would (SPEC §20.11), then reads,
// walks, verifies and checks the tree: errors are fine, a panic, a hang or
// a pin left behind is not (BT-02).
func FuzzForgedTree(f *testing.F) {
	f.Add(uint16(3), []byte{0, 24, 9})
	f.Add(uint16(5), []byte{0, 40, 0x10, 0, 60, 0xff})
	f.Add(uint16(8), []byte{0, 32, 1})
	f.Fuzz(func(t *testing.T, which uint16, edits []byte) {
		image, pages := pristineImage(t)
		id := page.ID(2 + int(which)%(pages-2))
		forged := bytes.Clone(image)
		var b page.Buf
		copy(b[:], forged[int(id)*page.Size:])
		for i := 0; i+3 <= len(edits) && i < 16*3; i += 3 {
			b[(int(edits[i])<<8|int(edits[i+1]))%page.Size] = edits[i+2]
		}
		page.Seal(&b)
		copy(forged[int(id)*page.Size:], b[:])
		fs := vfs.NewMemFS()
		file, err := fs.Create(btreetest.FileName)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.WriteAt(forged, 0); err != nil {
			t.Fatal(err)
		}
		file.Close()
		exerciseForged(t, fs)
	})
}
