package generation

import "math/big"

// RotationPolicy contains effective integer settings. Zero, negative and
// inverted limits retain the source loop's behavior; no defaults are applied.
type RotationPolicy struct {
	Min, Max, MaxAgeSeconds big.Int
}

type RotationEntry struct {
	Date    Timestamp
	Current bool
}

// PlanRotation returns original-order indices without changing the entries or
// policy. The age predicate samples now separately for each reached candidate;
// a count-driven deletion and a current entry do not access the clock.
func PlanRotation(entries []RotationEntry, policy RotationPolicy, now func() Timestamp) []int {
	selected := []int{}
	remaining := new(big.Int).SetUint64(uint64(len(entries)))
	if remaining.Cmp(&policy.Min) <= 0 {
		return selected
	}
	for i, entry := range entries {
		if entry.Current {
			continue
		}
		remove := remaining.Cmp(&policy.Max) > 0
		if !remove {
			limit := Timestamp{
				seconds:  new(big.Int).Add(entry.Date.Seconds(), &policy.MaxAgeSeconds),
				fraction: entry.Date.Fraction(),
			}
			remove = limit.Compare(now()) < 0
		}
		if remove {
			selected = append(selected, i)
			remaining.Sub(remaining, big.NewInt(1))
		}
		if remaining.Cmp(&policy.Min) <= 0 {
			break
		}
	}
	return selected
}
