// spans.go holds the semantic conventions of this service: one place that
// names every span and attribute, so dashboards and trace queries stay
// stable even if the call sites move.
//
// The keys follow the OpenTelemetry semantic conventions where one exists
// (db.*, messaging.*, http.*) and use the `wallet.*` namespace otherwise.
// Naming them here — instead of inlining string literals at each call site —
// keeps the vocabulary auditable: it is the contract the dashboards query.
package otel

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span names. The `wallet.` prefix groups the service in the trace UI.
const (
	SpanHTTPSubmit      = "wallet.http.submit_transaction"
	SpanWagerExecute    = "wallet.wager.execute"
	SpanWagerPrepare    = "wallet.wager.prepare"
	SpanWagerResolve    = "wallet.wager.resolve_reference"
	SpanWalletOpen      = "wallet.wallet.open"
	SpanWalletMovement  = "wallet.wallet.apply_movement"
	SpanOutboxPublish   = "wallet.outbox.publish"
	SpanOutboxClaim     = "wallet.outbox.claim"
	SpanConsumerHandle  = "wallet.sqs.consume"
	SpanConsumerPublish = "wallet.sqs.publish"
	SpanReferenceTick   = "wallet.worker.reference_tick"
)

// Terminal transaction statuses that mark a span as an error. They mirror
// domain.TxStatus but are declared here so this package keeps no dependency
// on the domain model (see RecordOutcome).
const (
	StatusRejected = "REJECTED"
	StatusFailed   = "FAILED"
)

// Attribute keys.
const (
	AttrKind           = attribute.Key("wallet.kind")
	AttrStatus         = attribute.Key("wallet.status")
	AttrFailureCode    = attribute.Key("wallet.failure_code")
	AttrChannel        = attribute.Key("wallet.channel")
	AttrOutcome        = attribute.Key("wallet.outcome")
	AttrDuplicate      = attribute.Key("wallet.idempotent_replay")
	AttrDuplicateSrc   = attribute.Key("wallet.duplicate_source")
	AttrProviderID     = attribute.Key("wallet.provider_id")
	AttrWalletID       = attribute.Key("wallet.wallet_id")
	AttrPlayerID       = attribute.Key("wallet.player_id")
	AttrTransactionID  = attribute.Key("wallet.transaction_id")
	AttrExternalTxID   = attribute.Key("wallet.external_transaction_id")
	AttrMessageID      = attribute.Key("wallet.message_id")
	AttrEventID        = attribute.Key("wallet.event_id")
	AttrEventType      = attribute.Key("wallet.event_type")
	AttrAttempts       = attribute.Key("wallet.attempts")
	AttrPendingCount   = attribute.Key("wallet.pending_count")
	AttrResolvedCount  = attribute.Key("wallet.resolved_count")
	AttrBalanceMinor   = attribute.Key("wallet.balance_minor")
	AttrWalletVersion  = attribute.Key("wallet.version")
	AttrAmountMinor    = attribute.Key("wallet.amount_minor")
	AttrDirection      = attribute.Key("wallet.direction")
	AttrCurrency       = attribute.Key("wallet.currency")
	AttrRowsAffected   = attribute.Key("wallet.rows_affected")
	AttrPoisonReason   = attribute.Key("wallet.poison_reason")
	AttrReceiptAttempt = attribute.Key("wallet.receive_count")
)

// SQL span attributes, following the db.* semantic conventions so generic
// database dashboards also work.
const (
	AttrDBSystem   = attribute.Key("db.system")
	AttrDBName     = attribute.Key("db.namespace")
	AttrDBOp       = attribute.Key("db.operation.name")
	AttrDBTable    = attribute.Key("db.collection.name")
	AttrDBRows     = attribute.Key("db.response.returned_rows")
	AttrDBSqlState = attribute.Key("db.response.status_code")
)

// Key is a local alias for attribute.Key that adds the terse builder methods
// used at call sites. Go forbids methods on foreign types, so the alias (not
// a new type) preserves assignability to attribute.KeyValue everywhere.
type Key = attribute.Key

// Attrs packs key/values into the slice expected by Span.SetAttributes.
func Attrs(kvs ...attribute.KeyValue) []attribute.KeyValue { return kvs }

// RecordOutcome stamps a classified business outcome on the span. A REJECTED
// or failed outcome is not an application error: it is a normal, expected
// result of a valid request, so the span stays non-error but keeps the
// reason. Only genuine faults call span.RecordError.
func RecordOutcome(span trace.Span, status, failureCode string) {
	span.SetAttributes(AttrStatus.String(status))
	if failureCode != "" {
		span.SetAttributes(AttrFailureCode.String(failureCode))
	}
	// A rejected business outcome is an error for tracing purposes. Setting
	// only the attribute left every rejected BET on a STATUS_CODE_UNSET span,
	// so the error-rate panels — which filter on STATUS_CODE_ERROR — stayed
	// empty while the failure was plainly visible in the attributes.
	if isFailureStatus(status) {
		detail := failureCode
		if detail == "" {
			detail = status
		}
		span.SetStatus(codes.Error, detail)
	}
}

// isFailureStatus reports whether a terminal transaction status represents a
// failure. The list is deliberately explicit: adding a new status must be a
// conscious decision about how it shows up in the traces.
func isFailureStatus(status string) bool {
	switch status {
	case StatusRejected, StatusFailed:
		return true
	default:
		return false
	}
}

// RecordError marks the span as failed and attaches the error. It is the
// single place that decides what "error" means for a span.
func RecordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Span is the tracing span type this package hands out. Aliasing instead of
// wrapping keeps the call sites idiomatic and lets an adapter satisfy any
// consumer interface without a second conversion.
type Span = trace.Span

// SpanAttrLike is the structural shape of one attribute: the service-layer
// port (service.SpanAttr) and any other {Key, Val} carrier satisfy it, so the
// port never has to import this package or the SDK.
type SpanAttrLike interface {
	AttrKey() string
	AttrVal() any
}

// KeyValues converts attributes into the slice the SDK expects. It is the
// seam the service-layer port crosses to reach this package, so the port
// itself never imports the SDK.
func KeyValues[T SpanAttrLike](attrs ...T) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		kvs = append(kvs, Key(a.AttrKey()).String(fmt.Sprint(a.AttrVal())))
	}
	return kvs
}

// SpanStartOptions turns attributes into start options, so they are attached
// at span creation (visible even if the span ends before any SetAttributes).
func SpanStartOptions[T SpanAttrLike](attrs ...T) trace.SpanStartOption {
	return trace.WithAttributes(KeyValues(attrs...)...)
}

// Typed attribute builders: booleans and numbers stay typed in the backend
// instead of being stringified by the generic formatter above.
func IntAttr(k string, v int64) attribute.KeyValue     { return attribute.Int64(k, v) }
func BoolAttr(k string, v bool) attribute.KeyValue     { return attribute.Bool(k, v) }
func StrAttr(k, v string) attribute.KeyValue           { return attribute.String(k, v) }
func FloatAttr(k string, v float64) attribute.KeyValue { return attribute.Float64(k, v) }
