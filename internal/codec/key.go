package codec

import (
	"encoding/binary"

	"github.com/TacFlow/tacdb/internal/errs"
)

// MaxKeyLen is the maximum length of an encoded B+tree key (SPEC §7.3).
const MaxKeyLen = 1024

// Composite keys (SPEC §7.4) concatenate components whose byte order
// matches their logical order: unsigned integers are big-endian, and
// strings escape 0x00 as 0x00 0xFF and end with 0x00 0x00.

// AppendKeyUint8 appends v.
func AppendKeyUint8(dst []byte, v uint8) []byte { return append(dst, v) }

// AppendKeyUint16 appends v big-endian.
func AppendKeyUint16(dst []byte, v uint16) []byte { return binary.BigEndian.AppendUint16(dst, v) }

// AppendKeyUint32 appends v big-endian.
func AppendKeyUint32(dst []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(dst, v) }

// AppendKeyUint64 appends v big-endian.
func AppendKeyUint64(dst []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(dst, v) }

// AppendKeyString appends s escaped and terminated.
func AppendKeyString(dst []byte, s string) []byte {
	return append(AppendKeyStringPrefix(dst, s), 0x00, 0x00)
}

// AppendKeyStringPrefix appends s escaped but not terminated. The result is
// a prefix of the encoding of every string that starts with s, for prefix
// scans over a string component.
func AppendKeyStringPrefix(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, s[i])
		}
	}
	return dst
}

// CheckKey returns LIMIT_EXCEEDED if key is longer than MaxKeyLen. Callers
// check before any mutation (SPEC §7.4).
func CheckKey(key []byte) error {
	if len(key) > MaxKeyLen {
		return errs.New(errs.CodeLimitExceeded, "codec.CheckKey", "key too long").
			With("bytes", int64(len(key))).With("limit", MaxKeyLen)
	}
	return nil
}

// PrefixEnd returns the smallest key greater than every key that starts
// with prefix, or nil if there is none (the prefix is empty or all 0xFF).
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// KeyReader decodes the components of a composite key in order.
type KeyReader struct {
	b   []byte
	off int
}

// NewKeyReader returns a reader over key.
func NewKeyReader(key []byte) *KeyReader { return &KeyReader{b: key} }

// Len returns the number of unread bytes.
func (r *KeyReader) Len() int { return len(r.b) - r.off }

const keyOp = "codec.KeyReader"

func (r *KeyReader) fixed(n int) ([]byte, error) {
	if r.Len() < n {
		return nil, errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: truncated key", r.off)
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, nil
}

// ReadUint8 reads a uint8 component.
func (r *KeyReader) ReadUint8() (uint8, error) {
	s, err := r.fixed(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

// ReadUint16 reads a big-endian uint16 component.
func (r *KeyReader) ReadUint16() (uint16, error) {
	s, err := r.fixed(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(s), nil
}

// ReadUint32 reads a big-endian uint32 component.
func (r *KeyReader) ReadUint32() (uint32, error) {
	s, err := r.fixed(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(s), nil
}

// ReadUint64 reads a big-endian uint64 component.
func (r *KeyReader) ReadUint64() (uint64, error) {
	s, err := r.fixed(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(s), nil
}

// ReadString reads an escaped, terminated string component.
func (r *KeyReader) ReadString() (string, error) {
	var out []byte
	for {
		if r.Len() == 0 {
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: unterminated string", r.off)
		}
		c := r.b[r.off]
		if c != 0x00 {
			out = append(out, c)
			r.off++
			continue
		}
		if r.Len() < 2 {
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: unterminated string", r.off)
		}
		switch r.b[r.off+1] {
		case 0x00:
			r.off += 2
			return string(out), nil
		case 0xFF:
			out = append(out, 0x00)
			r.off += 2
		default:
			return "", errs.Newf(errs.CodeCorrupt, keyOp, "offset %d: invalid escape 0x00 0x%02x", r.off, r.b[r.off+1])
		}
	}
}
