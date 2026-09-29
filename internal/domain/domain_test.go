package domain

import (
	"testing"
	"time"
)

func TestAddIntervalClampsMonthEnds(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		n    int
		want time.Time
	}{
		{1, time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC)},
		{2, time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC)},
		{3, time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)},
		{13, time.Date(2027, 2, 28, 10, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := NthPeriodEnd(jan31, IntervalMonth, 1, c.n); !got.Equal(c.want) {
			t.Errorf("period %d: got %v want %v", c.n, got, c.want)
		}
	}
	// Leap year.
	if got := AddInterval(time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC), IntervalMonth, 1); got.Day() != 29 {
		t.Errorf("leap February: got %v", got)
	}
	if got := AddInterval(time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), IntervalYear, 1); !got.Equal(time.Date(2029, 2, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("29 Feb + 1y: got %v", got)
	}
	if got := AddInterval(jan31, IntervalWeek, 2); !got.Equal(jan31.AddDate(0, 0, 14)) {
		t.Errorf("weeks: got %v", got)
	}
}

func TestProrate(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 30)
	if got := Prorate(3000, start, end, start.AddDate(0, 0, 10)); got != 2000 {
		t.Errorf("two thirds left: got %d", got)
	}
	if got := Prorate(3000, start, end, end); got != 0 {
		t.Errorf("at end: got %d", got)
	}
	if got := Prorate(3000, start, end, start.Add(-time.Hour)); got != 3000 {
		t.Errorf("before start: got %d", got)
	}
	// Truncates rather than rounds.
	if got := Prorate(100, start, start.Add(3*time.Second), start.Add(time.Second)); got != 66 {
		t.Errorf("truncation: got %d", got)
	}
}

func TestAmountRoundTrip(t *testing.T) {
	cases := []struct {
		raw, cur string
		minor    int64
		pretty   string
	}{
		{"1234.56", "MNT", 123456, "1,234.56 MNT"},
		{"1,000", "USD", 100000, "1,000.00 USD"},
		{"0.5", "EUR", 50, "0.50 EUR"},
		{"1500", "KRW", 1500, "1,500 KRW"},
	}
	for _, c := range cases {
		got, err := ParseAmount(c.raw, c.cur)
		if err != nil || got != c.minor {
			t.Errorf("ParseAmount(%q, %s) = %d, %v; want %d", c.raw, c.cur, got, err, c.minor)
		}
		if s := FormatAmount(c.minor, c.cur); s != c.pretty {
			t.Errorf("FormatAmount(%d, %s) = %q; want %q", c.minor, c.cur, s, c.pretty)
		}
	}
	if _, err := ParseAmount("1.234", "USD"); err == nil {
		t.Error("too many decimals must be rejected, not rounded")
	}
	if _, err := ParseAmount("1.5", "JPY"); err == nil {
		t.Error("JPY has no minor unit")
	}
	if _, err := LookupCurrency("XXX"); err == nil {
		t.Error("unknown currency accepted")
	}
}

func TestReferenceAlphabet(t *testing.T) {
	for range 200 {
		r := NewReference(6)
		for _, ch := range r {
			if ch == '0' || ch == 'O' || ch == '1' || ch == 'I' {
				t.Fatalf("ambiguous character in %q", r)
			}
		}
	}
}
