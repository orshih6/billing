// Command billing is the multi-tenant billing, subscription and ledger
// service.
//
//	billing serve                  API + web UI (+ worker unless WORKER_ENABLED=false)
//	billing worker [--once] [--now RFC3339]
//	billing migrate [up|down|status]  apply (or roll back one) SQL migration
//	billing tenants create --slug acme --name "Acme" [--currency MNT] [--admin-key]
//	billing tenants list
//	billing keys create --name ops --scopes admin [--tenant <id|slug>] [--platform] [--expires-in 720h]
//	billing keys list [--tenant <id|slug>]
//	billing version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/orshih6/billing/internal/api"
	"github.com/orshih6/billing/internal/auth"
	"github.com/orshih6/billing/internal/billing"
	"github.com/orshih6/billing/internal/config"
	"github.com/orshih6/billing/internal/database"
	"github.com/orshih6/billing/internal/events"
	"github.com/orshih6/billing/internal/models"
	"github.com/orshih6/billing/internal/netguard"
	"github.com/orshih6/billing/internal/providers"
	"github.com/orshih6/billing/internal/secretbox"
	"github.com/orshih6/billing/internal/web"
)

var version = "dev"

func main() {
	time.Local = time.UTC
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "worker":
		err = worker(args)
	case "migrate":
		err = migrate(args)
	case "tenants":
		err = tenantsCmd(args)
	case "keys":
		err = keysCmd(args)
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve, worker, migrate, tenants, keys, version)\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func openDB(log *slog.Logger) (*gorm.DB, error) {
	return database.Open(config.LoadDatabase(), log)
}

func newService(cfg *config.Config, db *gorm.DB, log *slog.Logger) *billing.Service {
	reg := providers.NewRegistry(providers.Mock{PublicURL: cfg.PublicURL}, providers.Manual{})
	svc := billing.New(db, reg, log, cfg.IsProduction())
	svc.WebhookAllowPrivate = cfg.Webhook.AllowPrivate
	key := cfg.Security.SecretsKey
	if key == "" && !cfg.IsProduction() {
		// Development only: a fixed, public key, so local credentials survive
		// restarts. Production refuses to start without SECRETS_KEY (serve).
		key = "insecure-development-secrets-key-do-not-use"
		log.Warn("SECRETS_KEY not set: using an insecure development key for provider credentials")
	}
	if box, err := secretbox.New(key); err == nil {
		svc.Secrets = box
	} else if key != "" {
		log.Error("SECRETS_KEY is invalid; provider credentials cannot be used", "error", err)
	}
	return svc
}

