package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// ExternalInput is the channel-agnostic submission for the five external
// kinds. HTTP and SQS both normalize into this struct, so both share the
// exact same use case, validations and idempotency.
type ExternalInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string
	ReceivedVia                    domain.ReceivedVia
	CorrelationID                  string
}

// WagerResult is the outcome of a submission: the persisted transaction and
// a wallet snapshot (the balance observed at processing time, or at replay
// time reconstructed from the original processing).
type WagerResult struct {
	Transaction      *domain.WagerTransaction
	WalletBalance    domain.Money
	WalletVersion    int64
	IdempotentReplay bool
}

// OpenWalletInput is the internal wallet opening command.
type OpenWalletInput struct {
	PlayerID       string
	InitialAmount  string
	Currency       string
	IdempotencyKey string
	CorrelationID  string
}

// WalletView is the read model for wallets.
type WalletView struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Currency  string
	Balance   domain.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// IdempotentReplay is true when the opening was served from a previously
	// committed request with the same Idempotency-Key (no new wallet, no new
	// ledger row): the response reproduces the original one.
	IdempotentReplay bool
}

// ReconciliationReport compares the stored balance with the ledger-derived
// balance in a single consistent snapshot. It never mutates anything.
type ReconciliationReport struct {
	WalletID          uuid.UUID
	StoredBalance     domain.Money
	CalculatedBalance domain.Money
	Difference        domain.Money
	Consistent        bool
	CheckedEntries    int64
}

// LedgerCursor is the opaque pagination cursor (base64 JSON of the keyset).
type LedgerCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// WalletRepository persists and locks wallets.
type WalletRepository interface {
	Create(ctx context.Context, w *domain.Wallet) error
	// UpdateBalance persists balance, version and updated_at after a
	// movement (called under the row lock, in the same transaction as the
	// ledger entry).
	UpdateBalance(ctx context.Context, w *domain.Wallet) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
	// GetForUpdate selects the row FOR UPDATE, serializing concurrent
	// operations on the same wallet (per-wallet locking; independent wallets
	// never block each other).
	GetForUpdate(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
	GetByPlayerAndCurrency(ctx context.Context, playerID uuid.UUID, currency string, forUpdate bool) (*domain.Wallet, error)
}

// WalletOpenRepository stores the durable idempotency identity of a wallet
// opening, so POST /wallets replays the original result instead of creating a
// second wallet (or failing with a conflict) when the same request is retried.
type WalletOpenRepository interface {
	// Record binds an idempotency key to the wallet it created, together
	// with the hash of the request payload. It returns ErrDuplicateInsert
	// when the key was already recorded.
	Record(ctx context.Context, key, payloadHash string, walletID uuid.UUID) error
	// Lookup returns the wallet created under key and the payload hash that
	// was recorded with it. It returns ErrRepoNotFound when the key is
	// unknown.
	Lookup(ctx context.Context, key string) (walletID uuid.UUID, payloadHash string, err error)
}

// TransactionRepository persists wager transactions and their durable
// idempotency identity.
type TransactionRepository interface {
	Insert(ctx context.Context, t *domain.WagerTransaction) error
	// Save updates the mutable projection (status, failure, results,
	// pending schedule) of a non-terminal row.
	Save(ctx context.Context, t *domain.WagerTransaction) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.WagerTransaction, error)
	GetByProviderAndKey(ctx context.Context, providerID, idempotencyKey string) (*domain.WagerTransaction, error)
	GetByProviderAndExternal(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error)
	ExistsProcessedReversal(ctx context.Context, referenceTxID uuid.UUID, kind domain.TxKind) (bool, error)
	// ClaimPendingReferences selects PENDING_REFERENCE rows due for retry
	// with FOR UPDATE SKIP LOCKED so multiple instances dispute safely.
	ClaimPendingReferences(ctx context.Context, now time.Time, limit int) ([]*domain.WagerTransaction, error)
}

// LedgerRepository is append-only accounting.
type LedgerRepository interface {
	Append(ctx context.Context, e domain.LedgerEntry) error
	ListByWallet(ctx context.Context, walletID uuid.UUID, cursor *LedgerCursor, limit int) ([]*domain.LedgerEntry, *LedgerCursor, error)
	// Reconcile returns (storedBalanceMinor, ledgerSumMinor, entryCount)
	// computed in ONE statement, i.e. one consistent snapshot.
	Reconcile(ctx context.Context, walletID uuid.UUID) (stored, sum int64, count int64, err error)
}

// InboxOutcome reports the state of the inbox row for a received message.
type InboxOutcome struct {
	Inserted  bool // row created by this call
	Exists    bool // row already existed
	HashMatch bool // existing row carries the same payload hash
	Completed bool // previous handling already committed
}

// InboxRepository records durable SQS consumption.
type InboxRepository interface {
	Begin(ctx context.Context, consumerName, messageID, hash string) (InboxOutcome, error)
	Complete(ctx context.Context, consumerName, messageID string, at time.Time) error
}

