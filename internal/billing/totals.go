package billing

import (
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/models"
)

// Totals is the arithmetic of an invoice.
type Totals struct {
	Subtotal            int64
	DiscountTotal       int64
	DiscountDescription string
	TaxRateBps          int
	TaxName             string
	TaxInclusive        bool
	TaxTotal            int64
	Total               int64
}

// computeTotals derives an invoice's discount, tax and total from its lines,
// its discount and the tenant/customer tax settings, and stores them.
//
//	subtotal = Σ lines
//	discount = percent of (or fixed amount up to) the eligible lines
//	base     = subtotal − discount
//	tax      = exclusive: round(base × rate)          total = base + tax
//	           inclusive: base − round(base / (1+rate)) total = base
//
// Rounding is half-up to the minor unit, once per invoice.
func (s *Service) computeTotals(tx *gorm.DB, inv *models.Invoice) error {
	var lines []models.InvoiceLine
	if err := tx.Where("invoice_id = ?", inv.ID).Find(&lines).Error; err != nil {
		return err
	}
	t := Totals{}
	for _, l := range lines {
		t.Subtotal += l.Amount
	}

	if inv.DiscountID != nil {
		var d models.Discount
		var c models.Coupon
		if err := tx.Take(&d, "id = ?", *inv.DiscountID).Error; err != nil {
			return err
		}
		if err := tx.Take(&c, "id = ?", d.CouponID).Error; err != nil {
			return err
		}
		var eligible int64
		for _, l := range lines {
			if c.AppliesTo(l.PlanID) {
				eligible += l.Amount
			}
		}
		switch {
		case c.PercentOff != nil:
			t.DiscountTotal = eligible * int64(*c.PercentOff) / 100
		case c.AmountOff != nil && c.Currency == inv.Currency:
			t.DiscountTotal = min(*c.AmountOff, eligible)
		}
		if t.DiscountTotal > 0 {
			t.DiscountDescription = describeCoupon(&c)
		}
	}

	var cust models.Customer
	if err := tx.Take(&cust, "id = ?", inv.CustomerID).Error; err != nil {
		return err
	}
	_, set, err := s.settings(tx, inv.TenantID)
	if err != nil {
		return err
	}
	if !cust.TaxExempt {
		if cust.TaxRateBps != nil {
			t.TaxRateBps = *cust.TaxRateBps
		} else if set.Tax != nil && set.Tax.Enabled {
			t.TaxRateBps = set.Tax.RateBps
		}
		if t.TaxRateBps > 0 {
			t.TaxName = "Tax"
			if set.Tax != nil {
				t.TaxInclusive = set.Tax.Inclusive
				if set.Tax.Name != "" {
					t.TaxName = set.Tax.Name
				}
			}
		}
	}
	base := t.Subtotal - t.DiscountTotal
	rate := int64(t.TaxRateBps)
	switch {
	case rate == 0:
		t.Total = base
	case t.TaxInclusive:
		net := (base*10000 + (10000+rate)/2) / (10000 + rate)
		t.TaxTotal = base - net
		t.Total = base
	default:
		t.TaxTotal = (base*rate + 5000) / 10000
		t.Total = base + t.TaxTotal
	}

	inv.Subtotal, inv.DiscountTotal, inv.DiscountDescription = t.Subtotal, t.DiscountTotal, t.DiscountDescription
	inv.TaxRateBps, inv.TaxName, inv.TaxInclusive, inv.TaxTotal = t.TaxRateBps, t.TaxName, t.TaxInclusive, t.TaxTotal
	inv.Total = t.Total
	inv.AmountDue = max(inv.Total-inv.AmountPaid-inv.CreditApplied, 0)
	return tx.Model(&models.Invoice{}).Where("id = ?", inv.ID).Updates(map[string]any{
		"subtotal": inv.Subtotal, "discount_total": inv.DiscountTotal, "discount_description": inv.DiscountDescription,
		"tax_rate_bps": inv.TaxRateBps, "tax_name": inv.TaxName, "tax_inclusive": inv.TaxInclusive, "tax_total": inv.TaxTotal,
		"total": inv.Total, "amount_due": inv.AmountDue,
	}).Error
}

// revenueAndTax splits a finalized invoice for the ledger. Revenue is net of
// tax; the discount is booked separately so reports show gross sales and
// discounts given.
func revenueAndTax(inv *models.Invoice) (revenue, discount, tax int64) {
	revenue = inv.Subtotal
	if inv.TaxInclusive {
		revenue -= inv.TaxTotal
	}
	return revenue, inv.DiscountTotal, inv.TaxTotal
}
