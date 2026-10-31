package value

import (
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
)

// Maxima fixed by the G2 format (SPEC §31.4). Limits may lower them, never
// raise them.
const (
	FormatMaxDepth            = 64
	FormatMaxValueBytes       = 16 << 20
	FormatMaxMapKeyBytes      = 64 << 10
	FormatMaxVectorDimensions = 65535
)

// Limits bounds the size and shape of a Value (SPEC §5.1, §24.1).
type Limits struct {
	MaxDepth            int // nesting depth; a scalar has depth 1
	MaxValueBytes       int // size of the G2 encoding
	MaxValueElements    int // List items plus Map entries, summed over all levels
	MaxMapKeyBytes      int // byte length of a key of a Map inside a value
	MaxVectorDimensions int // dimension of a Vector
}

// DefaultLimits returns the defaults of SPEC §24.1.
func DefaultLimits() Limits {
	return Limits{
		MaxDepth:            64,
		MaxValueBytes:       16 << 20,
		MaxValueElements:    1_000_000,
		MaxMapKeyBytes:      64 << 10,
		MaxVectorDimensions: 4096,
	}
}

// Check reports whether the limits are positive and within the format
// maxima.
func (l Limits) Check() error {
	bad := func(name string, v, max int) error {
		return errs.Newf(errs.CodeInvalidArgument, "value.Limits", "%s = %d, want 1..%d", name, v, max)
	}
	switch {
	case l.MaxDepth < 1 || l.MaxDepth > FormatMaxDepth:
		return bad("MaxDepth", l.MaxDepth, FormatMaxDepth)
	case l.MaxValueBytes < 1 || l.MaxValueBytes > FormatMaxValueBytes:
		return bad("MaxValueBytes", l.MaxValueBytes, FormatMaxValueBytes)
	case l.MaxValueElements < 0:
		return errs.Newf(errs.CodeInvalidArgument, "value.Limits", "MaxValueElements = %d, want >= 0", l.MaxValueElements)
	case l.MaxMapKeyBytes < 1 || l.MaxMapKeyBytes > FormatMaxMapKeyBytes:
		return bad("MaxMapKeyBytes", l.MaxMapKeyBytes, FormatMaxMapKeyBytes)
	case l.MaxVectorDimensions < 1 || l.MaxVectorDimensions > FormatMaxVectorDimensions:
		return bad("MaxVectorDimensions", l.MaxVectorDimensions, FormatMaxVectorDimensions)
	}
	return nil
}

// Validate checks v against lim: valid UTF-8 in strings and map keys,
// nesting depth, element count, map key length and vector dimension. The
// encoded size is checked by the codec, which owns the encoding.
func Validate(v Value, lim Limits) error {
	w := validator{lim: lim}
	return w.walk(v, 1)
}

type validator struct {
	lim      Limits
	elements int
}

func (w *validator) walk(v Value, depth int) error {
	const op = "value.Validate"
	if depth > w.lim.MaxDepth {
		return errs.New(errs.CodeLimitExceeded, op, "value nested too deeply").
			With("depth", int64(depth)).With("limit", int64(w.lim.MaxDepth))
	}
	switch v.kind {
	case KindString:
		if !utf8.ValidString(v.str) {
			return errs.New(errs.CodeInvalidArgument, op, "string is not valid UTF-8")
		}
	case KindVector:
		n := len(v.ref.([]float32))
		if n == 0 {
			return errs.New(errs.CodeInvalidArgument, op, "vector has no components")
		}
		if n > w.lim.MaxVectorDimensions {
			return errs.New(errs.CodeLimitExceeded, op, "vector dimension above limit").
				With("dimensions", int64(n)).With("limit", int64(w.lim.MaxVectorDimensions))
		}
	case KindList:
		items := v.ref.([]Value)
		if err := w.count(len(items)); err != nil {
			return err
		}
		for _, item := range items {
			if err := w.walk(item, depth+1); err != nil {
				return err
			}
		}
	case KindMap:
		entries := v.ref.([]Entry)
		if err := w.count(len(entries)); err != nil {
			return err
		}
		for _, e := range entries {
			if !utf8.ValidString(e.Key) {
				return errs.New(errs.CodeInvalidArgument, op, "map key is not valid UTF-8")
			}
			if len(e.Key) > w.lim.MaxMapKeyBytes {
				return errs.New(errs.CodeLimitExceeded, op, "map key too long").
					With("bytes", int64(len(e.Key))).With("limit", int64(w.lim.MaxMapKeyBytes))
			}
			if err := w.walk(e.Value, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *validator) count(n int) error {
	w.elements += n
	if w.elements > w.lim.MaxValueElements {
		return errs.New(errs.CodeLimitExceeded, "value.Validate", "too many elements").
			With("elements", int64(w.elements)).With("limit", int64(w.lim.MaxValueElements))
	}
	return nil
}
