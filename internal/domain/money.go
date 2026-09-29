// Package domain holds the billing rules that need no infrastructure: money,
// currencies, billing periods and the status vocabulary. It imports nothing
// from this module, so every rule here is testable without a database.
package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Amounts are always int64 in the currency's minor unit (cents, möngö). No
// float ever touches money anywhere in this service.

// Currency describes an ISO 4217 currency this service accepts.
type Currency struct {
	Code string
	// Exponent is the number of minor-unit digits: 2 for USD (cents), 0 for JPY.
	Exponent int
}

// currencies is the fixed catalogue. There is no FX: an account, a price and an
// invoice each have exactly one currency, and they must agree.
var currencies = map[string]Currency{
	"MNT": {"MNT", 2},
	"USD": {"USD", 2},
	"EUR": {"EUR", 2},
	"CNY": {"CNY", 2},
	"KRW": {"KRW", 0},
	"JPY": {"JPY", 0},
	"RUB": {"RUB", 2},
	"GBP": {"GBP", 2},
}

// DefaultCurrency is used when a tenant does not choose one.
const DefaultCurrency = "MNT"

// LookupCurrency normalises and validates a currency code.
func LookupCurrency(code string) (Currency, error) {
	c, ok := currencies[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, fmt.Errorf("unsupported currency %q", code)
	}
	return c, nil
}

// Currencies lists the supported codes.
func Currencies() []Currency {
	out := make([]Currency, 0, len(currencies))
	for _, c := range currencies {
		out = append(out, c)
	}
	return out
}

// FormatAmount renders minor units for humans: FormatAmount(123456, "MNT") is
// "1,234.56 MNT".
func FormatAmount(amount int64, code string) string {
	c, err := LookupCurrency(code)
	if err != nil {
		return strconv.FormatInt(amount, 10) + " " + code
	}
	neg := amount < 0
	if neg {
		amount = -amount
	}
	div := int64(1)
	for range c.Exponent {
		div *= 10
	}
	whole, frac := amount/div, amount%div

	s := groupThousands(strconv.FormatInt(whole, 10))
	if c.Exponent > 0 {
		s += "." + fmt.Sprintf("%0*d", c.Exponent, frac)
	}
	if neg {
		s = "-" + s
	}
	return s + " " + c.Code
}

// ParseAmount reads a human amount ("1234.5", "1,234.50") into minor units. It
// rejects more fractional digits than the currency has, instead of rounding.
func ParseAmount(raw, code string) (int64, error) {
	c, err := LookupCurrency(code)
	if err != nil {
		return 0, err
	}
	s := strings.ReplaceAll(strings.TrimSpace(raw), ",", "")
	if s == "" {
		return 0, fmt.Errorf("amount is required")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && len(frac) > c.Exponent {
		return 0, fmt.Errorf("%s allows at most %d decimal places", c.Code, c.Exponent)
	}
	for len(frac) < c.Exponent {
		frac += "0"
	}
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", raw)
	}
	return n, nil
}

func groupThousands(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}
