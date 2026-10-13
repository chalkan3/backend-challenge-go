package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// DefaultLedgerPageSize bounds ledger pagination when no option says
// otherwise, protecting the database from unbounded reads.
const DefaultLedgerPageSize = 200

// WalletService covers internal wallet operations (opening, reads, ledger
// pagination and reconciliation) over the same TxManager used by wagers.
type WalletService struct {
	txm     TxManager
	log     *slog.Logger
	hook    Instrumentation
	pageMax int
}

// WalletOption customizes a WalletService at construction time (functional
// options pattern).
type WalletOption func(*WalletService)

// WithLedgerPageSize overrides the maximum number of ledger entries per
// page; non-positive values keep DefaultLedgerPageSize.
func WithLedgerPageSize(n int) WalletOption {
	return func(s *WalletService) {
		if n > 0 {
			s.pageMax = n
		}
	}
}

// NewWalletService wires the service with the default page size.
func NewWalletService(txm TxManager, log *slog.Logger, hook Instrumentation, opts ...WalletOption) *WalletService {
	s := &WalletService{txm: txm, log: log, hook: hook, pageMax: DefaultLedgerPageSize}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Open creates the wallet, the internal OPENING transaction, its ledger
// entry and the two integration events in ONE committing transaction.
// A positive opening produces PROCESSED OPENING + CREDIT ledger +
// WagerTransactionProcessed + WalletBalanceChanged; a zero opening creates
// only the wallet (version 1, no financial rows).
func (s *WalletService) Open(ctx context.Context, in OpenWalletInput) (view WalletView, err error) {
	ctx, span := s.hook.StartSpan(ctx, SpanWalletOpen,
		Attr(AttrPlayerID, in.PlayerID),
		Attr(AttrCurrency, in.Currency),
	)
	defer func() {
		if err == nil {
			span.SetAttributes(
				Attr(AttrWalletID, view.ID.String()),
				Attr(AttrBalanceMinor, view.Balance.AmountMinor()),
			)
		}
		span.RecordError(err)
		span.End()
	}()

	playerID, err := domain.ParseID(in.PlayerID)
	if err != nil {
		return WalletView{}, &ValidationError{Detail: "playerId: " + err.Error()}
	}
	if in.Currency == "" {
		return WalletView{}, &ValidationError{Detail: "currency is required"}
	}
	initial, err := domain.ParseMoney(in.InitialAmount, in.Currency)
	if err != nil {
		return WalletView{}, &ValidationError{Detail: "initialBalance: " + err.Error()}
	}
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		// Durable idempotency: a replay of the same Idempotency-Key must
		// reproduce the original result, never open a second wallet nor
		// double the opening balance.
		if in.IdempotencyKey != "" {
			replay, revoked, err := s.replayOpen(ctx, r, in, playerID, &view)
			if err != nil {
				return err
			}
			if revoked {
				now := time.Now().UTC()
				return s.createWallet(ctx, r, in, playerID, initial, now, &view)
			}
			if replay {
				return nil
			}
		}
		now := time.Now().UTC()
		return s.createWallet(ctx, r, in, playerID, initial, now, &view)
	})
	if err != nil {
		return WalletView{}, err
	}
	s.log.InfoContext(ctx, "wallet opened",
		slog.String("walletId", view.ID.String()),
		slog.String("playerId", view.PlayerID.String()),
		slog.String("balance", view.Balance.String()),
		slog.Bool("idempotentReplay", view.IdempotentReplay))
	return view, nil
}

