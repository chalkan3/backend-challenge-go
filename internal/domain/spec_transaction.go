package domain

// External transaction rules, expressed as composable Specifications. These
// are the single source of truth for the "shape" of an external operation;
// NewExternalTransaction evaluates them in order and returns the first
// violated rule's typed error, preserving the exact codes the HTTP/SQS
// contract already publishes.

// externalKindSpecification validates that Kind is one of the five externally
// processable kinds. OPENING is internal-only and is rejected with its own
// message so callers never read a missing-metadata error for it.
var externalKindSpecification = SpecFunc[ExternalTxParams]{
	Desc: "kind is external (BET/WIN/LOSS/REFUND/ROLLBACK)",
	Fn: func(p ExternalTxParams) error {
		switch p.Kind {
		case KindOpening:
			return ErrInvalidKind.With("kind %q is internal-only and rejected over HTTP/SQS", p.Kind)
		case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
			return nil
		default:
			return ErrInvalidKind.With("unknown kind %q", p.Kind)
		}
	},
}

// externalIdentitySpecification requires the external coordinates that
// identify the operation for idempotency by (providerId, externalTransactionId).
var externalIdentitySpecification = SpecFunc[ExternalTxParams]{
	Desc: "providerId and externalTransactionId present",
	Fn: func(p ExternalTxParams) error {
		if p.ProviderID == "" || p.ExternalTransactionID == "" {
			return ErrInvalidTransaction.With("providerId and externalTransactionId are required")
		}
		return nil
	},
}

// externalAmountSpecification enforces the per-kind zero/positive policy:
// LOSS must be exactly zero; every other external kind must be positive.
var externalAmountSpecification = SpecFunc[ExternalTxParams]{
	Desc: "amount matches the kind's zero/positive policy",
	Fn: func(p ExternalTxParams) error {
		if p.Kind == KindLoss {
			if !p.Money.IsZero() {
				return ErrInvalidAmount.With("LOSS requires amount 0.00, got %s", p.Money.String())
			}
			return nil
		}
		if !p.Money.IsPositive() {
			return ErrInvalidAmount.With("%s requires a positive amount, got %s", p.Kind, p.Money.String())
		}
		return nil
	},
}

// externalReferenceSpecification requires referenceExternalTransactionId for
// the kinds that undoes a prior operation (REFUND, ROLLBACK).
var externalReferenceSpecification = SpecFunc[ExternalTxParams]{
	Desc: "referenceExternalTransactionId present when the kind requires it",
	Fn: func(p ExternalTxParams) error {
		if (p.Kind == KindRefund || p.Kind == KindRollback) && p.ReferenceExternalTxID == "" {
			return ErrMissingReference
		}
		return nil
	},
}

// externalTransactionSpecification is the composed rule set for external
// transaction creation, evaluated in the order the contract guarantees.
//
// Note on currency: the movement-vs-wallet currency equality is enforced by
// the Wallet movement rules (Credit/Debit) at apply time, which is where the
// wallet currency is known; it is intentionally not duplicated here.
var externalTransactionSpecification = And(
	externalKindSpecification,
	externalIdentitySpecification,
	externalAmountSpecification,
	externalReferenceSpecification,
)
