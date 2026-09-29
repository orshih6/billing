// Package web is the operator UI: server-rendered HTML embedded in the binary.
//
// You log in by pasting an API key. The key is kept in an AES-GCM encrypted,
// HttpOnly, SameSite=Strict cookie and every page calls the billing service as
// that key, so the UI can do exactly what the key's scopes allow and nothing
// more. A platform key additionally gets a tenant switcher.
package web

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/domain"
	"github.com/orshih6/billing/internal/models"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "billing_session"
	tenantCookie  = "billing_tenant"
	flashCookie   = "billing_flash"
	sessionTTL    = 12 * time.Hour
)

// UI serves the web interface.
type UI struct {
	svc    *billing.Service
	auth   *auth.Authenticator
	cfg    *config.Config
	log    *slog.Logger
	aead   cipher.AEAD
	macKey []byte
	pages  map[string]*template.Template
}

// New builds the UI.
func New(svc *billing.Service, a *auth.Authenticator, cfg *config.Config, log *slog.Logger) (*UI, error) {
	secret := []byte(cfg.Security.UICookieKey)
	if len(secret) == 0 {
		if cfg.IsProduction() {
			return nil, errors.New("UI_COOKIE_KEY is required in production")
		}
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
		log.Warn("UI_COOKIE_KEY not set: using a random key, UI sessions end on restart")
	} else if len(secret) < 32 {
		return nil, errors.New("UI_COOKIE_KEY must be at least 32 characters")
	}
	encKey := sha256.Sum256(append([]byte("enc:"), secret...))
	macKey := sha256.Sum256(append([]byte("mac:"), secret...))
	block, err := aes.NewCipher(encKey[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	u := &UI{svc: svc, auth: a, cfg: cfg, log: log, aead: aead, macKey: macKey[:]}
	if err := u.parseTemplates(); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *UI) parseTemplates() error {
	funcs := template.FuncMap{
		"money": domain.FormatAmount,
		"amountInput": func(amount int64, cur string) string {
			s := domain.FormatAmount(amount, cur)
			s = strings.TrimSuffix(s, " "+cur)
			return strings.ReplaceAll(s, ",", "")
		},
		"dt":    func(t any) string { return fmtTime(t, "2006-01-02 15:04") },
		"date":  func(t any) string { return fmtTime(t, "2006-01-02") },
		"short": func(id uuid.UUID) string { return id.String()[:8] },
		"str": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		"json": func(v any) string {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
		"join":       strings.Join,
		"keyDisplay": auth.Display,
		"scopeList":  func(s []string) string { return strings.Join(s, ", ") },
		"add":        func(a, b int) int { return a + b },
		"currencies": func() []string {
			out := []string{}
			for _, c := range domain.Currencies() {
				out = append(out, c.Code)
			}
			sortStrings(out)
			return out
		},
		"statusLabel": statusLabel,
		"kindLabel": func(v any) string {
			switch fmt.Sprint(v) {
			case "subscription_create":
				return "First invoice of a subscription"
			case "subscription_cycle":
				return "Renewal"
			case "proration":
				return "Mid-period change"
			case "one_off":
				return "One-off"
			}
			return fmt.Sprint(v)
		},
		"mul":    func(a int64, b int) int64 { return a * int64(b) },
		"ptrInt": func(v int) *int { return &v },
		"derefAmount": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"percent": func(bps int) string {
			if bps%100 == 0 {
				return fmt.Sprintf("%d%%", bps/100)
			}
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(bps)/100), "0"), ".") + "%"
		},
		"percentInput": func(bps *int) string {
			if bps == nil {
				return ""
			}
			return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(*bps)/100), "0"), ".")
		},
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"setupDone": func(steps []setupStep) bool {
			for _, s := range steps {
				if !s.Done {
					return false
				}
			}
			return true
		},
		"derefID": func(id *uuid.UUID) uuid.UUID {
			if id == nil {
				return uuid.Nil
			}
			return *id
		},
	}
	layout, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		return err
	}
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	u.pages = map[string]*template.Template{}
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		body, err := templateFS.ReadFile(e)
		if err != nil {
			return err
		}
		t := template.New("layout").Funcs(funcs)
		if _, err := t.Parse(string(layout)); err != nil {
			return fmt.Errorf("layout: %w", err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		u.pages[name] = t
	}
	return nil
}

