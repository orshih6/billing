package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	for _, s := range []string{"10.1.2.3", "127.0.0.1", "169.254.169.254", "172.20.0.1", "192.168.1.1",
		"100.101.102.103", "::1", "fe80::1", "fd00::1", "::ffff:10.0.0.1", "0.0.0.0"} {
		if IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111"} {
		if !IsPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

func TestClientRefusesPrivateAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()

	_, err := Client(2*time.Second, false).Get(srv.URL) // 127.0.0.1
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback must be refused at dial time, got %v", err)
	}
	res, err := Client(2*time.Second, true).Get(srv.URL)
	if err != nil || res.StatusCode != 204 {
		t.Fatalf("allowPrivate must reach it: %v", err)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	res, err := Client(2*time.Second, true).Get(srv.URL)
	if err != nil || res.StatusCode != http.StatusFound {
		t.Fatalf("redirect must be returned, not followed: %v %v", res, err)
	}
}

func TestCheckURL(t *testing.T) {
	ctx := context.Background()
	for _, u := range []string{"http://localhost/x", "http://127.0.0.1/", "http://10.0.0.5/hook",
		"http://postgres.shared.svc.cluster.local:5432", "http://169.254.169.254/latest", "ftp://example.com",
		"https://user:pass@example.com/", "http://[::1]/", "http://metadata"} {
		if err := CheckURL(ctx, u, false); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	if err := CheckURL(ctx, "https://203.0.114.10/hook", false); err != nil {
		t.Errorf("public IP refused: %v", err)
	}
	if err := CheckURL(ctx, "http://localhost:3000/hook", true); err != nil {
		t.Errorf("allowPrivate: %v", err)
	}
}