// replayOpen answers the idempotency question for a key-bearing opening:
//   - not seen before      → (false, true, nil):  proceed and create, then record.
//   - same payload replay  → (true,  false, nil): serve the stored wallet.
//   - same key, new body   → 409 IDEMPOTENCY_KEY_CONFLICT.
//
// The returned "revoked" flag means the caller must create the wallet (first
// attempt); a false "replay" with no error means nothing to do.
func (s *WalletService) replayOpen(ctx context.Context, r Repos, in OpenWalletInput, playerID uuid.UUID, out *WalletView) (replay, revoked bool, err error) {
	hash, err := openPayloadHash(in, playerID)
	if err != nil {
		return false, false, err
	}
	walletID, storedHash, err := r.WalletOpens().Lookup(ctx, in.IdempotencyKey)
	if errors.Is(err, ErrRepoNotFound) {
		return false, true, nil // first time we see this key: create it
	}
	if err != nil {
		return false, false, err
	}
	if storedHash != hash {
		return false, false, &ConflictError{Code: CodeIdemKeyConflict,
			Detail: "Idempotency-Key was already used with a different payload"}
	}
	w, err := r.Wallets().GetByID(ctx, walletID)
	if err != nil {
		return false, false, err
	}
	s.hook.Duplicate("idempotent_replay")
	view := walletView(w)
	view.IdempotentReplay = true
	*out = view
	return true, false, nil
}

// createWallet performs the actual opening: uniqueness guard, wallet,
// OPENING transaction, ledger entry, outbox events and — when the caller
// supplied a key — the durable idempotency record, all in one transaction.
func (s *WalletService) createWallet(
	ctx context.Context, r Repos, in OpenWalletInput, playerID uuid.UUID,
	initial domain.Money, now time.Time, out *WalletView,
) error {
	if _, err := r.Wallets().GetByPlayerAndCurrency(ctx, playerID, in.Currency, false); err == nil {
		return &ConflictError{Code: CodeWalletExists, Detail: "wallet already exists for this player and currency"}
	} else if !errors.Is(err, ErrRepoNotFound) {
		return err
	}
	w, mv, err := domain.OpenWallet(playerID, in.Currency, initial, now)
	if err != nil {
		return &ValidationError{Detail: err.Error()}
	}
	if err := r.Wallets().Create(ctx, w); err != nil {
		if errors.Is(err, ErrDuplicateInsert) {
			return &ConflictError{Code: CodeWalletExists, Detail: "wallet already exists for this player and currency"}
		}
		return err
	}
	// Record the idempotency key in the SAME transaction as the wallet, so
	// the key and the money commit or roll back together.
	if in.IdempotencyKey != "" {
		hash, err := openPayloadHash(in, playerID)
		if err != nil {
			return err
		}
		if err := r.WalletOpens().Record(ctx, in.IdempotencyKey, hash, w.ID()); err != nil {
			return err
		}
	}
	if !mv.Direction.Valid() { // zero opening: no financial rows
		*out = walletView(w)
		return nil
	}
	tx, err := domain.NewOpeningTransaction(w.PlayerID(), w.ID(), w.Currency(), initial, now)
	if err != nil {
		return err
	}
	entry, err := domain.NewLedgerEntry(w.ID(), tx.ID(), mv, now)
	if err != nil {
		return err
	}
	if err := tx.MarkProcessed(domain.ProcessedResult{
		Direction: mv.Direction, ResultAmount: mv.Amount,
		BalanceBefore: mv.BalanceBefore, BalanceAfter: mv.BalanceAfter, WalletVersion: mv.WalletVersion,
	}, now); err != nil {
		return err
	}
	if err := r.Transactions().Insert(ctx, tx); err != nil {
		return err
	}
	if err := r.Ledger().Append(ctx, entry); err != nil {
		return err
	}
	processed, err := domain.NewWagerTransactionProcessedEvent(tx)
	if err != nil {
		return err
	}
	balanceChanged, err := domain.NewWalletBalanceChangedEvent(tx, mv)
	if err != nil {
		return err
	}
	if err := r.Outbox().Enqueue(ctx, []domain.Envelope{processed, balanceChanged}); err != nil {
		return err
	}
	*out = walletView(w)
	return nil
}

// Get reads a wallet.
func (s *WalletService) Get(ctx context.Context, walletID string) (WalletView, error) {
	id, err := domain.ParseID(walletID)
	if err != nil {
		return WalletView{}, &ValidationError{Detail: "walletId: " + err.Error()}
	}
	var view WalletView
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		w, err := r.Wallets().GetByID(ctx, id)
		if err != nil {
			return mapRepoNotFound(err, CodeWalletNotFound, "wallet")
		}
		view = walletView(w)
		return nil
	})
	return view, err
}