// OutboxItem is a claimed, ready-to-publish integration event.
type OutboxItem struct {
	EventID     uuid.UUID
	AggregateID string
	EventType   string
	Attempts    int
	Payload     []byte
	// TraceParent is the W3C traceparent captured when the event was
	// enqueued (inside the balance transaction). The publisher forwards it
	// with the message so the consumer resumes the originating trace instead
	// of starting an unrelated one. Empty means the request had no upstream
	// trace context.
	TraceParent string
}

// OutboxRepository is the transactional outbox store.
type OutboxRepository interface {
	// Enqueue writes the envelopes durably. It captures the trace context
	// active in ctx as the row's trace_parent, so the relay can resume the
	// trace of the request that produced the event.
	Enqueue(ctx context.Context, envelopes []domain.Envelope) error
	// ClaimBatch leases rows (attempts++, available_at += lease) using
	// FOR UPDATE SKIP LOCKED; crashed publishers are recovered when the
	// lease expires.
	ClaimBatch(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxItem, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, at time.Time) error
	MarkFailed(ctx context.Context, eventID uuid.UUID, nextAvailable time.Time, lastErr string) error
	Stats(ctx context.Context) (pending int64, oldestAgeSeconds float64, err error)
}

// Repos bundles the repositories bound to one executor (pool or tx).
type Repos interface {
	Wallets() WalletRepository
	WalletOpens() WalletOpenRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Inbox() InboxRepository
	Outbox() OutboxRepository
}

// TxManager delimits the SQL transaction: every financial write (domain
// rows, ledger, inbox, outbox) commits atomically inside it.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repos) error) error
	Repos() Repos
}

// Instrumentation is the observation port (Observer pattern): the use cases
// report domain outcomes through it, and the composition root adapts it to
// the go-kit metrics registry. This keeps the service layer free of any
// metrics-library import.
type Instrumentation interface {
	// WagerOutcome reports one processed wager (persisted result or replay).
	WagerOutcome(kind, status, failureCode, channel string)
	// Duplicate reports an idempotent replay and its detection source.
	Duplicate(source string)
	// ReferenceOutcome reports a pending-reference resolution outcome.
	ReferenceOutcome(outcome string)
	// ReconciliationDivergence reports a ledger/balance mismatch.
	ReconciliationDivergence()

	// StartSpan opens a child span for a use case and returns the derived
	// context plus a Span façade. It never returns a nil Span, so call sites
	// never need a nil check (a disabled tracer yields a no-op span).
	StartSpan(ctx context.Context, name string, attrs ...SpanAttr) (context.Context, Span)
}

// SpanCarrier serialises and restores trace context across a boundary that
// cannot carry a context.Context — here, the integration_outbox row and the
// SQS message body that outlives the request.
//
// It exists because the outbox deliberately breaks the call chain: the HTTP
// request commits and returns, while a detached relay publishes the event
// afterwards. Without a way to persist the W3C traceparent and resume it on
// the far side, the relay's trace is unrelated to the request that caused it.
//
// Keeping this a port — rather than calling the tracing SDK directly — is the
// same boundary the Span façade enforces: the service and messaging packages
// stay free of SDK imports, and the format (W3C traceparent today) is an
// adapter's concern. An empty Inject result means "no active trace context",
// which is not an error: the far side simply starts a new trace.
type SpanCarrier interface {
	// Inject renders the span active in ctx as a transferable string, or ""
	// when ctx carries no sampled trace context.
	Inject(ctx context.Context) string
	// Extract returns a context carrying the trace described by carrier as a
	// remote parent. An empty or malformed carrier yields ctx unchanged, so a
	// missing link degrades to a new trace instead of failing the work.
	Extract(ctx context.Context, carrier string) context.Context
}

// SpanAttr is one key/value attribute attached to a span. The port speaks
// its own tiny vocabulary so the service layer never imports the tracing
// SDK — the adapter translates these into the provider's attribute type.
type SpanAttr struct {
	Key string
	Val any
}

// AttrKey/AttrVal expose the pair structurally so the otel adapter can
// accept any {Key, Val} carrier without importing this package's type.
func (a SpanAttr) AttrKey() string { return a.Key }
func (a SpanAttr) AttrVal() any    { return a.Val }

// Attr is the terse constructor used at call sites: Attr(AttrKind, "BET").
func Attr(key string, val any) SpanAttr { return SpanAttr{Key: key, Val: val} }

// Span is the minimal façade of a tracing span. It is intentionally small:
// the use cases annotate outcomes and record faults, nothing else.
type Span interface {
	// SetAttributes adds attributes after the span was started.
	SetAttributes(attrs ...SpanAttr)
	// SetOutcome stamps the classified business result. A rejected or failed
	// outcome marks the span as errored (so the rejection is visible in the
	// trace), while a processed outcome leaves it successful.
	SetOutcome(status, failureCode string)
	// RecordError marks the span as failed and attaches the cause.
	RecordError(err error)
	// End closes the span; it must be called exactly once (use defer).
	End()
}
