package domain

import (
	"errors"
	"testing"
	"time"
)

func mustNewExternal(t *testing.T, p ExternalTxParams) *WagerTransaction {
	t.Helper()
	tx, err := NewExternalTransaction(p)
	if err != nil {
		t.Fatalf("NewExternalTransaction: %v", err)
	}
	return tx
}

func extParams(kind TxKind, amount string) ExternalTxParams {
	return ExternalTxParams{
		ProviderID: "provider-a", ExternalTransactionID: "tx-1",
		PlayerID: NewIDMustT(), WalletID: NewIDMustT(),
		RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Money: mustParseT(amount),
		ReceivedVia: ViaHTTP, Now: time.Now().UTC(),
	}
}

func mustParseT(s string) Money {
	m, err := ParseMoney(s, "BRL")
	if err != nil {
		panic(err)
	}
	return m
}

func TestExternalTransactionValidation(t *testing.T) {
	// the five external kinds with their zero/positive policy
	for _, k := range []TxKind{KindBet, KindWin, KindRefund, KindRollback} {
		p := extParams(k, "25.00")
		if k == KindRefund || k == KindRollback {
			p.ReferenceExternalTxID = "tx-0"
		}
		if _, err := NewExternalTransaction(p); err != nil {
			t.Errorf("%s positive: %v", k, err)
		}
	}
	if _, err := NewExternalTransaction(extParams(KindLoss, "0.00")); err != nil {
		t.Errorf("LOSS zero: %v", err)
	}
	if _, err := NewExternalTransaction(extParams(KindLoss, "0.01")); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("LOSS non-zero must fail: %v", err)
	}
	if _, err := NewExternalTransaction(extParams(KindBet, "0.00")); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("BET zero must fail: %v", err)
	}
	if _, err := NewExternalTransaction(extParams(KindWin, "-1.00")); !errors.Is(err, ErrInvalidAmount) {
		t.Errorf("WIN negative must fail: %v", err)
	}
	// reversals require a reference
	for _, k := range []TxKind{KindRefund, KindRollback} {
		if _, err := NewExternalTransaction(extParams(k, "25.00")); !errors.Is(err, ErrMissingReference) {
			t.Errorf("%s without reference: %v", k, err)
		}
	}
	// OPENING is internal-only
	if _, err := NewExternalTransaction(extParams(KindOpening, "1.00")); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("OPENING via external channel: %v", err)
	}
	if _, err := NewExternalTransaction(ExternalTxParams{Kind: "NOPE", Money: mustParseT("1.00")}); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("unknown kind: %v", err)
	}
	// missing mandatory external metadata
	bad := extParams(KindBet, "1.00")
	bad.ProviderID = ""
	if _, err := NewExternalTransaction(bad); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("missing provider: %v", err)
	}
	bad2 := extParams(KindBet, "1.00")
	bad2.ExternalTransactionID = ""
	if _, err := NewExternalTransaction(bad2); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("missing external id: %v", err)
	}
	// valid reversal
	p := extParams(KindRefund, "25.00")
	p.ReferenceExternalTxID = "tx-1"
	if _, err := NewExternalTransaction(p); err != nil {
		t.Errorf("valid REFUND: %v", err)
	}
}

