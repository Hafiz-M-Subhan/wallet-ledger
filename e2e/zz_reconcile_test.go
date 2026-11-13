package e2e

// Kept in its own file so it runs after every other test (go test runs
// files in name order), i.e. after all the concurrent load above.

import (
	"fmt"
	"testing"
)

// TestZZReconcileWholeLedger runs last (alphabetical order) and performs the
// reviewers' check over every user: balances == sum(credits) - sum(debits),
// nothing negative, and balance_after is a correct running total.
func TestZZReconcileWholeLedger(t *testing.T) {
	pool := db(t)
	ctx := t.Context()
	var mismatched, negative int64
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM balances b
		FULL JOIN (
			SELECT user_id, sum(CASE WHEN type = 'credit' THEN amount ELSE -amount END) AS s
			FROM ledger GROUP BY user_id
		) l USING (user_id)
		WHERE b.balance IS DISTINCT FROM l.s`).Scan(&mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM balances WHERE balance < 0`).Scan(&negative); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 || negative != 0 {
		t.Fatalf("reconciliation failed: %d mismatched users, %d negative balances", mismatched, negative)
	}

	// Running totals for this run's users (checking all 10M rows is in seed/verify.sql).
	var badRunning int64
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT balance_after,
			       sum(CASE WHEN type = 'credit' THEN amount ELSE -amount END)
			           OVER (PARTITION BY user_id ORDER BY id) AS running
			FROM ledger WHERE user_id LIKE $1
		) x WHERE balance_after <> running OR running < 0`, fmt.Sprintf("t\\_%%\\_%s\\_%%", runID)).Scan(&badRunning)
	if err != nil {
		t.Fatal(err)
	}
	if badRunning != 0 {
		t.Fatalf("%d ledger rows have a wrong balance_after", badRunning)
	}
}
