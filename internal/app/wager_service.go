package app

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// WagerService implements the shared use case for the five external kinds.
// HTTP and the SQS consumer both call Submit; the reference worker calls
// ResolvePending for durably waiting operations. Every financial effect —
// transaction row, wallet balance, ledger, inbox/outbox — commits in one
// SQL transaction delimited by TxManager.
type WagerService struct {
	txm  TxManager
	log  *slog.Logger
	hook Instrumentation

	refMaxAttempts int
	refTTL         time.Duration
}

// ReferenceConfigSource keeps the app layer decoupled from the config package.
type ReferenceConfigSource interface {
	MaxReferenceAttempts() int
	ReferenceTTLDuration() time.Duration
}

// NewWagerService wires the service from configuration.
func NewWagerService(txm TxManager, log *slog.Logger, hook Instrumentation, cfg ReferenceConfigSource) *WagerService {
	return NewWagerServiceWithPolicy(txm, log, hook, cfg.MaxReferenceAttempts(), cfg.ReferenceTTLDuration())
}

// NewWagerServiceWithPolicy receives the retry policy explicitly (tests).
func NewWagerServiceWithPolicy(txm TxManager, log *slog.Logger, hook Instrumentation, refMaxAttempts int, refTTL time.Duration) *WagerService {
	return &WagerService{txm: txm, log: log, hook: hook, refMaxAttempts: refMaxAttempts, refTTL: refTTL}
}

// Stable domain failure codes (persisted and returned to providers).
const (
	FailInsufficientFunds         = "INSUFFICIENT_FUNDS"
	FailReversalInsufficientFunds = "REVERSAL_INSUFFICIENT_FUNDS"
	FailCurrencyMismatch          = "CURRENCY_MISMATCH"
	FailPlayerWalletMismatch      = "PLAYER_WALLET_MISMATCH"
	FailReferenceMismatch         = "REFERENCE_MISMATCH"
	FailReferenceKindMismatch     = "REVERSAL_KIND_MISMATCH"
	FailReferenceAmountMismatch   = "REVERSAL_AMOUNT_MISMATCH"
	FailDuplicateReversal         = "DUPLICATE_REVERSAL"
	FailReferenceNotFound         = "REFERENCE_NOT_FOUND"
	FailReferenceNotProcessed     = "REFERENCE_NOT_PROCESSED"
)

// Prepare validates and builds the domain transaction from wire input
// (shared by HTTP Submit and the SQS consumer).
func (s *WagerService) Prepare(in ExternalInput) (*domain.WagerTransaction, error) {
	return s.buildTransaction(in)
}

// ExecuteInTx runs the use case inside an EXISTING SQL transaction (the SQS
// consumer's inbox+domain+outbox transaction). HTTP goes through Submit,
// which opens its own transaction.
func (s *WagerService) ExecuteInTx(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	res, err := s.execute(ctx, r, t)
	if err != nil {
		return WagerResult{}, err
	}
	s.emitOutcome(t.Kind(), res, t.ReceivedVia())
	return res, nil
}

// GetByID returns one transaction for queries (authorization is a transport
// concern and is enforced there).
func (s *WagerService) GetByID(ctx context.Context, transactionID string) (*domain.WagerTransaction, error) {
	id, err := domain.ParseID(transactionID)
	if err != nil {
		return nil, &ValidationError{Detail: "transactionId: " + err.Error()}
	}
	var t *domain.WagerTransaction
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		t, err = r.Transactions().GetByID(ctx, id)
		if errors.Is(err, ErrRepoNotFound) {
			return &NotFoundError{Code: CodeTransactionNotFound, Detail: "transaction not found"}
		}
		return err
	})
	return t, err
}

