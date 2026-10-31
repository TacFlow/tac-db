package value_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/value"
)

func nest(depth int) value.Value {
	v := value.Int(1)
	for i := 1; i < depth; i++ {
		v = value.List(v)
	}
	return v
}

func TestValidateAcceptsDefaults(t *testing.T) {
	lim := value.DefaultLimits()
	if err := lim.Check(); err != nil {
		t.Fatal(err)
	}
	ok := []value.Value{
		value.Null(), value.String("a\x00ç"), nest(64),
		value.Vector(make([]float32, 4096)),
		value.MapOf(map[string]value.Value{"": value.Null(), "\x00": value.Int(1)}),
		value.MapOf(map[string]value.Value{strings.Repeat("k", 64<<10): value.Null()}),
	}
	for _, v := range ok {
		if err := value.Validate(v, lim); err != nil {
			t.Errorf("Validate(%.40s) = %v", v.String(), err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	lim := value.DefaultLimits()
	limitExceeded := errs.Sentinel(errs.CodeLimitExceeded)
	invalid := errs.Sentinel(errs.CodeInvalidArgument)
	cases := []struct {
		name string
		v    value.Value
		want error
	}{
		{"depth 65", nest(65), limitExceeded},
		{"bad UTF-8 string", value.String("\xff"), invalid},
		{"bad UTF-8 map key", value.MapOf(map[string]value.Value{"\xc3": value.Null()}), invalid},
		{"map key above 64 KiB", value.MapOf(map[string]value.Value{strings.Repeat("k", 64<<10+1): value.Null()}), limitExceeded},
		{"empty vector", value.Vector(nil), invalid},
		{"vector above 4096", value.Vector(make([]float32, 4097)), limitExceeded},
		{"nested bad string", value.List(value.Int(1), value.List(value.String("\xff"))), invalid},
	}
	for _, c := range cases {
		if err := value.Validate(c.v, lim); !errors.Is(err, c.want) {
			t.Errorf("%s: Validate = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestValidateCountsElementsAcrossLevels(t *testing.T) {
	lim := value.DefaultLimits()
	lim.MaxValueElements = 5
	inner := value.List(value.Null(), value.Null(), value.Null())  // 3 elements
	if err := value.Validate(value.List(inner), lim); err != nil { // 1 + 3 = 4
		t.Fatalf("4 elements rejected: %v", err)
	}
	err := value.Validate(value.List(inner, value.Null()), lim) // 2 + 3 = 5
	if err != nil {
		t.Fatalf("5 elements rejected: %v", err)
	}
	err = value.Validate(value.List(inner, inner), lim) // 2 + 3 + 3 = 8
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != errs.CodeLimitExceeded || e.Details["limit"] != 5 {
		t.Fatalf("8 elements: err = %v, want LIMIT_EXCEEDED with limit=5", err)
	}
}

func TestLimitsCheck(t *testing.T) {
	bad := value.DefaultLimits()
	bad.MaxVectorDimensions = 70000
	if err := bad.Check(); !errors.Is(err, errs.Sentinel(errs.CodeInvalidArgument)) {
		t.Fatalf("Check = %v, want INVALID_ARGUMENT", err)
	}
}
