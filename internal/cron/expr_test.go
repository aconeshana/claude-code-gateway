package cron

import (
	"testing"
	"time"
)

func TestParseSchedule_Basic(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr bool
	}{
		{"* * * * *", false},
		{"0 0 1 1 0", false},
		{"*/5 * * * *", false},
		{"0 9 * * 1-5", false},
		{"30 14 1,15 * *", false},
		{"0 0 1-7/2 * *", false},
		{"0 0 * JAN,FEB *", false},
		{"0 0 * * MON-FRI", false},
		{"0 0 * * 7", false}, // 7 = Sunday (normalised to 0)
		// Errors
		{"* * *", true},       // too few fields
		{"* * * * * *", true}, // too many fields
		{"60 * * * *", true},  // minute out of range
		{"* 24 * * *", true},  // hour out of range
		{"* * 0 * *", true},   // dom out of range (min is 1)
		{"* * * 13 *", true},  // month out of range
		{"* * * * 8", true},   // dow out of range
		{"* * * * FOO", true}, // invalid name
		{"*/0 * * * *", true}, // step of 0
		{"5-2 * * * *", true}, // inverted range
	}
	for _, tt := range tests {
		_, err := ParseSchedule(tt.expr)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSchedule(%q): err=%v, wantErr=%v", tt.expr, err, tt.wantErr)
		}
	}
}

func TestSchedule_Match(t *testing.T) {
	loc := time.Local
	tests := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"* * * * *", time.Date(2026, 6, 1, 12, 30, 0, 0, loc), true},
		{"30 12 * * *", time.Date(2026, 6, 1, 12, 30, 0, 0, loc), true},
		{"30 12 * * *", time.Date(2026, 6, 1, 12, 31, 0, 0, loc), false},
		{"0 9 * * 1-5", time.Date(2026, 6, 1, 9, 0, 0, 0, loc), true},   // Mon
		{"0 9 * * 1-5", time.Date(2026, 5, 31, 9, 0, 0, 0, loc), false}, // Sun
		{"*/15 * * * *", time.Date(2026, 1, 1, 0, 0, 0, 0, loc), true},
		{"*/15 * * * *", time.Date(2026, 1, 1, 0, 15, 0, 0, loc), true},
		{"*/15 * * * *", time.Date(2026, 1, 1, 0, 7, 0, 0, loc), false},
		{"0 0 1 JAN *", time.Date(2026, 1, 1, 0, 0, 0, 0, loc), true},
		{"0 0 1 JAN *", time.Date(2026, 2, 1, 0, 0, 0, 0, loc), false},
		{"0 0 * * SUN", time.Date(2026, 6, 7, 0, 0, 0, 0, loc), true}, // Sun
	}
	for _, tt := range tests {
		s, err := ParseSchedule(tt.expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", tt.expr, err)
		}
		if got := s.Match(tt.t); got != tt.want {
			t.Errorf("Match(%q, %v) = %v, want %v", tt.expr, tt.t, got, tt.want)
		}
	}
}

func TestSchedule_NextAfter(t *testing.T) {
	loc := time.Local
	tests := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{
			"0 9 * * *",
			time.Date(2026, 6, 1, 8, 0, 0, 0, loc),
			time.Date(2026, 6, 1, 9, 0, 0, 0, loc),
		},
		{
			"0 9 * * *",
			time.Date(2026, 6, 1, 9, 0, 0, 0, loc),
			time.Date(2026, 6, 2, 9, 0, 0, 0, loc), // after current match
		},
		{
			"30 14 1,15 * *",
			time.Date(2026, 6, 1, 14, 0, 0, 0, loc),
			time.Date(2026, 6, 1, 14, 30, 0, 0, loc),
		},
		{
			"30 14 1,15 * *",
			time.Date(2026, 6, 1, 14, 30, 0, 0, loc),
			time.Date(2026, 6, 15, 14, 30, 0, 0, loc),
		},
		{
			"*/5 * * * *",
			time.Date(2026, 6, 1, 12, 3, 0, 0, loc),
			time.Date(2026, 6, 1, 12, 5, 0, 0, loc),
		},
		{
			"0 0 29 2 *", // Feb 29 — only leap years
			time.Date(2026, 1, 1, 0, 0, 0, 0, loc),
			time.Date(2028, 2, 29, 0, 0, 0, 0, loc),
		},
		{
			"0 9 * * 1",                             // Mondays at 9
			time.Date(2026, 6, 1, 10, 0, 0, 0, loc), // Mon after 9am
			time.Date(2026, 6, 8, 9, 0, 0, 0, loc),  // next Mon
		},
	}
	for _, tt := range tests {
		s, err := ParseSchedule(tt.expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", tt.expr, err)
		}
		got := s.NextAfter(tt.from)
		if !got.Equal(tt.want) {
			t.Errorf("NextAfter(%q, %v):\n  got  %v\n  want %v", tt.expr, tt.from, got, tt.want)
		}
	}
}

func TestSchedule_MonthNames(t *testing.T) {
	s, err := ParseSchedule("0 0 1 jan,mar,may,jul,sep,nov *")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.Local
	if !s.Match(time.Date(2026, 1, 1, 0, 0, 0, 0, loc)) {
		t.Error("should match Jan 1")
	}
	if s.Match(time.Date(2026, 2, 1, 0, 0, 0, 0, loc)) {
		t.Error("should not match Feb 1")
	}
}

func TestSchedule_DowNames(t *testing.T) {
	s, err := ParseSchedule("0 0 * * mon,wed,fri")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.Local
	// 2026-06-01 is Monday
	if !s.Match(time.Date(2026, 6, 1, 0, 0, 0, 0, loc)) {
		t.Error("should match Monday")
	}
	// 2026-06-02 is Tuesday
	if s.Match(time.Date(2026, 6, 2, 0, 0, 0, 0, loc)) {
		t.Error("should not match Tuesday")
	}
	// 2026-06-03 is Wednesday
	if !s.Match(time.Date(2026, 6, 3, 0, 0, 0, 0, loc)) {
		t.Error("should match Wednesday")
	}
}

func TestSchedule_StepRange(t *testing.T) {
	s, err := ParseSchedule("0-30/10 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.Local
	for _, min := range []int{0, 10, 20, 30} {
		if !s.Match(time.Date(2026, 1, 1, 0, min, 0, 0, loc)) {
			t.Errorf("should match minute %d", min)
		}
	}
	for _, min := range []int{5, 15, 25, 31, 40} {
		if s.Match(time.Date(2026, 1, 1, 0, min, 0, 0, loc)) {
			t.Errorf("should not match minute %d", min)
		}
	}
}

func TestFieldSet_Next(t *testing.T) {
	var fs fieldSet
	fs.lo = 0
	fs.hi = 59
	fs.set(0)
	fs.set(15)
	fs.set(30)
	fs.set(45)

	v, wrapped := fs.next(0)
	if v != 0 || wrapped {
		t.Errorf("next(0) = %d, wrapped=%v", v, wrapped)
	}
	v, wrapped = fs.next(1)
	if v != 15 || wrapped {
		t.Errorf("next(1) = %d, wrapped=%v", v, wrapped)
	}
	v, wrapped = fs.next(46)
	if v != 0 || !wrapped {
		t.Errorf("next(46) = %d, wrapped=%v", v, wrapped)
	}
}