// GetByProviderExternal resolves by (providerId, externalTransactionId).
func (s *WagerService) GetByProviderExternal(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	if providerID == "" || externalID == "" {
		return nil, &ValidationError{Detail: "providerId and externalTransactionId are required"}
	}
	var t *domain.WagerTransaction
	var err error
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		t, err = r.Transactions().GetByProviderAndExternal(ctx, providerID, externalID)
		if errors.Is(err, ErrRepoNotFound) {
			return &NotFoundError{Code: CodeTransactionNotFound, Detail: "transaction not found"}
		}
		return err
	})
	return t, err
}

// Submit validates, checks durable idempotency and executes the operation.
// Business rejections are PERSISTED (REJECTED + audit event) and returned as
// a normal result — only infrastructure failures come back as errors.
func (s *WagerService) Submit(ctx context.Context, in ExternalInput) (WagerResult, error) {
	t, err := s.buildTransaction(in)
	if err != nil {
		return WagerResult{}, err
	}
	var res WagerResult
	err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		res, err = s.execute(ctx, r, t)
		return err
	})
	if errors.Is(err, ErrDuplicateInsert) {
		// Lost an insert race against a concurrent request with the same
		// identity. The unique violation only surfaces after the winner's
		// transaction commits, and our own transaction is aborted (25P02),
		// so the persisted outcome is re-read in a FRESH transaction and
		// served as an idempotent replay.
		var rep WagerResult
		err = s.txm.WithinTx(ctx, func(ctx context.Context, r Repos) error {
			var rerr error
			rep, rerr = s.duplicateRaceReplay(ctx, r, t)
			return rerr
		})
		if err != nil {
			return WagerResult{}, err
		}
		s.emitOutcome(t.Kind(), rep, in.ReceivedVia)
		return rep, nil
	}
	if err != nil {
		return WagerResult{}, err
	}
	s.emitOutcome(t.Kind(), res, in.ReceivedVia)
	return res, nil
}

// ResolvePending advances one PENDING_REFERENCE transaction (row already
// locked by the worker's claim). Persisted with Save, not Insert.
func (s *WagerService) ResolvePending(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	res, err := s.resolve(ctx, r, t)
	if err != nil {
		return WagerResult{}, err
	}
	s.emitOutcome(t.Kind(), res, t.ReceivedVia())
	return res, nil
}

func (s *WagerService) emitOutcome(kind domain.TxKind, res WagerResult, ch domain.ReceivedVia) {
	code := res.Transaction.FailureCode()
	s.hook.WagerOutcome(string(kind), string(res.Transaction.Status()), code, string(ch))
}

// buildTransaction parses/validates the wire input and constructs the domain
// aggregate (shape validations only; business rules run inside the tx).
func (s *WagerService) buildTransaction(in ExternalInput) (*domain.WagerTransaction, error) {
	if in.IdempotencyKey == "" {
		return nil, &ValidationError{Detail: "Idempotency-Key / idempotencyKey is required"}
	}
	playerID, err := domain.ParseID(in.PlayerID)
	if err != nil {
		return nil, &ValidationError{Detail: "playerId: " + err.Error()}
	}
	walletID, err := domain.ParseID(in.WalletID)
	if err != nil {
		return nil, &ValidationError{Detail: "walletId: " + err.Error()}
	}
	amount, err := domain.ParseMoney(in.Amount, in.Currency)
	if err != nil {
		return nil, &ValidationError{Detail: "money: " + err.Error()}
	}
	hash, err := canonicalHash(in, amount)
	if err != nil {
		return nil, err
	}
	kind, err := domain.ParseTxKind(in.Kind)
	if err != nil {
		return nil, &ValidationError{Detail: err.Error()}
	}
	t, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID,
		PlayerID: playerID, WalletID: walletID,
		RoundID: in.RoundID, GameID: in.GameID,
		Kind: kind, Money: amount,
		ReferenceExternalTxID: in.ReferenceExternalTransactionID,
		ReceivedVia:           in.ReceivedVia, CorrelationID: in.CorrelationID,
		Now: time.Now().UTC(),
	})
	if err != nil {
		return nil, &ValidationError{Detail: err.Error()}
	}
	t.SetIdempotency(in.IdempotencyKey, hash)
	return t, nil
}

