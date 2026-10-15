package inputs

import (
	"math"
	"testing"
)

func TestOrderedLockNumericSourceFrontEnd(t *testing.T) {
	// These explicit arithmetic boundaries supplement the original J/F/E
	// byte/bit tests. They define the accepted modulo compatibility operation;
	// they do not assert portable signed C overflow or all-float equivalence.
	for _, c := range []struct {
		magnitude, digits uint64
		negative          bool
		want              int64
	}{
		{0, 19, false, 0}, {0, 20, false, math.MaxInt64}, {0, 20, true, math.MinInt64},
		{uint64(math.MaxInt64), 19, false, math.MaxInt64},
		{uint64(math.MaxInt64) + 1, 19, false, math.MaxInt64},
		{uint64(math.MaxInt64) + 1, 19, true, math.MinInt64},
		{7, 1, true, -7},
	} {
		if got := orderedLockExponent(c.magnitude, c.digits, c.negative); got != c.want {
			t.Fatalf("exponent %+v => %d", c, got)
		}
	}
	for _, c := range []struct {
		exponent int64
		fraction uint64
		want     int64
	}{
		{math.MinInt64, 1, math.MaxInt64},
		{math.MinInt64, 2, math.MaxInt64 - 1},
		{math.MinInt64, 0, math.MinInt64},
		{0, 1, -1},
		{math.MaxInt64, 1, math.MaxInt64 - 1},
	} {
		if got := orderedLockFractionExponent(c.exponent, c.fraction); got != c.want {
			t.Fatalf("fraction %+v => %d", c, got)
		}
	}
	// F2 and E1-E4 already assert both saturation signs and zero-mantissa
	// ordering through the actual parser, including generator-phase failures.
	// The int32 edge itself must precede any conversion of the supplied token.
	for _, c := range []struct {
		exponent int64
		negative bool
		bits     uint64
	}{
		{int64(math.MaxInt32) + 1, false, 0x7ff0000000000000},
		{int64(math.MaxInt32) + 1, true, 0xfff0000000000000},
		{int64(math.MinInt32) - 1, false, 0},
		{int64(math.MinInt32) - 1, true, 0x8000000000000000},
		{-400, false, 0}, {-400, true, 0x8000000000000000},
	} {
		f, err := orderedLockParseFloat("0", 1, c.exponent, c.negative)
		if err != nil || math.Float64bits(f) != c.bits {
			t.Fatalf("branch %+v => %016x, %v", c, math.Float64bits(f), err)
		}
	}
}
