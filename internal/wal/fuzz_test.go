package wal

import (
	"bytes"
	"testing"

	"github.com/TacFlow/tacdb/internal/vfs"
)

// Target names must not be substrings of each other within the package.

// FuzzRecordDecode decodes arbitrary bytes as a record; one that decodes
// re-encodes to the same bytes.
func FuzzRecordDecode(f *testing.F) {
	for _, img := range goldenImages(&testing.T{}) {
		f.Add(img)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, n, err := DecodeRecord(data)
		if err != nil {
			return
		}
		if again := AppendRecord(nil, &r); !bytes.Equal(again, data[:n]) {
			t.Fatal("a decoded record does not re-encode to the same bytes")
		}
		switch r.Type {
		case TypeBegin:
			DecodeBegin(r.Payload)
		case TypeCommit:
			DecodeCommit(r.Payload)
		case TypeReserveIDs:
			DecodeReserve(r.Payload)
		}
	})
}

// FuzzSegmentScan changes up to 16 bytes of a valid log of two segments
// and scans it: errors are fine, a panic is not, and a scan that succeeds
// returns a prefix of the original commits.
func FuzzSegmentScan(f *testing.F) {
	f.Add([]byte{0, 0, 0x20, 0x10, 0x01, 0xff})
	f.Add([]byte{1, 0x10, 0x80, 0x00})
	f.Fuzz(func(t *testing.T, edits []byte) {
		fs := vfs.NewMemFS()
		w := newWriter(t, fs, 16<<10)
		var c chain
		for i := 0; i < 6; i++ {
			if _, err := w.AppendGroup(c.next(t, 3, 4)); err != nil {
				t.Fatal(err)
			}
			if i%2 == 1 {
				w.Sync()
			}
		}
		w.Sync()
		w.Close()
		for i := 0; i+4 <= len(edits) && i < 16*4; i += 4 {
			seq := uint64(1 + edits[i]%2)
			off := int64(edits[i+1])<<8 | int64(edits[i+2])
			name := dir + "/" + SegmentName(seq)
			if ok, _ := fs.Exists(name); !ok {
				continue
			}
			f, err := fs.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if f.ReadAt(b[:], off) == nil {
				b[0] ^= edits[i+3] | 1
				f.WriteAt(b[:], off)
				f.Sync()
			}
			f.Close()
		}
		// The first byte also cuts the last segment to any size, aligned or not.
		if len(edits) > 0 && edits[0]%4 == 0 {
			name := dir + "/" + SegmentName(2)
			if f, err := fs.Open(name); err == nil {
				if size, err := f.Size(); err == nil {
					f.Truncate(max(0, size-int64(edits[0])*37))
					f.Sync()
				}
				f.Close()
			}
		}
		res, err := scanFS(fs)
		if err != nil {
			return
		}
		for i, g := range res.Groups {
			if g.Commit.State.CommitID != uint64(i+1) {
				t.Fatalf("recovered commit %d at position %d", g.Commit.State.CommitID, i)
			}
		}
	})
}
