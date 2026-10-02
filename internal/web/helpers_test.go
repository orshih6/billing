package web

import (
	"strings"
	"testing"
)

func TestPlanCodeFrom(t *testing.T) {
	cases := map[string]string{
		"Pro":               "pro",
		"Pro Monthly":       "pro-monthly",
		"  Team (50 seats)": "team-50-seats",
		"Plus+  Extra!":     "plus-extra",
	}
	for in, want := range cases {
		if got := planCodeFrom(in); got != want {
			t.Errorf("planCodeFrom(%q) = %q, want %q", in, got, want)
		}
	}
	if got := planCodeFrom("Үндсэн"); !strings.HasPrefix(got, "plan-") || len(got) != len("plan-")+8 {
		t.Errorf("non-ASCII name: got %q, want plan-xxxxxxxx", got)
	}
}