func (s *WagerService) execute(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	// 1. Durable idempotency by (providerId, idempotencyKey).
	existing, err := r.Transactions().GetByProviderAndKey(ctx, t.ProviderID(), t.IdempotencyKey())
	switch {
	case err == nil:
		if existing.PayloadHash() != t.PayloadHash() {
			return WagerResult{}, &ConflictError{Code: CodeIdemKeyConflict,
				Detail: "idempotency key reused with a different payload"}
		}
		s.hook.Duplicate("idempotent_replay")
		return s.replayResult(ctx, r, existing)
	case errors.Is(err, ErrRepoNotFound):
		// new operation
	default:
		return WagerResult{}, err
	}

	// 2. Same (providerId, externalTransactionId) under ANOTHER key is
	// forbidden — one financial operation, one identity. Under the SAME key
	// this is the concurrent winner of our own identity: serve its persisted
	// outcome as an idempotent replay instead of a spurious conflict.
	byExternal, err := r.Transactions().GetByProviderAndExternal(ctx, t.ProviderID(), t.ExternalTxID())
	switch {
	case err == nil:
		if byExternal.IdempotencyKey() != t.IdempotencyKey() {
			return WagerResult{}, &ConflictError{Code: CodeExternalIDConflict,
				Detail: "external transaction id already processed under a different idempotency key"}
		}
		s.hook.Duplicate("idempotent_replay")
		return s.replayResult(ctx, r, byExternal)
	case errors.Is(err, ErrRepoNotFound):
	default:
		return WagerResult{}, err
	}

	// 3. Lock the wallet row (per-wallet serialization; independent wallets
	// proceed in parallel). Missing wallets cannot be persisted (FK), so the
	// rejection stays at the transport/log level.
	w, err := r.Wallets().GetForUpdate(ctx, t.WalletID())
	if err != nil {
		if errors.Is(err, ErrRepoNotFound) {
			return WagerResult{}, &NotFoundError{Code: CodeWalletNotFound, Detail: "wallet does not exist"}
		}
		return WagerResult{}, err
	}
	if w.PlayerID() != t.PlayerID() {
		return s.persistRejection(ctx, r, t, w, false, FailPlayerWalletMismatch, "wallet does not belong to the informed player")
	}

	// 4. Kind dispatch. A lost insert race surfaces as ErrDuplicateInsert
	// after the transaction aborts; recovery lives in Submit (fresh tx).
	return s.dispatch(ctx, r, t, w)
}

// dispatch runs the kind-specific path (shared by execute).
func (s *WagerService) dispatch(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet) (WagerResult, error) {
	switch t.Kind() {
	case domain.KindLoss:
		return s.processLoss(ctx, r, t, w, false)
	case domain.KindBet, domain.KindWin:
		return s.processMovement(ctx, r, t, w, nil, false)
	case domain.KindRefund, domain.KindRollback:
		return s.processReversal(ctx, r, t, w, false)
	default:
		return WagerResult{}, &ValidationError{Detail: "kind is not processable"}
	}
}

// duplicateRaceReplay resolves a lost insert race: a concurrent request with
// the same (providerId, idempotencyKey) — or the same external id under a
// different key — won the unique index. PostgreSQL only raises the violation
// after the winner's transaction commits, so the persisted outcome is visible
// and becomes an idempotent replay instead of a spurious 500.
func (s *WagerService) duplicateRaceReplay(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	existing, err := r.Transactions().GetByProviderAndKey(ctx, t.ProviderID(), t.IdempotencyKey())
	if err != nil {
		if errors.Is(err, ErrRepoNotFound) { // winner used a different key for the same external id
			existing, err = r.Transactions().GetByProviderAndExternal(ctx, t.ProviderID(), t.ExternalTxID())
		}
		if err != nil {
			return WagerResult{}, err
		}
	}
	if existing.PayloadHash() != t.PayloadHash() {
		return WagerResult{}, &ConflictError{Code: CodeIdemKeyConflict,
			Detail: "idempotency key reused with a different payload"}
	}
	s.hook.Duplicate("insert_race_replay")
	return s.replayResult(ctx, r, existing)
}

