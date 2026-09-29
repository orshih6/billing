// Package config reads every runtime knob from the environment, so the binary
// stays deployable without config files.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the service configuration.
type Config struct {
	Env      string
	LogLevel string
	LogJSON  bool
	// PublicURL is where this service is reachable from a browser. It builds
	// mock checkout links.
	PublicURL string
	HTTP      HTTPConfig
	Database  DatabaseConfig
	Security  SecurityConfig
	Worker    WorkerConfig
	Webhook   WebhookConfig
}

type HTTPConfig struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

type DatabaseConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	LogQueries      bool
}

type SecurityConfig struct {
	// BootstrapAPIKeys are platform admin keys read from the environment. They
	// exist to create the first tenant and key; day to day use should be
	// database keys minted with `billing keys create` or POST /api-keys.
	BootstrapAPIKeys []string
	// SecretsKey encrypts provider credentials at rest. Required in
	// production; in development a fixed insecure key is used if unset.
	SecretsKey string
	// UICookieKey encrypts the web UI session cookie (which holds the API key
	// the operator logged in with). 32+ characters. When empty a random key is
	// generated per process, which logs everyone out on restart.
	UICookieKey string
}

type WorkerConfig struct {
	// Enabled runs the billing engine and webhook dispatcher inside `serve`.
	// The dedicated `worker` subcommand always runs them.
	Enabled  bool
	Interval time.Duration
}

type WebhookConfig struct {
	Timeout     time.Duration
	MaxAttempts int
	UserAgent   string
	// AllowPrivate lets webhooks reach private, loopback and link-local
	// addresses. For local development only: in a multi-tenant deployment it
	// lets any tenant make the server call internal services (SSRF).
	AllowPrivate bool
}

// Load reads configuration from the environment, applying defaults.
func Load() *Config {
	return &Config{
		Env:       env("APP_ENV", "development"),
		LogLevel:  env("LOG_LEVEL", "info"),
		LogJSON:   envBool("LOG_JSON", false),
		PublicURL: strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		HTTP: HTTPConfig{
			Addr:            env("HTTP_ADDR", ":8080"),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:     envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		},
		Database: LoadDatabase(),
		Security: SecurityConfig{
			BootstrapAPIKeys: envList("BOOTSTRAP_API_KEYS"),
			UICookieKey:      env("UI_COOKIE_KEY", ""),
			SecretsKey:       env("SECRETS_KEY", ""),
		},
		Worker: WorkerConfig{
			Enabled:  envBool("WORKER_ENABLED", true),
			Interval: envDuration("WORKER_INTERVAL", 30*time.Second),
		},
		Webhook: WebhookConfig{
			Timeout:      envDuration("WEBHOOK_TIMEOUT", 10*time.Second),
			MaxAttempts:  envInt("WEBHOOK_MAX_ATTEMPTS", 10),
			UserAgent:    env("WEBHOOK_USER_AGENT", "Billing-Webhooks/1.0"),
			AllowPrivate: envBool("WEBHOOK_ALLOW_PRIVATE", false),
		},
	}
}

// IsProduction switches on production behaviour (mock provider off by default,
// refusal to start without a usable key).
func (c *Config) IsProduction() bool { return c.Env == "production" }

// LoadDatabase reads only the database settings, so CLI subcommands need
// nothing but DATABASE_URL.
func LoadDatabase() DatabaseConfig {
	return DatabaseConfig{
		DSN:             env("DATABASE_URL", "postgres://postgres:postgres@localhost:5434/billing?sslmode=disable"),
		MaxOpenConns:    envInt("DATABASE_MAX_OPEN_CONNS", 20),
		MaxIdleConns:    envInt("DATABASE_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: envDuration("DATABASE_CONN_MAX_LIFETIME", time.Hour),
		LogQueries:      envBool("DATABASE_LOG_QUERIES", false),
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envList(key string) []string {
	raw := env(key, "")
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
