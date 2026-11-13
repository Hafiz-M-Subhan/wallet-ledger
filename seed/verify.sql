-- Consistency checks. Run via:
--   docker compose exec db psql -U ledger -d ledger -f /seed/verify.sql
-- Every query below should return 0.

\timing on

\echo '1. users whose balances row disagrees with sum(credits) - sum(debits)'
SELECT count(*) AS mismatched_users
FROM balances b
FULL JOIN (
    SELECT user_id, sum(CASE WHEN type = 'credit' THEN amount ELSE -amount END) AS ledger_balance
    FROM ledger GROUP BY user_id
) l USING (user_id)
WHERE b.balance IS DISTINCT FROM l.ledger_balance;

\echo '2. negative balances'
SELECT count(*) AS negative_balances FROM balances WHERE balance < 0;

\echo '3. ledger rows whose balance_after is not the running total (balance below 0 at any point is caught here too)'
SELECT count(*) AS bad_running_totals
FROM (
    SELECT balance_after,
           sum(CASE WHEN type = 'credit' THEN amount ELSE -amount END)
               OVER (PARTITION BY user_id ORDER BY id) AS running
    FROM ledger
) t
WHERE balance_after <> running OR running < 0;

\echo '4. last balance_after per user vs balances'
SELECT count(*) AS last_entry_mismatch
FROM balances b
JOIN LATERAL (
    SELECT balance_after FROM ledger l
    WHERE l.user_id = b.user_id ORDER BY id DESC LIMIT 1
) last ON true
WHERE last.balance_after <> b.balance;
