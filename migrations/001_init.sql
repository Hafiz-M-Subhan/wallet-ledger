-- Wallet ledger schema.
--
-- Every statement is idempotent, so this file can be applied on every API start
-- (and by the seed script) without a migration-tracking table.

-- 'credit' / 'debit'. An enum is 4 bytes, rejects anything else, and still
-- compares against plain string literals in reviewers' SQL (type = 'credit').
DO $$
BEGIN
    CREATE TYPE entry_type AS ENUM ('credit', 'debit');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END
$$;

-- Source of truth: one immutable row per credit or debit.
CREATE TABLE IF NOT EXISTS ledger (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id       text        NOT NULL,
    type          entry_type  NOT NULL,
    amount        bigint      NOT NULL CHECK (amount > 0),
    -- The user's balance immediately after this entry. Written under the
    -- balance row lock, so it forms a verifiable running total per user and
    -- the CHECK makes "never below zero at any point" a database invariant.
    balance_after bigint      NOT NULL CHECK (balance_after >= 0),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Per-user history / audit lookups ("last N entries of user X", per-user
-- reconciliation). Not used by the three API endpoints themselves.
CREATE INDEX IF NOT EXISTS ledger_user_id_id_idx ON ledger (user_id, id);

-- Ledger rows are never updated or deleted.
CREATE OR REPLACE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger rows are immutable (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

CREATE OR REPLACE TRIGGER ledger_immutable
    BEFORE UPDATE OR DELETE ON ledger
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();

-- Materialised current balance: one row per user, updated in the same
-- statement (same transaction) that appends the ledger row. This makes
-- get_balance an O(1) primary-key lookup, and the row lock on it is what
-- serialises concurrent writes for one user.
--
-- fillfactor 80 leaves free space on each page so balance updates can be HOT
-- updates (no index write, since the only index is on user_id, which never
-- changes).
CREATE TABLE IF NOT EXISTS balances (
    user_id text   PRIMARY KEY,
    balance bigint NOT NULL CHECK (balance >= 0)
) WITH (fillfactor = 80);

-- The hottest table in the system gets vacuumed far more eagerly than the
-- default so dead HOT tuples are pruned quickly.
ALTER TABLE balances SET (
    autovacuum_vacuum_scale_factor = 0.01,
    autovacuum_analyze_scale_factor = 0.02
);

-- Optional Idempotency-Key support for /credit and /debit.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    key        text        PRIMARY KEY,
    op         entry_type  NOT NULL,
    user_id    text        NOT NULL,
    amount     bigint      NOT NULL,
    outcome    text,          -- 'ok' | 'user_not_found' | 'insufficient_funds'
    balance    bigint,        -- balance returned when outcome = 'ok'
    created_at timestamptz NOT NULL DEFAULT now()
);
