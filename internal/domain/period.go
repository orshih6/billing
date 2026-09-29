package domain

import (
	"fmt"
	"time"
)

// Interval is how often a recurring price bills.
type Interval string

const (
	IntervalDay     Interval = "day"
	IntervalWeek    Interval = "week"
	IntervalMonth   Interval = "month"
	IntervalYear    Interval = "year"
	IntervalOneTime Interval = "one_time"
)

// ParseInterval validates an interval name.
func ParseInterval(s string) (Interval, error) {
	switch i := Interval(s); i {
	case IntervalDay, IntervalWeek, IntervalMonth, IntervalYear, IntervalOneTime:
		return i, nil
	}
	return "", fmt.Errorf("unknown interval %q (day, week, month, year, one_time)", s)
}

// Recurring reports whether a price with this interval can back a subscription.
func (i Interval) Recurring() bool { return i != IntervalOneTime && i != "" }

// AddInterval advances t by count intervals.
//
// Months are calendar months anchored on the original day: a period starting
// on 31 January ends on 28/29 February, and the next one on 31 March, not 28
// March. Go's AddDate would overflow 31 Jan + 1 month into March, so month and
// year arithmetic clamps to the last day of the target month instead.
func AddInterval(t time.Time, i Interval, count int) time.Time {
	if count < 1 {
		count = 1
	}
	switch i {
	case IntervalDay:
		return t.AddDate(0, 0, count)
	case IntervalWeek:
		return t.AddDate(0, 0, 7*count)
	case IntervalMonth:
		return addMonthsClamped(t, count)
	case IntervalYear:
		return addMonthsClamped(t, 12*count)
	}
	return t
}

// NthPeriodEnd returns the end of the n-th period (1-based) counted from an
// anchor. Computing every period from the anchor, rather than chaining
// AddInterval, is what keeps the 31st-of-the-month customer on the 31st after
// passing through February.
func NthPeriodEnd(anchor time.Time, i Interval, count, n int) time.Time {
	if count < 1 {
		count = 1
	}
	return AddInterval(anchor, i, count*n)
}

func addMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	targetMonth := time.Month(int(m) + months)
	// Day 0 of the following month is the last day of the target month.
	last := time.Date(y, targetMonth+1, 0, 0, 0, 0, 0, t.Location()).Day()
	if d > last {
		d = last
	}
	return time.Date(y, targetMonth, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// Prorate returns the share of amount that corresponds to the time left in a
// period: amount * remaining / total, truncated toward zero. Truncation always
// favours the customer by at most one minor unit.
func Prorate(amount int64, periodStart, periodEnd, at time.Time) int64 {
	total := periodEnd.Sub(periodStart)
	if total <= 0 || !at.Before(periodEnd) {
		return 0
	}
	remaining := periodEnd.Sub(at)
	if remaining >= total {
		return amount
	}
	// Seconds keep the product inside int64 for any realistic amount: a
	// year is ~3.2e7 s, so amounts up to ~2.8e11 minor units are exact.
	return amount * int64(remaining/time.Second) / int64(total/time.Second)
}
