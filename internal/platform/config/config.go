// Package config loads and validates all environment configuration.
// Validation happens at Fx Provide time, so the app refuses to start with
// invalid configuration (challenge section 4, lifecycle).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr      string
	ShutdownGrace time.Duration
	LogLevel      string

	DBDSN          string
	DBMaxConns     int32
	MigrateOnStart bool

	SQSEndpoint     string
	SQSRegion       string
	SQSAccessKeyID  string
	SQSSecretKey    string
	SQSRequestQueue string
	SQSEventsQueue  string
	SQSDLQQueue     string
	SQSWaitSeconds  int32
	SQSMaxMessages  int32
	SQSVisibility   int32
	SQSMaxReceive   int32

	WorkersEnabled       bool
	ConsumerConcurrency  int
	OutboxInterval       time.Duration
	OutboxBatch          int
	OutboxLease          time.Duration
	ReferenceInterval    time.Duration
	ReferenceBatch       int
	ReferenceMaxAttempts int
	ReferenceTTL         time.Duration

	AuthIssuer   string // e.g. http://localhost:8080/realms/wallet
	AuthJWKSURL  string // optional override; derived from issuer when empty
	AuthAudience string // optional audience check; empty disables it

	LedgerPageMax int
}

func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:      env("WALLET_HTTP_ADDR", ":8080"),
		ShutdownGrace: envDuration("WALLET_SHUTDOWN_GRACE", 20*time.Second),
		LogLevel:      env("WALLET_LOG_LEVEL", "info"),

		DBDSN:          env("WALLET_DB_DSN", "postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable"),
		DBMaxConns:     envInt32("WALLET_DB_MAX_CONNS", 20),
		MigrateOnStart: envBool("WALLET_MIGRATE_ON_START", true),

		SQSEndpoint:     env("WALLET_SQS_ENDPOINT", "http://localhost:4566"),
		SQSRegion:       env("WALLET_SQS_REGION", "us-east-1"),
		SQSAccessKeyID:  env("WALLET_SQS_ACCESS_KEY_ID", "test"),
		SQSSecretKey:    env("WALLET_SQS_SECRET_KEY", "test"),
		SQSRequestQueue: env("WALLET_SQS_REQUEST_QUEUE", "wager-transactions.fifo"),
		SQSEventsQueue:  env("WALLET_SQS_EVENTS_QUEUE", "integration-events.fifo"),
		SQSDLQQueue:     env("WALLET_SQS_DLQ_QUEUE", "wager-transactions-dlq.fifo"),
		SQSWaitSeconds:  envInt32("WALLET_SQS_WAIT_SECONDS", 5),
		SQSMaxMessages:  envInt32("WALLET_SQS_MAX_MESSAGES", 10),
		SQSVisibility:   envInt32("WALLET_SQS_VISIBILITY", 60),
		SQSMaxReceive:   envInt32("WALLET_SQS_MAX_RECEIVE", 5),

		WorkersEnabled:       envBool("WALLET_WORKERS_ENABLED", true),
		ConsumerConcurrency:  envInt("WALLET_CONSUMER_CONCURRENCY", 4),
		OutboxInterval:       envDuration("WALLET_OUTBOX_INTERVAL", 500*time.Millisecond),
		OutboxBatch:          envInt("WALLET_OUTBOX_BATCH", 100),
		OutboxLease:          envDuration("WALLET_OUTBOX_LEASE", 30*time.Second),
		ReferenceInterval:    envDuration("WALLET_REFERENCE_INTERVAL", 1*time.Second),
		ReferenceBatch:       envInt("WALLET_REFERENCE_BATCH", 50),
		ReferenceMaxAttempts: envInt("WALLET_REFERENCE_MAX_ATTEMPTS", 10),
		ReferenceTTL:         envDuration("WALLET_REFERENCE_TTL", 15*time.Minute),

		AuthIssuer:   env("WALLET_AUTH_ISSUER", "http://localhost:8080/realms/wallet"),
		AuthJWKSURL:  env("WALLET_AUTH_JWKS_URL", ""),
		AuthAudience: env("WALLET_AUTH_AUDIENCE", ""),

		LedgerPageMax: envInt("WALLET_LEDGER_PAGE_MAX", 200),
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var errs []string
	if c.HTTPAddr == "" {
		errs = append(errs, "WALLET_HTTP_ADDR is required")
	}
	if c.DBDSN == "" {
		errs = append(errs, "WALLET_DB_DSN is required")
	}
	if c.DBMaxConns < 1 {
		errs = append(errs, "WALLET_DB_MAX_CONNS must be >= 1")
	}
	if c.SQSRegion == "" {
		errs = append(errs, "WALLET_SQS_REGION is required")
	}
	if c.SQSRequestQueue == "" || c.SQSEventsQueue == "" || c.SQSDLQQueue == "" {
		errs = append(errs, "SQS queue names are required")
	}
	if !strings.HasSuffix(c.SQSRequestQueue, ".fifo") || !strings.HasSuffix(c.SQSEventsQueue, ".fifo") {
		errs = append(errs, "SQS queues must be .fifo")
	}
	if c.SQSWaitSeconds < 0 || c.SQSWaitSeconds > 20 {
		errs = append(errs, "WALLET_SQS_WAIT_SECONDS must be within [0,20]")
	}
	if c.SQSMaxMessages < 1 || c.SQSMaxMessages > 10 {
		errs = append(errs, "WALLET_SQS_MAX_MESSAGES must be within [1,10]")
	}
	if c.AuthIssuer == "" {
		errs = append(errs, "WALLET_AUTH_ISSUER is required (OAuth2/OIDC is mandatory)")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, "WALLET_LOG_LEVEL must be debug|info|warn|error")
	}
	if c.ConsumerConcurrency < 1 {
		errs = append(errs, "WALLET_CONSUMER_CONCURRENCY must be >= 1")
	}
	if c.ReferenceMaxAttempts < 1 || c.ReferenceTTL <= 0 {
		errs = append(errs, "reference retry policy is invalid")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// LedgerPageSize implements app.LedgerConfigSource.
func (c *Config) LedgerPageSize() int { return c.LedgerPageMax }

// MaxReferenceAttempts implements app.ReferenceConfigSource.
func (c *Config) MaxReferenceAttempts() int { return c.ReferenceMaxAttempts }

// ReferenceTTLDuration implements app.ReferenceConfigSource.
func (c *Config) ReferenceTTLDuration() time.Duration { return c.ReferenceTTL }

// JWKSURL derives the discovery JWKS address from the issuer unless
// explicitly overridden (useful when the issuer host differs between the
// token producer and the validator, e.g. inside Docker Compose).
func (c *Config) JWKSURL() string {
	if c.AuthJWKSURL != "" {
		return c.AuthJWKSURL
	}
	return strings.TrimSuffix(c.AuthIssuer, "/") + "/protocol/openid-connect/certs"
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt32(key string, def int32) int32 {
	return int32(envInt(key, int(def)))
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
