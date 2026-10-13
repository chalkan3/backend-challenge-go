package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chalkan3/backend-challenge-go/internal/domain"
	"github.com/chalkan3/backend-challenge-go/internal/service"
)

type postingRepo struct{ ex querier }

// Append writes every leg of one balanced journal entry in a single statement.
// The entry is validated by domain.NewJournalEntry before it gets here, so the
// rows are balanced by construction; the DB trigger re-checks the player leg
// against the ledger row that was committed moments earlier in the SAME
// transaction.
//
// It uses one INSERT with unnest over parallel arrays rather than N statements:
// a journal entry is a unit, it should hit the database as one.
func (r *postingRepo) Append(ctx context.Context, e domain.JournalEntry) error {
	ps := e.Postings()
	if len(ps) == 0 {
		// Unreachable: NewJournalEntry rejects an empty entry. Guard anyway so
		// a future caller cannot turn a no-op into a silently balanced journal.
		return domain.ErrUnbalancedJournal.With("refusing to append an empty journal entry for transaction %s", e.TransactionID())
	}

	ids := make([]uuid.UUID, len(ps))
	kinds := make([]string, len(ps))
	refs := make([]string, len(ps))
	dirs := make([]string, len(ps))
	amounts := make([]int64, len(ps))
	currency := e.Currency()
	createdAt := e.CreatedAt()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	for i, p := range ps {
		id, err := domain.NewID()
		if err != nil {
			return err
		}
		ids[i] = id
		kinds[i] = p.Account().Kind().String()
		refs[i] = p.Account().Ref()
		dirs[i] = string(p.Direction())
		amounts[i] = p.Amount().AmountMinor()
	}

	_, err := r.ex.Exec(ctx, `
		INSERT INTO postings
			(id, transaction_id, account_kind, account_ref, direction, currency, amount_minor, created_at)
		SELECT unnest($1::uuid[]), $2, unnest($3::text[]), unnest($4::text[]),
		       unnest($5::text[]), $6, unnest($7::bigint[]), $8`,
		ids, e.TransactionID(), kinds, refs, dirs, currency, amounts, createdAt)
	return err
}

var _ service.PostingRepository = (*postingRepo)(nil)
