// Package valuetest generates values for tests. It favors edge cases:
// NaN payloads, signed zeros, integers around 2^53, multibyte UTF-8, NUL
// bytes and nesting.
package valuetest

import (
	"math"
	"math/rand/v2"

	"github.com/TacFlow/tacdb/value"
)

var specialFloats = []float64{
	0, math.Copysign(0, -1), 1, -1, 0.5, math.Inf(1), math.Inf(-1), math.NaN(),
	math.Float64frombits(0x7ff8000000000001), // NaN with payload
	math.MaxFloat64, math.SmallestNonzeroFloat64,
	1 << 53, 1<<53 + 2, -(1 << 53), 0x1p63, -0x1p63, 0x1p64,
}

var specialInts = []int64{
	0, 1, -1, math.MaxInt64, math.MinInt64, 1 << 53, 1<<53 + 1, -(1<<53 + 1),
	math.MaxInt32, math.MinInt32,
}

var specialStrings = []string{"", "a", "ação", "a\x00b", "日本語", "\U0001F600", "zzz"}

// Value returns a random valid value with nesting depth at most maxDepth.
func Value(r *rand.Rand, maxDepth int) value.Value {
	kinds := 7
	if maxDepth > 1 {
		kinds = 9
	}
	switch r.IntN(kinds) {
	case 0:
		return value.Null()
	case 1:
		return value.Bool(r.IntN(2) == 1)
	case 2:
		if r.IntN(2) == 0 {
			return value.Int(specialInts[r.IntN(len(specialInts))])
		}
		return value.Int(int64(r.Uint64()))
	case 3:
		if r.IntN(2) == 0 {
			return value.Float(specialFloats[r.IntN(len(specialFloats))])
		}
		return value.Float(r.NormFloat64() * 1e6)
	case 4:
		if r.IntN(2) == 0 {
			return value.String(specialStrings[r.IntN(len(specialStrings))])
		}
		return value.String(randomString(r))
	case 5:
		b := make([]byte, r.IntN(12))
		for i := range b {
			b[i] = byte(r.IntN(3)) * 0x7f // 0x00, 0x7f, 0xfe
		}
		return value.Bytes(b)
	case 6:
		v := make([]float32, 1+r.IntN(5))
		for i := range v {
			if r.IntN(4) == 0 {
				v[i] = float32(specialFloats[r.IntN(len(specialFloats))])
			} else {
				v[i] = float32(r.NormFloat64())
			}
		}
		return value.Vector(v)
	case 7:
		items := make([]value.Value, r.IntN(4))
		for i := range items {
			items[i] = Value(r, maxDepth-1)
		}
		return value.List(items...)
	default:
		m := make(map[string]value.Value)
		for range r.IntN(4) {
			m[randomString(r)] = Value(r, maxDepth-1)
		}
		return value.MapOf(m)
	}
}

func randomString(r *rand.Rand) string {
	runes := []rune{'a', 'b', 'z', 'ç', 'é', 0, '日', '\U0001F600'}
	out := make([]rune, r.IntN(6))
	for i := range out {
		out[i] = runes[r.IntN(len(runes))]
	}
	return string(out)
}
