package domain

import (
	"errors"
	"fmt"
)

// Error is a domain error carrying a stable, machine-readable Code.
// Codes are part of the external contract (HTTP bodies, failureCode on
// transactions) and must never change meaning once published.
type Error struct {
	Code    string
	Message string
	base    error // identity chain for errors.Is
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// With returns a copy of the error with a more specific message, preserving
// identity for errors.Is via the unwrap chain.
func (e *Error) With(format string, args ...any) *Error {
	return &Error{Code: e.Code, Message: fmt.Sprintf(format, args...), base: e}
}

// Unwrap exposes the identity chain so errors.Is(err, ErrSentinel) holds for
// every error derived from a sentinel via With.
func (e *Error) Unwrap() error { return e.base }

// NewError builds a domain error with a stable code.
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Domain error sentinels. Identity is the pointer; wrap with .With to add
// context while keeping errors.Is working.
var (
	ErrInvalidMoney        = &Error{Code: "INVALID_MONEY", Message: "invalid money value"}
	ErrMoneyOverflow       = &Error{Code: "MONEY_OVERFLOW", Message: "money amount out of supported range"}
	ErrInvalidCurrency     = &Error{Code: "INVALID_CURRENCY", Message: "currency must be a 3-letter ISO 4217 code"}
	ErrCurrencyMismatch    = &Error{Code: "CURRENCY_MISMATCH", Message: "operation currency differs from wallet currency"}
	ErrInvalidAmount       = &Error{Code: "INVALID_AMOUNT", Message: "amount violates the operation value policy"}
	ErrInvalidKind         = &Error{Code: "INVALID_KIND", Message: "unknown transaction kind"}
	ErrKindNotAllowed      = &Error{Code: "KIND_NOT_ALLOWED", Message: "operation kind not allowed on this channel"}
	ErrInsufficientFunds   = &Error{Code: "INSUFFICIENT_FUNDS", Message: "insufficient funds"}
	ErrInvalidTransition   = &Error{Code: "INVALID_STATE_TRANSITION", Message: "invalid transaction state transition"}
	ErrWalletAlreadyExists = &Error{Code: "WALLET_ALREADY_EXISTS", Message: "wallet already exists for player and currency"}
	ErrLedgerArithmetic    = &Error{Code: "LEDGER_ARITHMETIC", Message: "ledger entry does not balance"}
	ErrInvalidTransaction  = &Error{Code: "INVALID_TRANSACTION", Message: "transaction payload is invalid"}
	ErrMissingReference    = &Error{Code: "REFERENCE_REQUIRED", Message: "referenceExternalTransactionId is required for REFUND/ROLLBACK"}
	ErrZeroVersion         = &Error{Code: "INVALID_WALLET_VERSION", Message: "wallet version must start at 1"}
)

// FailureCode classifies any error into a stable code. Non-domain errors map
// to INTERNAL_ERROR (they must never leak as business rejections).
func FailureCode(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return "INTERNAL_ERROR"
}
