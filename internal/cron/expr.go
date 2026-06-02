package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule holds the parsed representation of a 5-field cron expression.
// Each field is stored as a bitset where bit N being set means value N is
// allowed. This gives O(1) matching and simple iteration for NextAfter.
type Schedule struct {
	fields [5]fieldSet
}

// fieldSet is a compact bitset for one cron field (minute, hour, etc.).
// bits uses the lower 64 bits — enough for all cron ranges (max 60 values).
type fieldSet struct {
	bits uint64
	lo   byte
	hi   byte
}

// set marks value v as allowed.
func (f *fieldSet) set(v byte) { f.bits |= 1 << v }

// has returns true if value v is allowed.
func (f fieldSet) has(v byte) bool { return f.bits&(1<<v) != 0 }

// next returns the smallest allowed value >= v, wrapping around if needed.
// The second return value is true when the search wrapped past hi.
func (f fieldSet) next(v byte) (byte, bool) {
	for i := v; i <= f.hi; i++ {
		if f.has(i) {
			return i, false
		}
	}
	for i := f.lo; i < v; i++ {
		if f.has(i) {
			return i, true
		}
	}
	return f.lo, true
}

// month name aliases (case-insensitive).
var monthNames = map[string]byte{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4,
	"may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// dow name aliases (case-insensitive). Both 0 and 7 map to Sunday.
var dowNames = map[string]byte{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3,
	"thu": 4, "fri": 5, "sat": 6,
}

// fieldDef describes the valid range and optional name mapping for one
// positional field in the cron expression.
type fieldDef struct {
	lo, hi byte
	names  map[string]byte
}

var fieldDefs = [5]fieldDef{
	{0, 59, nil},        // minute
	{0, 23, nil},        // hour
	{1, 31, nil},        // day-of-month
	{1, 12, monthNames}, // month
	{0, 6, dowNames},    // day-of-week (normalised: 7→0)
}

// ParseSchedule parses a standard 5-field cron expression and returns
// a Schedule. Supported per-field syntax:
//
//   - — all values in range
//     N        — single value
//     N-M      — inclusive range
//     N,M,K    — list of values
//     */S      — every S values across the full range
//     N-M/S    — every S values within N-M
//
// Month and day-of-week fields accept 3-letter English names (JAN-DEC,
// SUN-SAT) in any case. Day-of-week value 7 is normalised to 0 (Sunday).
func ParseSchedule(expr string) (*Schedule, error) {
	tokens := strings.Fields(strings.TrimSpace(expr))
	if len(tokens) != 5 {
		return nil, fmt.Errorf("want 5 fields, got %d", len(tokens))
	}
	var s Schedule
	for i, tok := range tokens {
		fs, err := parseToken(tok, fieldDefs[i])
		if err != nil {
			return nil, fmt.Errorf("field %d (%s): %w", i, tok, err)
		}
		s.fields[i] = fs
	}
	return &s, nil
}

// parseToken turns a single cron field token (e.g. "1-5/2", "*/10", "MON,FRI")
// into a fieldSet.
func parseToken(tok string, def fieldDef) (fieldSet, error) {
	fs := fieldSet{lo: def.lo, hi: def.hi}
	for _, part := range strings.Split(tok, ",") {
		if err := parsePart(part, def, &fs); err != nil {
			return fs, err
		}
	}
	return fs, nil
}

func parsePart(part string, def fieldDef, fs *fieldSet) error {
	var step int
	if idx := strings.IndexByte(part, '/'); idx >= 0 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("bad step: %s", part)
		}
		step = s
		part = part[:idx]
	}

	lo, hi := def.lo, def.hi

	switch {
	case part == "*":
		// full range, step applied below
	case strings.ContainsRune(part, '-'):
		dash := strings.IndexByte(part, '-')
		a, err := resolveValue(part[:dash], def)
		if err != nil {
			return err
		}
		b, err := resolveValue(part[dash+1:], def)
		if err != nil {
			return err
		}
		if a > b {
			return fmt.Errorf("inverted range: %d-%d", a, b)
		}
		lo, hi = a, b
	default:
		v, err := resolveValue(part, def)
		if err != nil {
			return err
		}
		if step == 0 {
			fs.set(v)
			return nil
		}
		lo = v
	}

	if step == 0 {
		step = 1
	}
	for v := lo; v <= hi; v += byte(step) {
		fs.set(v)
	}
	return nil
}

// resolveValue converts a string token to a numeric value, accepting
// either a plain integer or a name alias (JAN, MON, etc.).
func resolveValue(s string, def fieldDef) (byte, error) {
	if def.names != nil {
		if v, ok := def.names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value: %s", s)
	}
	v := byte(n)
	// Normalise dow 7 → 0 (both mean Sunday).
	if def.hi == 6 && v == 7 {
		v = 0
	}
	if v < def.lo || v > def.hi {
		return 0, fmt.Errorf("value %d out of range [%d, %d]", v, def.lo, def.hi)
	}
	return v, nil
}

// Match reports whether the schedule matches time t (truncated to the minute).
func (s *Schedule) Match(t time.Time) bool {
	return s.fields[0].has(byte(t.Minute())) &&
		s.fields[1].has(byte(t.Hour())) &&
		s.fields[2].has(byte(t.Day())) &&
		s.fields[3].has(byte(t.Month())) &&
		s.fields[4].has(byte(t.Weekday()%7))
}

// NextAfter returns the earliest time after t that matches the schedule.
// Searches up to 4 years ahead; returns the zero Time if no match is found
// (theoretically impossible for valid schedules, but guards infinite loops).
func (s *Schedule) NextAfter(t time.Time) time.Time {
	// Start from the next whole minute.
	t = t.Truncate(time.Minute).Add(time.Minute)
	loc := t.Location()
	year := t.Year()
	ceiling := year + 4

	for t.Year() <= ceiling {
		// Month
		mon := byte(t.Month())
		nmon, wrapped := s.fields[3].next(mon)
		if wrapped {
			t = time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if nmon != mon {
			t = time.Date(t.Year(), time.Month(nmon), 1, 0, 0, 0, 0, loc)
			continue
		}

		// Day-of-month
		day := byte(t.Day())
		nday, wrapped := s.fields[2].next(day)
		if wrapped || int(nday) > daysIn(t.Month(), t.Year()) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if nday != day {
			t = time.Date(t.Year(), t.Month(), int(nday), 0, 0, 0, 0, loc)
			continue
		}

		// Day-of-week check (additional filter, not a substitute for dom).
		dow := byte(t.Weekday() % 7)
		if !s.fields[4].has(dow) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}

		// Hour
		hr := byte(t.Hour())
		nhr, wrapped := s.fields[1].next(hr)
		if wrapped {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if nhr != hr {
			t = time.Date(t.Year(), t.Month(), t.Day(), int(nhr), 0, 0, 0, loc)
			continue
		}

		// Minute
		mn := byte(t.Minute())
		nmn, wrapped := s.fields[0].next(mn)
		if wrapped {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if nmn != mn {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), int(nmn), 0, 0, loc)
			continue
		}

		return t
	}

	return time.Time{}
}

// daysIn returns the number of days in month m of year y.
func daysIn(m time.Month, y int) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