func TestOpeningTransaction(t *testing.T) {
	tx, err := NewOpeningTransaction(NewIDMustT(), NewIDMustT(), "BRL", mustParseT("1000.00"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if tx.Origin() != OriginInternal || tx.Kind() != KindOpening || tx.Status() != StatusPending {
		t.Fatalf("opening tx kind=%s origin=%s status=%s", tx.Kind(), tx.Origin(), tx.Status())
	}
	if tx.ProviderID() != "" || tx.ExternalTxID() != "" || tx.IdempotencyKey() != "" ||
		tx.RoundID() != "" || tx.GameID() != "" || tx.PayloadHash() != "" {
		t.Fatal("opening must carry no external metadata")
	}
	// zero opening is valid too
	if _, err := NewOpeningTransaction(NewIDMustT(), NewIDMustT(), "BRL", mustParseT("0.00"), time.Now().UTC()); err != nil {
		t.Errorf("zero opening: %v", err)
	}
	// negative opening is not
	if _, err := NewOpeningTransaction(NewIDMustT(), NewIDMustT(), "BRL", mustParseT("-0.01"), time.Now().UTC()); !errors.Is(err, ErrInvalidAmount) {
		t.Error("negative opening must fail")
	}
}

func TestStateMachine(t *testing.T) {
	now := time.Now().UTC()
	tx := mustNewExternal(t, extParams(KindBet, "25.00"))
	if tx.Status() != StatusPending {
		t.Fatal("starts PENDING")
	}
	before := mustParseT("1000.00")
	after := mustParseT("975.00")
	// PENDING -> PROCESSED with the financial result observed at processing time
	if err := tx.MarkProcessed(ProcessedResult{
		Direction: DirectionDebit, ResultAmount: tx.Amount(),
		BalanceBefore: before, BalanceAfter: after, WalletVersion: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusProcessed || !tx.Terminal() {
		t.Fatal("PROCESSED terminal")
	}
	if tx.BalanceAfter() == nil || !tx.BalanceAfter().Equals(after) {
		t.Fatal("processed must persist the observed balance")
	}
	// terminal transactions never transition again
	if err := tx.MarkRejected("X", "y", now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("reject after processed: %v", err)
	}
	if err := tx.MarkPendingReference(now, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("pending-ref after processed: %v", err)
	}
	if err := tx.MarkProcessed(ProcessedResult{Direction: DirectionNone, ResultAmount: tx.Amount()}, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("reprocess: %v", err)
	}
	if err := tx.MarkFailed("X", "y", now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("fail after processed: %v", err)
	}

	// terminal transitions require a failure code
	txr := mustNewExternal(t, extParams(KindWin, "1.00"))
	if err := txr.MarkRejected("", "", now); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("terminal without failure code: %v", err)
	}

	// no-movement processed result (LOSS) must carry a zero amount
	tx2 := mustNewExternal(t, extParams(KindLoss, "0.00"))
	if err := tx2.MarkProcessed(ProcessedResult{Direction: DirectionNone, ResultAmount: mustParseT("0.01"),
		BalanceBefore: before, BalanceAfter: before, WalletVersion: 1}, now); !errors.Is(err, ErrLedgerArithmetic) {
		t.Errorf("LOSS with non-zero result: %v", err)
	}
	if err := tx2.MarkProcessed(ProcessedResult{Direction: DirectionNone, ResultAmount: mustParseT("0.00"),
		BalanceBefore: before, BalanceAfter: before, WalletVersion: 1}, now); err != nil {
		t.Errorf("LOSS valid processed: %v", err)
	}
	if tx2.ResultDirection() != DirectionNone {
		t.Error("LOSS must have no movement direction")
	}

	// PENDING -> PENDING_REFERENCE -> REJECTED (ttl)
	rp := extParams(KindRefund, "25.00")
	rp.ReferenceExternalTxID = "tx-1"
	tx3 := mustNewExternal(t, rp)
	if err := tx3.MarkPendingReference(now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if tx3.Status() != StatusPendingReference || tx3.PendingAttempts() != 1 {
		t.Fatalf("pending ref: %s attempts=%d", tx3.Status(), tx3.PendingAttempts())
	}
	// durable retry without leaving PENDING_REFERENCE
	if err := tx3.RetryPendingReference(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tx3.PendingAttempts() != 2 {
		t.Fatal("retry must bump attempts")
	}
	if err := tx3.MarkRejected("REFERENCE_NOT_FOUND", "ttl", now); err != nil {
		t.Fatal(err)
	}
	if tx3.FailureCode() != "REFERENCE_NOT_FOUND" {
		t.Fatal("failure code persisted")
	}

	// FAILED path
	tx4 := mustNewExternal(t, extParams(KindWin, "1.00"))
	if err := tx4.MarkFailed("INFRA", "boom", now); err != nil {
		t.Fatal(err)
	}
	if !tx4.Terminal() {
		t.Fatal("FAILED terminal")
	}

	// rehydration rebuilds state without transitioning anything
	fc := tx3.FailureCode()
	rp2 := RehydrateParams{
		ID: tx3.ID(), Kind: tx3.Kind(), Origin: tx3.Origin(),
		ProviderID: tx3.providerID, ExternalTxID: tx3.externalTxID,
		IdempotencyKey: tx3.idempotencyKey, PayloadHash: tx3.payloadHash,
		PlayerID: tx3.PlayerID(), WalletID: tx3.WalletID(),
		Currency: "BRL", AmountMinor: tx3.Amount().AmountMinor(),
		Status: StatusRejected, FailureCode: &fc,
		ResultDirection: tx3.ResultDirection(), ResultAmountMinor: 0,
		ReceivedVia: ViaHTTP,
		CreatedAt:   now, UpdatedAt: now,
	}
	r, err := RehydrateWagerTransaction(rp2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status() != StatusRejected || r.PendingAttempts() != 0 {
		t.Fatal("rehydration changed state")
	}
	if !r.Terminal() {
		t.Fatal("rehydrated REJECTED must be terminal")
	}
}

func TestSetReference(t *testing.T) {
	p := extParams(KindRollback, "25.00")
	p.ReferenceExternalTxID = "tx-9"
	tx := mustNewExternal(t, p)
	if tx.ReferenceTxID() != nil {
		t.Fatal("reference starts unresolved")
	}
	id := NewIDMustT()
	tx.SetReference(id)
	if tx.ReferenceTxID() == nil || *tx.ReferenceTxID() != id {
		t.Fatal("resolved reference stored")
	}
}
