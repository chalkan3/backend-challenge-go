package app

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
	PlayerID      string
	InitialAmount string
	Currency      string
	CorrelationID string
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
}

// OutboxRepository is the transactional outbox store.
type OutboxRepository interface {
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
