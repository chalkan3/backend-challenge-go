package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
)

// openPayloadHash computes the deterministic payload hash of a wallet opening
// so a replay of the same Idempotency-Key can be told apart from a reuse of
// the key with a different body:
//
//   - Business fields only: playerId, currency and the normalized opening
//     amount ("25", "25.0" and "25.00" hash identically).
//   - The key itself and transport metadata are EXCLUDED, exactly as in the
//     wager canonical hash.
//   - Algorithm: SHA-256 over canonical JSON (map keys are sorted), hex.
func openPayloadHash(in OpenWalletInput, playerID uuid.UUID) (string, error) {
	amount, err := domain.ParseMoney(in.InitialAmount, in.Currency)
	if err != nil {
		return "", &ValidationError{Detail: "initialBalance: " + err.Error()}
	}
	payload := map[string]any{
		"playerId": playerID.String(),
		"currency": in.Currency,
		"money": map[string]string{
			"amount":   amount.String(),
			"currency": amount.Currency(),
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", &ValidationError{Detail: "payload is not canonicalizable: " + err.Error()}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalHash computes the deterministic payload hash used by idempotency:
//
//   - Business fields only: providerId, externalTransactionId, playerId,
//     walletId, roundId, gameId, kind, money{amount,currency} and
//     referenceExternalTransactionId.
//   - The idempotency key itself and transport metadata (channel,
//     correlationId, envelope messageId) are EXCLUDED.
//   - Money amount is normalized through the Money type ("25", "25.0" and
//     "25.00" hash identically); JSON marshaling sorts map keys, so field
//     order in the incoming payload is irrelevant.
//   - Algorithm: SHA-256 over the canonical JSON, hex-encoded.
//
// HTTP and SQS submissions of the same business content therefore produce
// the same hash, enabling cross-channel replay detection.
func CanonicalHash(in ExternalInput, amount domain.Money) (string, error) {
	payload := map[string]any{
		"providerId":            in.ProviderID,
		"externalTransactionId": in.ExternalTransactionID,
		"playerId":              in.PlayerID,
		"walletId":              in.WalletID,
		"roundId":               in.RoundID,
		"gameId":                in.GameID,
		"kind":                  in.Kind,
		"money": map[string]string{
			"amount":   amount.String(),
			"currency": amount.Currency(),
		},
		"referenceExternalTransactionId": in.ReferenceExternalTransactionID,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", &ValidationError{Detail: "payload is not canonicalizable: " + err.Error()}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