// resolve advances a claimed PENDING_REFERENCE transaction.
func (s *WagerService) resolve(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	w, err := r.Wallets().GetForUpdate(ctx, t.WalletID())
	if err != nil {
		if errors.Is(err, ErrRepoNotFound) {
			return s.persistRejection(ctx, r, t, nil, true, FailReferenceNotFound, "wallet disappeared before reference resolution")
		}
		return WagerResult{}, err
	}
	if w.PlayerID() != t.PlayerID() {
		return s.persistRejection(ctx, r, t, w, true, FailPlayerWalletMismatch, "wallet does not belong to the informed player")
	}

	ref, err := r.Transactions().GetByProviderAndExternal(ctx, t.ProviderID(), t.ReferenceExternalTxID())
	if err != nil {
		if !errors.Is(err, ErrRepoNotFound) {
			return WagerResult{}, err
		}
		return s.scheduleOrExpire(ctx, r, t, w, "reference not found")
	}
	if agreeErr := s.checkReferenceAgreement(t, w, ref); agreeErr != "" {
		return s.persistRejection(ctx, r, t, w, true, FailReferenceMismatch, agreeErr)
	}
	if ref.Status() != domain.StatusProcessed {
		if ref.Status().Terminal() {
			return s.persistRejection(ctx, r, t, w, true, FailReferenceNotProcessed,
				"referenced operation finished without success ("+string(ref.Status())+")")
		}
		return s.scheduleOrExpire(ctx, r, t, w, "reference still pending")
	}
	if kindErr := s.checkReversalKind(t.Kind(), ref.Kind()); kindErr != "" {
		return s.persistRejection(ctx, r, t, w, true, FailReferenceKindMismatch, kindErr)
	}
	if c, _ := ref.Amount().Compare(t.Amount()); c != 0 {
		return s.persistRejection(ctx, r, t, w, true, FailReferenceAmountMismatch,
			"reversal amount "+t.Amount().String()+" differs from referenced "+ref.Amount().String())
	}
	dup, err := r.Transactions().ExistsProcessedReversal(ctx, ref.ID(), t.Kind())
	if err != nil {
		return WagerResult{}, err
	}
	if dup {
		return s.persistRejection(ctx, r, t, w, true, FailDuplicateReversal,
			"referenced transaction already received a successful "+string(t.Kind()))
	}
	return s.applyReversal(ctx, r, t, w, ref, true)
}

func (s *WagerService) processLoss(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, save bool) (WagerResult, error) {
	now := time.Now().UTC()
	if t.Amount().Currency() != w.Currency() {
		return s.persistRejection(ctx, r, t, w, save, FailCurrencyMismatch, "operation currency differs from wallet currency")
	}
	bal := w.Balance()
	ver := w.Version()
	if err := t.MarkProcessed(domain.ProcessedResult{
		Direction: domain.DirectionNone, ResultAmount: domain.ZeroMoneyMust(t.Currency()),
		BalanceBefore: bal, BalanceAfter: bal, WalletVersion: ver,
	}, now); err != nil {
		return WagerResult{}, err
	}
	if err := r.Transactions().Insert(ctx, t); err != nil {
		return WagerResult{}, err
	}
	processed, err := domain.NewWagerTransactionProcessedEvent(t)
	if err != nil {
		return WagerResult{}, err
	}
	if err := r.Outbox().Enqueue(ctx, []domain.Envelope{processed}); err != nil {
		return WagerResult{}, err
	}
	return WagerResult{Transaction: t, WalletBalance: w.Balance(), WalletVersion: w.Version()}, nil
}

