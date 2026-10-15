package generation

import (
	"testing"
	"time"
)

// Runtime-observed Ruby3.4.9 adapter values from this unit's explicit bounded
// calendar characterization. They do not replace/promote old Expected files.
func TestRubyObservedTimeCases(t *testing.T) {
	cases := []struct {
		input, seconds, fraction string
		offset                   int
		utc                      bool
	}{
		{"2026-10-08T12:00:00Z", "1791460800", "0", 0, true},
		{" \t2026-10-08t12:00:00.123456789123z\n", "1791460800", "123456789123/1000000000000", 0, true},
		{"2026-02-31T12:00:00Z", "1772539200", "0", 0, true},
		{"2026-12-31T24:00:00Z", "1798761600", "0", 0, true},
		{"2026-12-31T23:59:60Z", "1798761600", "0", 0, true},
		{"2026-10-08T12:00:00+01:99", "1791451260", "0", 9540, false},
		{"2026-10-08T12:99:00+01:30", "1791461340", "0", 5400, false},
		{"-1-01-01T00:00:00Z", "-62198755200", "0", 0, true},
		{"2026-01-01T00:00:00", "1767225600", "0", 0, false},
		{"2026-01-01T00:00:00+00", "1767225600", "0", 0, false},
		{"2026-01-01T00:00:00-00", "1767225600", "0", 0, true},
		{"2026-01-01T12:00:00.000000000000000000001+01", "1767265200", "1/1000000000000000000000", 3600, false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseISO8601(tc.input, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			fraction := got.Fraction().RatString()
			if got.Seconds().String() != tc.seconds || fraction != tc.fraction || got.OffsetSeconds != tc.offset || got.UTC != tc.utc {
				t.Fatalf("%s %s %d %v", got.Seconds(), fraction, got.OffsetSeconds, got.UTC)
			}
		})
	}
}
func TestISOGrammarAndFractionOrder(t *testing.T) {
	for _, input := range []string{"2026-10-08T12:00:00+01", "2026-10-08T12:00:00+0130", "2026-10-08T12:00:00+01:30", "0-01-01T00:00:00Z", "100000000000000000000-01-01T00:00:00Z"} {
		if _, err := ParseISO8601(input, time.UTC); err != nil {
			t.Fatal(input, err)
		}
	}
	for _, input := range []string{"2026-10-08", "2026-10-08 12:00:00Z", "+2026-10-08T12:00:00Z", "2026-10-08T12:00:00.Z", "2026-10-08T12:00:00,1Z", "2026-10-08T12:00:00+01:30:00", "2026-10-08T12:00:00Zx", "2026-00-01T00:00:00Z", "2026-02-00T00:00:00Z", "2026-12-31T24:01:00Z", "2026-10-08T12:00:00+24:00"} {
		if _, err := ParseISO8601(input, time.UTC); err == nil {
			t.Fatal("accepted", input)
		}
	}
	a, _ := ParseISO8601("2026-10-08T12:00:00.123456789123Z", time.UTC)
	b, _ := ParseISO8601("2026-10-08T12:00:00.123456789124Z", time.UTC)
	if a.Compare(b) >= 0 {
		t.Fatal("subnanosecond order lost")
	}
	seconds := a.Seconds()
	seconds.SetInt64(0)
	fraction := a.Fraction()
	fraction.SetInt64(0)
	if a.Compare(b) >= 0 || a.Seconds().Sign() == 0 {
		t.Fatal("accessor changed timestamp")
	}
}
func TestObservedLocalDSTEdges(t *testing.T) {
	cases := []struct {
		zone, input, seconds string
		offset               int
	}{
		{"Europe/Amsterdam", "2026-03-29T02:30:00", "1774747800", 7200},
		{"Europe/Amsterdam", "2026-10-25T02:30:00", "1792891800", 3600},
		{"America/New_York", "2026-03-08T02:30:00", "1772955000", -14400},
		{"America/New_York", "2026-11-01T01:30:00", "1793514600", -18000},
		{"Europe/Dublin", "2026-10-25T01:30:00", "1792891800", 0},
		{"Australia/Lord_Howe", "2026-04-05T01:45:00", "1775315700", 37800},
		{"Australia/Lord_Howe", "2026-10-04T02:15:00", "1791042300", 39600},
		{"Pacific/Apia", "2011-12-30T12:00:00", "1325282400", 50400},
	}
	for _, tc := range cases {
		t.Run(tc.zone+tc.input, func(t *testing.T) {
			location, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseISO8601(tc.input, location)
			if err != nil {
				t.Fatal(err)
			}
			if got.Seconds().String() != tc.seconds || got.OffsetSeconds != tc.offset {
				t.Fatal(got.Seconds(), got.OffsetSeconds)
			}
		})
	}
}
