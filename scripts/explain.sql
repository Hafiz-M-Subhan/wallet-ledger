-- Query plans for the three API statements, on a whale user (100k+ rows).
--   docker compose exec db psql -U ledger -d ledger -f /scripts/explain.sql
-- Writes run inside a transaction that is rolled back.

\echo '--- get_balance'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT balance FROM balances WHERE user_id = 'seed_0000001';

BEGIN;

\echo '--- credit'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
WITH b AS (
    INSERT INTO balances AS cur (user_id, balance) VALUES ('seed_0000001', 10000)
    ON CONFLICT (user_id) DO UPDATE SET balance = cur.balance + EXCLUDED.balance
    RETURNING balance
), l AS (
    INSERT INTO ledger (user_id, type, amount, balance_after)
    SELECT 'seed_0000001', 'credit', 10000, balance FROM b
)
SELECT balance FROM b;

\echo '--- debit'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
WITH b AS (
    UPDATE balances SET balance = balance - 4000
    WHERE user_id = 'seed_0000001' AND balance >= 4000
    RETURNING balance
), l AS (
    INSERT INTO ledger (user_id, type, amount, balance_after)
    SELECT 'seed_0000001', 'debit', 4000, balance FROM b
)
SELECT true AS applied, balance FROM b
UNION ALL
SELECT false, balance FROM balances
WHERE user_id = 'seed_0000001' AND NOT EXISTS (SELECT 1 FROM b);

ROLLBACK;
