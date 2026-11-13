// Package ledger implements credit, debit and balance on PostgreSQL.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUserNotFound         = errors.New("user not found")
	ErrInsufficientFunds    = errors.New("insufficient funds")
	ErrBalanceOverflow      = errors.New("balance would exceed the maximum supported value")
	ErrIdempotencyKeyReused = errors.New("idempotency key was already used for a different request")
)

type Op string

const (
	OpCredit Op = "credit"
	OpDebit  Op = "debit"
)

// creditSQL upserts the balance row and appends the ledger row in ONE
// statement. A single statement is its own transaction, so both writes commit
// or neither does, with one network round trip.
//
// INSERT ... ON CONFLICT DO UPDATE takes the row lock on balances; concurrent
// credits for the same user queue on that lock and each one adds to the
// latest committed balance (no lost updates). The first credit for a new user
// creates the row; two concurrent "first" credits are resolved by the
// primary key + ON CONFLICT.
const creditSQL = `
WITH b AS (
    INSERT INTO balances AS cur (user_id, balance)
    VALUES ($1, $2)
    ON CONFLICT (user_id) DO UPDATE SET balance = cur.balance + EXCLUDED.balance
    RETURNING balance
), l AS (
    INSERT INTO ledger (user_id, type, amount, balance_after)
    SELECT $1, 'credit', $2, balance FROM b
)
SELECT balance FROM b`

// debitSQL is a conditional update: the WHERE balance >= amount check and the
// subtraction happen atomically under the row lock. Under READ COMMITTED, if a
// concurrent transaction changed the row first, Postgres waits for it and
// re-evaluates the WHERE clause against the new committed version, so two
// debits of 60 against 100 cannot both pass.
//
// If no row was updated, the second branch tells "user does not exist" (no
// row at all) apart from "insufficient funds" (row exists) in the same round
// trip. Nothing is written in either case.
const debitSQL = `
WITH b AS (
    UPDATE balances SET balance = balance - $2
    WHERE user_id = $1 AND balance >= $2
    RETURNING balance
), l AS (
    INSERT INTO ledger (user_id, type, amount, balance_after)
    SELECT $1, 'debit', $2, balance FROM b
)
SELECT true AS applied, balance FROM b
UNION ALL
SELECT false, balance FROM balances
WHERE user_id = $1 AND NOT EXISTS (SELECT 1 FROM b)`

const balanceSQL = `SELECT balance FROM balances WHERE user_id = $1`

type Options struct {
	// StatementTimeout bounds the database work of one request once it holds
	// the per-user lock. The database call is detached from the client's
	// context so a client hanging up mid-commit cannot leave the outcome
	// ambiguous on our side.
	StatementTimeout time.Duration
	// MaxAttempts for transient errors (serialization failure, deadlock,
	// connection errors that are known to be safe to retry).
	MaxAttempts int
	// MaxBatch caps how many queued writes for one user share a transaction.
	MaxBatch int
}

type Store struct {
	pool *pgxpool.Pool
	comb *combiner
	opts Options
}

func NewStore(pool *pgxpool.Pool, opts Options) *Store {
	if opts.StatementTimeout <= 0 {
		opts.StatementTimeout = 10 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 128
	}
	s := &Store{pool: pool, opts: opts}
	s.comb = newCombiner(opts.MaxBatch, s.execBatch)
	return s
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Apply performs a credit or debit and returns the balance right after it.
// idemKey may be empty.
//
// The request joins its user's queue (see combiner). If the client goes away
// while it is still queued, it is dropped before anything reaches the
// database. Once it is taken for execution, the database work runs on a
// context detached from the client, so a client hanging up mid-commit cannot
// leave the outcome ambiguous on our side.
func (s *Store) Apply(ctx context.Context, op Op, userID string, amount int64, idemKey string) (int64, error) {
	if op != OpCredit && op != OpDebit {
		return 0, fmt.Errorf("unknown op %q", op)
	}
	p := &pending{op: op, amount: amount, key: idemKey, done: make(chan outcome, 1)}
	return s.comb.submit(ctx, userID, p)
}

// execBatch runs one batch for one user. It is only ever called by that
// user's single drainer goroutine.
func (s *Store) execBatch(userID string, batch []*pending) []outcome {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.StatementTimeout)
	defer cancel()
	outs := make([]outcome, len(batch))

	if len(batch) == 1 {
		outs[0] = s.applyOne(ctx, userID, batch[0])
		return outs
	}

	err := s.retry(ctx, func(ctx context.Context) error { return s.applyBatch(ctx, userID, batch, outs) })
	if err == nil {
		return outs
	}
	// The batch transaction failed. If the server reported the error, the
	// transaction was rolled back and nothing was written, so it is safe to
	// apply the requests one by one (one bad request, e.g. an overflow, must
	// not fail its neighbours). Any other error (connection lost around
	// COMMIT) leaves the outcome unknown, so it is reported, never re-applied.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		for i, p := range batch {
			outs[i] = s.applyOne(ctx, userID, p)
		}
		return outs
	}
	for i := range outs {
		outs[i] = outcome{err: err}
	}
	return outs
}

func (s *Store) applyOne(ctx context.Context, userID string, p *pending) outcome {
	var bal int64
	err := s.retry(ctx, func(ctx context.Context) error {
		var err error
		if p.key == "" {
			bal, err = apply(ctx, s.pool, p.op, userID, p.amount)
		} else {
			bal, err = s.applyIdempotent(ctx, p.op, userID, p.amount, p.key)
		}
		return err
	})
	return outcome{balance: bal, err: err}
}