// processMovement applies BET (debit) / WIN (credit).
func (s *WagerService) processMovement(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, ref *domain.WagerTransaction, save bool) (WagerResult, error) {
	now := time.Now().UTC()
	if t.Amount().Currency() != w.Currency() {
		return s.persistRejection(ctx, r, t, w, save, FailCurrencyMismatch, "operation currency differs from wallet currency")
	}
	var mv domain.Movement
	var err error
	if t.Kind() == domain.KindBet {
		mv, err = w.Debit(t.Amount(), now)
	} else {
		mv, err = w.Credit(t.Amount(), now)
	}
	if err != nil {
		if errors.Is(err, domain.ErrInsufficientFunds) {
			return s.persistRejection(ctx, r, t, w, save, FailInsufficientFunds,
				"balance "+w.Balance().String()+" is not sufficient for "+t.Amount().String())
		}
		return WagerResult{}, err
	}
	return s.finishProcessed(ctx, r, t, w, &mv, ref, save, now)
}

// applyReversal computes the inverse movement of the reference.
func (s *WagerService) applyReversal(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, ref *domain.WagerTransaction, save bool) (WagerResult, error) {
	now := time.Now().UTC()
	if err := t.SetReference(ref.ID()); err != nil {
		return WagerResult{}, err
	}
	credit := false
	switch {
	case t.Kind() == domain.KindRefund: // refunding a BET: give money back
		credit = true
	case ref.Kind() == domain.KindBet: // rollback of a BET: give money back
		credit = true
	default: // rollback of WIN/REFUND: take the money back
		credit = false
	}
	var mv domain.Movement
	var err error
	if credit {
		mv, err = w.Credit(t.Amount(), now)
	} else {
		mv, err = w.Debit(t.Amount(), now)
	}
	if err != nil {
		if errors.Is(err, domain.ErrInsufficientFunds) {
			return s.persistRejection(ctx, r, t, w, save, FailReversalInsufficientFunds,
				"reversal requires debiting "+t.Amount().String()+" but balance is "+w.Balance().String())
		}
		return WagerResult{}, err
	}
	return s.finishProcessed(ctx, r, t, w, &mv, ref, save, now)
}

func (s *WagerService) finishProcessed(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, mv *domain.Movement, ref *domain.WagerTransaction, save bool, now time.Time) (WagerResult, error) {
	// 1. finalize state in memory, 2. persist the row (the ledger entry's FK
	// is immediate, so the transaction row must exist first), 3. ledger.
	before, after := mv.BalanceBefore, mv.BalanceAfter
	ver := mv.WalletVersion
	dir := mv.Direction
	if err := t.MarkProcessed(domain.ProcessedResult{
		Direction: dir, ResultAmount: mv.Amount,
		BalanceBefore: before, BalanceAfter: after, WalletVersion: ver,
	}, now); err != nil {
		return WagerResult{}, err
	}
	if err := persist(ctx, r, t, save); err != nil {
		return WagerResult{}, err
	}
	if mv != nil {
		entry, err := domain.NewLedgerEntry(w.ID(), t.ID(), *mv, now)
		if err != nil {
			return WagerResult{}, err
		}
		if err := r.Ledger().Append(ctx, entry); err != nil {
			return WagerResult{}, err
		}
		if err := r.Wallets().UpdateBalance(ctx, w); err != nil {
			return WagerResult{}, err
		}
	}
	processed, err := domain.NewWagerTransactionProcessedEvent(t)
	if err != nil {
		return WagerResult{}, err
	}
	balanceChanged, err := domain.NewWalletBalanceChangedEvent(t, *mv)
	if err != nil {
		return WagerResult{}, err
	}
	events := []domain.Envelope{processed, balanceChanged}
	if err := r.Outbox().Enqueue(ctx, events); err != nil {
		return WagerResult{}, err
	}
	return WagerResult{Transaction: t, WalletBalance: w.Balance(), WalletVersion: w.Version()}, nil
}

