package service

// Spans and attributes used by the use cases. The names live here, next to
// the port that consumes them, so the service layer owns its observability
// vocabulary and never imports a tracing SDK. The adapter (bootstrap) maps
// these strings onto the OpenTelemetry conventions; the drift is caught by
// the parity test in the otel package.
const (
	SpanWagerExecute   = "wallet.wager.execute"
	SpanWagerPrepare   = "wallet.wager.prepare"
	SpanWagerResolve   = "wallet.wager.resolve_reference"
	SpanWalletOpen     = "wallet.wallet.open"
	SpanWalletMovement = "wallet.wallet.apply_movement"
)

// Attribute keys reported on spans. They follow the OpenTelemetry semantic
// conventions where one exists and the `wallet.` namespace otherwise.
const (
	AttrKind         = "wallet.kind"
	AttrStatus       = "wallet.status"
	AttrFailureCode  = "wallet.failure_code"
	AttrChannel      = "wallet.channel"
	AttrDuplicate    = "wallet.idempotent_replay"
	AttrDuplicateSrc = "wallet.duplicate_source"
	AttrProviderID   = "wallet.provider_id"
	AttrWalletID     = "wallet.wallet_id"
	AttrPlayerID     = "wallet.player_id"
	AttrCurrency     = "wallet.currency"
	AttrAmountMinor  = "wallet.amount_minor"
	AttrBalanceMinor = "wallet.balance_minor"
	AttrWalletVer    = "wallet.version"
	AttrOutcome      = "wallet.reference_outcome"
)
