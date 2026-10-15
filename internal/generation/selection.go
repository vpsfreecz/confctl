package generation

import (
	"fmt"
	"math/big"
	"regexp"
	"time"
)

type SelectionOptions struct {
	Local, Remote bool
	Pattern       *string
	// Match is the existing core matcher; no glob implementation lives here.
	Match func(string, string) bool
	// Now is sampled once after collection, only for an age selector.
	Now func() Timestamp
}

type HostLoader func(string) (*HostList, error)
type BuildLoader func(string) (*BuildList, error)

// SelectGenerations collects completed remote lists in inventory order, then
// loads/validates included local lists. Core owns remote query concurrency.
func SelectGenerations(hosts []string, options SelectionOptions, remote HostLoader, local BuildLoader) (*UnifiedList, error) {
	list := &UnifiedList{Generations: []*Unified{}}
	if options.Remote {
		for _, host := range hosts {
			generations, err := remote(host)
			if err != nil {
				return nil, err
			}
			if generations != nil {
				list.AddHosts(generations)
			}
		}
	}
	if options.Local || !options.Remote {
		for _, host := range hosts {
			generations, err := local(host)
			if err != nil {
				return nil, err
			}
			if options.Pattern != nil {
				if err := generations.CheckSelection(*options.Pattern); err != nil {
					return nil, err
				}
				if *options.Pattern == "old" {
					if err := generations.RequireResolvedCurrent(); err != nil {
						return nil, err
					}
				}
			}
			list.AddBuilds(generations)
		}
	}
	return SelectList(list, options)
}

// Ruby's \Z admits one final LF for age syntax, whereas offsets use \z.
var ageSelector = regexp.MustCompile(`^([0-9]+)d\n?$`)

// SelectList is a pure ordered projection: it changes neither wrappers nor
// attached input records. An out-of-range offset falls through to the matcher.
func SelectList(list *UnifiedList, options SelectionOptions) (*UnifiedList, error) {
	result := &UnifiedList{Generations: []*Unified{}}
	if options.Pattern == nil {
		result.Generations = append(result.Generations, list.Generations...)
		return result, nil
	}
	pattern := *options.Pattern
	var threshold *Timestamp
	if m := ageSelector.FindStringSubmatch(pattern); m != nil {
		now := options.Now
		if now == nil {
			now = currentTimestamp
		}
		stamp := now()
		days, _ := new(big.Int).SetString(m[1], 10)
		seconds := stamp.Seconds()
		seconds.Sub(seconds, days.Mul(days, big.NewInt(86400)))
		t := Timestamp{seconds: seconds, fraction: stamp.Fraction()}
		threshold = &t
	}
	var at *Unified
	if decimalSelector.MatchString(pattern) {
		offset, _ := new(big.Int).SetString(pattern, 10)
		at = list.AtOffset(offset)
	}
	if pattern != "current" && pattern != "old" && threshold == nil && at == nil && options.Match == nil {
		return nil, fmt.Errorf("generation name selection requires the existing pattern matcher")
	}
	for _, g := range list.Generations {
		include := false
		switch {
		case pattern == "current":
			include = g.Current
		case pattern == "old":
			include = !g.Current
		case threshold != nil:
			include = g.Date.Compare(*threshold) < 0
		case at != nil:
			include = g == at
		default:
			include = options.Match(pattern, g.Name)
		}
		if include {
			result.Generations = append(result.Generations, g)
		}
	}
	return result, nil
}

func currentTimestamp() Timestamp {
	now := time.Now()
	return Timestamp{seconds: big.NewInt(now.Unix()), fraction: new(big.Rat).SetFrac(big.NewInt(int64(now.Nanosecond())), big.NewInt(1000000000))}
}
