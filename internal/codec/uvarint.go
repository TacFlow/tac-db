// Package codec implements the G2 encodings that do not depend on pages:
// minimal uvarints, the Value codec (SPEC §5.3) and composite B+tree keys
// (SPEC §7.4).
package codec

import (
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/errs"
)

// UvarintLen returns the size of the minimal uvarint encoding of x.
func UvarintLen(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}

// AppendUvarint appends the minimal uvarint (LEB128) encoding of x.
func AppendUvarint(dst []byte, x uint64) []byte {
	return binary.AppendUvarint(dst, x)
}

// ReadUvarint decodes a uvarint from the start of b and returns it with the
// number of bytes read. Truncated, overflowing and non-minimal encodings
// are CORRUPT (SPEC §7.1).
func ReadUvarint(b []byte) (uint64, int, error) {
	const op = "codec.ReadUvarint"
	var x uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == binary.MaxVarintLen64-1 && c > 1 {
			return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint overflows 64 bits")
		}
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint is not in minimal form")
			}
			return x | uint64(c)<<shift, i + 1, nil
		}
		x |= uint64(c&0x7f) << shift
		shift += 7
	}
	return 0, 0, errs.New(errs.CodeCorrupt, op, "uvarint is truncated")
}
