package generation

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Host is a remote profile record. Its path is literal, not locally resolved;
// nil and an explicitly empty toplevel remain distinct.
type Host struct {
	Host, Profile string
	ID            *big.Int
	Toplevel      *string
	Date          Timestamp
	KernelVersion *string
	Current       bool
}

type HostList struct {
	Host        string
	CurrentPath *string
	Generations []*Host
}

func (l *HostList) Current() *Host {
	for _, g := range l.Generations {
		if g.Current {
			return g
		}
	}
	return nil
}

func (g *Host) ApproxName() string {
	seconds := g.Date.Seconds()
	seconds.Add(seconds, big.NewInt(int64(g.Date.OffsetSeconds)))
	year, month, day, hour, minute, second := epochCalendar(seconds)
	sign, digits := "", year.String()
	if year.Sign() < 0 {
		sign, digits = "-", digits[1:]
	}
	if len(digits) < 4 {
		digits = strings.Repeat("0", 4-len(digits)) + digits
	}
	return fmt.Sprintf("%s%s-%02d-%02d--%02d-%02d-%02d", sign, digits, month, day, hour, minute, second)
}

// ProfileScript deliberately retains the source's profile interpolation and
// kernel/bzImage concatenation. Transport and carried routing belong to core.
func ProfileScript(profile string) []byte {
	return []byte("realpath " + profile + "\n\n" +
		"for generation in `ls -d -1 " + profile + "-*-link` ; do\n" +
		"  echo -n \"$generation;\"\n" +
		"  echo -n \"$(readlink $generation);\"\n" +
		"  echo -n \"$(stat --format=%Y $generation);\"\n\n" +
		"  for kernel_file in kernel bzImage ; do\n" +
		"    [ -h \"$generation/$kernel_file\" ] && echo -n $(readlink \"$generation/$kernel_file\")\n" +
		"  done\n\n  echo\n" +
		"done\n")
}

// ProfileRunner distinguishes completed ordinary failure from infrastructure
// errors. The production closure uses the existing SSHArgs and Engine.Process.
type ProfileRunner func(context.Context, []string, []byte) ([]byte, bool, error)

func FetchHostList(ctx context.Context, host, profile string, options Options, run ProfileRunner) (*HostList, error) {
	out, success, err := run(ctx, []string{"bash", "--norc"}, ProfileScript(profile))
	if err != nil {
		return nil, err
	}
	if !success {
		return nil, nil
	}
	return ParseHostList(host, profile, out, options), nil
}

func ParseHostList(host, profile string, output []byte, options Options) *HostList {
	list := &HostList{Host: host, Generations: []*Host{}}
	lines := rubySplit(strings.Trim(string(output), "\x00 \t\r\n\v\f"), "\n")
	if len(lines) == 0 {
		return list
	}
	list.CurrentPath = &lines[0]
	idPattern := regexp.MustCompile("^" + regexp.QuoteMeta(profile) + "-([0-9]+)-link$")
	for _, line := range lines[1:] {
		fields := rubySplit(line, ";")
		link := fieldString(fields, 0)
		match := idPattern.FindStringSubmatch(link)
		if match == nil {
			if options.Warn != nil {
				options.Warn(fmt.Sprintf("Invalid profile generation link '%s'", link))
			}
			continue
		}
		id, _ := new(big.Int).SetString(match[1], 10)
		top := optionalField(fields, 1)
		date := epochTimestamp(rubyEpochInteger(fieldString(fields, 2)), options.Location)
		var kernel *string
		if m := kernelPattern.FindStringSubmatch(fieldString(fields, 3)); m != nil {
			kernel = &m[1]
		}
		list.Generations = append(list.Generations, &Host{Host: host, Profile: profile, ID: id, Toplevel: top, Date: date, KernelVersion: kernel, Current: samePath(top, list.CurrentPath)})
	}
	// No extra tie breaker: retain enumeration order for equal numeric IDs.
	sort.SliceStable(list.Generations, func(i, j int) bool { return list.Generations[i].ID.Cmp(list.Generations[j].ID) < 0 })
	return list
}

func rubySplit(s, separator string) []string {
	parts := strings.Split(s, separator)
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
func fieldString(fields []string, i int) string {
	if i >= len(fields) {
		return ""
	}
	return fields[i]
}
func optionalField(fields []string, i int) *string {
	if i >= len(fields) {
		return nil
	}
	return &fields[i]
}
func samePath(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// String#to_i accepts a signed decimal prefix, including single underscores
// between digits. Missing/nonnumeric fields become epoch zero.
func rubyEpochInteger(s string) *big.Int {
	s = strings.TrimLeft(s, " \t\r\n\v\f")
	i := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		i++
	}
	start := i
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			i++
			continue
		}
		if s[i] == '_' && i > start && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			i++
			continue
		}
		break
	}
	if i == start {
		return new(big.Int)
	}
	value, _ := new(big.Int).SetString(strings.ReplaceAll(s[:i], "_", ""), 10)
	return value
}

func epochTimestamp(seconds *big.Int, location *time.Location) Timestamp {
	if location == nil {
		location = time.Local
	}
	probe := new(big.Int).Set(seconds)
	margin := big.NewInt(7 * 86400)
	if !probe.IsInt64() || !new(big.Int).Add(probe, margin).IsInt64() || !new(big.Int).Sub(probe, margin).IsInt64() {
		year, month, day, hour, minute, second := epochCalendar(seconds)
		cycle := new(big.Int).Mod(year, big.NewInt(400)).Int64()
		representative := int64(2400) + cycle
		if year.Sign() < 0 {
			representative = -10000 + cycle
		}
		probe = calendarSeconds(big.NewInt(representative), month, day, hour, minute, second)
	}
	_, offset := time.Unix(probe.Int64(), 0).In(location).Zone()
	return Timestamp{seconds: new(big.Int).Set(seconds), fraction: new(big.Rat), OffsetSeconds: offset}
}

// Inverse of R1's proleptic Gregorian calendar, bounded to one 400-year cycle
// even for epochs outside int64. DivMod provides floor division for negatives.
func epochCalendar(seconds *big.Int) (*big.Int, int, int, int, int, int) {
	days, clock := new(big.Int), new(big.Int)
	days.DivMod(seconds, big.NewInt(86400), clock)
	days.Add(days, big.NewInt(719162))
	cycle, remainder := new(big.Int), new(big.Int)
	cycle.DivMod(days, big.NewInt(146097), remainder)
	year := new(big.Int).Mul(cycle, big.NewInt(400))
	year.Add(year, big.NewInt(1))
	d := int(remainder.Int64())
	for {
		yearDays := 365
		if leapYear(year) {
			yearDays++
		}
		if d < yearDays {
			break
		}
		d -= yearDays
		year.Add(year, big.NewInt(1))
	}
	month := 1
	for d >= monthDays(year, month) {
		d -= monthDays(year, month)
		month++
	}
	c := int(clock.Int64())
	return year, month, d + 1, c / 3600, c % 3600 / 60, c % 60
}