// statusLabel turns a status code into words a beginner understands. The raw
// code stays available in the badge's title for anyone matching it to the API.
func statusLabel(v any) string {
	labels := map[string]string{
		"incomplete": "Waiting for first payment", "trialing": "Free trial", "active": "Active",
		"past_due": "Payment overdue", "canceled": "Canceled", "expired": "Ended",
		"draft": "Draft", "open": "Unpaid", "paid": "Paid", "void": "Voided", "uncollectible": "Written off",
		"pending": "Waiting", "succeeded": "Paid", "failed": "Failed", "refunded": "Refunded",
	}
	s := fmt.Sprint(v)
	if l, ok := labels[s]; ok {
		return l
	}
	return s
}

func fmtTime(v any, layout string) string {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format(layout)
	case *time.Time:
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.UTC().Format(layout)
	}
	return "—"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---- session ------------------------------------------------------------------

type session struct {
	Key     string    `json:"k"`
	Expires time.Time `json:"e"`
}

func (u *UI) seal(s session) (string, error) {
	plain, _ := json.Marshal(s)
	nonce := make([]byte, u.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(u.aead.Seal(nonce, nonce, plain, []byte(sessionCookie))), nil
}

func (u *UI) open(v string) (*session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(raw) < u.aead.NonceSize() {
		return nil, errors.New("bad session")
	}
	plain, err := u.aead.Open(nil, raw[:u.aead.NonceSize()], raw[u.aead.NonceSize():], []byte(sessionCookie))
	if err != nil {
		return nil, errors.New("bad session")
	}
	var s session
	if err := json.Unmarshal(plain, &s); err != nil || time.Now().After(s.Expires) {
		return nil, errors.New("expired session")
	}
	return &s, nil
}

func (u *UI) secure() bool { return strings.HasPrefix(u.cfg.PublicURL, "https://") }

func (u *UI) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: u.secure(), SameSite: http.SameSiteStrictMode,
	})
}

// csrf derives the form token from the session cookie, so it needs no storage.
func (u *UI) csrf(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, u.macKey)
	mac.Write([]byte("csrf:" + c.Value))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ---- request context ---------------------------------------------------------------

type reqCtx struct {
	P       *auth.Principal
	Tenant  *models.Tenant
	Tenants []models.Tenant
	CSRF    string
}

type ctxKey struct{}

func (u *UI) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		s, err := u.open(c.Value)
		if err != nil {
			u.setCookie(w, sessionCookie, "", -1)
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		p, err := u.auth.Authenticate(r.Context(), s.Key)
		if err != nil {
			u.setCookie(w, sessionCookie, "", -1)
			u.flash(w, "error", "Your API key is no longer valid. Log in again.")
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !hmac.Equal([]byte(r.FormValue("_csrf")), []byte(u.csrf(r))) {
			http.Error(w, "invalid form token; reload the page and try again", http.StatusForbidden)
			return
		}
		rc := &reqCtx{P: p, CSRF: u.csrf(r)}
		requested := ""
		if tc, err := r.Cookie(tenantCookie); err == nil {
			requested = tc.Value
		}
		if p.IsPlatform() {
			list, err := u.svc.ListTenants(r.Context(), billing.Page{Limit: 100})
			if err == nil {
				rc.Tenants = list.Data
			}
		}
		if tid, err := p.ResolveTenant(requested); err == nil {
			if t, err := u.svc.Tenant(r.Context(), tid); err == nil {
				rc.Tenant = t
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rc)))
	})
}

func rcFrom(r *http.Request) *reqCtx {
	rc, _ := r.Context().Value(ctxKey{}).(*reqCtx)
	return rc
}

// ---- rendering -------------------------------------------------------------------------

type pageData struct {
	Title    string
	Nav      string
	P        *auth.Principal
	Tenant   *models.Tenant
	Tenants  []models.Tenant
	CSRF     string
	Flash    string
	FlashErr string
	Version  string
	D        map[string]any
}

