package generation

import (
	"fmt"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"
)

// These are finite predictions from Generation#generations_rotate, not original
// runtime observations or a new policy validation contract.
func rotationInteger(t *testing.T, text string) big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(text, 10)
	if !ok {
		t.Fatal(text)
	}
	return *n
}

func rotationPolicy(t *testing.T, min, max, age string) RotationPolicy {
	t.Helper()
	return RotationPolicy{rotationInteger(t, min), rotationInteger(t, max), rotationInteger(t, age)}
}

func rotationDate(t *testing.T, text string) Timestamp {
	t.Helper()
	date, err := ParseISO8601(text, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return date
}

func rotationInputSignature(entries []RotationEntry, p RotationPolicy) []string {
	ret := []string{p.Min.String(), p.Max.String(), p.MaxAgeSeconds.String()}
	for _, entry := range entries {
		ret = append(ret, fmt.Sprintf("%s/%s/%d/%t/%t", entry.Date.Seconds(), entry.Date.Fraction(), entry.Date.OffsetSeconds, entry.Date.UTC, entry.Current))
	}
	return ret
}

func TestRotationEffectiveLimitsAndCurrentOrder(t *testing.T) {
	for _, test := range []struct {
		name          string
		count         int
		current       []int
		min, max, age string
		want          []int
		clocks        int
	}{
		{"empty", 0, nil, "0", "0", "0", []int{}, 0},
		{"below minimum", 2, nil, "3", "0", "0", []int{}, 0},
		{"equal minimum", 3, nil, "3", "0", "0", []int{}, 0},
		{"current first", 4, []int{0}, "1", "2", "0", []int{1, 2}, 1},
		{"current middle", 4, []int{1}, "1", "0", "0", []int{0, 2, 3}, 0},
		{"current last and early stop", 3, []int{2}, "2", "0", "0", []int{0}, 0},
		{"all current", 3, []int{0, 1, 2}, "0", "0", "0", []int{}, 0},
		{"multiple protected current", 3, []int{0, 2}, "0", "0", "0", []int{1}, 0},
		{"no current zero limits", 3, nil, "0", "0", "0", []int{0, 1, 2}, 0},
		{"negative limits", 3, nil, "-1", "-1", "0", []int{0, 1, 2}, 0},
		{"inverted limits", 4, nil, "3", "1", "0", []int{0}, 0},
		{"negative age", 3, nil, "1", "9", "-1", []int{0, 1}, 2},
		{"huge minimum", 3, nil, "100000000000000000000000000000000000000", "0", "0", []int{}, 0},
		{"huge maximum", 3, nil, "0", "100000000000000000000000000000000000000", "0", []int{}, 3},
		{"huge negative maximum", 3, nil, "1", "-100000000000000000000000000000000000000", "0", []int{0, 1}, 0},
		{"huge positive age", 3, nil, "0", "9", "100000000000000000000000000000000000000", []int{}, 3},
		{"huge negative age", 3, nil, "1", "9", "-100000000000000000000000000000000000000", []int{0, 1}, 2},
		{"default below minimum", 4, nil, "5", "30", "15552000", []int{}, 0},
		{"default maximum then age", 32, nil, "5", "30", "15552000", []int{0, 1}, 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			date := rotationDate(t, "1970-01-01T00:00:00Z")
			entries := make([]RotationEntry, test.count)
			for i := range entries {
				entries[i].Date = date
			}
			for _, i := range test.current {
				entries[i].Current = true
			}
			p := rotationPolicy(t, test.min, test.max, test.age)
			before := rotationInputSignature(entries, p)
			clocks := 0
			got := PlanRotation(entries, p, func() Timestamp {
				clocks++
				if test.clocks == 0 {
					t.Fatal("count/current/minimum branch accessed clock")
				}
				return date
			})
			if !reflect.DeepEqual(got, test.want) || clocks != test.clocks {
				t.Fatalf("indices %v clocks %d, want %v/%d", got, clocks, test.want, test.clocks)
			}
			if !reflect.DeepEqual(before, rotationInputSignature(entries, p)) {
				t.Fatal("planner mutated input storage")
			}
		})
	}
}

