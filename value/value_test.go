package value_test

import (
	"errors"
	"math"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func TestZeroValueIsNull(t *testing.T) {
	var v value.Value
	if !v.IsNull() || v.Kind() != value.KindNull {
		t.Fatalf("zero Value is %v, want Null", v.Kind())
	}
}

func TestScalarAccessors(t *testing.T) {
	if b, ok := value.Bool(true).AsBool(); !ok || !b {
		t.Error("Bool(true).AsBool() failed")
	}
	if i, ok := value.Int(math.MinInt64).AsInt(); !ok || i != math.MinInt64 {
		t.Error("Int(MinInt64) did not round-trip")
	}
	negZero := math.Copysign(0, -1)
	if f, ok := value.Float(negZero).AsFloat(); !ok || math.Float64bits(f) != math.Float64bits(negZero) {
		t.Error("Float(-0) lost the sign")
	}
	payload := math.Float64frombits(0x7ff8000000000001)
	if f, _ := value.Float(payload).AsFloat(); math.Float64bits(f) != 0x7ff8000000000001 {
		t.Error("Float lost the NaN payload")
	}
	if s, ok := value.String("ação").AsString(); !ok || s != "ação" {
		t.Error("String did not round-trip")
	}
	if _, ok := value.Int(1).AsString(); ok {
		t.Error("AsString on an Int reported ok")
	}
}

// VAL-04 (input part): mutating the caller's slices after construction
// must not change the value.
func TestConstructorsCopyInputs(t *testing.T) {
	b := []byte{1, 2, 3}
	bv := value.Bytes(b)
	b[0] = 9
	if got, _ := bv.AsBytes(); got[0] != 1 {
		t.Error("Bytes shares memory with the caller")
	}
	got, _ := bv.AsBytes()
	got[1] = 9
	if again, _ := bv.AsBytes(); again[1] != 2 {
		t.Error("AsBytes exposes internal memory")
	}

	vec := []float32{1, 2}
	vv := value.Vector(vec)
	vec[0] = 9
	if vv.VectorAt(0) != 1 {
		t.Error("Vector shares memory with the caller")
	}

	items := []value.Value{value.Int(1), value.Int(2)}
	lv := value.List(items...)
	items[0] = value.Int(9)
	if i, _ := lv.Index(0).AsInt(); i != 1 {
		t.Error("List shares memory with the caller")
	}

	entries := []value.Entry{{Key: "a", Value: value.Int(1)}}
	mv, err := value.MapFromEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].Key = "z"
	if k, _ := mv.EntryAt(0); k != "a" {
		t.Error("MapFromEntries shares memory with the caller")
	}
}

func TestMapsAreSortedByKeyBytes(t *testing.T) {
	m := value.MapOf(map[string]value.Value{"b": value.Int(2), "a": value.Int(1), "B": value.Int(0)})
	var keys []string
	for i := 0; i < m.Len(); i++ {
		k, _ := m.EntryAt(i)
		keys = append(keys, k)
	}
	want := []string{"B", "a", "b"}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %q, want %q", keys, want)
		}
	}
	if v, ok := m.Lookup("a"); !ok || !value.Identical(v, value.Int(1)) {
		t.Error("Lookup(a) failed")
	}
	if _, ok := m.Lookup("c"); ok {
		t.Error("Lookup(c) found a missing key")
	}
}

func TestMapFromEntriesRejectsDuplicates(t *testing.T) {
	_, err := value.MapFromEntries([]value.Entry{{Key: "k"}, {Key: "k"}})
	if !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestMakeMapRequiresIncreasingKeys(t *testing.T) {
	keys := []string{"b", "a"}
	_, err := value.MakeMap(2, func(i int) (value.Entry, error) {
		return value.Entry{Key: keys[i]}, nil
	})
	if !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestIdenticalDistinguishesBits(t *testing.T) {
	cases := []struct {
		a, b value.Value
		want bool
	}{
		{value.Int(1), value.Float(1), false},
		{value.Float(0), value.Float(math.Copysign(0, -1)), false},
		{value.Float(math.NaN()), value.Float(math.NaN()), true},
		{value.String("x"), value.Bytes([]byte("x")), false},
		{value.List(value.Int(1)), value.List(value.Int(1)), true},
		{value.List(value.Int(1)), value.List(value.Int(1), value.Null()), false},
		{value.Vector([]float32{1}), value.Vector([]float32{1}), true},
	}
	for i, c := range cases {
		if got := value.Identical(c.a, c.b); got != c.want {
			t.Errorf("case %d: Identical(%v, %v) = %v, want %v", i, c.a, c.b, got, c.want)
		}
	}
}

func TestDebugString(t *testing.T) {
	v := value.MapOf(map[string]value.Value{
		"l": value.List(value.Null(), value.Bool(true), value.Bytes([]byte{0, 255})),
		"v": value.Vector([]float32{1, 0.5}),
	})
	want := `{"l": [null, true, 0x00ff], "v": vector[1 0.5]}`
	if got := v.String(); got != want {
		t.Fatalf("String() = %s, want %s", got, want)
	}
}
