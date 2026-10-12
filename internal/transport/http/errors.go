package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	kithttp "github.com/go-kit/kit/transport/http"

	"github.com/chalkan3/backend-challenge-go/internal/service"
)

// errorEncoder maps classified errors to HTTP contracts:
//
//	400 VALIDATION_FAILED        — malformed input (never persisted)
//	401 UNAUTHORIZED             — missing/invalid/expired credentials
//	403 FORBIDDEN                — authenticated but not allowed
//	404 *_NOT_FOUND              — wallet/transaction does not exist
//	409 IDEMPOTENCY_KEY_CONFLICT / EXTERNAL_TRANSACTION_ID_CONFLICT /
//	    WALLET_ALREADY_EXISTS
//	422                          — persisted business rejection (REJECTED),
//	                               body carries failureCode/failureMessage
//	503 TRANSIENT_UNAVAILABLE    — PostgreSQL/SQS temporarily unavailable
//	500 INTERNAL_ERROR           — bug or unknown failure
type errorEncoder = kithttp.ErrorEncoder

func makeErrorEncoder() kithttp.ErrorEncoder {
	return func(_ context.Context, err error, w http.ResponseWriter) {
		if he, ok := isHTTPError(err); ok {
			writeError(w, he.Status, he.Code, he.Detail)
			return
		}
		switch {
		case service.IsValidation(err) || isValidationErr(err):
			writeError(w, http.StatusBadRequest, service.CodeValidationFailed, err.Error())
		case service.IsNotFound(err):
			writeError(w, http.StatusNotFound, service.NotFoundCode(err), err.Error())
		case service.IsConflict(err):
			writeError(w, http.StatusConflict, service.ConflictCode(err), err.Error())
		case service.IsUnavailable(err):
			writeError(w, http.StatusServiceUnavailable, service.CodeTransient, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
	}
}

func isValidationErr(err error) bool {
	var v *validationError
	return errors.As(err, &v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": detail},
	})
}

// encodeJSONWithStatus returns an encoder with a fixed status code.
func encodeJSONWithStatus(status int) kithttp.EncodeResponseFunc {
	return func(ctx context.Context, w http.ResponseWriter, response interface{}) error {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		return json.NewEncoder(w).Encode(response)
	}
}

// encodeSubmit maps the persisted outcome to its status:
// PROCESSED/replay → 200, PENDING_REFERENCE → 202, REJECTED → 422.
func encodeSubmit(ctx context.Context, w http.ResponseWriter, response interface{}) error {
	resp := response.(submitResponse)
	status := http.StatusOK
	switch resp.Status {
	case "PENDING_REFERENCE", "PENDING":
		status = http.StatusAccepted
	case "REJECTED":
		status = http.StatusUnprocessableEntity
	case "FAILED":
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(resp)
}
