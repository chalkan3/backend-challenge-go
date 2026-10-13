// Pure-function coverage for the WagerService internals. These are the rules
// that never touch a repository — reference agreement and reversal-kind
// compatibility — plus the small helpers (backoff, nil-string deref). They are
// tested white-box (same package) precisely because they are unexported: the
// behaviour is a property of the service, and pinning it here is cheaper and
// more focused than driving a full transaction through Submit to observe a
// string that comes out of a switch.
package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

func mustMoney(t *testing.T, amount, currency string) domain.Money {
	t.Helper()
	m, err := domain.NewMoney(100, currency) // 100 minor units = 1.00
	if err != nil {
		t.Fatalf("NewMoney(%s): %v", amount, err)
	}
	return m
}

func mustTx(t *testing.T, kind string, player, wallet uuid.UUID, currency, round string) *domain.WagerTransaction {
	t.Helper()
	tx, err := domain.NewExternalTransaction(domain.ExternalTxParams{
		ProviderID:            "p",
		ExternalTransactionID: "e-" + uuid.NewString(),
		PlayerID:              player,
		WalletID:              wallet,
		RoundID:               round,
		GameID:                "g",
		Kind:                  domain.TxKind(kind),
		Money:                 mustMoney(t, "1.00", currency),
		ReceivedVia:           domain.ViaHTTP,
		CorrelationID:         "c",
		Now:                   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("building a %s transaction: %v", kind, err)
	}
	return tx
}

// TestCheckReferenceAgreement walks every mismatch branch and the agreeing
// case. The point is that the message names *which* dimension differs: a
// caller debugging a rejected REFUND needs to know it was the round, not the
// wallet.
func TestCheckReferenceAgreement(t *testing.T) {
	player := uuid.New()
	wallet := uuid.New()
	base := mustTx(t, "BET", player, wallet, "BRL", "round-1")

	tests := []struct {
		name string
		ref  *domain.WagerTransaction
		want string
	}{
		{
			name: "same player, wallet, currency and round agrees",
			ref:  mustTx(t, "BET", player, wallet, "BRL", "round-1"),
			want: "",
		},
		{
			name: "different player",
			ref:  mustTx(t, "BET", uuid.New(), wallet, "BRL", "round-1"),
			want: "referenced operation belongs to another player",
		},
		{
			name: "different wallet",
			ref:  mustTx(t, "BET", player, uuid.New(), "BRL", "round-1"),
			want: "referenced operation belongs to another wallet",
		},
		{
			name: "different currency",
			ref:  mustTx(t, "BET", player, wallet, "USD", "round-1"),
			want: "referenced operation currency differs",
		},
		{
			name: "different round",
			ref:  mustTx(t, "BET", player, wallet, "BRL", "round-2"),
			want: "referenced operation belongs to another round",
		},
	}
	svc := &WagerService{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.checkReferenceAgreement(base, nil, tc.ref); got != tc.want {
				t.Fatalf("checkReferenceAgreement() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckReversalKind covers both reversal rules. REFUND may only undo a
// BET; ROLLBACK may undo a BET, WIN or REFUND — and every other target is
// rejected, which is the branch a table makes obvious.
func TestCheckReversalKind(t *testing.T) {
	svc := &WagerService{}
	tests := []struct {
		name    string
		kind    domain.TxKind
		refKind domain.TxKind
		want    string
	}{
		{"REFUND over BET is allowed", domain.KindRefund, domain.KindBet, ""},
		{"REFUND over WIN is rejected", domain.KindRefund, domain.KindWin, "REFUND must reference a processed BET"},
		{"REFUND over REFUND is rejected", domain.KindRefund, domain.KindRefund, "REFUND must reference a processed BET"},
		{"ROLLBACK over BET is allowed", domain.KindRollback, domain.KindBet, ""},
		{"ROLLBACK over WIN is allowed", domain.KindRollback, domain.KindWin, ""},
		{"ROLLBACK over REFUND is allowed", domain.KindRollback, domain.KindRefund, ""},
		{"ROLLBACK over LOSS is rejected", domain.KindRollback, domain.KindLoss, "ROLLBACK must reference a processed BET, WIN or REFUND"},
		{"a non-reversal kind is unconstrained", domain.KindBet, domain.KindWin, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.checkReversalKind(tc.kind, tc.refKind); got != tc.want {
				t.Fatalf("checkReversalKind(%s, %s) = %q, want %q", tc.kind, tc.refKind, got, tc.want)
			}
		})
	}
}

// TestRefBackoff pins the exponential schedule and its two guards. The
// negative-attempts case is not hypothetical: the column is an int and a
// rollback could leave it unset, so the function must not compute
// math.Pow(2, -1) and return a sub-second retry.
func TestRefBackoff(t *testing.T) {
	svc := &WagerService{}
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{-1, 1 * time.Second}, // clamped to 0 → 2^0
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},  // 32s exceeds the cap
		{64, 30 * time.Second}, // 2^64 overflows; the d<=0 guard still caps
	}
	for _, tc := range tests {
		if got := svc.refBackoff(tc.attempts); got != tc.want {
			t.Errorf("refBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

// TestDerefStr documents the nil-to-empty contract used all over the service.
func TestDerefStr(t *testing.T) {
	if got := derefStr(nil); got != "" {
		t.Fatalf("derefStr(nil) = %q, want empty", got)
	}
	s := "value"
	if got := derefStr(&s); got != "value" {
		t.Fatalf("derefStr(&%q) = %q", s, got)
	}
}