func TestRotationExactThresholdsAndPerCandidateClock(t *testing.T) {
	threshold := rotationDate(t, "1970-01-01T00:00:01.000000000000000000000000000001Z")
	entries := []RotationEntry{
		{Date: rotationDate(t, "1970-01-01T00:00:00.000000000000000000000000000000Z")},
		{Date: rotationDate(t, "1970-01-01T00:00:00.000000000000000000000000000001Z")},
		{Date: rotationDate(t, "1970-01-01T00:00:00.000000000000000000000000000002Z")},
	}
	p := rotationPolicy(t, "0", "9", "1")
	before := rotationInputSignature(entries, p)
	calls := 0
	if got := PlanRotation(entries, p, func() Timestamp { calls++; return threshold }); !reflect.DeepEqual(got, []int{0}) || calls != 3 {
		t.Fatalf("strict fractional threshold %v/%d", got, calls)
	}
	if !reflect.DeepEqual(before, rotationInputSignature(entries, p)) {
		t.Fatal("exact fractions or policy changed")
	}

	date := rotationDate(t, "1970-01-01T00:00:00Z")
	for _, test := range []struct {
		name    string
		samples []string
		want    []int
	}{
		{"crossing equality", []string{"1970-01-01T00:00:00Z", "1970-01-01T00:00:01Z", "1970-01-01T00:00:01Z"}, []int{1, 2}},
		{"backwards clock", []string{"1970-01-01T00:00:01Z", "1970-01-01T00:00:00Z", "1969-12-31T23:59:59Z"}, []int{0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			i := 0
			got := PlanRotation([]RotationEntry{{Date: date}, {Date: date}, {Date: date}}, rotationPolicy(t, "0", "9", "0"), func() Timestamp {
				ret := rotationDate(t, test.samples[i])
				i++
				return ret
			})
			if i != 3 || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("per-candidate sampling %v/%d", got, i)
			}
		})
	}
	// The first deletion uses count; only the second reaches age and minimum
	// stops traversal before the third candidate.
	calls = 0
	got := PlanRotation([]RotationEntry{{Date: date}, {Date: date}, {Date: date}}, rotationPolicy(t, "1", "2", "0"), func() Timestamp {
		calls++
		return threshold
	})
	if !reflect.DeepEqual(got, []int{0, 1}) || calls != 1 {
		t.Fatalf("mixed count/age stop %v/%d", got, calls)
	}
}

func TestRotationHistoricalInstantsAndInputOrder(t *testing.T) {
	for _, test := range []struct{ date, now string }{
		{"-100000000000000000000-01-01T00:00:00.1Z", "-100000000000000000000-01-01T00:00:00.2Z"},
		{"100000000000000000000-01-01T00:00:00.1Z", "100000000000000000000-01-01T00:00:00.2Z"},
		{"1969-12-31T23:59:59.999999999999999999999Z", "1970-01-01T00:00:00Z"},
		{"2026-10-09T01:00:00+01:00", "2026-10-09T00:00:00.000000000001Z"},
		{"2026-10-09T00:00:00", "2026-10-09T00:00:00.000000000001Z"},
	} {
		entries := []RotationEntry{{Date: rotationDate(t, test.date)}}
		p := rotationPolicy(t, "0", "1", "0")
		before := rotationInputSignature(entries, p)
		got := PlanRotation(entries, p, func() Timestamp { return rotationDate(t, test.now) })
		if !reflect.DeepEqual(got, []int{0}) || !reflect.DeepEqual(before, rotationInputSignature(entries, p)) {
			t.Fatal(test, got)
		}
	}
	// Model numeric-ID order with dates in the opposite order. Only the supplied
	// index order controls count-driven retention.
	entries := []RotationEntry{{Date: rotationDate(t, "2026-10-09T00:00:00Z")}, {Date: rotationDate(t, "1970-01-01T00:00:00Z")}, {Date: rotationDate(t, "2000-01-01T00:00:00Z")}}
	got := PlanRotation(entries, rotationPolicy(t, "2", "0", "0"), func() Timestamp { panic("unreached age") })
	if !reflect.DeepEqual(got, []int{0}) {
		t.Fatal("planner sorted input", got)
	}
}

