package codec

import (
	"encoding/binary"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

// Budget is charged for decoding work before memory is allocated, so that
// a small encoding cannot expand into a huge decoded value (SPEC §5.3).
type Budget interface {
	Debit(bytes int64) error
}

// DecodeOptions controls DecodeValue.
type DecodeOptions struct {
	Limits value.Limits
	Budget Budget // optional
}

// elementCost is the minimum charge per decoded element (SPEC §5.3).
const elementCost = 64

// EncodedSize returns the size of the G2 encoding of v.
func EncodedSize(v value.Value) int64 {
	switch v.Kind() {
	case value.KindBool:
		return 2
	case value.KindInt, value.KindFloat:
		return 9
	case value.KindString, value.KindBytes:
		return 5 + int64(v.Len())
	case value.KindVector:
		return 5 + 4*int64(v.Len())
	case value.KindList:
		n := int64(5)
		for i := 0; i < v.Len(); i++ {
			n += EncodedSize(v.Index(i))
		}
		return n
	case value.KindMap:
		n := int64(5)
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			n += 4 + int64(len(k)) + EncodedSize(item)
		}
		return n
	}
	return 1 // Null
}

// AppendValue validates v against lim and appends its G2 encoding to dst.
// Integers are little-endian; maps are written in key order, which makes
// the encoding canonical.
func AppendValue(dst []byte, v value.Value, lim value.Limits) ([]byte, error) {
	if err := value.Validate(v, lim); err != nil {
		return dst, err
	}
	size := EncodedSize(v)
	if size > int64(lim.MaxValueBytes) {
		return dst, errs.New(errs.CodeLimitExceeded, "codec.AppendValue", "encoded value too large").
			With("bytes", size).With("limit", int64(lim.MaxValueBytes))
	}
	dst = slices.Grow(dst, int(size))
	return appendValue(dst, v), nil
}

func appendValue(dst []byte, v value.Value) []byte {
	dst = append(dst, byte(v.Kind()))
	switch v.Kind() {
	case value.KindBool:
		if b, _ := v.AsBool(); b {
			return append(dst, 1)
		}
		return append(dst, 0)
	case value.KindInt:
		i, _ := v.AsInt()
		return binary.LittleEndian.AppendUint64(dst, uint64(i))
	case value.KindFloat:
		f, _ := v.AsFloat()
		return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f))
	case value.KindString, value.KindBytes:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		return v.AppendBytes(dst)
	case value.KindVector:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(v.VectorAt(i)))
		}
		return dst
	case value.KindList:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			dst = appendValue(dst, v.Index(i))
		}
		return dst
	case value.KindMap:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Len()))
		for i := 0; i < v.Len(); i++ {
			k, item := v.EntryAt(i)
			dst = binary.LittleEndian.AppendUint32(dst, uint32(len(k)))
			dst = append(dst, k...)
			dst = appendValue(dst, item)
		}
		return dst
	}
	return dst // Null
}

// DecodeValue decodes exactly one G2 value occupying all of b.
//
// Every length is checked against the remaining bytes and every allocation
// is charged to opt.Budget before it happens. Malformed or non-canonical
// input is CORRUPT; well-formed input above opt.Limits is LIMIT_EXCEEDED.
func DecodeValue(b []byte, opt DecodeOptions) (value.Value, error) {
	if len(b) > opt.Limits.MaxValueBytes {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, "codec.DecodeValue", "encoded value too large").
			With("bytes", int64(len(b))).With("limit", int64(opt.Limits.MaxValueBytes))
	}
	d := decoder{b: b, opt: opt}
	if err := d.charge(elementCost); err != nil {
		return value.Value{}, err
	}
	v, err := d.value(1)
	if err != nil {
		return value.Value{}, err
	}
	if d.off != len(b) {
		return value.Value{}, d.corrupt("%d trailing bytes", len(b)-d.off)
	}
	return v, nil
}

type decoder struct {
	b        []byte
	off      int
	opt      DecodeOptions
	elements int
}

const decodeOp = "codec.DecodeValue"

func (d *decoder) corrupt(format string, args ...any) error {
	return errs.Newf(errs.CodeCorrupt, decodeOp, "offset %d: "+format, append([]any{d.off}, args...)...)
}

func (d *decoder) charge(n int64) error {
	if d.opt.Budget == nil {
		return nil
	}
	return d.opt.Budget.Debit(n)
}

func (d *decoder) remaining() int { return len(d.b) - d.off }

func (d *decoder) u8() (byte, error) {
	if d.remaining() < 1 {
		return 0, d.corrupt("truncated")
	}
	c := d.b[d.off]
	d.off++
	return c, nil
}

func (d *decoder) u32() (uint32, error) {
	if d.remaining() < 4 {
		return 0, d.corrupt("truncated")
	}
	x := binary.LittleEndian.Uint32(d.b[d.off:])
	d.off += 4
	return x, nil
}

func (d *decoder) u64() (uint64, error) {
	if d.remaining() < 8 {
		return 0, d.corrupt("truncated")
	}
	x := binary.LittleEndian.Uint64(d.b[d.off:])
	d.off += 8
	return x, nil
}

