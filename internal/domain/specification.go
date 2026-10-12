package domain

// Specification is the classic Specification pattern (Evans/Fowler) adapted
// to idiomatic Go: a small, composable, self-describing predicate that either
// is satisfied or reports the stable domain error explaining the violation.
//
// Why a Specification instead of an if-chain:
//   - each business rule has a name, so it is individually testable and
//     greppable ("which rule rejected this?");
//   - rules compose with And/Or/Not, so a use case reads as a sentence of
//     named rules rather than a wall of conditionals;
//   - the error returned is the exact typed sentinel the transports already
//     map to HTTP codes and failureCode values, so composing rules cannot
//     silently change the external contract.
//
// A Specification over a value of type T is intentionally total: it never
// panics and never returns a raw error — only nil or a *Error with a stable
// Code.
type Specification[T any] interface {
	// IsSatisfiedBy reports nil when the candidate satisfies the rule, or a
	// descriptive *Error otherwise.
	IsSatisfiedBy(candidate T) error
	// Describe returns a human/telemetry-friendly label for the rule.
	Describe() string
}

// SpecFunc adapts a function to the Specification interface, mirroring the
// http.HandlerFunc idiom. It lets rules be declared inline while still being
// first-class, named, composable values.
type SpecFunc[T any] struct {
	Desc string
	Fn   func(T) error
}

// IsSatisfiedBy runs the underlying predicate.
func (s SpecFunc[T]) IsSatisfiedBy(candidate T) error { return s.Fn(candidate) }

// Describe returns the rule label.
func (s SpecFunc[T]) Describe() string { return s.Desc }

// AndSpec composes two specifications: satisfied only if both hold. The
// left rule is evaluated first so the most fundamental error wins (order is
// part of the contract and is asserted by tests).
type AndSpec[T any] struct{ Left, Right Specification[T] }

// IsSatisfiedBy returns the left error first, then the right.
func (s AndSpec[T]) IsSatisfiedBy(candidate T) error {
	if err := s.Left.IsSatisfiedBy(candidate); err != nil {
		return err
	}
	return s.Right.IsSatisfiedBy(candidate)
}

// Describe labels the composition.
func (s AndSpec[T]) Describe() string { return s.Left.Describe() + " AND " + s.Right.Describe() }

// And composes rules left-to-right, preserving evaluation order and the
// first-violated error. It is variadic so a rule set reads top-to-bottom.
func And[T any](specs ...Specification[T]) Specification[T] {
	if len(specs) == 0 {
		return SpecFunc[T]{Desc: "always", Fn: func(T) error { return nil }}
	}
	acc := specs[0]
	for _, next := range specs[1:] {
		acc = AndSpec[T]{Left: acc, Right: next}
	}
	return acc
}

// NotSpec inverts a predicate into a failure with the supplied code/message.
type NotSpec[T any] struct {
	Inner Specification[T]
	Code  string
	Msg   string
}

// IsSatisfiedBy fails when the inner rule holds.
func (s NotSpec[T]) IsSatisfiedBy(candidate T) error {
	if s.Inner.IsSatisfiedBy(candidate) == nil {
		return NewError(s.Code, s.Msg)
	}
	return nil
}

// Describe labels the negation.
func (s NotSpec[T]) Describe() string { return "NOT(" + s.Inner.Describe() + ")" }