func (u *UI) render(w http.ResponseWriter, r *http.Request, name, title, nav string, d map[string]any) {
	t, ok := u.pages[name]
	if !ok {
		http.Error(w, "unknown page "+name, 500)
		return
	}
	pd := pageData{Title: title, Nav: nav, D: d}
	if rc := rcFrom(r); rc != nil {
		pd.P, pd.Tenant, pd.Tenants, pd.CSRF = rc.P, rc.Tenant, rc.Tenants, rc.CSRF
	}
	pd.Flash, pd.FlashErr = u.takeFlash(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	formAction := "'self'"
	if name == "checkout" {
		// The mock checkout redirects to the integrator's return_url after
		// its form is submitted, and browsers apply form-action to that
		// redirect.
		formAction = "'self' https: http:"
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; form-action "+formAction+"; frame-ancestors 'none'")
	if err := t.ExecuteTemplate(w, "layout", pd); err != nil {
		u.log.Error("render", "page", name, "error", err)
	}
}

func (u *UI) flash(w http.ResponseWriter, kind, msg string) {
	u.setCookie(w, flashCookie, url.QueryEscape(kind+"|"+msg), 60)
}

func (u *UI) takeFlash(w http.ResponseWriter, r *http.Request) (ok, bad string) {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return "", ""
	}
	u.setCookie(w, flashCookie, "", -1)
	v, _ := url.QueryUnescape(c.Value)
	kind, msg, _ := strings.Cut(v, "|")
	if kind == "error" {
		return "", msg
	}
	return msg, ""
}

// done redirects after a POST with a flash message, or the error.
func (u *UI) done(w http.ResponseWriter, r *http.Request, to string, err error, okMsg string) {
	if err != nil {
		msg := "Something went wrong; see the server log."
		if e, ok := billing.AsError(err); ok {
			msg = e.Message
		} else {
			u.log.Error("ui action failed", "path", r.URL.Path, "error", err)
		}
		u.flash(w, "error", msg)
		http.Redirect(w, r, sameHostReferer(r, to), http.StatusSeeOther)
		return
	}
	if okMsg != "" {
		u.flash(w, "ok", okMsg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// sameHostReferer returns the path of the page the form was posted from, so an
// error lands the user back where they were. Only a same-host path is ever
// used; anything else falls back to `to`, so this cannot become an open
// redirect.
func sameHostReferer(r *http.Request, to string) string {
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, "/") {
		return to
	}
	if ref.RawQuery != "" {
		return ref.Path + "?" + ref.RawQuery
	}
	return ref.Path
}

func (u *UI) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	u.render(w, r, "error", "Error", "", map[string]any{"Status": status, "Message": msg})
}

// can checks a scope and renders a 403 page when missing.
func (u *UI) can(w http.ResponseWriter, r *http.Request, scope auth.Scope) bool {
	if rcFrom(r).P.Has(scope) {
		return true
	}
	u.fail(w, r, http.StatusForbidden, "Your API key does not have the "+string(scope)+" scope.")
	return false
}

// tenant returns the tenant in use, or sends a platform key to pick one.
func (u *UI) tenant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	rc := rcFrom(r)
	if rc.Tenant == nil {
		if rc.P.IsPlatform() {
			u.flash(w, "error", "Choose a tenant first.")
			http.Redirect(w, r, "/ui/tenants", http.StatusSeeOther)
		} else {
			u.fail(w, r, http.StatusForbidden, "This key has no tenant.")
		}
		return uuid.Nil, false
	}
	return rc.Tenant.ID, true
}

func (u *UI) getErr(w http.ResponseWriter, r *http.Request, err error) {
	if e, ok := billing.AsError(err); ok {
		u.fail(w, r, e.Status, e.Message)
		return
	}
	u.log.Error("ui page failed", "path", r.URL.Path, "error", err)
	u.fail(w, r, 500, "Something went wrong; see the server log.")
}

func idParam(r *http.Request, name string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, name))
}

func keyIDOf(p *auth.Principal) *uuid.UUID {
	if p == nil || p.KeyID == uuid.Nil {
		return nil
	}
	id := p.KeyID
	return &id
}

// Static assets.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	return http.StripPrefix("/ui/static/", http.FileServer(http.FS(sub)))
}
