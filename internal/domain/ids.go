package domain

import "github.com/google/uuid"

// NewID returns a UUIDv7 (time-ordered) identifier. UUIDv7 keeps index
// locality in PostgreSQL and makes cursor pagination stable.
func NewID() (uuid.UUID, error) {
	return uuid.NewV7()
}

// MustNewID is only for tests and non-failure paths where ID generation
// failing means the process cannot run at all.
func MustNewID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic("uuid generation failed: " + err.Error())
	}
	return id
}

// ParseID parses a UUID string, reporting an invalid value as a domain
// error so callers can map it to a validation failure.
func ParseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrInvalidTransaction.With("%q is not a valid id", s)
	}
	return id, nil
}
