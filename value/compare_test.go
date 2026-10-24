package value_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

func TestCompareFixedCases(t *testing.T) {
	nan := value.Float(math.NaN())
	cases := []struct {
		name string
		a, b value.Value
		want int
	}{
		{"family Bool<number", value.Bool(true), value.Int(-5), -1},
		{"family number<String", value.Float(math.Inf(1)), value.String(""), -1},
		{"family String<Bytes", value.String("z"), value.Bytes(nil), -1},
		{"family Bytes<Vector", value.Bytes([]byte{255}), value.Vector([]float32{-1}), -1},
		{"family Vector<List", value.Vector([]float32{9}), value.List(), -1},
		{"family List<Map", value.List(value.Int(9)), value.MapOf(nil), -1},
		{"family Map<Null", value.MapOf(nil), value.Null(), -1},
		{"Null equals Null", value.Null(), value.Null(), 0},
		{"false<true", value.Bool(false), value.Bool(true), -1},
		{"Int exact above 2^53", value.Int(1<<53 + 1), value.Float(1 << 53), 1},
		{"Int exact below 2^53", value.Float(1<<53 + 2), value.Int(1<<53 + 1), 1},
		{"Int equals Float", value.Int(3), value.Float(3), 0},
		{"Int below fraction", value.Int(2), value.Float(2.5), -1},
		{"negative fraction", value.Int(-2), value.Float(-2.5), 1},
		{"MaxInt64 below 2^63", value.Int(math.MaxInt64), value.Float(0x1p63), -1},
		{"MinInt64 equals -2^63", value.Int(math.MinInt64), value.Float(-0x1p63), 0},
		{"-Inf below MinInt64", value.Float(math.Inf(-1)), value.Int(math.MinInt64), -1},
		{"NaN after +Inf", nan, value.Float(math.Inf(1)), 1},
		{"NaN after Int", nan, value.Int(math.MaxInt64), 1},
		{"NaN equals NaN", nan, value.Float(math.Float64frombits(0x7ff8000000000001)), 0},
		{"NaN before String", nan, value.String(""), -1},
		{"-0 equals +0", value.Float(math.Copysign(0, -1)), value.Float(0), 0},
		{"String by bytes", value.String("B"), value.String("a"), -1},
		{"String prefix first", value.String("a"), value.String("a\x00"), -1},
		{"List prefix first", value.List(value.Int(1)), value.List(value.Int(1), value.Null()), -1},
		{"List element order", value.List(value.Int(2)), value.List(value.Int(1), value.Int(9)), 1},
		{"Map key first", value.MapOf(map[string]value.Value{"a": value.Int(9)}),
			value.MapOf(map[string]value.Value{"b": value.Int(0)}), -1},
		{"Map value second", value.MapOf(map[string]value.Value{"a": value.Int(1)}),
			value.MapOf(map[string]value.Value{"a": value.Int(2)}), -1},
		{"Vector NaN component last", value.Vector([]float32{float32(math.NaN())}),
			value.Vector([]float32{float32(math.Inf(1))}), 1},
	}
	for _, c := range cases {
		if got := value.Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare(%v, %v) = %d, want %d", c.name, c.a, c.b, got, c.want)
		}
		if got := value.Compare(c.b, c.a); got != -c.want {
			t.Errorf("%s: reversed Compare(%v, %v) = %d, want %d", c.name, c.b, c.a, got, -c.want)
		}
	}
}

// Compare must be a total preorder: reflexive, antisymmetric in sign and
// transitive.
func TestCompareIsTotalPreorder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	vals := make([]value.Value, 300)
	for i := range vals {
		vals[i] = valuetest.Value(r, 3)
	}
	for _, a := range vals {
		if value.Compare(a, a) != 0 {
			t.Fatalf("Compare(%v, itself) != 0", a)
		}
	}
	for _, a := range vals {
		for _, b := range vals {
			ab, ba := value.Compare(a, b), value.Compare(b, a)
			if ab != -ba {
				t.Fatalf("Compare(%v,%v)=%d but Compare(%v,%v)=%d", a, b, ab, b, a, ba)
			}
		}
	}
	for i := 0; i < 20000; i++ {
		a, b, c := vals[r.IntN(len(vals))], vals[r.IntN(len(vals))], vals[r.IntN(len(vals))]
		if value.Compare(a, b) <= 0 && value.Compare(b, c) <= 0 && value.Compare(a, c) > 0 {
			t.Fatalf("not transitive: %v <= %v <= %v but %v > %v", a, b, c, a, c)
		}
	}
}
