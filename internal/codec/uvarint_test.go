package codec

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestUvarintRoundTrip(t *testing.T) {
	for _, x := range []uint64{0, 1, 127, 128, 255, 1023, 1024, 16383, 16384, 1 << 24, math.MaxUint32, math.MaxUint64} {
		enc := AppendUvarint(nil, x)
		if len(enc) != UvarintLen(x) {
			t.Errorf("UvarintLen(%d) = %d, encoding has %d bytes", x, UvarintLen(x), len(enc))
		}
		got, n, err := ReadUvarint(append(enc, 0xAA)) // trailing byte is not consumed
		if err != nil || got != x || n != len(enc) {
			t.Errorf("ReadUvarint(%x) = %d, %d, %v; want %d, %d", enc, got, n, err, x, len(enc))
		}
	}
}

func TestUvarintLenBoundaries(t *testing.T) {
	cases := map[uint64]int{0: 1, 127: 1, 128: 2, 1024: 2, 16383: 2, 16384: 3, 1 << 24: 4, math.MaxUint64: 10}
	for x, want := range cases {
		if got := UvarintLen(x); got != want {
			t.Errorf("UvarintLen(%d) = %d, want %d", x, got, want)
		}
	}
}

func TestReadUvarintRejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":            nil,
		"truncated":        {0x80},
		"non-minimal zero": {0x80, 0x00},
		"non-minimal one":  {0x81, 0x00},
		"overflow":         {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02},
		"too long":         bytes.Repeat([]byte{0x80}, 11),
	}
	for name, b := range cases {
		if _, _, err := ReadUvarint(b); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}