// applyBatch executes the batch's statements in order inside one
// transaction, pipelined in a single network round trip, then commits once.
// Each statement sees the effects of the ones before it, so every request
// gets the exact balance after its own operation, and a debit that would
// overdraw simply updates nothing (and is reported as insufficient funds)
// without aborting the others.
func (s *Store) applyBatch(ctx context.Context, userID string, batch []*pending, outs []outcome) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	b := &pgx.Batch{}
	for _, p := range batch {
		if p.op == OpCredit {
			b.Queue(creditSQL, userID, p.amount)
		} else {
			b.Queue(debitSQL, userID, p.amount)
		}
	}
	br := tx.SendBatch(ctx, b)
	for i, p := range batch {
		bal, err := scanOp(p.op, br.QueryRow())
		if err != nil && !isBusinessErr(err) {
			br.Close()
			return err
		}
		outs[i] = outcome{balance: bal, err: err}
	}
	if err := br.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isBusinessErr(err error) bool {
	return errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrInsufficientFunds)
}

// Balance is a primary-key lookup on balances: O(log users), independent of
// how many ledger rows the user or the table has.
func (s *Store) Balance(ctx context.Context, userID string) (int64, error) {
	var bal int64
	err := s.retry(ctx, func(ctx context.Context) error {
		err := s.pool.QueryRow(ctx, balanceSQL, userID).Scan(&bal)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	})
	return bal, err
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func apply(ctx context.Context, q querier, op Op, userID string, amount int64) (int64, error) {
	sql := creditSQL
	if op == OpDebit {
		sql = debitSQL
	}
	return scanOp(op, q.QueryRow(ctx, sql, userID, amount))
}

func scanOp(op Op, row pgx.Row) (int64, error) {
	var bal int64
	if op == OpCredit {
		if err := row.Scan(&bal); err != nil {
			return 0, mapErr(err)
		}
		return bal, nil
	}
	var applied bool
	err := row.Scan(&applied, &bal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrUserNotFound
	case err != nil:
		return 0, mapErr(err)
	case !applied:
		return bal, ErrInsufficientFunds
	}
	return bal, nil
}

func (s *Store) applyIdempotent(ctx context.Context, op Op, userID string, amount int64, key string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	// Claim the key. If another transaction holds an uncommitted claim on the
	// same key, Postgres blocks here until it finishes; then we either own
	// the key (it rolled back) or see its committed outcome.
	tag, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, op, user_id, amount)
		 VALUES ($1, $2::text::entry_type, $3, $4)
		 ON CONFLICT (key) DO NOTHING`,
		key, string(op), userID, amount)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return replay(ctx, tx, op, userID, amount, key)
	}

	bal, opErr := apply(ctx, tx, op, userID, amount)
	var outcome string
	var stored any // NULL unless outcome is ok
	switch {
	case opErr == nil:
		outcome, stored = "ok", bal
	case errors.Is(opErr, ErrUserNotFound):
		outcome = "user_not_found"
	case errors.Is(opErr, ErrInsufficientFunds):
		outcome = "insufficient_funds"
	default:
		return 0, opErr // rolls back, key is released
	}
	if _, err := tx.Exec(ctx,
		`UPDATE idempotency_keys SET outcome = $2, balance = $3 WHERE key = $1`,
		key, outcome, stored); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return bal, opErr
}

func replay(ctx context.Context, tx pgx.Tx, op Op, userID string, amount int64, key string) (int64, error) {
	var (
		sOp, sUser, outcome string
		sAmount             int64
		sBal                *int64
	)
	err := tx.QueryRow(ctx,
		`SELECT op::text, user_id, amount, outcome, balance FROM idempotency_keys WHERE key = $1`,
		key).Scan(&sOp, &sUser, &sAmount, &outcome, &sBal)
	if err != nil {
		return 0, err
	}
	if sOp != string(op) || sUser != userID || sAmount != amount {
		return 0, ErrIdempotencyKeyReused
	}
	switch outcome {
	case "ok":
		if sBal == nil {
			return 0, fmt.Errorf("idempotency key %q stored without balance", key)
		}
		return *sBal, nil
	case "user_not_found":
		return 0, ErrUserNotFound
	case "insufficient_funds":
		return 0, ErrInsufficientFunds
	}
	return 0, fmt.Errorf("idempotency key %q has unknown outcome %q", key, outcome)
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22003" { // numeric_value_out_of_range
		return ErrBalanceOverflow
	}
	return err
}

// retry re-runs fn on transient errors with jittered exponential backoff.
// Business outcomes (not found, insufficient funds) are never retried.
func (s *Store) retry(ctx context.Context, fn func(context.Context) error) error {
	backoff := 2 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || attempt >= s.opts.MaxAttempts || !isRetryable(err) {
			return err
		}
		t := time.NewTimer(backoff/2 + rand.N(backoff))
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff *= 2
	}
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"55P03": // lock_not_available
			return true
		}
		return false
	}
	// Connection-level failures where pgx guarantees nothing reached the
	// server (e.g. a broken idle connection), so a retry cannot double-apply.
	return pgconn.SafeToRetry(err)
}
