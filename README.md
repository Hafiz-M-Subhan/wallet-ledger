# Wallet Ledger API

An HTTP/JSON wallet ledger written in Go on PostgreSQL 17. It supports `credit`, `debit` and `get_balance` for many users, stays consistent under heavy concurrency, and keeps per-request latency flat at 10M+ ledger rows.

## What is this for?

This is my submission for the **Niobi Wallet Ledger API recruitment test**. The test asks for a small "bank" service that:

- keeps a **money ledger** for many users: every credit and debit is stored forever as one row;
- answers three questions over HTTP: **add money** (`POST /credit`), **take money out** (`POST /debit`, never below zero), and **what is my balance** (`GET /balance/{user_id}`);
- stays **100% correct** when many clients hit it at the same moment (no negative balances, no lost updates, balances always equal the ledger);
- stays **fast** with **10 million+ ledger rows** in the database.

| | |
|---|---|
| **Language** | Go 1.24 (standard library HTTP server, `pgx` driver) |
| **Database** | PostgreSQL 17 |
| **Run** | `docker compose up` (API on `http://localhost:8080`) |
| **Data** | seed script: 10,070,988 ledger rows, 100,000 users, 10 users with 100k+ rows, in about 2 minutes |
| **Speed (10M rows, laptop)** | about 6,400 req/s; p99 27 ms for balance, 41 ms for credit and debit; 0 failures |

