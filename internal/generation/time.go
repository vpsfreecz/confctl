package generation

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Timestamp keeps arbitrary decimal fractional precision and signed years.
// Seconds/Fraction return copies so comparisons never depend on float64 or
// nanosecond truncation. OffsetSeconds records the original/local zone context.
type Timestamp struct {
	seconds       *big.Int
	fraction      *big.Rat
	OffsetSeconds int
	UTC           bool
}

func (t Timestamp) Seconds() *big.Int  { return new(big.Int).Set(t.seconds) }
func (t Timestamp) Fraction() *big.Rat { return new(big.Rat).Set(t.fraction) }
func (t Timestamp) Compare(other Timestamp) int {
	if c := t.seconds.Cmp(other.seconds); c != 0 {
		return c
	}
	return t.fraction.Cmp(other.fraction)
}

var isoPattern = regexp.MustCompile(`(?i)^[\t\n\v\f\r ]*(-?[0-9]+)-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?(Z|[+-][0-9]{2}(?::?[0-9]{2})?)?[\t\n\v\f\r ]*$`)

// ParseISO8601 follows Ruby3.4.9 time.rb xmlschema, including its offset-before-
// calendar-validation behavior. Absent zones use the supplied local context.
func ParseISO8601(input string, location *time.Location) (Timestamp, error) {
	m := isoPattern.FindStringSubmatch(input)
	if m == nil {
		return Timestamp{}, fmt.Errorf("invalid xmlschema format: %q", input)
	}
	year, _ := new(big.Int).SetString(m[1], 10)
	values := make([]int, 5)
	for i := range values {
		values[i], _ = strconv.Atoi(m[i+2])
	}
	month, day, hour, minute, second := values[0], values[1], values[2], values[3], values[4]
	fraction := new(big.Rat)
	if m[7] != "" {
		fraction.SetString("0" + m[7])
	}
	zone := strings.ToUpper(m[8])
	offset := 0
	utc := zone == "Z" || zone == "-00" || zone == "-0000" || zone == "-00:00"
	if zone != "" {
		if zone != "Z" {
			digits := strings.ReplaceAll(zone[1:], ":", "")
			h, _ := strconv.Atoi(digits[:2])
			min := 0
			if len(digits) > 2 {
				min, _ = strconv.Atoi(digits[2:])
			}
			offset = h*3600 + min*60
			if zone[0] == '-' {
				offset = -offset
			}
		}
		month, day, hour, minute, second = applyOffset(year, month, day, hour, minute, second, offset)
	}
	if err := validateCalendar(month, day, hour, minute, second); err != nil {
		return Timestamp{}, err
	}
	seconds := calendarSeconds(year, month, day, hour, minute, second)
	if zone == "" {
		if location == nil {
			location = time.Local
		}
		subtract, actual := localOffset(year, month, day, hour, minute, second, location)
		offset = actual
		seconds.Sub(seconds, big.NewInt(int64(subtract)))
		// Ruby local(UTC) is not flagged utc even though its offset is zero.
		utc = false
	} else if offset <= -86400 || offset >= 86400 {
		return Timestamp{}, fmt.Errorf("utc_offset out of range")
	}
	return Timestamp{seconds: seconds, fraction: fraction, OffsetSeconds: offset, UTC: utc}, nil
}

func validateCalendar(month, day, hour, minute, second int) error {
	if month < 1 || month > 12 {
		return fmt.Errorf("mon out of range")
	}
	if day < 1 || day > 31 {
		return fmt.Errorf("mday out of range")
	}
	if hour < 0 || hour > 24 {
		return fmt.Errorf("hour out of range")
	}
	if minute < 0 || minute > 59 || hour == 24 && minute != 0 {
		return fmt.Errorf("min out of range")
	}
	if second < 0 || second > 60 || hour == 24 && second != 0 {
		return fmt.Errorf("sec out of range")
	}
	return nil
}
func floorDivMod(a, b int) (int, int) {
	q, r := a/b, a%b
	if r < 0 {
		q--
		r += b
	}
	return q, r
}
func leapYear(year *big.Int) bool {
	return new(big.Int).Mod(year, big.NewInt(4)).Sign() == 0 && (new(big.Int).Mod(year, big.NewInt(100)).Sign() != 0 || new(big.Int).Mod(year, big.NewInt(400)).Sign() == 0)
}
func monthDays(year *big.Int, month int) int {
	days := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if month < 1 || month > 12 {
		return 0
	}
	if month == 2 && leapYear(year) {
		return 29
	}
	return days[month-1]
}