// span reserves n payload bytes after charging them to the budget.
func (d *decoder) span(n uint32) ([]byte, error) {
	if int64(n) > int64(d.remaining()) {
		return nil, d.corrupt("length %d exceeds the %d remaining bytes", n, d.remaining())
	}
	if err := d.charge(int64(n)); err != nil {
		return nil, err
	}
	s := d.b[d.off : d.off+int(n)]
	d.off += int(n)
	return s, nil
}

// count registers n new elements and charges them before any allocation.
func (d *decoder) count(n uint32, minBytesEach int) error {
	if int64(n)*int64(minBytesEach) > int64(d.remaining()) {
		return d.corrupt("%d elements cannot fit in the %d remaining bytes", n, d.remaining())
	}
	d.elements += int(n)
	if d.elements > d.opt.Limits.MaxValueElements {
		return errs.New(errs.CodeLimitExceeded, decodeOp, "too many elements").
			With("elements", int64(d.elements)).With("limit", int64(d.opt.Limits.MaxValueElements))
	}
	return d.charge(elementCost * int64(n))
}

func (d *decoder) value(depth int) (value.Value, error) {
	if depth > d.opt.Limits.MaxDepth {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, decodeOp, "value nested too deeply").
			With("depth", int64(depth)).With("limit", int64(d.opt.Limits.MaxDepth))
	}
	tag, err := d.u8()
	if err != nil {
		return value.Value{}, err
	}
	switch value.Kind(tag) {
	case value.KindNull:
		return value.Null(), nil
	case value.KindBool:
		c, err := d.u8()
		if err != nil {
			return value.Value{}, err
		}
		if c > 1 {
			return value.Value{}, d.corrupt("bool byte %d", c)
		}
		return value.Bool(c == 1), nil
	case value.KindInt:
		x, err := d.u64()
		return value.Int(int64(x)), err
	case value.KindFloat:
		x, err := d.u64()
		return value.Float(math.Float64frombits(x)), err
	case value.KindString, value.KindBytes:
		n, err := d.u32()
		if err != nil {
			return value.Value{}, err
		}
		s, err := d.span(n)
		if err != nil {
			return value.Value{}, err
		}
		if value.Kind(tag) == value.KindBytes {
			return value.Bytes(s), nil
		}
		if !utf8.Valid(s) {
			return value.Value{}, d.corrupt("string is not valid UTF-8")
		}
		return value.String(string(s)), nil
	case value.KindVector:
		return d.vector()
	case value.KindList:
		n, err := d.u32()
		if err != nil {
			return value.Value{}, err
		}
		if err := d.count(n, 1); err != nil {
			return value.Value{}, err
		}
		return value.MakeList(int(n), func(int) (value.Value, error) { return d.value(depth + 1) })
	case value.KindMap:
		return d.mapValue(depth)
	}
	d.off--
	return value.Value{}, d.corrupt("unknown tag 0x%02x", tag)
}

func (d *decoder) vector() (value.Value, error) {
	n, err := d.u32()
	if err != nil {
		return value.Value{}, err
	}
	if n == 0 || n > value.FormatMaxVectorDimensions {
		return value.Value{}, d.corrupt("vector dimension %d", n)
	}
	if int(n) > d.opt.Limits.MaxVectorDimensions {
		return value.Value{}, errs.New(errs.CodeLimitExceeded, decodeOp, "vector dimension above limit").
			With("dimensions", int64(n)).With("limit", int64(d.opt.Limits.MaxVectorDimensions))
	}
	raw, err := d.span(4 * n)
	if err != nil {
		return value.Value{}, err
	}
	return value.MakeVector(int(n), func(i int) float32 {
		return math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}), nil
}

func (d *decoder) mapValue(depth int) (value.Value, error) {
	n, err := d.u32()
	if err != nil {
		return value.Value{}, err
	}
	if err := d.count(n, 5); err != nil { // key length + tag
		return value.Value{}, err
	}
	var prev string
	return value.MakeMap(int(n), func(i int) (value.Entry, error) {
		klen, err := d.u32()
		if err != nil {
			return value.Entry{}, err
		}
		if int(klen) > d.opt.Limits.MaxMapKeyBytes {
			return value.Entry{}, errs.New(errs.CodeLimitExceeded, decodeOp, "map key too long").
				With("bytes", int64(klen)).With("limit", int64(d.opt.Limits.MaxMapKeyBytes))
		}
		raw, err := d.span(klen)
		if err != nil {
			return value.Entry{}, err
		}
		if !utf8.Valid(raw) {
			return value.Entry{}, d.corrupt("map key is not valid UTF-8")
		}
		key := string(raw)
		if i > 0 && key <= prev {
			return value.Entry{}, d.corrupt("map keys out of order or duplicated")
		}
		prev = key
		item, err := d.value(depth + 1)
		if err != nil {
			return value.Entry{}, err
		}
		return value.Entry{Key: key, Value: item}, nil
	})
}
