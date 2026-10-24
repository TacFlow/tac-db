package value

import (
	"math"
	"strings"
)

// family orders kinds for Compare (SPEC §5.3): Bool < number < String <
// Bytes < Vector < List < Map < Null.
func family(k Kind) int {
	switch k {
	case KindBool:
		return 0
	case KindInt, KindFloat:
		return 1
	case KindString:
		return 2
	case KindBytes:
		return 3
	case KindVector:
		return 4
	case KindList:
		return 5
	case KindMap:
		return 6
	}
	return 7 // Null
}

// Compare returns -1, 0 or +1 ordering a before, equal to or after b in the
// shared total order of SPEC §5.3, used by ORDER BY, DISTINCT and
// aggregation.
//
// Int and Float compare by exact numeric value, never converting an int64
// to float64. NaN orders after every other number and equals any NaN; -0
// equals +0. Compare is a total preorder: Compare == 0 does not imply
// Identical (Int(1) and Float(1) compare equal). Null orders last, so it
// comes last in ascending order and first in descending order.
func Compare(a, b Value) int {
	fa, fb := family(a.kind), family(b.kind)
	if fa != fb {
		return cmpInt(fa, fb)
	}
	switch a.kind {
	case KindNull:
		return 0
	case KindBool:
		return cmpInt(int(a.num), int(b.num))
	case KindInt, KindFloat:
		return compareNumbers(a, b)
	case KindString, KindBytes:
		return strings.Compare(a.str, b.str)
	case KindVector:
		x, y := a.ref.([]float32), b.ref.([]float32)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := compareFloats(float64(x[i]), float64(y[i])); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case KindList:
		x, y := a.ref.([]Value), b.ref.([]Value)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := Compare(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	case KindMap:
		x, y := a.ref.([]Entry), b.ref.([]Entry)
		for i := 0; i < len(x) && i < len(y); i++ {
			if c := strings.Compare(x[i].Key, y[i].Key); c != 0 {
				return c
			}
			if c := Compare(x[i].Value, y[i].Value); c != 0 {
				return c
			}
		}
		return cmpInt(len(x), len(y))
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareNumbers(a, b Value) int {
	switch {
	case a.kind == KindInt && b.kind == KindInt:
		x, y := int64(a.num), int64(b.num)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case a.kind == KindFloat && b.kind == KindFloat:
		return compareFloats(math.Float64frombits(a.num), math.Float64frombits(b.num))
	case a.kind == KindInt:
		return compareIntFloat(int64(a.num), math.Float64frombits(b.num))
	default:
		return -compareIntFloat(int64(b.num), math.Float64frombits(a.num))
	}
}

// compareFloats orders NaN after every number and treats -0 as +0.
func compareFloats(x, y float64) int {
	xn, yn := math.IsNaN(x), math.IsNaN(y)
	switch {
	case xn && yn:
		return 0
	case xn:
		return 1
	case yn:
		return -1
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// compareIntFloat compares i and f exactly.
func compareIntFloat(i int64, f float64) int {
	switch {
	case math.IsNaN(f):
		return -1
	case f >= 0x1p63:
		return -1
	case f < -0x1p63:
		return 1
	}
	// -2^63 <= f < 2^63, so the truncation fits in an int64 and is exact.
	t := int64(f)
	switch {
	case i < t:
		return -1
	case i > t:
		return 1
	}
	// i == trunc(f); the fractional part of a float64 is exact.
	frac := f - float64(t)
	switch {
	case frac > 0:
		return -1
	case frac < 0:
		return 1
	}
	return 0
}
