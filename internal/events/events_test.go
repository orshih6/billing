package events

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

func TestSignatureFormat(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	sig := Sign("whsec_x", ts, []byte(`{"a":1}`))
	if !strings.HasPrefix(sig, "t=1700000000,v1=") || len(sig) != len("t=1700000000,v1=")+64 {
		t.Fatalf("signature %q", sig)
	}
	if Sign("whsec_y", ts, []byte(`{"a":1}`)) == sig {
		t.Fatal("secret must change the signature")
	}
}

// Deliveries are signed, retried on failure and succeed once the receiver
// answers 2xx; endpoints only get the event types they asked for.
func TestDispatchRetriesAndSigns(t *testing.T) {
	db := testsupport.DB(t)
	var calls atomic.Int32
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody, gotSig = string(body), r.Header.Get("X-Billing-Signature")
		if calls.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	tenant := uuid.New()
	wanted := models.WebhookEndpoint{TenantID: tenant, URL: srv.URL, Secret: "whsec_test", Enabled: true, EventTypes: []string{InvoicePaid}}
	other := models.WebhookEndpoint{TenantID: tenant, URL: srv.URL + "/other", Secret: "s", Enabled: true, EventTypes: []string{PaymentFailed}}
	db.Create(&wanted)
	db.Create(&other)
	if err := Emit(db, tenant, InvoicePaid, map[string]any{"id": "inv_1"}); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.Model(&models.WebhookDelivery{}).Count(&n)
	if n != 1 {
		t.Fatalf("%d deliveries queued, want 1 (filtered by type)", n)
	}

	d := &Dispatcher{DB: db, Client: srv.Client(), MaxAttempts: 5, UserAgent: "test", Log: testsupport.Logger()}
	d.RunOnce(context.Background())
	var del models.WebhookDelivery
	db.Take(&del)
	if del.Status != "pending" || del.Attempts != 1 || del.LastStatusCode != 500 {
		t.Fatalf("after failure: %+v", del)
	}
	// Make it due again and retry.
	db.Model(&del).Update("next_attempt_at", time.Now().Add(-time.Second))
	d.RunOnce(context.Background())
	db.Take(&del, "id = ?", del.ID)
	if del.Status != "succeeded" || del.Attempts != 2 || del.DeliveredAt == nil {
		t.Fatalf("after success: %+v", del)
	}
	ts, _, _ := strings.Cut(strings.TrimPrefix(gotSig, "t="), ",")
	unix, _ := strconv.ParseInt(ts, 10, 64)
	if want := Sign("whsec_test", time.Unix(unix, 0), []byte(gotBody)); want != gotSig {
		t.Fatalf("signature does not verify: %s vs %s", gotSig, want)
	}
	if !strings.Contains(gotBody, `"type":"invoice.paid"`) {
		t.Fatalf("envelope: %s", gotBody)
	}
}
