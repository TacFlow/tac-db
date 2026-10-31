package codec

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
)

func TestKeyStringRoundTrip(t *testing.T) {
	for _, s := range []string{"", "a", "\x00", "a\x00b", "\x00\x00", "\xff", "ação"} {
		key := AppendKeyString(nil, s)
		key = AppendKeyUint32(key, 7)
		r := NewKeyReader(key)
		got, err := r.ReadString()
		if err != nil || got != s {
			t.Fatalf("ReadString(%x) = %q, %v; want %q", key, got, err, s)
		}
		if n, err := r.ReadUint32(); err != nil || n != 7 || r.Len() != 0 {
			t.Fatalf("component after %q: %d, %v, %d left", s, n, err, r.Len())
		}
	}
}

func TestKeyIntegersAreBigEndian(t *testing.T) {
	key := AppendKeyUint64(AppendKeyUint16(AppendKeyUint8(nil, 1), 0x0203), 0x0405060708090a0b)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if !bytes.Equal(key, want) {
		t.Fatalf("key = %x, want %x", key, want)
	}
	r := NewKeyReader(key)
	a, _ := r.ReadUint8()
	b, _ := r.ReadUint16()
	c, err := r.ReadUint64()
	if a != 1 || b != 0x0203 || c != 0x0405060708090a0b || err != nil {
		t.Fatalf("read back %d %d %d %v", a, b, c, err)
	}
	if _, err := r.ReadUint8(); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
		t.Fatalf("reading past the end: err = %v, want CORRUPT", err)
	}
}

// The encoded order of (string, uint64) keys must equal the logical order,
// including strings that contain 0x00 and strings that are prefixes of
// others.
func TestKeyOrderMatchesLogicalOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	alphabet := []byte{0x00, 0x01, 'a', 0xfe, 0xff}
	randomString := func() string {
		b := make([]byte, r.IntN(5))
		for i := range b {
			b[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(b)
	}
	for i := 0; i < 20000; i++ {
		s1, s2 := randomString(), randomString()
		n1, n2 := r.Uint64N(3), r.Uint64N(3)
		k1 := AppendKeyUint64(AppendKeyString(nil, s1), n1)
		k2 := AppendKeyUint64(AppendKeyString(nil, s2), n2)
		want := strings.Compare(s1, s2)
		if want == 0 {
			want = cmpU64(n1, n2)
		}
		if got := bytes.Compare(k1, k2); got != want {
			t.Fatalf("(%q,%d) vs (%q,%d): bytes order %d, logical order %d", s1, n1, s2, n2, got, want)
		}
		if strings.HasPrefix(s2, s1) && !bytes.HasPrefix(k2, AppendKeyStringPrefix(nil, s1)) {
			t.Fatalf("prefix encoding of %q is not a prefix of the key of %q", s1, s2)
		}
	}
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestReadStringRejects(t *testing.T) {
	cases := map[string][]byte{
		"no terminator":  []byte("abc"),
		"lone zero":      {'a', 0x00},
		"invalid escape": {0x00, 0x01, 0x00, 0x00},
	}
	for name, key := range cases {
		if _, err := NewKeyReader(key).ReadString(); !errors.Is(err, errs.Sentinel(errs.CodeCorrupt)) {
			t.Errorf("%s: err = %v, want CORRUPT", name, err)
		}
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{1, 2}, []byte{1, 3}},
		{[]byte{1, 0xff}, []byte{2}},
		{[]byte{0xff, 0xff}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("PrefixEnd(%x) = %x, want %x", c.in, got, c.want)
		}
	}
	in := []byte{1, 2}
	PrefixEnd(in)
	if in[1] != 2 {
		t.Error("PrefixEnd modified its input")
	}
}

func TestCheckKey(t *testing.T) {
	if err := CheckKey(make([]byte, MaxKeyLen)); err != nil {
		t.Fatalf("1024-byte key: %v", err)
	}
	if err := CheckKey(make([]byte, MaxKeyLen+1)); !errors.Is(err, errs.Sentinel(errs.CodeLimitExceeded)) {
		t.Fatalf("1025-byte key: err = %v, want LIMIT_EXCEEDED", err)
	}
}