func TestRotationReaderCompositionAndCurrentBeforePolicy(t *testing.T) {
	for _, currentKind := range []string{"absent", "broken", "external", "regular", "resolved"} {
		t.Run(currentKind, func(t *testing.T) {
			root := t.TempDir()
			top := root + "/private-store/top"
			if err := os.MkdirAll(top, 0755); err != nil {
				t.Fatal(err)
			}
			// Deliberately nonlexical tie order, old spelling, unknown metadata.
			seed(t, root, "h", "z", fmt.Sprintf(`{"mode":"flakes","date":"2026-10-09T00:00:00Z","toplevel":%q,"inputs":{},"inputsInfo":{"x":{"unknown":[1]}},"extra":true}`, top))
			seed(t, root, "h", "a", record(top, "2026-10-09T00:00:00Z"))
			seed(t, root, "h", "legacy", `{"mode":"swpins"}`)
			seed(t, root, "h", "corrupt", `{`)
			rollback := top
			written, err := CreateFlake(root, "h", top, &rollback, Object{}, Object{}, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			w, err := NewWriter(root, func() (string, error) { return root + "/private-roots", nil })
			if err != nil || w.Save(written) != nil {
				t.Fatal("seed writer", err)
			}
			current := root + "/.confctl/generations/h/current"
			switch currentKind {
			case "broken":
				err = os.Symlink("gone", current)
			case "external":
				err = os.Mkdir(root+"/a", 0755)
				if err == nil {
					err = os.Symlink(root+"/a", current)
				}
			case "regular":
				err = os.WriteFile(current, []byte("current bytes"), 0640)
			case "resolved":
				err = os.Symlink("z", current)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := retentionForest(t, root)
			warnings := []string{}
			list, err := LoadBuildList(root, "h", Options{Location: time.UTC, Enumerate: func(string) ([]string, error) {
				return []string{"z", "a", "legacy", "corrupt", written.Name}, nil
			}, Warn: func(message string) error { warnings = append(warnings, message); return nil }})
			if err != nil || len(list.Builds) != 3 || len(list.Rejected) != 2 || list.Builds[0].Name != "z" || list.Builds[1].Name != "a" || len(warnings) < 2 {
				t.Fatal("supported reader order/count", list, warnings, err)
			}
			// This is the later adapter's ordering: resolution is required even
			// when the generous minimum would make pure planning do nothing.
			err = list.RequireResolvedCurrent()
			bad := currentKind == "broken" || currentKind == "external" || currentKind == "regular"
			if (err != nil) != bad {
				t.Fatal("current resolution", err)
			}
			if err == nil {
				entries := []RotationEntry{}
				for _, b := range list.Builds {
					entries = append(entries, RotationEntry{b.Date, b.Current})
				}
				if got := PlanRotation(entries, rotationPolicy(t, "100", "0", "0"), func() Timestamp { panic("minimum branch") }); len(got) != 0 {
					t.Fatal(got)
				}
				want := []int{0, 1} // z then a; newest is current when absent.
				if currentKind == "resolved" {
					want = []int{1, 2} // z is protected; a precedes the newer record.
				}
				if got := PlanRotation(entries, rotationPolicy(t, "1", "0", "0"), func() Timestamp { panic("count branch") }); !reflect.DeepEqual(got, want) {
					t.Fatal("reader projection/tie order changed", got, want)
				}
			}
			if !reflect.DeepEqual(before, retentionForest(t, root)) {
				t.Fatal("reader/planner changed private forest")
			}
		})
	}
	root := t.TempDir()
	list, err := LoadBuildList(root, "empty", Options{})
	if err != nil || list.RequireResolvedCurrent() != nil || len(list.Builds) != 0 {
		t.Fatal("empty no-current list", list, err)
	}
}