**Contents:** [Requirements checklist](#requirements-checklist) · [How to use it, step by step](#how-to-use-it-step-by-step) · [Quick start](#quick-start) · [API](#api) · [Design](#design) · [Concurrency and consistency](#concurrency-and-consistency) · [Query plans](#query-plans) · [Tests](#tests) · [Load test results](#load-test-results) · [Seed](#seed) · [Limits, scaling and what I'd do next](#limits-scaling-and-what-id-do-next) · [Project layout](#project-layout)

---

## Requirements checklist

Every requirement in the brief, and where it is met.

| Requirement | Status | Where |
|---|:---:|---|
| `docker compose up` starts API + database, no other steps | ✅ | [docker-compose.yml](docker-compose.yml); the schema is applied automatically at API start-up |
| Serves on port 8080 | ✅ | [docker-compose.yml](docker-compose.yml) |
| Client-server SQL database (not SQLite) | ✅ | PostgreSQL 17 |
| `API_TOKEN` env var, `recruitment-test-token` in compose, overridable | ✅ | [docker-compose.yml](docker-compose.yml): `API_TOKEN: ${API_TOKEN:-recruitment-test-token}` |
| Bearer token on every endpoint, 401 before any DB access | ✅ | [internal/httpapi/server.go](internal/httpapi/server.go), tested in [server_test.go](internal/httpapi/server_test.go) |
| Token not hard-coded, logged or returned; constant-time compare | ✅ | [internal/httpapi/auth.go](internal/httpapi/auth.go) (SHA-256 + `subtle.ConstantTimeCompare`) |
| `POST /credit`, `POST /debit`, `GET /balance/{user_id}` with the exact JSON shapes | ✅ | [API](#api) |
| Input rules (`user_id` 1–64 of `A-Za-z0-9_-`; `amount` integer 1..1,000,000,000; unknown fields ignored) | ✅ | [internal/httpapi/validate.go](internal/httpapi/validate.go) |
| Error shape and codes 400 / 401 / 404 / 409 | ✅ | [API](#api) |
| Immutable ledger rows (id, user_id, type, amount > 0, server timestamp) | ✅ | [migrations/001_init.sql](migrations/001_init.sql), with a trigger that blocks UPDATE and DELETE |
| Balance = Σcredits − Σdebits, checked with SQL | ✅ | [seed/verify.sql](seed/verify.sql), `TestZZReconcileWholeLedger` |
| `get_balance` fast at 10M+ rows (no table or history scans) | ✅ | primary-key lookup; see [Query plans](#query-plans) |
| Seed: 10M+ rows, 100k+ users, some users with 100k+ rows, never negative, time stated | ✅ | [seed/seed.sql](seed/seed.sql); [Seed](#seed) (123 s) |
| Two concurrent debits of 60 vs 100 → one 200 + one 409 | ✅ | `TestConcurrentDebitsNeverOverdraw` (100 rounds) |
| 1,000 concurrent credits of 100 → +100,000 exactly | ✅ | `TestNoLostUpdates` |
| Atomic writes, crash-safe | ✅ | one SQL statement per write = one transaction |
| Read-your-writes | ✅ | `TestReadYourWrites` |
| Hot user does not slow other users | ✅ | per-user queue; `TestHotUserDoesNotBlockOthers`, hot-phase load test |
| Deadlocks/conflicts handled, never a 500 or a wrong balance | ✅ | retries with backoff in [internal/ledger/store.go](internal/ledger/store.go) |
| Load test, p50/p99 per endpoint at 10M rows, ~100 clients, mixed + hot user | ✅ | [cmd/loadtest](cmd/loadtest/main.go); [Load test results](#load-test-results) |
| 1M vs 10M comparison | ✅ | [Load test results](#load-test-results) |
| `EXPLAIN` plans for credit, debit and get_balance | ✅ | [Query plans](#query-plans) |
| README: how to run, design, plans, limits, next steps | ✅ | this file |
| Extras: `Idempotency-Key`, health endpoint, graceful shutdown | ✅ | [API](#api) |

## How to use it, step by step

You only need **Docker Desktop** (or Docker Engine with the Compose plugin). Go and Postgres don't need to be installed on your machine.

**1. Get the code and start the service**

```bash
git clone <this repository URL>
cd wallet-ledger
docker compose up -d          # builds the API image, starts Postgres and the API
docker compose ps             # both services should show "healthy"
```

The first build takes a minute or two. The database schema is created automatically.

**2. Load the 10 million row test data** (optional, about 2 minutes)

```bash
docker compose --profile seed run --rm seed
```

Smaller data set: `SEED_ROWS=1000000 SEED_WHALES=2 docker compose --profile seed run --rm seed`.

**3. Try the API**

```bash
TOKEN=recruitment-test-token

# add 100.00 (amounts are integers in cents)
curl -s -X POST localhost:8080/credit -H "Authorization: Bearer $TOKEN" -d '{"user_id":"alice","amount":10000}'
# -> {"user_id":"alice","balance":10000}

# take out 40.00
curl -s -X POST localhost:8080/debit -H "Authorization: Bearer $TOKEN" -d '{"user_id":"alice","amount":4000}'
# -> {"user_id":"alice","balance":6000}

# read the balance
curl -s localhost:8080/balance/alice -H "Authorization: Bearer $TOKEN"
# -> {"user_id":"alice","balance":6000}

# try to take out more than she has
curl -s -X POST localhost:8080/debit -H "Authorization: Bearer $TOKEN" -d '{"user_id":"alice","amount":999999}'
# -> 409 {"error":"insufficient_funds","message":"balance is lower than the debit amount"}

# no token
curl -s localhost:8080/balance/alice
# -> 401 {"error":"unauthorized","message":"missing or invalid bearer token"}
```

On Windows PowerShell, use `curl.exe` and put the JSON body in a file (`-d "@body.json"`), or use Git Bash.

**4. Run the tests** (end-to-end, concurrency and whole-ledger reconciliation)

```bash
docker compose --profile tools run --rm tests
```

**5. Run the load test** (100 concurrent clients: 60 s mixed traffic, then 30 s with one hot user)

```bash
docker compose --profile tools run --rm loadtest
```

**6. Check that every balance matches the ledger**

```bash
docker compose exec db psql -U ledger -d ledger -f /seed/verify.sql      # all checks must return 0
docker compose exec db psql -U ledger -d ledger -f /scripts/explain.sql  # query plans
```

In Git Bash on Windows, run `export MSYS_NO_PATHCONV=1` first. Otherwise Git Bash rewrites `/seed/...` into a Windows path.

**7. Use a different token, or stop and reset**

```bash
API_TOKEN=my-secret docker compose up -d   # every client must then send this token
docker compose down                        # stop, keep the data
docker compose down -v                     # stop and delete all data
```

## Quick start

```bash
docker compose up -d                              # API on :8080 + Postgres (schema auto-applied)
docker compose --profile seed run --rm seed       # 10M+ ledger rows, 100k users (~2 min)

TOKEN=recruitment-test-token
curl -s -X POST localhost:8080/credit -H "Authorization: Bearer $TOKEN" -d '{"user_id":"u123","amount":10000}'
curl -s localhost:8080/balance/u123 -H "Authorization: Bearer $TOKEN"

docker compose --profile tools run --rm tests     # end-to-end + concurrency tests (+ SQL reconciliation)
docker compose --profile tools run --rm loadtest  # p50/p99 per endpoint, mixed + hot-user phases
docker compose exec db psql -U ledger -d ledger -f /seed/verify.sql   # full-ledger consistency check
```

| Setting | Value |
|---|---|
| API | `http://localhost:8080` |
| Token | `API_TOKEN` env var, default `recruitment-test-token`; override with `API_TOKEN=... docker compose up` |
| Database | `postgres://ledger:ledger@localhost:5432/ledger` (host port override: `DB_PORT=...`) |
| Seed size | `SEED_ROWS` (default 10,000,000), `SEED_USERS` (100,000), `SEED_WHALES` (10) |
| Reset | `docker compose down -v` |

Unit tests run during the `tools` image build (`go vet` + `go test ./internal/...`). Without Docker they also run with `go test ./internal/...`.

## API

All endpoints require `Authorization: Bearer <token>`. The token is checked first for every path, including unknown ones, before any body parsing or database access.

| Endpoint | Body | 200 response |
|---|---|---|
| `POST /credit` | `{"user_id":"u123","amount":10000}` | `{"user_id":"u123","balance":25000}` |
| `POST /debit` | `{"user_id":"u123","amount":4000}` | `{"user_id":"u123","balance":21000}` |
| `GET /balance/{user_id}` | – | `{"user_id":"u123","balance":21000}` |
| `GET /health` (extra) | – | `{"status":"ok"}` (pings the DB) |

Errors are always `{"error": "<code>", "message": "<text>"}`:

| Status | Code | When |
|---|---|---|
| 400 | `invalid_request` | malformed JSON, missing field, `user_id` not `^[A-Za-z0-9_-]{1,64}$`, `amount` not a JSON integer in 1..1,000,000,000 (`100.0`, `1e2`, `"100"`, `null`, `0` and negatives are all rejected) |
| 401 | `unauthorized` | missing header, wrong scheme, wrong token |
| 404 | `user_not_found` | balance/debit for a user never credited |
| 409 | `insufficient_funds` | debit larger than balance; nothing written |
| 404 / 405 | `not_found` / `method_not_allowed` | unknown path / wrong method |
| 422 | `idempotency_key_reused` | same `Idempotency-Key` with a different request (extra) |

**Extras:** an optional `Idempotency-Key` header on `/credit` and `/debit`, a `/health` endpoint used by the Docker healthcheck, and graceful shutdown on SIGTERM, which drains in-flight requests before closing the pool.

**Auth details:** the token comes only from the environment and is never logged or echoed. Both the presented and the expected token are SHA-256 hashed and compared with `crypto/subtle.ConstantTimeCompare`, so neither the content nor the *length* of the token leaks through timing. The scheme `Bearer` is matched case-insensitively (RFC 9110); the token is matched exactly.

## Design

### Schema ([migrations/001_init.sql](migrations/001_init.sql))

```sql
ledger (                                   -- source of truth, append-only
  id            bigint identity PRIMARY KEY,
  user_id       text        NOT NULL,
  type          entry_type  NOT NULL,      -- enum('credit','debit')
  amount        bigint      NOT NULL CHECK (amount > 0),
  balance_after bigint      NOT NULL CHECK (balance_after >= 0),
  created_at    timestamptz NOT NULL DEFAULT now()
)
INDEX ledger_user_id_id_idx (user_id, id)
TRIGGER ledger_immutable BEFORE UPDATE OR DELETE -> raise

balances (                                 -- one row per user, derived state
  user_id text PRIMARY KEY,
  balance bigint NOT NULL CHECK (balance >= 0)
) WITH (fillfactor = 80)

idempotency_keys (key PK, op, user_id, amount, outcome, balance, created_at)
```

The API applies the schema at startup. The file is idempotent and guarded by an advisory lock, so several instances can start together.

### How the balance is computed

The balance is **materialised** in `balances` and updated **in the same statement** that appends the ledger row. `get_balance` is a primary-key lookup on a 100k-row table, so it costs the same at 1M or 1B ledger rows.

The two can never disagree, because each write is one SQL statement, and a statement is one transaction:

```sql
-- credit
WITH b AS (
    INSERT INTO balances AS cur (user_id, balance) VALUES ($1, $2)
    ON CONFLICT (user_id) DO UPDATE SET balance = cur.balance + EXCLUDED.balance
    RETURNING balance
), l AS (
    INSERT INTO ledger (user_id, type, amount, balance_after)
    SELECT $1, 'credit', $2, balance FROM b
)
SELECT balance FROM b;

-- debit
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
SELECT false, balance FROM balances WHERE user_id = $1 AND NOT EXISTS (SELECT 1 FROM b);
```

- **One round trip and one commit per request.** There's no `BEGIN … SELECT … FOR UPDATE … UPDATE … INSERT … COMMIT` chatter: the statement is autocommitted.
- A **debit that fails** updates nothing, so the ledger insert (`SELECT … FROM b`) inserts nothing. The `UNION ALL` branch tells 404 (no row) apart from 409 (row exists) in the same round trip.
- **`balance_after`** records the running balance on every ledger row. It is written under the row lock, so it is exact, and its `CHECK (>= 0)` turns "balance never below 0 at any point" into a database invariant. Reviewers (and [seed/verify.sql](seed/verify.sql)) can check every row's running total with one window query.

### Indexes and why

| Index | Used by | Why |
|---|---|---|
| `balances_pkey (user_id)` | every request | O(log n) lookup / row lock for the user |
| `ledger_pkey (id)` | identity | required PK; appends always go to the right-most leaf |
| `ledger_user_id_id_idx (user_id, id)` | audit, per-user history, `verify.sql` check 4 | "last N entries of a user", per-user reconciliation without a full scan |

No index is needed on the hot path's ledger insert other than these two. I deliberately did **not** index `created_at` or `type`: every extra ledger index is another B-tree write per request and buys nothing for the three endpoints.

### Other details that matter for latency

- **Database created with `--locale=C`.** User ids are ASCII, and byte-order comparison makes text B-tree comparisons cheaper than ICU or libc collation.
- **`balances` has `fillfactor=80` and aggressive autovacuum.** `balance` is not indexed, so updates are HOT (heap-only tuple) updates with no index write, and dead versions are pruned in-page.
- **pgx prepared-statement cache.** Statements are parsed and planned once per connection.
- **Responses are written without reflection.** The `user_id` is already validated to `[A-Za-z0-9_-]`, so it never needs escaping.
- **Hot users are group-committed** (see *Isolation across users* below). This is the biggest single win: it raised one wallet's write throughput from about 140/s to about 1,000+/s.
- **Durability is not traded for speed.** `synchronous_commit=on` and `fsync=on` stay at their defaults, so a 200 means the ledger row is durable.

### Alternatives considered and rejected

| Option | Why not |
|---|---|
| **Compute the balance with `SUM()` over the ledger** | O(rows per user): the whales have 100k+ rows, and the brief rules it out explicitly. |
| **Ledger only, with the last row's `balance_after` as the balance** (`ORDER BY id DESC LIMIT 1` via the `(user_id,id)` index) | Reads are fine, but concurrent writers have no row to lock. Serialising them needs advisory locks or SERIALIZABLE with retries, and creating a user is racy. A `balances` row gives a natural lock and an O(1) read. |
| **Periodic snapshots + sum of the tail** | Bounded but not O(1), plus snapshot jobs and harder reasoning. Unnecessary when the balance row is updated atomically anyway. |
| **`SELECT … FOR UPDATE` then `UPDATE` + `INSERT` in an explicit transaction** | Correct, but 4–5 round trips while holding the row lock. The single-statement version holds the lock for less time and is ~3× fewer network trips. |
| **SERIALIZABLE isolation** | Would also be correct, but conflicts become 40001 errors that must be retried. The hot user would thrash. Row locks + conditional UPDATE give the same guarantees with no aborts. |
| **Plain per-user mutex in the API** (my first version) | Fixes pool starvation, but every write still pays its own fsync in sequence: the hot user maxed out at 140 writes/s with p99 2.4 s. The queue with batching keeps the same fairness and amortises the fsync. |
| **Optimistic concurrency (version column + retry)** | Under a hot user, most attempts lose and retry, so p99 explodes. Pessimistic row locking queues fairly instead. |
| **Balance cache in Redis / in memory** | A second source of truth: read-your-writes and crash consistency become distributed-systems problems. Postgres serves a PK lookup in well under a millisecond already. |
| **Event sourcing with async projections** | Fails read-your-writes unless reads wait for the projection. |
| **`text` type column / `smallint`** | The enum is 4 bytes, rejects bad values, and still reads as `type = 'credit'` in SQL. |
| **UUID ids for ledger** | `bigint identity` is 8 bytes and always appends to the right edge of the PK B-tree. Random UUIDs would scatter writes across a 10M-row index. |
| **Partitioning the ledger now** | Useful at billions of rows for retention and archiving, but at 10M it only adds planning overhead. It's listed under next steps. |

## Concurrency and consistency

**Isolation level: READ COMMITTED (the Postgres default), and why it is enough.** Each write is a single statement that takes a row lock on `balances(user_id)`:

1. **Non-negative balance.** `UPDATE … WHERE balance >= $amount` checks and subtracts under the row lock. If another transaction changed the row first, Postgres waits for it, then **re-evaluates the WHERE clause against the newly committed version** (EvalPlanQual). Two debits of 60 against 100: the second one re-checks `40 >= 60`, updates nothing, and gets 409. The `CHECK (balance >= 0)` on both tables is a second line of defence.
2. **No lost updates.** `balance = balance + $amount` is computed from the locked, latest row version, never from a value read earlier by the application. A user's first two concurrent credits are resolved by the PK and `ON CONFLICT DO UPDATE`.
3. **Atomicity.** The balance update and ledger insert are one statement, so one transaction. A crash before commit loses both, and after commit the WAL replays both. There is no window where one exists without the other.
4. **Read-your-writes.** There is a single primary and no cache. The response is sent after COMMIT returns, and any later read sees committed data.
5. **Isolation across users.** Locks are per row, so users never contend in the database. In the application, writes go through a **per-user queue** ([internal/ledger/combiner.go](internal/ledger/combiner.go)) with at most one executor per user:
   - **Fair connection pool.** A hot user occupies at most one pool connection per API instance, and its backlog waits as goroutines. Without this, 100 requests for one user would take 100 connections and all block on the same row lock, starving every other user.
   - **Group commit.** All of a user's writes are serialised by the row lock anyway, so each one would pay a full WAL fsync back to back. Instead, when several requests for one user are waiting, the executor runs them **in one transaction, in arrival order, pipelined in one round trip**, so N writes cost one fsync. Each request still gets its own ledger row and the exact balance after its own operation. A debit that would overdraw updates nothing inside the batch and gets its 409, while the others continue. If the batch transaction fails with a server error (meaning it was rolled back), each request is re-applied individually, so one bad request can't fail its neighbours. A request whose client disconnects while it is still queued is dropped before reaching the database. Requests with an `Idempotency-Key` always run alone.
   - **No added latency without contention.** An uncontended user's batch is a single request, and it runs as the one autocommitted statement shown above.
   - **Correctness doesn't depend on the queue.** The row lock and single-transaction writes make results correct even with several API instances, each with its own queue.
6. **No silent failures.** Every transaction locks exactly one `balances` row, so deadlocks are impossible by construction. Even so, `40001`, `40P01`, `55P03` and connection errors that pgx marks as safe to retry are retried with jittered exponential backoff (5 attempts). Once a request is taken for execution, its database work runs on a context detached from the client (with its own timeout), so a client hanging up mid-commit can't cancel a commit halfway and leave our side unsure of the outcome. A batch that fails with a connection error around COMMIT is reported as an error and never blindly re-applied.

**Idempotency-Key (extra):** the key is claimed with `INSERT … ON CONFLICT DO NOTHING` *in the same transaction* as the ledger write, and the outcome (`ok`/`user_not_found`/`insufficient_funds` + balance) is stored with it. A retry after a timeout or a crash replays the stored outcome. Two concurrent requests with the same key block on the key row, and the second one replays the first one's result. If the same key is reused with a different body, the API returns 422.

**Ledger immutability:** a `BEFORE UPDATE OR DELETE` trigger rejects any change to ledger rows, even from a SQL console. (`TRUNCATE` isn't blocked so the seed and test environments can be reset.)

## Query plans

Captured with `EXPLAIN (ANALYZE, BUFFERS)` on the seeded 10M-row database (inside a rolled-back transaction for the writes). The plans are generated by [scripts/explain.sql](scripts/explain.sql).

**get_balance** (whale user `seed_0000001`, 100k+ ledger rows)

```
                                      QUERY PLAN                                      
--------------------------------------------------------------------------------------
 Index Scan using balances_pkey on balances (actual time=0.066..0.068 rows=1 loops=1)
   Index Cond: (user_id = 'seed_0000001'::text)
   Buffers: shared hit=4
 Planning:
   Buffers: shared hit=67
 Planning Time: 1.599 ms
 Execution Time: 0.238 ms
```

**credit** (whale user `seed_0000001`, 100k+ ledger rows)

```
                                QUERY PLAN                                 
---------------------------------------------------------------------------
 CTE Scan on b (actual time=0.264..0.268 rows=1 loops=1)
   Buffers: shared hit=12
   CTE b
     ->  Insert on balances cur (actual time=0.261..0.263 rows=1 loops=1)
           Conflict Resolution: UPDATE
           Conflict Arbiter Indexes: balances_pkey
           Tuples Inserted: 0
           Conflicting Tuples: 1
           Buffers: shared hit=12
           ->  Result (actual time=0.003..0.003 rows=1 loops=1)
   CTE l
     ->  Insert on ledger (actual time=1.349..1.349 rows=0 loops=1)
           Buffers: shared hit=56 read=4 dirtied=1
           ->  CTE Scan on b b_1 (actual time=0.449..0.450 rows=1 loops=1)
                 Buffers: shared hit=7 read=3
 Planning:
   Buffers: shared hit=5 read=1
 Planning Time: 1.127 ms
 Execution Time: 3.342 ms
```

**debit** (whale user `seed_0000001`, 100k+ ledger rows)

```
                                                  QUERY PLAN                                                   
---------------------------------------------------------------------------------------------------------------
 Append (actual time=0.136..0.145 rows=1 loops=1)
   Buffers: shared hit=6
   CTE b
     ->  Update on balances balances_1 (actual time=0.130..0.133 rows=1 loops=1)
           Buffers: shared hit=6
           ->  Index Scan using balances_pkey on balances balances_1 (actual time=0.051..0.053 rows=1 loops=1)
                 Index Cond: (user_id = 'seed_0000001'::text)
                 Filter: (balance >= 4000)
                 Buffers: shared hit=4
   CTE l
     ->  Insert on ledger (actual time=0.075..0.075 rows=0 loops=1)
           Buffers: shared hit=7
           ->  CTE Scan on b b_1 (actual time=0.009..0.009 rows=1 loops=1)
                 Buffers: shared hit=1
   ->  CTE Scan on b (actual time=0.135..0.138 rows=1 loops=1)
         Buffers: shared hit=6
   ->  Result (actual time=0.002..0.003 rows=0 loops=1)
         One-Time Filter: (NOT (InitPlan 3).col1)
         InitPlan 3
           ->  CTE Scan on b b_2 (actual time=0.001..0.001 rows=1 loops=1)
         ->  Index Scan using balances_pkey on balances (never executed)
               Index Cond: (user_id = 'seed_0000001'::text)
 Planning:
   Buffers: shared hit=11
 Planning Time: 0.925 ms
 Execution Time: 0.499 ms
```

Every plan is a single-row `balances_pkey` index operation plus, for writes, one append to `ledger`. None of them touches the user's history or depends on table size. On success the debit's 404/409 branch shows `(never executed)`. The `Insert on ledger` time includes maintaining the two ledger indexes.

## Tests

| Where | What |
|---|---|
| [internal/httpapi/server_test.go](internal/httpapi/server_test.go) | auth (every endpoint, 8 bad header variants, storage never touched before auth), 25 invalid bodies, boundaries, unknown fields, error mapping, response shape, routing |
| [internal/ledger/combiner_test.go](internal/ledger/combiner_test.go) | one executor per user, batching actually happens (500 credits → 9 transactions), distinct per-request balances, debits never overdraw, users independent, keyed requests run alone, abandoned requests never execute, no leaked queue entries |
| [e2e/api_test.go](e2e/api_test.go) | contract against the real stack: 401s, 400s, 404s, 409s, full credit/debit flow, idempotency replay/mismatch, ledger UPDATE/DELETE rejected by DB |
| [e2e/concurrency_test.go](e2e/concurrency_test.go) | **rule 1:** 100 rounds of 2 concurrent debits of 60 vs 100 → exactly one 200 + one 409; 200 concurrent debits draining 1000 → exactly 66 succeed. **Rule 2:** 1,000 concurrent credits of 100 → +100,000 exactly, 1,000 distinct returned balances, 1,001 ledger rows. **Rule 4:** read-your-writes under concurrency. **Rule 5:** other user's p99 while 96 goroutines hammer one user. Plus a random credit/debit race on 4 users checked against acknowledged ops |
| `TestZZReconcileWholeLedger` | the reviewers' SQL: every user's balance == Σcredits − Σdebits over the whole table, no negatives, correct `balance_after` running totals |

Latest run:

```
$ docker compose --profile tools run --rm tests      # against the 10M-row database
--- PASS: TestAuthOnEveryEndpoint (0.05s)
--- PASS: TestHealth (0.00s)
--- PASS: TestInputValidation (0.06s)
--- PASS: TestContract (0.04s)
--- PASS: TestIdempotencyKey (0.05s)
--- PASS: TestLedgerRowsAreImmutable (0.03s)
--- PASS: TestConcurrentDebitsNeverOverdraw (1.10s)
--- PASS: TestConcurrentDrain (0.14s)
--- PASS: TestNoLostUpdates (0.35s)
--- PASS: TestMixedRaceMatchesAcknowledgedOps (0.72s)
--- PASS: TestReadYourWrites (0.46s)
    concurrency_test.go:302: cold user alone:          p50=3.276862ms p99=13.285973ms
    concurrency_test.go:303: cold user with hot load:  p50=7.499785ms p99=41.938706ms (hot user served 12943 requests,
0 failed)
--- PASS: TestHotUserDoesNotBlockOthers (2.93s)
--- PASS: TestZZReconcileWholeLedger (3.71s)
PASS

$ go test ./internal/...                             # runs in the tools image build
ok   wallet-ledger/internal/httpapi
ok   wallet-ledger/internal/ledger    (combiner: 500 concurrent credits -> 9 transactions)
```

## Load test results

[cmd/loadtest](cmd/loadtest/main.go) runs 100 concurrent workers. The **mixed** phase is 30% credit / 30% debit / 40% balance over the 100k seeded users (5% of picks go to the whales). The **hot** phase puts 80 workers on one user while 20 keep running the mixed workload on other users. 200, 404 and 409 count as expected outcomes; "failures" are transport errors or any other status.

Machine: Intel Core i7-8565U laptop (4 cores / 8 threads, 1.8 GHz base), 16 GB RAM, SSD, Windows 11 + Docker Desktop (WSL2 VM with 8 vCPU / 8 GB). The API, Postgres and the load generator all share this machine, so absolute numbers are conservative.

### With 10M+ ledger rows

**Mixed workload (1m0s, 100 workers, 30% credit / 30% debit / 40% balance)**

| endpoint | requests | req/s | p50 ms | p90 ms | p99 ms | max ms | 200 | 404 | 409 | failures |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| balance | 153086 | 2551 | 10.10 | 16.92 | 26.64 | 71.09 | 153086 | 0 | 0 | 0 |
| credit | 114842 | 1914 | 17.66 | 26.68 | 40.59 | 93.50 | 114842 | 0 | 0 | 0 |
| debit | 114528 | 1909 | 17.88 | 26.96 | 40.74 | 104.47 | 114381 | 0 | 147 | 0 |

**Hot user (30s, 80 workers on one user + 20 workers on other users)**

| endpoint | requests | req/s | p50 ms | p90 ms | p99 ms | max ms | 200 | 404 | 409 | failures |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| hot balance | 31035 | 1034 | 3.48 | 7.77 | 14.52 | 37.79 | 31035 | 0 | 0 | 0 |
| hot credit | 23535 | 784 | 46.43 | 61.40 | 102.91 | 178.49 | 23535 | 0 | 0 | 0 |
| hot debit | 23387 | 780 | 46.59 | 61.34 | 102.05 | 179.18 | 23387 | 0 | 0 | 0 |
| other balance | 39416 | 1314 | 2.39 | 5.97 | 11.67 | 45.40 | 39416 | 0 | 0 | 0 |
| other credit | 29414 | 980 | 7.17 | 12.05 | 21.89 | 138.62 | 29414 | 0 | 0 | 0 |
| other debit | 29319 | 977 | 7.27 | 12.30 | 22.66 | 144.41 | 29277 | 0 | 42 | 0 |

### With 1M ledger rows (same 100k users, 2 whales), for comparison

**Mixed workload (1m0s, 100 workers, 30% credit / 30% debit / 40% balance)**

| endpoint | requests | req/s | p50 ms | p90 ms | p99 ms | max ms | 200 | 404 | 409 | failures |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| balance | 158668 | 2644 | 9.23 | 16.07 | 26.11 | 89.01 | 158668 | 0 | 0 | 0 |
| credit | 119347 | 1989 | 16.82 | 26.92 | 46.57 | 164.85 | 119347 | 0 | 0 | 0 |
| debit | 119563 | 1993 | 16.93 | 27.17 | 47.81 | 153.58 | 117943 | 0 | 1620 | 0 |

**Hot user (30s, 80 workers on one user + 20 workers on other users)**

| endpoint | requests | req/s | p50 ms | p90 ms | p99 ms | max ms | 200 | 404 | 409 | failures |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| hot balance | 29178 | 973 | 3.88 | 8.85 | 15.60 | 46.86 | 29178 | 0 | 0 | 0 |
| hot credit | 21658 | 722 | 49.57 | 69.27 | 112.15 | 187.43 | 21658 | 0 | 0 | 0 |
| hot debit | 21680 | 723 | 49.79 | 69.16 | 112.86 | 187.35 | 21680 | 0 | 0 | 0 |
| other balance | 35397 | 1180 | 2.62 | 6.74 | 13.52 | 44.66 | 35397 | 0 | 0 | 0 |
| other credit | 26476 | 883 | 7.81 | 13.86 | 29.09 | 82.94 | 26476 | 0 | 0 | 0 |
| other debit | 26452 | 882 | 7.87 | 14.03 | 29.80 | 101.26 | 26177 | 0 | 275 | 0 |

**Reading the numbers:**

- **Table size doesn't move latency.** From 1M to 10M rows, balance p50/p99 goes from 9.2/26.1 to 10.1/26.6 ms, and credit from 16.8/46.6 to 17.7/40.6 ms. The difference is run-to-run noise. Every request touches one `balances` row by primary key and appends at the right edge of the ledger indexes, and none of that depends on how many rows exist.
- **Most of each number is queueing, not SQL.** 100 client goroutines, the API and Postgres all share 4 laptop cores. One request on an idle system takes about 2–3 ms end to end (`cold user alone` in the test output above). In the query plans, the SQL itself takes 0.2–3 ms.
- **Hot-user isolation.** While 80 workers hammer one user, the other users still get p50 ≈ 7 ms and p99 ≈ 22 ms. The hot user itself sustains about **1,560 writes/s on a single wallet**, and its balance reads stay at p50 3.5 ms (MVCC: reads never wait for writers).
- **Zero failures** across about 670k requests per run. The only non-200 responses are the expected 409s for debits larger than a small balance.

**Effect of per-user batching (group commit).** My first version used a plain per-user mutex. Measured back to back on the same machine, at 1M rows, with the same background load:

| hot user (80 workers) | writes/s | p50 | p99 |
|---|---:|---:|---:|
| per-user mutex, one transaction per request | 140 | 444 ms | 2,455 ms |
| per-user queue, one transaction per batch | 1,040 | 57 ms | 203 ms |

The mixed-workload write p99 also fell from 430 ms to 116 ms in that pair of runs, because the whales (5% of traffic) had been behaving like small hot users.

## Seed

[seed/seed.sql](seed/seed.sql) runs entirely inside Postgres (`generate_series`), so no data crosses the network:

- `seed_0000001 … seed_0000010` are whales with 100,000–200,000 rows each. The other 99,990 users get exponentially distributed row counts (median ≈ 60, long tail into the hundreds), topped up so the total is ≥ `SEED_ROWS`.
- **Non-negative at every point, by construction.** Each user's history is a sequence of `(credit c, debit d)` pairs with `1 ≤ d ≤ c`, so the running balance never decreases across a pair and never goes below zero within one. `balance_after` is computed as the running sum, and the `CHECK (balance_after >= 0)` would abort the load if this were ever wrong.
- `balances` is built from the ledger aggregate in the same transaction, so it is consistent by definition. The secondary index is dropped during the bulk insert and rebuilt afterwards, then `VACUUM ANALYZE` runs.
- It is safe to re-run: if seed users already exist, it does nothing.

**Timing on this laptop:** 10,070,988 rows across 100,000 users (10 whales with 107k–179k rows each, median user 60 rows) in **123 s** end to end: row generation 95 s, index rebuild 14 s, `balances` 6 s, VACUUM/ANALYZE 2 s. The 1M-row variant (`SEED_ROWS=1000000 SEED_WHALES=2`) takes 17 s. All four checks in `seed/verify.sql` return 0 on the 10M dataset (about 22 s to run).

## Limits, scaling and what I'd do next

**What breaks first at 10× load (≈1,000 concurrent clients):**

1. **Commit/WAL fsync throughput.** Every request is a durable commit. Postgres group commit helps, but the WAL device's fsync rate is the ceiling. The mitigations are a faster disk, `commit_delay`/`commit_siblings` to batch commits, or, if the business accepts it, `synchronous_commit=off` for a bounded loss window. I wouldn't do the last one for money.
2. **Connections.** 1,000 clients against `max_connections=200`. The API's pool already caps at 48 per instance. With many instances, put PgBouncer (transaction pooling) in front.
3. **A single hot user** is serial by nature: one row and one lock. Batching already gives it about 1,000–1,500 writes/s here (capped by `MAX_BATCH`=128 per commit and by statement execution time). Beyond that, split the wallet's balance across N sub-balance rows ("bucketed counters"), with debits drawing from a bucket that has funds, at the cost of a more complex "balance ≥ amount" check.
4. **Ledger growth.** At billions of rows: range-partition `ledger` by `created_at` (monthly) for cheap archiving and smaller indexes, keeping `(user_id, id)` local per partition.

**Running several API instances:** correctness is unchanged, since all invariants live in Postgres row locks and single-statement transactions, not in the process. What changes:

- The per-user queue is per instance, so a hot user can occupy up to one connection *per instance* (still bounded), and batches from different instances interleave on the row lock. Results stay correct, but batching is less effective. Routing by `user_id` (consistent hashing at the load balancer) would restore full batching.
- Total connections = instances × pool size, so add PgBouncer.
- Idempotency keys already live in the database, so they work across instances.
- Schema application is guarded by an advisory lock.
- Read replicas could serve `GET /balance` only if read-your-writes is preserved, for example by routing a client's reads to the primary for a short time after its writes, or by waiting on the replica LSN.

**With more time:**

- Keyset-paginated `GET /users/{id}/ledger` using the `(user_id, id)` index.
- Expire and garbage-collect idempotency keys (for example after 24h, via a partitioned or TTL table).
- Prometheus metrics (latency histograms, pool wait, lock wait, retries) and request tracing.
- A crash-injection test (kill Postgres mid-load, then run `verify.sql`).
- Benchmarks on dedicated hardware with the load generator on a separate machine.
- Amount overflow currently returns 422 `balance_limit_exceeded` (a balance above 9.2×10¹⁸ minor units). It's unreachable in practice, but it's handled explicitly.

## Project layout

```
cmd/api/              HTTP server entrypoint (config, pool, graceful shutdown, healthcheck)
cmd/loadtest/         load generator, prints p50/p90/p99/max per endpoint
internal/httpapi/     routing, auth, validation, error mapping (+ unit tests)
internal/ledger/      SQL, retries, idempotency, per-user queue / group commit (+ unit tests)
results/              raw outputs: load tests, EXPLAIN, seed log, verify, e2e
migrations/           schema (embedded into the binary, also used by the seed)
seed/                 seed.sql / seed.sh / verify.sql
scripts/explain.sql   query plans for the README
e2e/                  black-box + concurrency tests against the running stack
```
