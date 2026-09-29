package auth

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestGrants(t *testing.T) {
	cases := []struct {
		held, want Scope
		ok         bool
	}{
		{ScopeAdmin, ScopePaymentsManual, true},
		{ScopeInvoicesWrite, ScopeInvoicesRead, true},
		{ScopeInvoicesRead, ScopeInvoicesWrite, false},
		{ScopePaymentsWrite, ScopePaymentsManual, false},
		{ScopePaymentsWrite, ScopePaymentsRead, true},
		{ScopeCustomersWrite, ScopeInvoicesRead, false},
	}
	for _, c := range cases {
		if got := Grants(c.held, c.want); got != c.ok {
			t.Errorf("Grants(%s, %s) = %v", c.held, c.want, got)
		}
	}
}

func TestParseAllRejectsUnknown(t *testing.T) {
	if _, err := ParseAll([]string{"invoices:read", "invoices:raed"}); err == nil {
		t.Fatal("typo accepted")
	}
	got, err := ParseAll([]string{"invoices:read", "invoices:read", "admin"})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestResolveTenant(t *testing.T) {
	own := uuid.New()
	tenantKey := &Principal{KeyTenantID: &own}
	if id, err := tenantKey.ResolveTenant(""); err != nil || id != own {
		t.Fatalf("tenant key without header: %v %v", id, err)
	}
	if _, err := tenantKey.ResolveTenant(uuid.NewString()); !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("tenant key must not reach another tenant, got %v", err)
	}
	platform := &Principal{}
	if _, err := platform.ResolveTenant(""); !errors.Is(err, ErrTenantRequired) {
		t.Fatalf("platform key without tenant: %v", err)
	}
	other := uuid.New()
	if id, err := platform.ResolveTenant(other.String()); err != nil || id != other {
		t.Fatalf("platform key with tenant: %v %v", id, err)
	}
}

func TestGenerate(t *testing.T) {
	g, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeToken(g.Token) || Hash(g.Token) != g.Hash || g.Token[:len(g.Prefix)] != g.Prefix {
		t.Fatalf("bad key %+v", g)
	}
}
