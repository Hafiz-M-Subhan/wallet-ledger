-- Seed the ledger with synthetic history.
--
-- psql variables (set by seed.sh):
--   rows    target number of ledger rows   (default 10,000,000)
--   users   number of users                (default 100,000)
--   whales  number of very heavy users     (default 10, each 100k-200k rows)
--
-- Users are named seed_0000001 .. seed_<users>. The first <whales> users get
-- 100,000-200,000 rows each; everyone else gets an exponentially distributed
-- row count (most users have a few dozen rows, a long tail has hundreds), and
-- the totals are topped up so the ledger holds at least <rows> rows.
--
-- Non-negative at every point, by construction: each user's history is a
-- sequence of (credit c, debit d) pairs with 1 <= d <= c, so the running
-- balance never drops below the value it had before the pair. balance_after is
-- the running sum, and the ledger's CHECK (balance_after >= 0) would reject the
-- whole load if that were ever violated.
--
-- Everything runs inside Postgres (generate_series), so no data crosses the
-- network; the secondary index is dropped during the bulk insert and rebuilt
-- afterwards, which is much faster than maintaining it row by row.

\set ON_ERROR_STOP on
\timing on

SET work_mem = '512MB';
SET maintenance_work_mem = '1GB';
SET max_parallel_maintenance_workers = 4;
-- Bulk load only: a crash mid-seed loses the seed transaction, which is fine.
SET synchronous_commit = off;

SELECT EXISTS (SELECT 1 FROM balances WHERE user_id = 'seed_0000001') AS already_seeded \gset
\if :already_seeded
    \echo 'Seed users already exist; nothing to do. (docker compose down -v to start over.)'
    \quit
\endif

\echo '== planning row counts per user'
SELECT setseed(0.42);

CREATE TEMP TABLE seed_users (
    user_id text PRIMARY KEY,
    n       int
);

INSERT INTO seed_users (user_id, n)
SELECT 'seed_' || lpad(i::text, 7, '0'),
       CASE WHEN i <= :whales THEN 100000 + (i * 7919) % 100001 END
FROM generate_series(1, :users) AS i;

SELECT ((:rows - COALESCE(sum(n), 0))::numeric
        / GREATEST(count(*) FILTER (WHERE n IS NULL), 1)) AS mean_rows
FROM seed_users \gset

-- Exponential(mean) counts, at least 1 row each.
UPDATE seed_users
SET n = 1 + floor(-ln(1 - random()) * GREATEST(:mean_rows - 1, 0))::int
WHERE n IS NULL;

-- Top up the regular users so the total is >= :rows.
SELECT GREATEST(:rows - sum(n), 0) AS deficit,
       count(*) FILTER (WHERE user_id > 'seed_' || lpad(:whales::text, 7, '0')) AS regulars
FROM seed_users \gset
UPDATE seed_users
SET n = n + ceil(:deficit::numeric / GREATEST(:regulars, 1))::int
WHERE :deficit > 0 AND user_id > 'seed_' || lpad(:whales::text, 7, '0');

SELECT count(*) AS users, sum(n) AS planned_rows, max(n) AS max_rows_per_user,
       percentile_disc(0.5) WITHIN GROUP (ORDER BY n) AS median_rows_per_user,
       count(*) FILTER (WHERE n >= 100000) AS users_with_100k_plus
FROM seed_users;

BEGIN;

DROP INDEX IF EXISTS ledger_user_id_id_idx;

\echo '== generating ledger rows'
INSERT INTO ledger (user_id, type, amount, balance_after, created_at)
SELECT g.user_id,
       g.type,
       g.amount,
       sum(CASE WHEN g.type = 'credit' THEN g.amount ELSE -g.amount END)
           OVER (PARTITION BY g.user_id ORDER BY g.seq),
       -- synthetic history: one entry per minute, ending now
       now() - make_interval(mins => g.n - g.seq)
FROM (
    SELECT s.user_id, s.n, e.seq, e.type, e.amount
    FROM seed_users s
    CROSS JOIN LATERAL generate_series(1, (s.n + 1) / 2) AS k(k)
    -- OFFSET 0 is an optimisation fence: it stops the planner from inlining
    -- the subquery, so each pair's credit amount c is drawn once and the same
    -- value is used both for the credit row and as the debit's upper bound.
    CROSS JOIN LATERAL (
        SELECT x.c, 1 + floor(random() * x.c)::bigint AS d
        FROM (SELECT (100 + floor(random() * 99901))::bigint AS c, k.k AS kk OFFSET 0) x
        OFFSET 0
    ) AS amt
    CROSS JOIN LATERAL (VALUES
        (2 * k.k - 1, 'credit'::entry_type, amt.c),
        (2 * k.k,     'debit'::entry_type,  amt.d)
    ) AS e(seq, type, amount)
    WHERE e.seq <= s.n
) AS g
ORDER BY g.user_id, g.seq;

\echo '== building balances'
INSERT INTO balances (user_id, balance)
SELECT user_id, sum(CASE WHEN type = 'credit' THEN amount ELSE -amount END)
FROM ledger
WHERE user_id LIKE 'seed\_%'
GROUP BY user_id;

\echo '== rebuilding index'
CREATE INDEX ledger_user_id_id_idx ON ledger (user_id, id);

COMMIT;

\echo '== vacuum / analyze'
VACUUM (ANALYZE) ledger;
VACUUM (ANALYZE) balances;

\echo '== summary'
SELECT count(*) AS ledger_rows, count(DISTINCT user_id) AS ledger_users FROM ledger;
SELECT count(*) AS balance_rows, min(balance) AS min_balance FROM balances;
