package ootle

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
)

// Reservation is the result of ReserveDispense -- the exact analog of
// internal/faucet's Reservation type, duplicated here (rather than
// imported) so this package stays fully self-contained; see
// faucet.Reservation's doc comment for the full semantics this type
// mirrors.
type Reservation struct {
	ID            int64
	RateLimited   bool
	LastSuccessAt time.Time
}

// Repository is the narrow persistence surface Service depends on, the
// Ootle-path analog of internal/faucet.Repository -- same
// ReserveDispense/FinalizeDispense/Ping shape, backed by its own,
// separate ootle_dispenses table (see migrations/tables.sql) so L1 and
// L2 rate limits never collide.
type Repository interface {
	// ReserveDispense atomically checks the rolling-window rate limit
	// for address and ip and, if neither is currently rate limited,
	// inserts a placeholder (success=false) audit row and returns its
	// ID -- see PGRepository.ReserveDispense for the exact locking
	// mechanism, identical in spirit to
	// faucet.PGRepository.ReserveDispense's advisory-lock approach,
	// just against ootle_dispenses instead of dispenses.
	ReserveDispense(ctx context.Context, address, ip string, amount uint64, since, now time.Time) (Reservation, error)
	// FinalizeDispense updates the placeholder row ReserveDispense
	// inserted (identified by id) with the wallet call's real outcome.
	// txID is a string here (not a uint64, as in internal/faucet),
	// since Ootle transaction ids are hex strings of the form
	// "tx_<hex>", not the L1 wallet's numeric transaction id.
	FinalizeDispense(ctx context.Context, id int64, success bool, errMsg string, txID string) error
	// Ping verifies the underlying database connection is reachable.
	Ping(ctx context.Context) error
}

// PGRepository is the production Repository, backed by a pgx connection
// pool -- deliberately the SAME pool internal/faucet.PGRepository uses
// in production (main.go wires both repositories around
// milieu.GetRawPGXPool()'s single pool), just issuing queries against
// the separate ootle_dispenses table instead of dispenses.
type PGRepository struct {
	pool *pgxpool.Pool
}

// NewPGRepository builds a PGRepository around an already-connected
// pool.
func NewPGRepository(pool *pgxpool.Pool) *PGRepository {
	return &PGRepository{pool: pool}
}

// ReserveDispense is the Ootle-path analog of
// faucet.PGRepository.ReserveDispense -- identical advisory-lock
// check-and-reserve strategy (same lock-ordering rule: the
// lexicographically smaller of {address, ip} is locked first,
// regardless of which field each caller happens to call "address" vs
// "ip", to avoid the exact cross-request deadlock
// faucet.PGRepository.ReserveDispense's doc comment describes), just
// against ootle_dispenses. See that method's doc comment for the full
// reasoning; it is not repeated verbatim here to avoid the two copies
// drifting out of sync in comment text while the actual SQL (correctly)
// stays independent per table.
func (r *PGRepository) ReserveDispense(ctx context.Context, address, ip string, amount uint64, since, now time.Time) (Reservation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	first, second := address, ip
	if first > second {
		first, second = second, first
	}
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext($1))", first); err != nil {
		return Reservation{}, err
	}
	if second != first {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext($1))", second); err != nil {
			return Reservation{}, err
		}
	}

	const lookupQuery = `
(select created_at from ootle_dispenses where (success or error is null) and address = $1 and created_at >= $3 order by created_at desc limit 1)
union all
(select created_at from ootle_dispenses where (success or error is null) and ip = $2 and created_at >= $3 order by created_at desc limit 1)
order by created_at desc
limit 1`
	var last time.Time
	scanErr := tx.QueryRow(ctx, lookupQuery, address, ip, since).Scan(&last)
	if scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
		return Reservation{}, scanErr
	}
	if scanErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return Reservation{}, err
		}
		committed = true
		return Reservation{RateLimited: true, LastSuccessAt: last}, nil
	}

	const insertQuery = `
insert into ootle_dispenses (address, ip, amount, success, error, created_at)
values ($1, $2, $3, false, null, $4)
returning id`
	var id int64
	if err := tx.QueryRow(ctx, insertQuery, address, ip, amount, now).Scan(&id); err != nil {
		return Reservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, err
	}
	committed = true
	return Reservation{ID: id}, nil
}

// FinalizeDispense updates the placeholder row ReserveDispense inserted
// (by id) with the wallet call's real outcome -- see
// faucet.PGRepository.FinalizeDispense's doc comment for why error is
// always written as a literal string, never NULL.
func (r *PGRepository) FinalizeDispense(ctx context.Context, id int64, success bool, errMsg string, txID string) error {
	const query = `update ootle_dispenses set success = $2, error = $3, tx_id = $4 where id = $1`
	_, err := r.pool.Exec(ctx, query, id, success, errMsg, txID)
	return err
}

// Ping runs a trivial round-trip query against Postgres.
func (r *PGRepository) Ping(ctx context.Context) error {
	var one int
	return r.pool.QueryRow(ctx, "select 1").Scan(&one)
}