// Port the original apply_offset, including its day=1 rollover convention.
// Correcting invalid-calendar normalization belongs to a separate decision.
func applyOffset(year *big.Int, month, day, hour, minute, second, off int) (int, int, int, int, int) {
	if off < 0 {
		off = -off
		q, o := floorDivMod(off, 60)
		off = q
		if o != 0 {
			second += o
			q, second = floorDivMod(second, 60)
			off += q
		}
		q, o = floorDivMod(off, 60)
		off = q
		if o != 0 {
			minute += o
			q, minute = floorDivMod(minute, 60)
			off += q
		}
		q, o = floorDivMod(off, 24)
		off = q
		if o != 0 {
			hour += o
			q, hour = floorDivMod(hour, 24)
			off += q
		}
		if off != 0 {
			day += off
			if days := monthDays(year, month); days != 0 && days < day {
				month++
				if month > 12 {
					month = 1
					year.Add(year, big.NewInt(1))
				}
				day = 1
			}
		}
	} else if off > 0 {
		q, o := floorDivMod(off, 60)
		off = q
		if o != 0 {
			second -= o
			q, second = floorDivMod(second, 60)
			off -= q
		}
		q, o = floorDivMod(off, 60)
		off = q
		if o != 0 {
			minute -= o
			q, minute = floorDivMod(minute, 60)
			off -= q
		}
		q, o = floorDivMod(off, 24)
		off = q
		if o != 0 {
			hour -= o
			q, hour = floorDivMod(hour, 24)
			off -= q
		}
		if off != 0 {
			day -= off
			if day < 1 {
				month--
				if month < 1 {
					year.Sub(year, big.NewInt(1))
					month = 12
				}
				day = monthDays(year, month)
			}
		}
	}
	return month, day, hour, minute, second
}
func calendarSeconds(year *big.Int, month, day, hour, minute, second int) *big.Int {
	y := new(big.Int).Sub(year, big.NewInt(1))
	days := new(big.Int).Mul(y, big.NewInt(365))
	days.Add(days, new(big.Int).Div(y, big.NewInt(4)))
	days.Sub(days, new(big.Int).Div(y, big.NewInt(100)))
	days.Add(days, new(big.Int).Div(y, big.NewInt(400)))
	days.Sub(days, big.NewInt(719162))
	for m := 1; m < month; m++ {
		days.Add(days, big.NewInt(int64(monthDays(year, m))))
	}
	days.Add(days, big.NewInt(int64(day-1)))
	days.Mul(days, big.NewInt(86400))
	return days.Add(days, big.NewInt(int64(hour*3600+minute*60+second)))
}

func localOffset(year *big.Int, month, day, hour, minute, second int, location *time.Location) (int, int) {
	wall := calendarSeconds(year, month, day, hour, minute, second)
	// Zone tables use 64-bit seconds. Gregorian dates outside that domain use
	// their equivalent 400-year calendar in the future POSIX rule or initial
	// historical zone, matching Ruby's out-of-range local-time calendar lookup.
	margin := big.NewInt(7 * 86400)
	if !wall.IsInt64() || !new(big.Int).Add(wall, margin).IsInt64() || !new(big.Int).Sub(wall, margin).IsInt64() {
		cycle := new(big.Int).Mod(year, big.NewInt(400)).Int64()
		representative := int64(2400) + cycle
		if year.Sign() < 0 {
			representative = -10000 + cycle
		}
		wall = calendarSeconds(big.NewInt(representative), month, day, hour, minute, second)
	}
	naive := wall.Int64()
	offsets := map[int]bool{}
	// Include each nearby transition, even when Go Date picks the other side of
	// a gap/overlap. The original local time prefers standard/later overlap and
	// moves a nonexistent wall time forward across the gap.
	for probe := naive - 3*86400; probe <= naive+3*86400; {
		t := time.Unix(probe, 0).In(location)
		_, off := t.Zone()
		offsets[off] = true
		_, end := t.ZoneBounds()
		if end.IsZero() || end.Unix() <= probe {
			break
		}
		probe = end.Unix()
	}
	best := int64(0)
	selected := 0
	found := false
	forward := int64(1 << 62)
	for off := range offsets {
		candidate := naive - int64(off)
		_, actual := time.Unix(candidate, 0).In(location).Zone()
		delta := int64(actual - off)
		if delta == 0 {
			if !found || candidate > best {
				best = candidate
				selected = off
				found = true
			}
		} else if !found && delta > 0 && delta < forward {
			forward = delta
			selected = off
		}
	}
	_, actual := time.Unix(naive-int64(selected), 0).In(location).Zone()
	return selected, actual
}
