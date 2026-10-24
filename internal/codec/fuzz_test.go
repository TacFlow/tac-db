package codec

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/valuetest"
	"github.com/TacFlow/tacdb/value"
)

// Target names must not be substrings of each other: CI runs the literal
// gate `go test ./internal/codec -fuzz=FuzzValue` (SPEC §25.4), and -fuzz
// must match exactly one target.

// FuzzValue decodes arbitrary bytes. It must never panic, and a successful
// decode must re-encode to the same bytes (canonical form).
func FuzzValue(f *testing.F) {
	for _, g := range goldenValues() {
		enc, err := AppendValue(nil, g.v, value.DefaultLimits())
		if err != nil {
			f.Fatal(err)
		}
		f.Add(enc)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeValue(data, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil {
			return
		}
		enc, err := AppendValue(nil, v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("decoded value does not re-encode: %v", err)
		}
		if !bytes.Equal(enc, data) {
			t.Fatalf("non-canonical input accepted:\n in  %x\n out %x", data, enc)
		}
	})
}

// FuzzRoundTrip encodes generated values and decodes them back.
func FuzzRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint8(3))
	f.Fuzz(func(t *testing.T, seed uint64, depth uint8) {
		v := valuetest.Value(rand.New(rand.NewPCG(seed, 0)), 1+int(depth%6))
		enc, err := AppendValue(nil, v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("AppendValue(%v): %v", v, err)
		}
		got, err := DecodeValue(enc, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil || !value.Identical(got, v) {
			t.Fatalf("round trip of %v gave %v, %v", v, got, err)
		}
	})
}

// FuzzUvarint checks that accepted uvarints are minimal.
func FuzzUvarint(f *testing.F) {
	f.Add([]byte{0x80, 0x01})
	f.Add([]byte{0x80, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		x, n, err := ReadUvarint(data)
		if err != nil {
			return
		}
		if !bytes.Equal(AppendUvarint(nil, x), data[:n]) || UvarintLen(x) != n {
			t.Fatalf("accepted non-minimal %x for %d", data[:n], x)
		}
	})
}

// FuzzKeyReader reads arbitrary bytes as key components. It must never
// panic, and a string it accepts must re-encode to the bytes it consumed.
func FuzzKeyReader(f *testing.F) {
	f.Add([]byte{'a', 0x00, 0xff, 0x00, 0x00, 1, 2})
	f.Add([]byte{0x00, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewKeyReader(data)
		s, err := r.ReadString()
		if err == nil {
			consumed := data[:len(data)-r.Len()]
			if enc := AppendKeyString(nil, s); !bytes.Equal(enc, consumed) {
				t.Fatalf("ReadString accepted %x but re-encodes to %x", consumed, enc)
			}
		}
		r = NewKeyReader(data)
		for r.Len() > 0 {
			if _, err := r.ReadUint64(); err != nil {
				break
			}
		}
	})
}

// FuzzKeyEncoding checks order preservation and round trips of string
// components.
func FuzzKeyEncoding(f *testing.F) {
	f.Add("a", "a\x00")
	f.Add("\x00\xff", "\x00")
	f.Fuzz(func(t *testing.T, a, b string) {
		ka, kb := AppendKeyString(nil, a), AppendKeyString(nil, b)
		if got, want := bytes.Compare(ka, kb), strings.Compare(a, b); got != want {
			t.Fatalf("order of %q vs %q: bytes %d, strings %d", a, b, got, want)
		}
		got, err := NewKeyReader(ka).ReadString()
		if err != nil || got != a {
			t.Fatalf("round trip of %q gave %q, %v", a, got, err)
		}
	})
}