func (s *WagerService) processReversal(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, save bool) (WagerResult, error) {
	if t.Amount().Currency() != w.Currency() {
		return s.persistRejection(ctx, r, t, w, save, FailCurrencyMismatch, "operation currency differs from wallet currency")
	}
	ref, err := r.Transactions().GetByProviderAndExternal(ctx, t.ProviderID(), t.ReferenceExternalTxID())
	if err != nil {
		if !errors.Is(err, ErrRepoNotFound) {
			return WagerResult{}, err
		}
		return s.scheduleReference(ctx, r, t, "reference not found yet")
	}
	if agreeErr := s.checkReferenceAgreement(t, w, ref); agreeErr != "" {
		return s.persistRejection(ctx, r, t, w, save, FailReferenceMismatch, agreeErr)
	}
	if ref.Status() != domain.StatusProcessed {
		if ref.Status().Terminal() {
			return s.persistRejection(ctx, r, t, w, save, FailReferenceNotProcessed,
				"referenced operation finished without success ("+string(ref.Status())+")")
		}
		return s.scheduleReference(ctx, r, t, "reference still pending")
	}
	if kindErr := s.checkReversalKind(t.Kind(), ref.Kind()); kindErr != "" {
		return s.persistRejection(ctx, r, t, w, save, FailReferenceKindMismatch, kindErr)
	}
	if c, _ := ref.Amount().Compare(t.Amount()); c != 0 {
		return s.persistRejection(ctx, r, t, w, save, FailReferenceAmountMismatch,
			"reversal amount "+t.Amount().String()+" differs from referenced "+ref.Amount().String())
	}
	dup, err := r.Transactions().ExistsProcessedReversal(ctx, ref.ID(), t.Kind())
	if err != nil {
		return WagerResult{}, err
	}
	if dup {
		return s.persistRejection(ctx, r, t, w, save, FailDuplicateReversal,
			"referenced transaction already received a successful "+string(t.Kind()))
	}
	return s.applyReversal(ctx, r, t, w, ref, save)
}

func (s *WagerService) checkReferenceAgreement(t *domain.WagerTransaction, w *domain.Wallet, ref *domain.WagerTransaction) string {
	switch {
	case ref.PlayerID() != t.PlayerID():
		return "referenced operation belongs to another player"
	case ref.WalletID() != t.WalletID():
		return "referenced operation belongs to another wallet"
	case ref.Currency() != t.Currency():
		return "referenced operation currency differs"
	case ref.RoundID() != t.RoundID():
		return "referenced operation belongs to another round"
	default:
		return ""
	}
}

func (s *WagerService) checkReversalKind(kind, refKind domain.TxKind) string {
	switch {
	case kind == domain.KindRefund && refKind != domain.KindBet:
		return "REFUND must reference a processed BET"
	case kind == domain.KindRollback &&
		refKind != domain.KindBet && refKind != domain.KindWin && refKind != domain.KindRefund:
		return "ROLLBACK must reference a processed BET, WIN or REFUND"
	default:
		return ""
	}
}

// scheduleReference durably waits for the reference (first encounter).
func (s *WagerService) scheduleReference(ctx context.Context, r Repos, t *domain.WagerTransaction, reason string) (WagerResult, error) {
	now := time.Now().UTC()
	next := now.Add(s.refBackoff(t.PendingAttempts()))
	if err := t.MarkPendingReference(next, now); err != nil {
		return WagerResult{}, err
	}
	if err := r.Transactions().Insert(ctx, t); err != nil {
		return WagerResult{}, err
	}
	pendingRef, err := domain.NewWagerTransactionPendingReferenceEvent(t)
	if err != nil {
		return WagerResult{}, err
	}
	if err := r.Outbox().Enqueue(ctx, []domain.Envelope{pendingRef}); err != nil {
		return WagerResult{}, err
	}
	s.hook.ReferenceOutcome("scheduled")
	s.log.InfoContext(ctx, "operation waiting for reference",
		slog.String("transactionId", t.ID().String()),
		slog.String("reference", t.ReferenceExternalTxID()),
		slog.String("reason", reason))
	return WagerResult{Transaction: t, WalletBalance: domain.ZeroMoneyMust(t.Currency()), WalletVersion: 0}, nil
}

