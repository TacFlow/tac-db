package wal

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TacFlow/tacdb/internal/page"
)

var (
	update   = flag.Bool("update", false, "rewrite golden files")
	testUUID = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	fixedNow = func() time.Time { return time.Unix(1_700_000_000, 0) }
)

// goldenImages returns the byte-exact images of the format (R13-12): a
// segment header with a fixed clock, one group (BEGIN, PAGE, COMMIT) and
// a RESERVE_IDS record.
func goldenImages(t *testing.T) map[string][]byte {
	t.Helper()
	var hdr [SegmentHeaderSize]byte
	h := SegmentHeader{UUID: testUUID, Seq: 2, FirstLSN: 41, BaseCheckpointLSN: 17,
		CreatedUnixNs: fixedNow().UnixNano(), PrevValidEnd: 8192}
	h.Encode(hdr[:])

	img := new(page.Buf)
	if err := page.EncodeOverflow(img, 9, []byte("golden")); err != nil {
		t.Fatal(err)
	}
	page.HeaderOf(img).SetCommitID(12)
	page.Seal(img)
	b := Begin{Token: [16]byte{0xaa, 0xbb}, Parent: 11, CommitID: 12, DurableLSN: 40}
	var m Manifest
	m.AddBegin(&b)
	m.AddPage(9, img)
	st := State{CommitID: 12, NodeHWM: 1024, PageCount: 10, FreePageCount: 1, Parent: 11}
	st.Roots[0] = 9
	c := Commit{IntentCount: 1, PageCount: 1, State: st, Digest: m.Digest()}
	var group []byte
	group = AppendRecord(group, &Record{Type: TypeBegin, LSN: 41, GroupID: 5, CommitID: 12, Payload: b.Encode()})
	group = AppendRecord(group, &Record{Type: TypePage, LSN: 42, GroupID: 5, CommitID: 12, PrevLSN: 41, PageID: 9, Payload: img[:]})
	group = AppendRecord(group, &Record{Type: TypeCommit, LSN: 43, GroupID: 5, CommitID: 12, PrevLSN: 42, Payload: c.Encode()})

	rv := Reserve{Kind: KindEdge, HighWatermark: 4096}
	reserve := AppendRecord(nil, &Record{Type: TypeReserveIDs, LSN: 44, Payload: rv.Encode()})
	return map[string][]byte{"segment-header.bin": hdr[:], "group.bin": group, "reserve.bin": reserve}
}

func TestGoldenRecords(t *testing.T) {
	for name, got := range goldenImages(t) {
		path := filepath.Join("testdata", "golden", name)
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run `go test ./internal/wal -run TestGoldenRecords -update` once and review the files)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the golden file", name)
		}
	}
	// The golden group decodes back to the same values.
	g := goldenImages(t)["group.bin"]
	r, n, err := DecodeRecord(g)
	if err != nil || r.Type != TypeBegin || n != RecordHeaderSize+BeginSize {
		t.Fatalf("BEGIN: %+v, %d, %v", r, n, err)
	}
	if b, err := DecodeBegin(r.Payload); err != nil || b.DurableLSN != 40 {
		t.Fatalf("BEGIN payload: %+v, %v", b, err)
	}
}
