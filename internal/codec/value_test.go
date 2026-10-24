package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

func defaultOpts() DecodeOptions { return DecodeOptions{Limits: value.DefaultLimits()} }

func mustEncode(t *testing.T, v value.Value) []byte {
	t.Helper()
	b, err := AppendValue(nil, v, value.DefaultLimits())
	if err != nil {
		t.Fatalf("AppendValue(%v): %v", v, err)
	}
	return b
}

// VAL-01 and VAL-03: every kind round-trips bit for bit.
func TestRoundTripPreservesBits(t *testing.T) {
	vals := []value.Value{
		value.Null(), value.Bool(false), value.Bool(true),
		value.Int(math.MaxInt64), value.Int(math.MinInt64), value.Int(1<<53 + 1),
		value.Float(math.Copysign(0, -1)), value.Float(math.Inf(-1)),
		value.Float(math.Float64frombits(0x7ff8000000000001)),
		value.String(""), value.String("a\x00ção"),
		value.Bytes(nil), value.Bytes([]byte{0, 0xff, 0}),
		value.Vector([]float32{float32(math.NaN()), float32(math.Copysign(0, -1)), 1}),
		value.List(), value.List(value.Null(), value.List(value.Int(1))),
		value.MapOf(map[string]value.Value{"": value.Null(), "k": value.MapOf(map[string]value.Value{"x": value.Bool(true)})}),
	}
	for _, v := range vals {
		enc := mustEncode(t, v)
		if int64(len(enc)) != EncodedSize(v) {
			t.Errorf("EncodedSize(%v) = %d, encoding has %d bytes", v, EncodedSize(v), len(enc))
		}
		got, err := DecodeValue(enc, defaultOpts())
		if err != nil {
			t.Fatalf("DecodeValue(%v): %v", v, err)
		}
		if !value.Identical(got, v) {
			t.Errorf("round trip of %v gave %v", v, got)
		}
	}
}

func TestRandomRoundTripIsCanonical(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < 2000; i++ {
		v := valuetest.Value(r, 4)
		enc := mustEncode(t, v)
		got, err := DecodeValue(enc, defaultOpts())
		if err != nil {
			t.Fatalf("DecodeValue(%v): %v", v, err)
		}
		if !value.Identical(got, v) {
			t.Fatalf("round trip of %v gave %v", v, got)
		}
		if again := mustEncode(t, got); !bytes.Equal(again, enc) {
			t.Fatalf("re-encoding %v changed the bytes", v)
		}
	}
}

func TestAppendValueValidates(t *testing.T) {
	if _, err := AppendValue(nil, value.String("\xff"), value.DefaultLimits()); !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Errorf("invalid UTF-8: err = %v, want INVALID_ARGUMENT", err)
	}
	lim := value.DefaultLimits()
	lim.MaxValueBytes = 10
	if _, err := AppendValue(nil, value.String("0123456789"), lim); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Errorf("15-byte encoding with limit 10: err = %v, want LIMIT_EXCEEDED", err)
	}
}

func u32(n uint32) []byte { return binary.LittleEndian.AppendUint32(nil, n) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestDecodeRejectsMalformed(t *testing.T) {
	corrupt := errs.Sentinel(errs.CodeCorrupt)
	cases := map[string][]byte{
		"empty":              nil,
		"unknown tag":        {0x09},
		"bool 2":             {0x01, 0x02},
		"truncated int":      {0x02, 1, 2, 3},
		"trailing byte":      {0x00, 0x00},
		"string past end":    cat([]byte{0x04}, u32(5), []byte("abc")),
		"invalid UTF-8":      cat([]byte{0x04}, u32(1), []byte{0xff}),
		"vector dimension 0": cat([]byte{0x06}, u32(0)),
		"vector too wide":    cat([]byte{0x06}, u32(65536)),
		"list count lies":    cat([]byte{0x07}, u32(3), []byte{0x00}),
		"map out of order": cat([]byte{0x08}, u32(2),
			u32(1), []byte("b"), []byte{0x00},
			u32(1), []byte("a"), []byte{0x00}),
		"map duplicate key": cat([]byte{0x08}, u32(2),
			u32(1), []byte("a"), []byte{0x00},
			u32(1), []byte("a"), []byte{0x00}),
		"map key bad UTF-8": cat([]byte{0x08}, u32(1), u32(1), []byte{0xc3}, []byte{0x00}),
	}
	for name, b := range cases {
		if _, err := DecodeValue(b, defaultOpts()); !errors.Is(err, corrupt) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}

func TestDecodeRejectsAboveLimits(t *testing.T) {
	limitExceeded := errs.Sentinel(errs.CodeLimitExceeded)
	deep := []byte{}
	for i := 0; i < 64; i++ {
		deep = append(deep, cat([]byte{0x07}, u32(1))...)
	}
	deep = append(deep, 0x00) // 65 levels
	bigKey := strings.Repeat("k", 64<<10+1)
	cases := map[string][]byte{
		"depth 65":           deep,
		"vector above 4096":  cat([]byte{0x06}, u32(4097), make([]byte, 4*4097)),
		"map key above 64 K": cat([]byte{0x08}, u32(1), u32(uint32(len(bigKey))), []byte(bigKey), []byte{0x00}),
	}
	for name, b := range cases {
		if _, err := DecodeValue(b, defaultOpts()); !errors.Is(err, limitExceeded) {
			t.Errorf("%s: err = %v, want LIMIT_EXCEEDED", name, err)
		}
	}
}

type countingBudget struct {
	debited, limit int64
}

func (b *countingBudget) Debit(n int64) error {
	b.debited += n
	if b.debited > b.limit {
		return errs.New(errs.CodeResourceLimit, "test.budget", "exhausted").With("limit", b.limit)
	}
	return nil
}

// VAL-05: 16 MiB of minimal elements is rejected before the element slice
// is allocated, both by MaxValueElements and by the budget.
func TestDecodeChargesBeforeAllocating(t *testing.T) {
	const n = 16<<20 - 5
	nulls := cat([]byte{0x07}, u32(n), make([]byte, n))

	if _, err := DecodeValue(nulls, defaultOpts()); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Fatalf("16M nulls with default limits: err = %v, want LIMIT_EXCEEDED", err)
	}

	opts := defaultOpts()
	opts.Limits.MaxValueElements = math.MaxInt32
	budget := &countingBudget{limit: 1 << 20}
	opts.Budget = budget
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := DecodeValue(nulls, opts)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errs.Sentinel(errs.CodeResourceLimit)) {
		t.Fatalf("budget of 1 MiB: err = %v, want RESOURCE_LIMIT", err)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 1<<20 {
		t.Fatalf("decoder allocated %d bytes before failing", grown)
	}
}