// scheduleOrExpire retries or finally rejects a waiting transaction (worker).
func (s *WagerService) scheduleOrExpire(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, reason string) (WagerResult, error) {
	now := time.Now().UTC()
	if t.PendingAttempts() >= s.refMaxAttempts || now.Sub(t.CreatedAt()) > s.refTTL {
		return s.persistRejection(ctx, r, t, w, true, FailReferenceNotFound,
			"reference was not delivered within the retry policy ("+reason+")")
	}
	next := now.Add(s.refBackoff(t.PendingAttempts()))
	if err := t.RetryPendingReference(next); err != nil {
		return WagerResult{}, err
	}
	if err := r.Transactions().Save(ctx, t); err != nil {
		return WagerResult{}, err
	}
	s.hook.ReferenceOutcome("retry_scheduled")
	return WagerResult{Transaction: t, WalletBalance: w.Balance(), WalletVersion: w.Version()}, nil
}

// persistRejection records the REJECTED transaction (auditable, stable code)
// plus the WagerTransactionRejected event — all in the same commit.
func (s *WagerService) persistRejection(ctx context.Context, r Repos, t *domain.WagerTransaction, w *domain.Wallet, save bool, code, msg string) (WagerResult, error) {
	now := time.Now().UTC()
	if err := t.MarkRejected(code, msg, now); err != nil {
		return WagerResult{}, err
	}
	if err := persist(ctx, r, t, save); err != nil {
		return WagerResult{}, err
	}
	rejected, err := domain.NewWagerTransactionRejectedEvent(t)
	if err != nil {
		return WagerResult{}, err
	}
	if err := r.Outbox().Enqueue(ctx, []domain.Envelope{rejected}); err != nil {
		return WagerResult{}, err
	}
	res := WagerResult{Transaction: t}
	if w != nil {
		res.WalletBalance, res.WalletVersion = w.Balance(), w.Version()
	} else {
		res.WalletBalance = domain.ZeroMoneyMust(t.Currency())
	}
	return res, nil
}

// replayResult rebuilds the original observed outcome without reapplying
// anything. For terminal rows the stored snapshot is authoritative.
func (s *WagerService) replayResult(ctx context.Context, r Repos, t *domain.WagerTransaction) (WagerResult, error) {
	res := WagerResult{Transaction: t, IdempotentReplay: true}
	if t.BalanceAfter() != nil {
		res.WalletBalance = *t.BalanceAfter()
		if t.WalletVersionAfter() != nil {
			res.WalletVersion = *t.WalletVersionAfter()
		}
		return res, nil
	}
	// No movement recorded (LOSS / PENDING_REFERENCE): report current state.
	w, err := r.Wallets().GetByID(ctx, t.WalletID())
	if err != nil {
		if errors.Is(err, ErrRepoNotFound) {
			res.WalletBalance = domain.ZeroMoneyMust(t.Currency())
			return res, nil
		}
		return WagerResult{}, err
	}
	res.WalletBalance, res.WalletVersion = w.Balance(), w.Version()
	return res, nil
}

// refBackoff: exponential 1s,2s,4s,... capped at 30s.
func (s *WagerService) refBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	d := time.Duration(math.Pow(2, float64(attempts))) * time.Second
	if d > 30*time.Second || d <= 0 {
		return 30 * time.Second
	}
	return d
}

func persist(ctx context.Context, r Repos, t *domain.WagerTransaction, save bool) error {
	if save {
		return r.Transactions().Save(ctx, t)
	}
	return r.Transactions().Insert(ctx, t)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
