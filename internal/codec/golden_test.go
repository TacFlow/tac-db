package codec

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/value"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenPath = "testdata/golden/values.golden"

type goldenValue struct {
	name string
	v    value.Value
}

// goldenValues fixes the G2 bytes of representative values (FORMAT.md).
func goldenValues() []goldenValue {
	return []goldenValue{
		{"null", value.Null()},
		{"bool-false", value.Bool(false)},
		{"bool-true", value.Bool(true)},
		{"int-zero", value.Int(0)},
		{"int-minus-one", value.Int(-1)},
		{"int-2^53+1", value.Int(1<<53 + 1)},
		{"int-min", value.Int(math.MinInt64)},
		{"float-one", value.Float(1)},
		{"float-minus-zero", value.Float(math.Copysign(0, -1))},
		{"float-nan-payload", value.Float(math.Float64frombits(0x7ff8000000000001))},
		{"float-plus-inf", value.Float(math.Inf(1))},
		{"string-empty", value.String("")},
		{"string-acao", value.String("ação")},
		{"string-with-nul", value.String("a\x00b")},
		{"bytes", value.Bytes([]byte{0x00, 0x01, 0xff})},
		{"vector", value.Vector([]float32{1, 2})},
		{"list", value.List(value.Int(1), value.Null())},
		{"map", value.MapOf(map[string]value.Value{"origem": value.String("manual"), "a": value.Bool(true)})},
		{"nested", value.List(value.MapOf(map[string]value.Value{"k": value.List()}))},
	}
}

func TestGoldenEncodings(t *testing.T) {
	var buf bytes.Buffer
	for _, g := range goldenValues() {
		enc, err := AppendValue(nil, g.v, value.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		fmt.Fprintf(&buf, "%s %s\n", g.name, hex.EncodeToString(enc))
	}
	if *update {
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/codec -run TestGoldenEncodings -update` once and review the file)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("encodings differ from %s:\n got:\n%s\nwant:\n%s", goldenPath, buf.String(), want)
	}
	// Every golden line must also decode back to its value.
	sc := bufio.NewScanner(bytes.NewReader(want))
	for i := 0; sc.Scan(); i++ {
		name, hexed, _ := strings.Cut(sc.Text(), " ")
		raw, err := hex.DecodeString(hexed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := DecodeValue(raw, DecodeOptions{Limits: value.DefaultLimits()})
		if err != nil || !value.Identical(got, goldenValues()[i].v) {
			t.Fatalf("%s: decoded %v, %v", name, got, err)
		}
	}
}