func serve() error {
	cfg := config.Load()
	log := newLogger(cfg)
	if cfg.IsProduction() {
		// Fail fast, before touching the database, on settings production
		// cannot run safely without.
		var missing []string
		if cfg.Security.SecretsKey == "" {
			missing = append(missing, "SECRETS_KEY (encrypts provider credentials; `openssl rand -hex 32`)")
		}
		if cfg.Security.UICookieKey == "" {
			missing = append(missing, "UI_COOKIE_KEY (encrypts UI sessions; `openssl rand -hex 32`)")
		}
		if len(missing) > 0 {
			return errors.New("production is missing: " + strings.Join(missing, ", "))
		}
	}
	db, err := openDB(log)
	if err != nil {
		return err
	}
	defer database.Close(db)

	svc := newService(cfg, db, log)
	if cfg.IsProduction() && svc.Secrets == nil {
		return errors.New("production needs SECRETS_KEY (32 random bytes, e.g. `openssl rand -hex 32`) to encrypt provider credentials")
	}
	if cfg.IsProduction() && len(cfg.Security.BootstrapAPIKeys) == 0 {
		ok, err := svc.HasUsableKey(context.Background())
		if err != nil {
			return fmt.Errorf("check api keys (did you run `billing migrate`?): %w", err)
		}
		if !ok {
			return errors.New("production needs a platform key: set BOOTSTRAP_API_KEYS or run `billing keys create --platform`")
		}
	}

	authn := auth.NewAuthenticator(db, cfg.Security.BootstrapAPIKeys, log)
	ui, err := web.New(svc, authn, cfg, log)
	if err != nil {
		return err
	}
	h := api.NewHandler(svc, authn, db, cfg, log)
	h.Extra = ui.Mount

	srv := &http.Server{
		Addr: cfg.HTTP.Addr, Handler: h.Routes(),
		ReadTimeout: cfg.HTTP.ReadTimeout, WriteTimeout: cfg.HTTP.WriteTimeout, IdleTimeout: cfg.HTTP.IdleTimeout,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Worker.Enabled {
		go runWorker(ctx, cfg, svc, db, log)
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("billing listening", "addr", cfg.HTTP.Addr, "version", version, "env", cfg.Env, "ui", cfg.PublicURL+"/ui")
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// runWorker ticks the billing engine and the webhook dispatcher.
func runWorker(ctx context.Context, cfg *config.Config, svc *billing.Service, db *gorm.DB, log *slog.Logger) {
	d := &events.Dispatcher{
		DB: db, Client: netguard.Client(cfg.Webhook.Timeout, cfg.Webhook.AllowPrivate), MaxAttempts: cfg.Webhook.MaxAttempts,
		UserAgent: cfg.Webhook.UserAgent, Log: log,
	}
	engineTick := time.NewTicker(cfg.Worker.Interval)
	webhookTick := time.NewTicker(5 * time.Second)
	defer engineTick.Stop()
	defer webhookTick.Stop()
	tickEngine := func() {
		rep, err := svc.RunEngine(ctx)
		if err != nil {
			log.Error("engine run failed", "error", err)
		} else if rep != (billing.EngineReport{}) {
			log.Info("engine run", "report", rep)
		}
	}
	tickEngine()
	for {
		select {
		case <-ctx.Done():
			return
		case <-engineTick.C:
			tickEngine()
		case <-webhookTick.C:
			if _, err := d.RunOnce(ctx); err != nil {
				log.Error("webhook dispatch failed", "error", err)
			}
		}
	}
}

func worker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	once := fs.Bool("once", false, "run the engine and one dispatch pass, then exit")
	at := fs.String("now", "", "pretend it is this time (RFC3339) — for testing renewals; implies --once")
	fs.Parse(args)

	cfg := config.Load()
	log := newLogger(cfg)
	db, err := openDB(log)
	if err != nil {
		return err
	}
	defer database.Close(db)
	svc := newService(cfg, db, log)

	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("--now: %w", err)
		}
		svc.Now = func() time.Time { return t }
		*once = true
	}
	if *once {
		rep, err := svc.RunEngine(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("%+v\n", rep)
		d := &events.Dispatcher{DB: db, Client: netguard.Client(cfg.Webhook.Timeout, cfg.Webhook.AllowPrivate), MaxAttempts: cfg.Webhook.MaxAttempts, UserAgent: cfg.Webhook.UserAgent, Log: log}
		_, err = d.RunOnce(context.Background())
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("billing worker started", "interval", cfg.Worker.Interval)
	runWorker(ctx, cfg, svc, db, log)
	return nil
}

func migrate(args []string) error {
	cfg := config.Load()
	log := newLogger(cfg)
	db, err := openDB(log)
	if err != nil {
		return err
	}
	defer database.Close(db)
	action := "up"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "up":
		if err := database.Migrate(db); err != nil {
			return err
		}
	case "down":
		if err := database.MigrateDown(db); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("usage: billing migrate [up|down|status]")
	}
	st, err := database.MigrationStatus(db)
	if err != nil {
		return err
	}
	for _, line := range st {
		fmt.Println(line)
	}
	return nil
}

func tenantsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billing tenants create|list")
	}
	cfg := config.Load()
	log := newLogger(cfg)
	db, err := openDB(log)
	if err != nil {
		return err
	}
	defer database.Close(db)
	svc := newService(cfg, db, log)
	ctx := context.Background()

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("tenants create", flag.ExitOnError)
		slug := fs.String("slug", "", "tenant slug (lowercase)")
		name := fs.String("name", "", "display name")
		cur := fs.String("currency", "MNT", "default currency")
		adminKey := fs.Bool("admin-key", true, "also mint an admin key for the tenant")
		fs.Parse(args[1:])
		out, err := svc.CreateTenant(ctx, billing.CreateTenantInput{Slug: *slug, Name: *name, DefaultCurrency: *cur, CreateAdminKey: *adminKey})
		if err != nil {
			return err
		}
		fmt.Printf("tenant %s (%s) created\n", out.Tenant.Slug, out.Tenant.ID)
		if out.APIKey != nil {
			fmt.Printf("admin key (shown once): %s\n", out.APIKey.Token)
		}
	case "list":
		var ts []models.Tenant
		if err := db.Order("created_at").Find(&ts).Error; err != nil {
			return err
		}
		for _, t := range ts {
			fmt.Printf("%s  %-20s %-8s %s  %s\n", t.ID, t.Slug, t.Status, t.DefaultCurrency, t.Name)
		}
	default:
		return fmt.Errorf("unknown tenants command %q", args[0])
	}
	return nil
}

func keysCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billing keys create|list")
	}
	cfg := config.Load()
	log := newLogger(cfg)
	db, err := openDB(log)
	if err != nil {
		return err
	}
	defer database.Close(db)
	svc := newService(cfg, db, log)
	ctx := context.Background()

	resolve := func(ref string) (*uuid.UUID, error) {
		if ref == "" {
			return nil, nil
		}
		var t models.Tenant
		q := db.Where("slug = ?", ref)
		if id, err := uuid.Parse(ref); err == nil {
			q = db.Where("id = ?", id)
		}
		if err := q.Take(&t).Error; err != nil {
			return nil, fmt.Errorf("tenant %q not found", ref)
		}
		return &t.ID, nil
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("keys create", flag.ExitOnError)
		name := fs.String("name", "", "key name")
		scopes := fs.String("scopes", "admin", "comma-separated scopes")
		tenant := fs.String("tenant", "", "tenant id or slug (omit with --platform)")
		platform := fs.Bool("platform", false, "mint a platform key (manages all tenants)")
		expires := fs.String("expires-in", "never", "lifetime, e.g. 720h, or never")
		fs.Parse(args[1:])
		if *platform == (*tenant != "") {
			return errors.New("give exactly one of --tenant or --platform")
		}
		tid, err := resolve(*tenant)
		if err != nil {
			return err
		}
		// The CLI runs with database access, which is already full power;
		// no creator principal means no escalation check.
		out, err := svc.CreateKey(ctx, nil, tid, billing.CreateKeyInput{Name: *name, Scopes: strings.Split(*scopes, ","), ExpiresIn: *expires})
		if err != nil {
			return err
		}
		fmt.Printf("key %s created (shown once):\n%s\n", out.Key.ID, out.Token)
	case "list":
		fs := flag.NewFlagSet("keys list", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id or slug (omit for platform keys)")
		fs.Parse(args[1:])
		tid, err := resolve(*tenant)
		if err != nil {
			return err
		}
		out, err := svc.ListKeys(ctx, tid, billing.Page{Limit: 100})
		if err != nil {
			return err
		}
		for _, k := range out.Data {
			state := "active"
			if k.RevokedAt != nil {
				state = "revoked"
			} else if k.Expired() {
				state = "expired"
			}
			fmt.Printf("%s  %-20s %-8s %s  %v\n", k.ID, k.Name, state, auth.Display(k.Prefix, k.LastFour), []string(k.Scopes))
		}
	default:
		return fmt.Errorf("unknown keys command %q", args[0])
	}
	return nil
}
