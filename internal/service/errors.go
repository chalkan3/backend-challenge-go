// Package service contains the use cases (application layer). It depends
// only on the domain and on the ports defined here; HTTP, SQS, Fx and pgx
// stay outside. Errors are classified so transports can map them
// deterministically.
package service

import (
	"errors"
)

// ValidationError: malformed request (HTTP 400; SQS: poison message).
type ValidationError struct{ Detail string }

func (e *ValidationError) Error() string { return "VALIDATION_FAILED: " + e.Detail }

// NotFoundError: referenced entity does not exist (HTTP 404).
type NotFoundError struct{ Code, Detail string }

func (e *NotFoundError) Error() string { return e.Code + ": " + e.Detail }

// ConflictError: idempotency/identity conflicts (HTTP 409).
type ConflictError struct{ Code, Detail string }

func (e *ConflictError) Error() string { return e.Code + ": " + e.Detail }

// UnavailableError: transient infrastructure failure (HTTP 503; SQS: retry).
type UnavailableError struct{ Cause error }

func (e *UnavailableError) Error() string { return "TRANSIENT_UNAVAILABLE: " + e.Cause.Error() }
func (e *UnavailableError) Unwrap() error { return e.Cause }

// Shared sentinels and transport codes.
var (
	// ErrRepoNotFound is returned by repositories when a row does not exist.
	ErrRepoNotFound = errors.New("repository: row not found")
	// ErrDuplicateInsert signals a unique-constraint violation during an
	// insert of an idempotent identity: a concurrent request won the race.
	// Callers re-fetch and replay the persisted outcome.
	ErrDuplicateInsert = errors.New("repository: duplicate identity")
)

// Stable transport codes.
const (
	CodeValidationFailed    = "VALIDATION_FAILED"
	CodeNotFound            = "NOT_FOUND"
	CodeWalletNotFound      = "WALLET_NOT_FOUND"
	CodeTransactionNotFound = "TRANSACTION_NOT_FOUND"
	CodeIdemKeyConflict     = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalIDConflict  = "EXTERNAL_TRANSACTION_ID_CONFLICT"
	CodeWalletExists        = "WALLET_ALREADY_EXISTS"
	CodeProviderMismatch    = "PROVIDER_MISMATCH"
	CodeUnauthorized        = "UNAUTHORIZED"
	CodeForbidden           = "FORBIDDEN"
	CodeTransient           = "TRANSIENT_UNAVAILABLE"
)

// Classifiers used by transports.
func IsNotFound(err error) bool {
	var n *NotFoundError
	return errors.As(err, &n)
}

func IsConflict(err error) bool {
	var c *ConflictError
	return errors.As(err, &c)
}

func IsValidation(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}

func IsUnavailable(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u)
}

// ConflictCode returns the stable code of a ConflictError.
func ConflictCode(err error) string {
	var c *ConflictError
	if errors.As(err, &c) {
		return c.Code
	}
	return CodeValidationFailed
}

// NotFoundCode returns the stable code of a NotFoundError.
func NotFoundCode(err error) string {
	var n *NotFoundError
	if errors.As(err, &n) {
		return n.Code
	}
	return CodeNotFound
}
