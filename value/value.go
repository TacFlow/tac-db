// Package value defines the typed values that tacdb stores in properties
// (SPEC §5). A Value is immutable: constructors copy their inputs and
// accessors never expose memory the caller could mutate.
package value

import (
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Kind is the type of a Value. Its numeric value is the G2 tag.
type Kind uint8

// Kinds, in tag order.
const (
	KindNull   Kind = 0x00
	KindBool   Kind = 0x01
	KindInt    Kind = 0x02
	KindFloat  Kind = 0x03
	KindString Kind = 0x04
	KindBytes  Kind = 0x05
	KindVector Kind = 0x06
	KindList   Kind = 0x07
	KindMap    Kind = 0x08
)

var kindNames = [...]string{"Null", "Bool", "Int", "Float", "String", "Bytes", "Vector", "List", "Map"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Value is an immutable typed value. The zero Value is Null.
//
// Values cannot be compared with == or used as map keys; use Compare or
// Identical.
type Value struct {
	_    [0]func() // makes == a compile error instead of a runtime panic
	kind Kind
	num  uint64 // Bool (0 or 1), Int (two's complement), Float (IEEE-754 bits)
	str  string // String and Bytes payload; a string keeps Bytes immutable
	ref  any    // []float32 (Vector), []Value (List) or []Entry (Map)
}

// Entry is a key/value pair of a Map.
type Entry struct {
	Key   string
	Value Value
}

// Null returns the Null value.
func Null() Value { return Value{} }

// Bool returns a Bool value.
func Bool(b bool) Value {
	if b {
		return Value{kind: KindBool, num: 1}
	}
	return Value{kind: KindBool}
}

// Int returns an Int value.
func Int(i int64) Value { return Value{kind: KindInt, num: uint64(i)} }

// Float returns a Float value. NaN payloads, infinities and the sign of
// zero are preserved.
func Float(f float64) Value { return Value{kind: KindFloat, num: math.Float64bits(f)} }

// String returns a String value. Validate checks that s is valid UTF-8.
func String(s string) Value { return Value{kind: KindString, str: s} }

// Bytes returns a Bytes value holding a copy of b.
func Bytes(b []byte) Value { return Value{kind: KindBytes, str: string(b)} }

// Vector returns a Vector value holding a copy of v.
func Vector(v []float32) Value {
	return Value{kind: KindVector, ref: append([]float32(nil), v...)}
}

// List returns a List value holding a copy of items.
func List(items ...Value) Value {
	return Value{kind: KindList, ref: append([]Value(nil), items...)}
}

// MapOf returns a Map value with the entries of m, sorted by key bytes.
func MapOf(m map[string]Value) Value {
	entries := make([]Entry, 0, len(m))
	for k, v := range m {
		entries = append(entries, Entry{Key: k, Value: v})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return Value{kind: KindMap, ref: entries}
}

// MapFromEntries returns a Map value with a sorted copy of entries. A
// duplicate key is an INVALID_ARGUMENT error.
func MapFromEntries(entries []Entry) (Value, error) {
	sorted := append([]Entry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Key == sorted[i-1].Key {
			return Value{}, errs.Newf(errs.CodeInvalidArgument, "value.MapFromEntries",
				"duplicate key at entries %d and %d", i-1, i)
		}
	}
	return Value{kind: KindMap, ref: sorted}, nil
}

// MakeVector returns a Vector of n components produced by at, without an
// intermediate copy.
func MakeVector(n int, at func(i int) float32) Value {
	v := make([]float32, n)
	for i := range v {
		v[i] = at(i)
	}
	return Value{kind: KindVector, ref: v}
}

// MakeList returns a List of n items produced by at, stopping at the first
// error.
func MakeList(n int, at func(i int) (Value, error)) (Value, error) {
	items := make([]Value, n)
	for i := range items {
		item, err := at(i)
		if err != nil {
			return Value{}, err
		}
		items[i] = item
	}
	return Value{kind: KindList, ref: items}, nil
}

// MakeMap returns a Map of n entries produced by at. Keys must arrive in
// strictly increasing byte order; otherwise it is an INVALID_ARGUMENT
// error.
func MakeMap(n int, at func(i int) (Entry, error)) (Value, error) {
	entries := make([]Entry, n)
	for i := range entries {
		e, err := at(i)
		if err != nil {
			return Value{}, err
		}
		if i > 0 && e.Key <= entries[i-1].Key {
			return Value{}, errs.Newf(errs.CodeInvalidArgument, "value.MakeMap",
				"entry %d is not after entry %d in key order", i, i-1)
		}
		entries[i] = e
	}
	return Value{kind: KindMap, ref: entries}, nil
}

// Kind returns the type of v.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether v is Null.
func (v Value) IsNull() bool { return v.kind == KindNull }

// AsBool returns the boolean and true if v is a Bool.
func (v Value) AsBool() (bool, bool) { return v.num == 1, v.kind == KindBool }

// AsInt returns the integer and true if v is an Int.
func (v Value) AsInt() (int64, bool) { return int64(v.num), v.kind == KindInt }

// AsFloat returns the float and true if v is a Float.
func (v Value) AsFloat() (float64, bool) {
	return math.Float64frombits(v.num), v.kind == KindFloat
}

// AsString returns the string and true if v is a String.
func (v Value) AsString() (string, bool) { return v.str, v.kind == KindString }

// AsBytes returns a copy of the bytes and true if v is Bytes.
func (v Value) AsBytes() ([]byte, bool) {
	if v.kind != KindBytes {
		return nil, false
	}
	return []byte(v.str), true
}

// AppendBytes appends the payload of a String or Bytes value to dst.
func (v Value) AppendBytes(dst []byte) []byte { return append(dst, v.str...) }

// Len returns the byte length of a String or Bytes, the dimension of a
// Vector, the number of items of a List or of entries of a Map, and 0 for
// scalars.
func (v Value) Len() int {
	switch v.kind {
	case KindString, KindBytes:
		return len(v.str)
	case KindVector:
		return len(v.ref.([]float32))
	case KindList:
		return len(v.ref.([]Value))
	case KindMap:
		return len(v.ref.([]Entry))
	}
	return 0
}

// VectorAt returns component i of a Vector.
func (v Value) VectorAt(i int) float32 { return v.ref.([]float32)[i] }

// AppendVector appends the components of a Vector to dst.
func (v Value) AppendVector(dst []float32) []float32 {
	return append(dst, v.ref.([]float32)...)
}

// Index returns item i of a List.
func (v Value) Index(i int) Value { return v.ref.([]Value)[i] }

// EntryAt returns entry i of a Map, in key order.
func (v Value) EntryAt(i int) (string, Value) {
	e := v.ref.([]Entry)[i]
	return e.Key, e.Value
}

// Lookup returns the value of key in a Map.
func (v Value) Lookup(key string) (Value, bool) {
	if v.kind != KindMap {
		return Value{}, false
	}
	entries := v.ref.([]Entry)
	i := sort.Search(len(entries), func(i int) bool { return entries[i].Key >= key })
	if i < len(entries) && entries[i].Key == key {
		return entries[i].Value, true
	}
	return Value{}, false
}

// Identical reports whether a and b have the same kind and the same bits,
// recursively. Unlike Compare, Identical distinguishes Int(1) from
// Float(1), -0 from +0 and NaN payloads.
func Identical(a, b Value) bool {
	if a.kind != b.kind || a.num != b.num || a.str != b.str {
		return false
	}
	switch a.kind {
	case KindVector:
		x, y := a.ref.([]float32), b.ref.([]float32)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if math.Float32bits(x[i]) != math.Float32bits(y[i]) {
				return false
			}
		}
	case KindList:
		x, y := a.ref.([]Value), b.ref.([]Value)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Identical(x[i], y[i]) {
				return false
			}
		}
	case KindMap:
		x, y := a.ref.([]Entry), b.ref.([]Entry)
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i].Key != y[i].Key || !Identical(x[i].Value, y[i].Value) {
				return false
			}
		}
	}
	return true
}