// LedgerItemView is one ledger line for the paginated read.
type LedgerItemView struct {
	ID            string
	TransactionID string
	Direction     domain.Direction
	Amount        domain.Money
	BalanceBefore domain.Money
	BalanceAfter  domain.Money
	CreatedAt     time.Time
}

// ListLedger returns entries in stable chronological order with an opaque
// keyset cursor (base64 of {t,i}).
func (s *WalletService) ListLedger(ctx context.Context, walletID, cursor string, limit int) ([]LedgerItemView, string, error) {
	id, err := domain.ParseID(walletID)
	if err != nil {
		return nil, "", &ValidationError{Detail: "walletId: " + err.Error()}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > s.pageMax {
		limit = s.pageMax
	}
	var cur *LedgerCursor
	if cursor != "" {
		cur, err = decodeCursor(cursor)
		if err != nil {
			return nil, "", &ValidationError{Detail: "cursor: " + err.Error()}
		}
	}
	var items []LedgerItemView
	var next string
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		if _, err := r.Wallets().GetByID(ctx, id); err != nil {
			return mapRepoNotFound(err, CodeWalletNotFound, "wallet")
		}
		entries, nextCur, err := r.Ledger().ListByWallet(ctx, id, cur, limit)
		if err != nil {
			return err
		}
		for _, e := range entries {
			items = append(items, LedgerItemView{
				ID: e.ID().String(), TransactionID: e.TransactionID().String(),
				Direction: e.Direction(), Amount: e.Amount(),
				BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(),
				CreatedAt: e.CreatedAt(),
			})
		}
		if nextCur != nil {
			next = encodeCursor(*nextCur)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

// Reconcile recomputes the balance from the ledger (opening included) in a
// single consistent snapshot and compares it with the stored balance.
// It never mutates state; divergences are reported AND instrumented.
func (s *WalletService) Reconcile(ctx context.Context, walletID string) (ReconciliationReport, error) {
	id, err := domain.ParseID(walletID)
	if err != nil {
		return ReconciliationReport{}, &ValidationError{Detail: "walletId: " + err.Error()}
	}
	var report ReconciliationReport
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		w, err := r.Wallets().GetByID(ctx, id)
		if err != nil {
			return mapRepoNotFound(err, CodeWalletNotFound, "wallet")
		}
		stored, sum, count, err := r.Ledger().Reconcile(ctx, id)
		if err != nil {
			return err
		}
		storedM, err := domain.NewMoney(stored, w.Currency())
		if err != nil {
			return err
		}
		calcM, err := domain.NewMoney(sum, w.Currency())
		if err != nil {
			return err
		}
		diffM, err := storedM.Sub(calcM)
		if err != nil {
			return err
		}
		report = ReconciliationReport{
			WalletID: id, StoredBalance: storedM, CalculatedBalance: calcM,
			Difference: diffM, Consistent: diffM.IsZero(), CheckedEntries: count,
		}
		if !report.Consistent {
			s.hook.ReconciliationDivergence()
			s.log.Error("reconciliation divergence",
				slog.String("walletId", id.String()),
				slog.String("stored", storedM.String()),
				slog.String("calculated", calcM.String()),
				slog.String("difference", diffM.String()))
		}
		return nil
	})
	return report, err
}

func walletView(w *domain.Wallet) WalletView {
	return WalletView{
		ID: w.ID(), PlayerID: w.PlayerID(), Currency: w.Currency(),
		Balance: w.Balance(), Version: w.Version(),
		CreatedAt: w.CreatedAt(), UpdatedAt: w.UpdatedAt(),
	}
}

func encodeCursor(c LedgerCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*LedgerCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("malformed cursor")
	}
	var c LedgerCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, errors.New("malformed cursor")
	}
	return &c, nil
}

func mapRepoNotFound(err error, code, what string) error {
	if errors.Is(err, ErrRepoNotFound) {
		return &NotFoundError{Code: code, Detail: what + " not found"}
	}
	return err
}