// String returns a debugging representation of v. It may contain user
// data, so it must not be used in logs or error messages.
func (v Value) String() string {
	var b strings.Builder
	v.format(&b)
	return b.String()
}

func (v Value) format(b *strings.Builder) {
	switch v.kind {
	case KindNull:
		b.WriteString("null")
	case KindBool:
		b.WriteString(strconv.FormatBool(v.num == 1))
	case KindInt:
		b.WriteString(strconv.FormatInt(int64(v.num), 10))
	case KindFloat:
		b.WriteString(strconv.FormatFloat(math.Float64frombits(v.num), 'g', -1, 64))
	case KindString:
		b.WriteString(strconv.Quote(v.str))
	case KindBytes:
		b.WriteString("0x")
		b.WriteString(hex.EncodeToString([]byte(v.str)))
	case KindVector:
		b.WriteString("vector[")
		for i, c := range v.ref.([]float32) {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
		}
		b.WriteByte(']')
	case KindList:
		b.WriteByte('[')
		for i, item := range v.ref.([]Value) {
			if i > 0 {
				b.WriteString(", ")
			}
			item.format(b)
		}
		b.WriteByte(']')
	case KindMap:
		b.WriteByte('{')
		for i, e := range v.ref.([]Entry) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(e.Key))
			b.WriteString(": ")
			e.Value.format(b)
		}
		b.WriteByte('}')
	}
}
