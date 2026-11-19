# Booking Search Modernization: Replacing Elasticsearch with a Postgres Read Layer

Nov 19, 2026 · @vijay vishwakarma

## Executive summary

**Decision proposed:** retire the Elasticsearch read store. The booking UI is served directly from PostgreSQL 17 (Cloud SQL in GCP, PostgreSQL on Anthos on-prem) through a new stateless **Booking Query API**. One database holds both the system of record and the 150-day UI read model.

- **Why it is fast enough:** almost every UI query is scoped to entitled accounts and a date range. Postgres answers those with composite B-tree index range scans in single-digit milliseconds. The Elasticsearch strengths we pay for (relevance-ranked full text over unscoped data) are not what the UI uses.
- **Why it is safer:** it removes the non-transactional dual write (Oracle + Elasticsearch) done by the processor and by every service that updates bookings, along with the drift and reconciliation work it causes.
- **Expected outcome:** p95 server-side latency at or below today's (target ≤ 50 ms), bookings visible on commit instead of after Elasticsearch's 1 s refresh, no Elasticsearch cluster to run or license, and one design that runs unchanged on Cloud SQL + GKE and on-prem Postgres + Anthos (China and India residency).
- **Before commitment:** a PoC that replays 2–4 weeks of real UI queries against both stacks. All latency figures in this document are engineering estimates until that PoC replaces them with measurements.

**Older data:** each calendar month older than 150 days moves to compressed Parquet files in object storage (GCS in GCP; Dell ECS or another S3-compatible store on-prem). It stays readable through the same API for 7 years, in seconds rather than milliseconds, at a fraction of database storage cost.

## Developer summary

Three new deployables share one PostgreSQL 17 schema: the **Booking Query API** (all UI reads, both tiers), the **Booking Archiver** (partitions, summaries, monthly archive to Parquet) and **booking-db** (Flyway migrations). All three run unchanged on GKE + Cloud SQL and on Anthos + on-prem Postgres.

### Components

| Component | Runtime | Responsibility | Reads | Writes |
| --- | --- | --- | --- | --- |
| Booking Processor (existing, small change) | Spring Boot | Kafka → account match → idempotent upsert; routes bookings for archived months to `booking_correct()` | Kafka, account reference | `booking` on the primary |
| Booking Query API (new) | Java 21, Spring Boot 4, embedded DuckDB | Search, detail, reference lookup, daily summary, CSV export across hot and cold tiers | Postgres replicas, manifest, Parquet files | Nothing in booking data |
| Booking Archiver (new) | Java 21, Spring Boot 4 (no web), DuckDB with its Postgres extension, AWS SDK S3 client | Creates partitions, maintains summaries, archives months, purges, merges corrections, expires after 7 years | Postgres replica | Parquet files, manifest, partition DDL, summaries |
| booking-db (new) | Flyway job | Tables, functions, triggers, roles | — | Schema |

### Data lifecycle

1. **Day 0:** the processor upserts a booking into its daily partition `booking_pYYYYMMDD`. A trigger marks the account-day dirty; the summary worker refreshes `account_daily_summary` within about a minute.
2. **Day 150:** the partition's search indexes (trigram, amount) are dropped.
3. **Month entirely older than 150 days:** the archiver freezes it (corrections now go to `archive.archive_override`), writes it to Parquet in account buckets, checks row counts and amount sums, then switches reads to the cold tier.
4. **Switch + 7 days:** the month's partitions are detached and dropped from Postgres.
5. **Weekly:** pending corrections are merged into new Parquet file versions.
6. **Age 7 years:** the month's files and summaries are deleted.

### Search request flow

1. Validate the request; check every `accountId` against the user's entitlements.
2. Split the date range at the hot boundary: the first day after the newest archived month.
3. Hot part: one SQL query on a replica, newest first.
4. If the page is not full and the range reaches archived months: size a date window from `account_daily_summary`, pick files from the manifest, run one DuckDB query.
5. Apply pending corrections, sort, cut to page size, sign the cursor.

### Rules every developer keeps

- Every query carries account IDs and a date range, in both tiers.
- Sort is always `booking_date DESC, booking_id DESC`, with keyset pagination only.
- Bucket = `account_id % bucket_count`, with `bucket_count` read from the month's manifest row. Use the shared function in `booking-archive-common`; never re-implement it.
- The API never lists object storage. It opens only files named in the manifest.
- Changes to existing bookings go through `booking_correct()` or `booking_delete()`, never a direct `UPDATE`, so archived months stay correct.
- DuckDB extensions are baked into the images; nothing is downloaded at runtime.

### API example

```http
POST /api/v1/bookings/search
Authorization: Bearer <JWT>
Content-Type: application/json

{
  "accountIds": [1234567],
  "fromDate": "2025-12-01",
  "toDate": "2026-11-19",
  "filters": { "currency": ["EUR"], "dcIndicator": "D" },
  "pageSize": 50
}
```

```json
{
  "items": [
    { "bookingDate": "2026-11-18", "bookingId": 90071234, "accountId": 1234567,
      "valueDate": "2026-11-18", "bookingType": "FINAL", "status": "BOOKED",
      "dcIndicator": "D", "amount": "1250.0000", "currency": "EUR",
      "counterpartyName": "ACME GMBH", "reference": "INV-2026-1188",
      "endToEndId": "E2E-77812", "narrative": "Invoice 1188", "attrs": {}, "tier": "HOT" }
  ],
  "page": { "pageSize": 50, "hasMore": true, "partial": false, "nextCursor": "eyJ2IjoxLCJkIjoi..." },
  "meta": { "tiers": ["HOT", "COLD"], "elapsedMs": 41 }
}
```

### Query examples

Hot tier, one account, next page after the cursor row:

```sql
SELECT booking_date, booking_id, account_id, value_date, booking_type, status, dr_cr,
       amount, currency, counterparty_name, reference, end_to_end_id, narrative, attrs
  FROM booking
 WHERE account_id = :accountId
   AND booking_date BETWEEN :fromDate AND :toDate
   AND status <> 'SUPERSEDED'
   AND currency = ANY(:currencies)
   AND (booking_date < :cursorDate OR (booking_date = :cursorDate AND booking_id < :cursorId))
 ORDER BY booking_date DESC, booking_id DESC
 LIMIT 51;
```

Cold tier, step 1: size the window from the summary (kept 7 years in Postgres):

```sql
SELECT booking_date, sum(txn_count) AS n
  FROM account_daily_summary
 WHERE account_id = ANY(:accountIds)
   AND booking_date BETWEEN :coldFrom AND :coldTo
   AND currency = ANY(:currencies) AND dr_cr = :dc
 GROUP BY booking_date
 ORDER BY booking_date DESC;
-- walk rows newest first, add n until the total reaches pageSize + 1: that day starts the window
```

Cold tier, step 2: pick files from the manifest (account 1234567, 256 buckets → bucket 135):

```sql
SELECT path
  FROM archive.archive_file
 WHERE month = ANY(:months) AND bucket = 135 AND active
   AND min_account_id <= 1234567 AND max_account_id >= 1234567;
```

Cold tier, step 3: DuckDB reads only the matching row groups of those files:

```sql
SELECT booking_date, booking_id, account_id, value_date, booking_type, status, dr_cr,
       amount, currency, counterparty_name, reference, end_to_end_id, narrative, attrs
  FROM read_parquet(['gs://bookings-archive-global/booking/region=GLOBAL/month=2026-05/v=7f3c9a1e/bucket=135/a1_data_2.parquet'])
 WHERE account_id = ?
   AND booking_date BETWEEN ? AND ?
   AND status <> 'SUPERSEDED' AND currency IN (?) AND dr_cr = ?
   AND (booking_date < ? OR (booking_date = ? AND booking_id < ?))
 ORDER BY booking_date DESC, booking_id DESC
 LIMIT 51;
```

Pending corrections for the same accounts and window, applied before the page is returned:

```sql
SELECT DISTINCT ON (booking_date, booking_id) *
  FROM archive.archive_override
 WHERE account_id = ANY(:accountIds) AND booking_date BETWEEN :from AND :to
   AND merged_run_id IS NULL
 ORDER BY booking_date, booking_id, override_id DESC;
```

Correcting a booking from any service (works for hot and archived months):

```sql
SELECT booking_correct('{"booking_date":"2026-03-14","booking_id":88123,"account_id":1234567,
                         "status":"RETURNED", ... full row ...}'::jsonb);
```

The complete build instructions are in the four **Implementation specification** sections near the end.

## Current state and its problems

Today every matched booking is written twice, to Oracle and to Elasticsearch, with no transaction spanning both.

1. Two kinds of data arrive on Kafka: **bookings** (intraday, many times a day, plus one final end-of-day set) and **PLS**.
2. Records are matched on the account identifier against a separate account reference source.
3. Matches go to the Spring Boot processor, which builds a booking object and writes it to **Oracle** (system of record) and to **Elasticsearch** (UI backend, 150 days).
4. Any later field update made in Oracle by any service is repeated against Elasticsearch to keep the two copies equal.

| Problem | Impact |
| --- | --- |
| Dual write without a shared transaction | One write can succeed and the other fail; the copies drift and need reconciliation |
| Every updating service must also update Elasticsearch | Sync logic spread across services; a missed path means the UI shows stale data |
| Two stores for the same rows | Duplicate storage, compute and operations cost (cluster sizing, shard management, upgrades) |
| Elasticsearch refresh interval | New bookings are searchable only after the next refresh (1 s by default) |
| Oracle is being replaced by Cloud SQL anyway | The migration is the natural point to remove the second store instead of re-wiring it |

## Target architecture

&#91;embedded content: target architecture · identical in GCP and on-prem\]

The processor and the other services write once, to the Postgres primary. The API reads replicas for recent data and Parquet in object storage for months older than 150 days; Elasticsearch and every sync path into it are gone.

## Data model, partitioning and indexing

One `booking` table, range-partitioned by day, serves as both the system of record and the UI read model. Partition pruning means the 150-day UI window costs nothing extra for older history.

### Booking table

```sql
CREATE TABLE booking (
  booking_date      date          NOT NULL,
  booking_id        bigint        NOT NULL,
  business_key      text          NOT NULL,  -- source booking reference (idempotency key)
  account_id        bigint        NOT NULL,
  value_date        date,
  booking_type      text          NOT NULL,  -- INTRADAY | FINAL
  status            text          NOT NULL,
  dr_cr             char(1)       NOT NULL,
  amount            numeric(20,4) NOT NULL,
  currency          char(3)       NOT NULL,
  counterparty_name text,
  reference         text,
  end_to_end_id     text,
  narrative         text,
  search_text       text GENERATED ALWAYS AS (
                      lower(coalesce(reference,'') || ' ' ||
                            coalesce(counterparty_name,'') || ' ' ||
                            coalesce(narrative,''))) STORED,
  attrs             jsonb,                   -- rarely used / variable PLS fields
  source_version    bigint        NOT NULL,  -- source sequence or Kafka offset
  created_at        timestamptz   NOT NULL DEFAULT now(),
  updated_at        timestamptz   NOT NULL DEFAULT now(),
  PRIMARY KEY (booking_date, booking_id),
  UNIQUE (booking_date, business_key)
) PARTITION BY RANGE (booking_date);
```

- Every column the UI filters or sorts on is a typed column. JSONB holds only fields that are displayed, not searched.
- Primary and unique keys must contain the partition key (`booking_date`). This is a common surprise for teams coming from Oracle.
- Assumption: a booking's `booking_date` does not change between its intraday and final versions. If it can, the idempotency key needs a separate lookup table.

### Idempotent upsert (intraday and final)

```sql
INSERT INTO booking (...) VALUES (...)
ON CONFLICT (booking_date, business_key) DO UPDATE
   SET status = EXCLUDED.status, booking_type = EXCLUDED.booking_type,
       amount = EXCLUDED.amount, ...,
       source_version = EXCLUDED.source_version, updated_at = now()
 WHERE booking.source_version < EXCLUDED.source_version;
```

- Kafka redelivery or replay cannot create duplicates or overwrite newer data with older data.
- The end-of-day final set flips rows to `FINAL`. Intraday rows absent from the final set are marked superseded, not physically deleted, so the audit trail stays.

### Indexes

Keep about five indexes. Each extra index slows every insert and update.

| Index | Definition | Serves | Partitions |
| --- | --- | --- | --- |
| `ix_acct_date` | `(account_id, booking_date DESC, booking_id DESC)` | Main list view, sort by date, keyset paging | All |
| `ix_ref` | `(reference)` | Exact reference lookup | All |
| `ix_e2e` | `(end_to_end_id)` | Exact end-to-end ID lookup | All |
| `ix_search_trgm` | `GIN (search_text gin_trgm_ops)` | "Contains" search on reference, counterparty, narrative | Hot 150 days only |
| `ix_acct_amount` | `(account_id, amount)` | Amount range filter | Hot 150 days, only if the query analysis shows it is used |

- No `INCLUDE` columns on `ix_acct_date`. Status and amount change during the day, and indexed columns block HOT (heap-only tuple) updates. Fetching 50 heap rows costs well under a millisecond when cached.
- Heavy indexes exist only on partitions younger than 150 days. Postgres allows per-partition indexes, which reproduces Elasticsearch's "150 days searchable" at a fraction of the cost. Beyond 150 days, free-text search is limited to one account and 3 months.
- `account_id = ANY(:accounts)` benefits from the PostgreSQL 17 B-tree improvement for multi-value searches, relevant for corporate users entitled to many accounts.

### Partition lifecycle

| When | Action | Run by |
| --- | --- | --- |
| Daily | Create partitions `booking_pYYYYMMDD` 7 days ahead with their indexes and `fillfactor = 85` | Archiver `partitions` mode |
| Partition turns 150 days old | `DROP INDEX CONCURRENTLY` on its trigram and amount indexes | Archiver `partitions` mode |
| Whole month older than 150 days | Freeze, write Parquet, validate, switch reads to the cold tier | Archiver `archive` mode |
| 7 days after the switch | `DETACH PARTITION ... CONCURRENTLY` and drop the month's partitions | Archiver `purge` mode |
| Month older than 7 years | Delete its Parquet files, manifest rows and summary rows | Archiver `expire` mode |

The Booking Archiver runs all of these as Kubernetes CronJobs, identically in GCP and on-prem.

### Summary table for dashboards

```sql
CREATE TABLE account_daily_summary (
  account_id   bigint        NOT NULL,
  booking_date date          NOT NULL,
  currency     char(3)       NOT NULL,
  dr_cr        char(1)       NOT NULL,
  status       text          NOT NULL,
  txn_count    bigint        NOT NULL,
  amount_sum   numeric(24,4) NOT NULL,
  refreshed_at timestamptz   NOT NULL,
  PRIMARY KEY (account_id, booking_date, currency, dr_cr, status)
) PARTITION BY RANGE (booking_date);   -- one partition per year, kept 7 years
```

- A row trigger on `booking` inserts `(account_id, booking_date)` into a small `summary_dirty` table.
- The archiver's summary worker recomputes only the dirty account-days every 30 s. This avoids the lock contention that same-transaction counters would cause on busy accounts.
- The summary is kept for 7 years. Dashboard tiles read it, and the API uses it to size cold-tier reads.

## Booking Query API

The Booking Query API is a stateless Spring Boot service that replaces Elasticsearch as the UI's backend. It keeps the UI's current search contract, so the UI does not change.

This section is the design overview. The complete build specification, including the cold tier, is in **Implementation specification: Booking Query API**.

### Stack

| Layer | Choice | Reason |
| --- | --- | --- |
| Runtime | Java 21, or Java 25 LTS if approved | Same skills as the processor team; Java 25 removes virtual-thread pinning on `synchronized` |
| Framework | Spring Boot 4.x, Spring MVC, `spring.threads.virtual.enabled=true` | Many concurrent blocking JDBC calls without a large thread pool |
| SQL | jOOQ (or `NamedParameterJdbcTemplate`) | Builds only the SQL for filters actually present; no ORM overhead |
| Driver and pool | pgJDBC 42.6 or later, HikariCP | pgJDBC 42.6+ avoids virtual-thread pinning |
| Cache | Caffeine, reference data only (currencies, account metadata) | Bookings are never cached; Postgres is fast enough and always current |

### Endpoints (v1 contract; an adapter maps any existing UI contract)

| Endpoint | Purpose |
| --- | --- |
| `POST /api/v1/bookings/search` | List view across hot and cold tiers: filters, keyset cursor, up to 200 rows |
| `GET /api/v1/accounts/{accountId}/bookings/{bookingDate}/{bookingId}` | Booking detail |
| `GET /api/v1/accounts/{accountId}/bookings/by-reference` | Exact lookup by reference or end-to-end ID within a date range |
| `GET /api/v1/accounts/{accountId}/daily-summary` | Daily counts and sums, up to 7 years |
| `GET /api/v1/bookings/export.csv` | Streaming CSV export, up to 7 years |

### Query rules that keep it fast

- **Allow-listed filters only.** The UI sends filter JSON; the API maps each allowed field to a parameterized predicate. No raw SQL, and no filter without an index behind it.
- **Mandatory scope.** Every query carries the user's entitled accounts and a date range. Interactive range is at most 400 days across both tiers; longer ranges use the streaming CSV export.
- **Keyset pagination**, never `OFFSET`. The cursor is the last row's `(booking_date, booking_id)`.
- **No exact totals.** Return `hasMore` (fetch 51 rows, show 50) or a count capped at 10,000, as Elasticsearch does by default.
- **Dynamic SQL.** Generate only the predicates present. Avoid `(:x IS NULL OR col = :x)`, which defeats index use under generic plans.

### Single-account list query

```sql
SELECT booking_date, booking_id, value_date, amount, currency, dr_cr,
       status, counterparty_name, reference
  FROM booking
 WHERE account_id = $1
   AND booking_date BETWEEN $2 AND $3
   AND status = $4                                  -- only if filtered
   AND (booking_date, booking_id) < ($5, $6)        -- keyset cursor
 ORDER BY booking_date DESC, booking_id DESC
 LIMIT 51;
```

### Multi-account list query (corporate users with many accounts)

The index is ordered by account first, so a plain `account_id = ANY(...)` with `ORDER BY booking_date` can read every row in the range before sorting. This pattern reads at most 51 rows per account and merges them:

```sql
SELECT b.*
  FROM unnest($1::bigint[]) AS a(account_id)
  CROSS JOIN LATERAL (
        SELECT booking_date, booking_id, account_id, amount, currency,
               dr_cr, status, counterparty_name, reference
          FROM booking
         WHERE account_id = a.account_id
           AND booking_date BETWEEN $2 AND $3
           AND (booking_date, booking_id) < ($4, $5)
         ORDER BY booking_date DESC, booking_id DESC
         LIMIT 51) b
 ORDER BY b.booking_date DESC, b.booking_id DESC
 LIMIT 51;
```

### Read routing and consistency

- Two data sources: **replica** for `@Transactional(readOnly = true)` reads, **primary** for writes and read-your-own-write screens.
- If replica lag exceeds 1 s (`now() - pg_last_xact_replay_timestamp()` on the replica, polled every few seconds), reads fall back to the primary until lag recovers.

### Connections and database roles

```yaml
# illustrative: two Hikari pools per pod
replica: { maximum-pool-size: 30, minimum-idle: 30, connection-timeout: 1000 }
primary: { maximum-pool-size: 10, minimum-idle: 10, connection-timeout: 1000 }
```

```sql
ALTER ROLE booking_api_ro SET statement_timeout = '2s';
ALTER ROLE booking_api_ro SET idle_in_transaction_session_timeout = '5s';
ALTER ROLE booking_api_ro SET jit = off;               -- JIT adds startup cost to short OLTP queries
ALTER ROLE booking_api_ro SET default_transaction_read_only = on;
```

- Role-level settings apply identically on Cloud SQL and on-prem, with no flag differences.
- Start with direct Hikari connections. Add a pooler (Cloud SQL Managed Connection Pooling in GCP, PgBouncer on-prem) only when total connections from all pods exceed a few hundred.
- Virtual threads do not add database connections. The Hikari pool size is the real concurrency limit and must be sized against the database, not the request rate.

### Entitlements and observability

- The user's entitled account list comes from the token or entitlement service; every query is restricted with it. Row-level security can be added as a second guard.
- Micrometer histograms per query shape (p50, p95, p99), plus `pg_stat_statements` and Cloud SQL Query Insights for the database side.

## Response time: Elasticsearch vs the new path

For account-scoped queries, which make up most UI traffic, the Postgres path is estimated at 5–20 ms server-side against 10–40 ms for Elasticsearch. It skips Elasticsearch's scatter to every shard and its second fetch round trip. All figures are planning estimates for an in-region deployment with hot data in memory; the PoC replaces them with measurements.

### Where the milliseconds go (single-account list, 50 rows)

| Step | Elasticsearch path (today) | Postgres path (new) |
| --- | --- | --- |
| UI → backend API (in-cluster, keep-alive) | \~1 ms | \~1 ms |
| API framework overhead (routing, validation) | 1–3 ms | 1–3 ms |
| Build and send the query | 1–2 ms (JSON query DSL over HTTP) | under 1 ms (pool checkout, prepared statement) |
| Network to the data store | 0.3–1 ms | 0.3–1 ms (private IP, same region) |
| Query phase | 2–20 ms: every shard of every matching index runs the query; the slowest shard sets the time | 1–10 ms: one B-tree range scan on one or a few daily partitions |
| Fetch phase | 2–10 ms: second round trip to fetch `_source` of the top hits | None: rows return with the query |
| Merge and map to JSON | 1–5 ms (coordinator merge, then parse in the API) | 1–2 ms (row mapping, JSON) |
| **Typical total, server side** | **10–40 ms** | **5–20 ms** |
| **p95 under load** | **50–150 ms** (segment merges, refresh, JVM GC on data nodes) | **Under 50 ms** (target) |

The UI already calls a backend service today, not Elasticsearch directly, so the new API adds no network hop.

### By query shape

&#91;embedded content: engineering estimates, not measurements · 7 UI query shapes · in-region, hot data in memory\]

The exception is free-text "contains" search across many accounts and the full 150 days, where a trigram scan over many partitions can exceed Elasticsearch. The API therefore requires an account scope and a date range for free-text search. If the query-log analysis shows broad cross-client search is a real need, that one feature gets a slim search index fed by CDC.

## Data freshness

The new design shows a booking to the UI as fast as today or faster, and it removes the case where the UI copy silently diverges.

| Aspect | Elasticsearch today | Postgres design |
| --- | --- | --- |
| When a new booking becomes visible | After the next index refresh (1 s by default; often raised on large clusters) | On commit, on the primary |
| Reads served from a replica | Not applicable | After replica apply lag: usually milliseconds, can reach seconds during end-of-day bursts |
| Updates by other services | A separate Elasticsearch write that each service must remember | Same row, same commit |
| Worst-case divergence | Unbounded: a failed dual write stays wrong until reconciliation | Bounded by replica lag; the API reads from the primary when lag exceeds 1 s |

Replica lag is monitored through the Cloud SQL `replica_lag` and `replica_byte_lag` metrics in GCP, and through `pg_stat_replication` on-prem.

## Tiered storage: 150 days hot, 7 years cold

Bookings stay in Postgres for at least 150 days. Each whole calendar month older than that moves to Parquet in object storage and stays readable through the same API until it is 7 years old. Cold reads take seconds, not milliseconds, and object storage costs a fraction of database storage with no always-on compute.

| Tier | Age | Store | UI latency target |
| --- | --- | --- | --- |
| Hot | 0 to 150–180 days (whole months move together) | PostgreSQL 17 | p95 ≤ 50 ms |
| Cold | Older months, up to 7 years | Parquet in GCS or Dell ECS, read by DuckDB inside the API | p95 ≤ 3 s for one account, ≤ 8 s for 50 accounts |
| Always in Postgres | 7 years | `account_daily_summary`, account data, archive manifest | Milliseconds |

### Why a multi-terabyte month stays fast to read

- The archiver splits each month into account buckets (`account_id % bucket_count`) and writes each bucket as files of about 512 MB, sorted by account, date and booking ID. `bucket_count` is chosen per month (64 to 4,096) so a bucket stays around 4 GB of Parquet.
- A manifest in Postgres lists every file with its account and date range. For one account and one month, the API usually opens a single file.
- Inside that file DuckDB reads the footer, then only the row groups (122,880 rows each) whose account and date ranges match. A page of results touches megabytes, not the terabytes of the month.
- `account_daily_summary` tells the API how many bookings an account has per day, so it reads only the days needed to fill one page, and skips months with no activity.

### Month lifecycle

| Status | Meaning | Reads served from |
| --- | --- | --- |
| (no row) | Month still only in Postgres | Hot |
| `PLANNED` | Archive run created; not yet frozen | Hot |
| `EXPORTING`, `COMPACTING` | Frozen; Parquet being written; corrections go to `archive_override` | Hot + overrides |
| `SWITCHED` | All buckets validated; files active | Cold + overrides |
| `PURGED` | Postgres partitions dropped (7 days after switch) | Cold + overrides |
| `ROLLED_BACK` | Switch reverted before purge; overrides applied back to Postgres | Hot |
| `EXPIRED` | Older than 7 years; files deleted | Not available |
| `FAILED` | A step failed 3 times; needs an operator | Hot |

### Storage per environment

| Aspect | GCP | On-prem (India, China) |
| --- | --- | --- |
| Object store | One regional GCS bucket per residency zone | Dell ECS bucket per region, S3 API, path-style URLs |
| DuckDB access | S3 interoperability with HMAC keys | Access key of an ECS object user |
| Archiver delete and list | AWS SDK S3 client against `https://storage.googleapis.com` | Same client against the ECS endpoint |
| Storage class | Autoclass | ECS default |
| Immutability | Bucket retention policy 7 years (Bucket Lock optional) | S3 Object Lock, compliance mode, 7 years |
| Staging cleanup | Lifecycle rule deletes `_staging/` objects after 14 days | Same rule |
| Encryption | Customer-managed keys (CMEK) | ECS encryption at rest |

GCS access from DuckDB needs HMAC keys for two dedicated service accounts (read-only for the API, read-write for the archiver). Many organizations block HMAC keys by policy, so security must approve this exception before build starts.

### Limits the API enforces on archived data

- At most 50 accounts per request when the range reaches archived months (500 for hot-only ranges).
- Free-text search on data older than 150 days: one account and at most 3 months.
- Interactive range at most 400 days; longer ranges use the streaming CSV export (up to 7 years).
- Reference and detail lookups in archived months need the account ID and a date range.

### Cold-tier risks

| Risk | Mitigation |
| --- | --- |
| HMAC keys blocked by GCP organization policy | Policy exception for two dedicated service accounts, decided before build |
| Archiving a multi-terabyte month runs long | Day and bucket tasks spread across several pods; each task resumes independently |
| DuckDB extensions cannot be downloaded (China, bank networks) | Extensions baked into images at build time; runtime install disabled |
| Corrections to archived months | `booking_correct()` writes overrides; the API merges them on read; weekly compaction folds them into files |
| Wrong data switched to cold | Per-bucket row count and amount checks before switch; 7-day grace before purge; rollback mode |

## Cloud SQL for PostgreSQL 17 settings

The defaults are tuned for a general workload. This design needs three changes of emphasis: keep the hot index set in memory, absorb end-of-day write bursts, and vacuum aggressively because intraday bookings are updated many times.

Example values assume an Enterprise Plus instance with 16 vCPU and 128 GB RAM; scale them with the machine. Defaults and allowed ranges are from the Cloud SQL flags reference ([source](https://docs.cloud.google.com/sql/docs/postgres/flags)). Manage flags in Terraform: `gcloud sql instances patch --database-flags` replaces the whole flag set each time.

### Instance and storage

| Setting | Recommendation | Why |
| --- | --- | --- |
| Edition | Enterprise Plus | Data cache on local SSD, 99.99% SLA, under 1 s planned maintenance downtime, and required for Managed Connection Pooling ([source](https://docs.cloud.google.com/sql/docs/postgres/editions-intro)) |
| Data cache | On (default on Enterprise Plus) | Reads go memory, then local-SSD cache, then storage ([source](https://docs.cloud.google.com/sql/docs/postgres/data-cache)); keeps the 150-day index set off the network disk |
| Storage | SSD, or Hyperdisk Balanced where offered; automatic storage increase on | Predictable IOPS during end-of-day bursts |
| High availability | Regional HA on the primary | The HA standby does not serve reads ([source](https://thebuild.com/blog/managed-postgres-examined-google-cloud-sql-for-postgresql/)); read capacity comes from replicas |
| Read replicas | 1–2 in the same region for UI reads | Separates UI reads from Kafka ingestion |
| Memory sizing | RAM + data cache large enough to hold all hot-partition indexes plus recent heap | The single biggest factor in read latency |

### Memory

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `shared_buffers` | One-third of memory (range 10–60%) | Keep default; up to 40% if the hot index set does not fit | Yes | OS cache and data cache also serve reads |
| `effective_cache_size` | 40% (range 10–70%) | 70% | No | Planner hint only; favours index scans |
| `work_mem` | 4 MB | 8 MB instance-wide; 16 MB for the API role; 128 MB for aggregator and export roles | No | Per sort or hash node per connection; set per role to avoid memory blow-up |
| `maintenance_work_mem` | 64 MB | 2 GB | No | Faster `CREATE INDEX` and vacuum |
| `autovacuum_work_mem` | -1 (uses `maintenance_work_mem`) | 1 GB | No | Bounds memory across many autovacuum workers |
| `wal_buffers` | 16 MB | 64 MB | Yes | Fewer WAL buffer stalls under concurrent upserts |
| `huge_pages` | try | Keep | Yes | Lower memory overhead for a large `shared_buffers` |

### Planner

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `random_page_cost` | 4 | 1.1 | No | Default assumes spinning disks; SSD random reads are nearly as cheap as sequential |
| `effective_io_concurrency` | 1 | 200 | No | Prefetch for bitmap heap scans on SSD |
| `default_statistics_target` | 100 | Keep; raise to 500–1000 per column on `account_id` with `ALTER TABLE ... SET STATISTICS` | No | Account sizes are skewed; better estimates for big corporates |
| `plan_cache_mode` | auto | Keep; `force_custom_plan` for the API role if generic plans pick bad paths for large accounts | No | Skewed parameters can mislead generic plans |
| `jit` | Standard default (on) | Off for the API role | No | JIT compile time hurts short queries |
| `enable_partitionwise_aggregate` | off | On for the aggregator role only | No | Aggregates per partition, then combines |

### WAL and checkpoints (ingestion throughput)

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `max_wal_size` | 1504 MB | 8–16 GB | No | Fewer forced checkpoints during end-of-day loads |
| `min_wal_size` | Standard default | 2 GB | No | Avoids recycling WAL segments under steady load |
| `checkpoint_timeout` | 300 s | 600–900 s | No | Fewer full-page writes; check Cloud SQL's operational guidelines first, since longer values lengthen crash recovery |
| `checkpoint_completion_target` | 0.9 | Keep | No | Spreads checkpoint I/O |
| `wal_compression` | off on Enterprise; on for Enterprise Plus above 32 vCPU | `lz4` | No | Less WAL to write and to ship to replicas, so lower replica lag |
| `synchronous_commit` | on | **Keep on** | No | System of record for bookings; never trade durability for speed here |

### Autovacuum (intraday updates make this critical)

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `autovacuum_max_workers` | 3 | 6–8 | Yes | Many active daily partitions |
| `autovacuum_naptime` | 60 s | 15 s | No | Hot partitions are revisited sooner |
| `autovacuum_vacuum_scale_factor` | 0.2 | 0.05 globally; 0.01 on the last 3 daily partitions | No | Vacuum after 1–5% dead rows instead of 20% |
| `autovacuum_vacuum_insert_scale_factor` | 0.2 | 0.05 | No | Keeps the visibility map current on insert-heavy partitions |
| `autovacuum_vacuum_cost_limit` | -1 (uses 200) | 2000 | No | Lets vacuum keep up with update volume |
| `log_autovacuum_min_duration` | Standard default | 10 s | No | Shows slow vacuums |

PostgreSQL 17 makes this cheaper: vacuum's new memory structure uses up to 20 times less memory, and vacuum is no longer silently capped at 1 GB ([source](https://www.postgresql.org/docs/17/release-17.html)).

### Parallelism

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `max_worker_processes` | Varies by memory | 32 | Yes | Room for parallel query, maintenance and replication workers |
| `max_parallel_workers` | 8 | 16 (one per vCPU) | No | Upper bound for all parallel work |
| `max_parallel_workers_per_gather` | 2 | 0 for the API role; 4 for aggregator and export roles | No | Small OLTP queries gain nothing; parallel startup adds tail latency |
| `max_parallel_maintenance_workers` | 2 | 4–8 | No | Faster index builds |

On replicas, `max_connections` and the parallel-worker flags must be equal to or higher than the primary's.

### Connections, timeouts and replicas

| Flag | Cloud SQL default | Recommended | Restart | Why |
| --- | --- | --- | --- | --- |
| `max_connections` | Depends on memory (up to 1,000 at 120 GB+) | Pods × pool size + 20% headroom | Yes | Each connection is a backend process |
| `idle_in_transaction_session_timeout` | 0 | 60 s instance-wide; 5 s for the API role | No | Idle open transactions block vacuum |
| `temp_file_limit` | 10% of initial disk | 5–10 GB | No | Stops one runaway sort from filling the disk |
| `hot_standby_feedback` (replicas) | off | on | No | Prevents UI queries on replicas being cancelled by vacuum; bloat cost is small with a 2 s statement timeout |
| `max_standby_streaming_delay` (replicas) | Standard default | Keep 30 s | No | Limits how long replay waits for a conflicting query |
| Managed Connection Pooling | Off | Only if total connections exceed a few hundred | Restart to enable | Transaction mode does not support `SET`/`RESET`, SQL-level `PREPARE`/`DEALLOCATE` or session advisory locks ([source](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)) |

### Extensions and observability

| Setting | Recommended | Why |
| --- | --- | --- |
| `CREATE EXTENSION pg_trgm, btree_gin` | Yes | Trigram "contains" search |
| `pg_stat_statements` and Query Insights | On | Per-query latency and plan drift |
| `track_io_timing` | on | I/O time visible in `EXPLAIN (ANALYZE, BUFFERS)` and statistics |
| `log_min_duration_statement` | 200 ms | Captures slow UI queries |
| `cloudsql.enable_auto_explain` | on (restart), log plans over 500 ms | Plans of slow queries without reproducing them |
| `cloudsql.enable_index_advisor` | on (restart) | Suggests missing indexes from real traffic |
| `cloudsql.enable_pg_cron` | Only if `pg_partman`/`pg_cron` are used | Partition jobs can also run as a CronJob |
| `cloudsql.logical_decoding` | Only if CDC is needed later | Keeps the option of a Debezium-fed search index |

### Indexing and ingestion speed

- **New partitions:** create them 7 days ahead with all indexes; building an index on an empty table costs nothing.
- **Adding an index later:** `CREATE INDEX ON ONLY booking (...)` on the parent, `CREATE INDEX CONCURRENTLY` on each partition, then `ALTER INDEX ... ATTACH PARTITION`. No long table locks.
- **Initial load from Oracle:** load partitions without secondary indexes using `COPY` (pgJDBC `CopyManager`), then build indexes with high `maintenance_work_mem` and parallel maintenance workers, then `ANALYZE`. PostgreSQL 17 also speeds up bulk loading ([source](https://www.postgresql.org/about/press/presskit17/)).
- **Kafka ingestion:** batch 100–500 rows per upsert, `reWriteBatchedInserts=true` on pgJDBC, one transaction per poll batch, and commit Kafka offsets only after the database commit.
- **HOT updates:** `fillfactor = 85` on recent partitions and no indexes on columns that change intraday (such as `status`), so updates avoid rewriting index entries.

## On-prem parity (China and India residency)

The same schema, API image and migrations run in GCP and on-prem; only the Postgres hosting differs.

| Concern | GCP | On-prem |
| --- | --- | --- |
| PostgreSQL 17 | Cloud SQL Enterprise Plus | CloudNativePG operator on Anthos (or Patroni on VMs) |
| High availability | Regional HA | Synchronous standby in a second failure domain |
| Read replicas | Cloud SQL read replicas | CloudNativePG replica instances (streaming) |
| Settings in section "Cloud SQL for PostgreSQL 17 settings" | Database flags via Terraform | `postgresql.parameters` in the cluster manifest, same values |
| Connection pooling (if needed) | Managed Connection Pooling | PgBouncer (CloudNativePG `Pooler` resource) |
| Partition jobs | Kubernetes CronJob on GKE | Same CronJob on Anthos |
| Schema migrations | Flyway or Liquibase | Same |
| Archive of old partitions | Parquet in GCS | Parquet in on-prem object storage (Dell ECS or another S3-compatible store) |
| Monitoring | Query Insights, Cloud Monitoring | `pg_stat_statements`, Prometheus exporter, Grafana |

**Portability rule:** use only vanilla PostgreSQL 17 plus `pg_trgm`, `btree_gin` and `pg_stat_statements` (optionally `pg_partman` and `pg_cron`). Do not depend on extensions or engines missing on one side, such as TimescaleDB, Citus, ParadeDB or AlloyDB's columnar engine. Cloud SQL's data cache is fine to use because it is transparent to the application; on-prem, give the database nodes local NVMe and enough RAM instead.

## Implementation plan

Elasticsearch is switched off only after measured results pass four gates; each gate is a go or no-go decision for the review board.

&#91;embedded content: implementation plan · 5 phases, 4 gates · durations to be set after query analysis\]

The Oracle-to-Cloud SQL migration and this work share phase 3: the processor is re-pointed to Postgres once, with a single write. Rollback until phase 5 completes is a feature-flag switch back to the Elasticsearch-backed API.

## Implementation specification: repository, conventions, database

The four implementation-specification sections let a developer or coding agent build the whole solution without further input. Where a fact was unknown, a default is fixed in **Assumptions and defaults** and exposed as configuration. Package names use `com.bank.booking`; rename to the organization's standard.

### Repository layout (Maven multi-module)

```text
booking-platform/
  pom.xml                     parent: Java 21, Spring Boot 4.x BOM, pinned DuckDB JDBC
  booking-db/                 Flyway SQL migrations + migration Job image
  booking-archive-common/     bucket function, object paths, manifest model, DuckDB bootstrap
  booking-query-api/          Spring Boot web service (Deployment)
  booking-archiver/           Spring Boot non-web service (CronJobs + summary worker Deployment)
  deploy/helm/                one chart per service; values-gcp.yaml, values-onprem-in.yaml, values-onprem-cn.yaml
  perf/                       k6 scripts and synthetic data generator
```

### Dependencies

| Dependency | Version rule | Used by |
| --- | --- | --- |
| Java | 21 LTS runtime (25 LTS when approved) | All |
| Spring Boot | Latest 4.x patch: Web MVC, Security OAuth2 resource server, Actuator, Validation and JDBC starters | API, archiver |
| `org.postgresql:postgresql` | 42.7.x or later | API, archiver |
| `org.duckdb:duckdb_jdbc` | Current DuckDB LTS release, exact version pinned in the parent pom | API, archiver |
| DuckDB extensions `httpfs`, `postgres_scanner` | Same version as the JDBC driver, `linux_amd64`, baked into images | API (`httpfs`), archiver (both) |
| AWS SDK for Java v2, `s3` module | Latest 2.x | Archiver only (list and delete objects) |
| Flyway with `flyway-database-postgresql` | Latest stable | booking-db |
| Caffeine, Micrometer Prometheus registry | Managed by Spring Boot | API |
| Testcontainers: `postgres:17`, MinIO | Latest | Tests |

The API and archiver use `NamedParameterJdbcTemplate` with a small hand-written SQL builder. No ORM.

### Fixed conventions

| Item | Value |
| --- | --- |
| Hot window | `HOT_DAYS = 150`; archiving moves whole calendar months |
| Retention | `RETENTION_YEARS = 7`, counted from `booking_date` |
| Region | One database and one bucket per region; `REGION` is `GLOBAL`, `IN` or `CN` |
| Booking partitions | Daily, named `booking_pYYYYMMDD` |
| Summary partitions | Yearly, named `account_daily_summary_yYYYY` |
| Bucket function | `bucket = account_id % bucket_count`; `account_id` is a positive bigint; `bucket_count` is a power of two stored per month in `archive.archive_month` |
| Sort order | API: `booking_date DESC, booking_id DESC`. Parquet files: `account_id, booking_date, booking_id` ascending |
| Money | `numeric(20,4)` in Postgres, `DECIMAL(20,4)` in Parquet, decimal string in JSON |
| Timestamps | `timestamptz`, stored and returned in UTC |
| Run ID | UUID per archive or recompact run, written without dashes in paths |
| Final object path | `{root}/booking/region={REGION}/month={YYYY-MM}/v={runId}/bucket={b}/a{attempt}_data_{i}.parquet` |
| Staging object path | `{root}/_staging/run={runId}/month={YYYY-MM}/day={YYYY-MM-DD}/attempt={n}/bucket={b}/data_{i}.parquet` |
| Parquet writer options | `COMPRESSION zstd`, `ROW_GROUP_SIZE 122880`, `FILE_SIZE_BYTES '512MB'` |
| Parquet columns | All `booking` columns except `search_text`; `attrs` stored as JSON text |

`booking-archive-common` owns the bucket function, path builders, Parquet column list and DuckDB bootstrap. Both services must call it rather than repeat the logic.

### Database migrations (booking-db)

| Migration | Contents |
| --- | --- |
| `V1__booking.sql` | Extensions `pg_trgm`, `btree_gin`; `booking` table as defined in the data model section; parent indexes |
| `V2__summary.sql` | `account_daily_summary` (yearly partitions), `summary_dirty`, dirty-marking trigger |
| `V3__archive.sql` | Schema `archive` with `archive_month`, `archive_task`, `archive_file`, `archive_override` |
| `V4__functions.sql` | `booking_correct(jsonb)`, `booking_delete(date, bigint, bigint)` |
| `V5__roles.sql` | Roles, grants and role-level settings |

```sql
-- V1: parent indexes (inherited by every daily partition)
CREATE INDEX ix_acct_date ON booking (account_id, booking_date DESC, booking_id DESC);
CREATE INDEX ix_ref       ON booking (reference);
CREATE INDEX ix_e2e       ON booking (end_to_end_id);
-- hot-only indexes are created per partition by the archiver (partitions mode), dropped at 150 days

-- V2: summary maintenance
CREATE TABLE summary_dirty (
  account_id   bigint NOT NULL,
  booking_date date   NOT NULL,
  PRIMARY KEY (account_id, booking_date)
);
CREATE FUNCTION mark_summary_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO summary_dirty VALUES (NEW.account_id, NEW.booking_date) ON CONFLICT DO NOTHING;
  RETURN NULL;
END $$;
CREATE TRIGGER trg_booking_summary AFTER INSERT OR UPDATE ON booking
  FOR EACH ROW EXECUTE FUNCTION mark_summary_dirty();

-- V3: archive schema
CREATE SCHEMA archive;

CREATE TABLE archive.archive_month (
  month           date        PRIMARY KEY,           -- first day of the month
  status          text        NOT NULL,              -- see Month lifecycle
  run_id          uuid        NOT NULL,
  bucket_count    int         NOT NULL,
  expected_rows   bigint,
  expected_amount numeric(28,4),
  actual_rows     bigint,
  actual_amount   numeric(28,4),
  frozen_at timestamptz, switched_at timestamptz, purged_at timestamptz, expired_at timestamptz,
  error           text,
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE archive.archive_task (
  task_id         bigserial   PRIMARY KEY,
  run_id          uuid        NOT NULL,
  month           date        NOT NULL,
  kind            text        NOT NULL,              -- EXPORT_DAY or COMPACT_BUCKET
  day             date,                              -- EXPORT_DAY only
  bucket          int,                               -- COMPACT_BUCKET only
  status          text        NOT NULL DEFAULT 'PENDING',  -- PENDING, RUNNING, DONE, FAILED
  attempts        int         NOT NULL DEFAULT 0,
  done_attempt    int,
  lease_until     timestamptz,
  worker          text,
  expected_rows   bigint,
  expected_amount numeric(28,4),
  error           text,
  UNIQUE NULLS NOT DISTINCT (run_id, kind, day, bucket)
);

CREATE TABLE archive.archive_file (
  path             text        PRIMARY KEY,
  month            date        NOT NULL,
  bucket           int         NOT NULL,
  run_id           uuid        NOT NULL,
  row_count        bigint      NOT NULL,
  bytes            bigint      NOT NULL,
  amount_sum       numeric(28,4) NOT NULL,
  min_account_id   bigint      NOT NULL,
  max_account_id   bigint      NOT NULL,
  min_booking_date date        NOT NULL,
  max_booking_date date        NOT NULL,
  active           boolean     NOT NULL DEFAULT false,
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_archive_file_lookup ON archive.archive_file (month, bucket) WHERE active;

CREATE TABLE archive.archive_override (
  override_id   bigserial   PRIMARY KEY,
  booking_date  date        NOT NULL,
  booking_id    bigint      NOT NULL,
  account_id    bigint      NOT NULL,
  op            text        NOT NULL,                -- UPSERT or DELETE
  row_image     jsonb,                               -- full booking row for UPSERT, keys = column names
  created_at    timestamptz NOT NULL DEFAULT now(),
  merged_run_id uuid                                 -- set when folded into Parquet
);
CREATE INDEX ix_override_pending ON archive.archive_override (account_id, booking_date)
  WHERE merged_run_id IS NULL;

-- V4: corrections from any service (hot and archived months)
CREATE FUNCTION booking_correct(p_row jsonb) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, archive AS $$
DECLARE
  r booking := jsonb_populate_record(NULL::booking, p_row);
  v_status text;
BEGIN
  SELECT status INTO v_status FROM archive.archive_month
   WHERE month = date_trunc('month', r.booking_date)::date FOR SHARE;
  IF v_status IN ('EXPORTING', 'COMPACTING', 'SWITCHED', 'PURGED') THEN
    INSERT INTO archive.archive_override (booking_date, booking_id, account_id, op, row_image)
    VALUES (r.booking_date, r.booking_id, r.account_id, 'UPSERT', p_row);
  ELSE
    UPDATE booking b SET
      business_key = r.business_key, value_date = r.value_date, booking_type = r.booking_type,
      status = r.status, dr_cr = r.dr_cr, amount = r.amount, currency = r.currency,
      counterparty_name = r.counterparty_name, reference = r.reference,
      end_to_end_id = r.end_to_end_id, narrative = r.narrative, attrs = r.attrs,
      source_version = r.source_version, updated_at = now()
     WHERE b.booking_date = r.booking_date AND b.booking_id = r.booking_id;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'booking % on % not found', r.booking_id, r.booking_date;
    END IF;
  END IF;
END $$;
-- booking_delete(p_date, p_booking_id, p_account_id): same status check;
-- inserts op = 'DELETE' (row_image NULL) for frozen or archived months, otherwise deletes from booking.

-- V5: roles
CREATE ROLE booking_api_ro     LOGIN;   -- Query API on replicas
CREATE ROLE booking_api_export LOGIN;   -- Query API CSV export on replicas
CREATE ROLE booking_archiver   LOGIN;   -- archiver on the primary; owns booking for partition DDL
CREATE ROLE booking_writer     LOGIN;   -- processor and other services
GRANT SELECT ON booking, account_daily_summary TO booking_api_ro, booking_api_export;
GRANT USAGE ON SCHEMA archive TO booking_api_ro, booking_api_export, booking_writer;
GRANT SELECT ON ALL TABLES IN SCHEMA archive TO booking_api_ro, booking_api_export;
GRANT INSERT, UPDATE, SELECT ON booking TO booking_writer;
GRANT EXECUTE ON FUNCTION booking_correct(jsonb), booking_delete(date, bigint, bigint) TO booking_writer;
ALTER TABLE booking OWNER TO booking_archiver;
ALTER FUNCTION booking_correct(jsonb) OWNER TO booking_archiver;
ALTER ROLE booking_api_ro SET statement_timeout = '2s';
ALTER ROLE booking_api_ro SET idle_in_transaction_session_timeout = '5s';
ALTER ROLE booking_api_ro SET jit = off;
ALTER ROLE booking_api_ro SET max_parallel_workers_per_gather = 0;
ALTER ROLE booking_api_export SET statement_timeout = '600s';
```

**Required change in the Booking Processor:** when a booking's month has an `archive.archive_month` status of `EXPORTING` or later (and not `ROLLED_BACK`), the processor writes it through `booking_correct()` instead of its normal upsert. The same applies to every service that changes existing bookings.

## Implementation specification: Booking Query API

The Booking Query API is a stateless Spring Boot service that serves all UI reads from both tiers. It writes nothing to booking data.

### Package layout

```text
com.bank.booking.query
  api/         SearchController, DetailController, SummaryController, ExportController,
               request/response records, ProblemDetailsHandler
  security/    SecurityConfig (JWT), EntitlementClient (HTTP), EntitlementService (Caffeine, 60 s)
  search/      SearchService (tier router), SearchRequestValidator, CursorCodec, PageAssembler
  hot/         HotSqlBuilder, HotRepository, DataSourceConfig (replica + primary pools), ReplicaLagMonitor
  cold/        DuckDbEngine, ColdSqlBuilder, ColdRepository, ManifestRepository, WindowPlanner, FileSelector
  overrides/   OverrideRepository, OverrideMerger
  summary/     SummaryRepository
  export/      CsvExportService
  config/      BookingProperties (@ConfigurationProperties("booking"))
```

### Configuration (`application.yml`)

```yaml
server:
  port: 8080
  shutdown: graceful
spring:
  threads.virtual.enabled: true
  security.oauth2.resourceserver.jwt.issuer-uri: ${OIDC_ISSUER_URI}
management.endpoints.web.exposure.include: health,prometheus
booking:
  region: ${REGION}                      # GLOBAL | IN | CN
  hot:
    replica-url: ${PG_REPLICA_URL}       # jdbc:postgresql://host:5432/booking?sslmode=verify-full
    primary-url: ${PG_PRIMARY_URL}
    username: ${PG_USER}                 # role booking_api_ro
    password: ${PG_PASSWORD}
    export-username: ${PG_EXPORT_USER}   # role booking_api_export
    export-password: ${PG_EXPORT_PASSWORD}
    replica-pool-size: 30
    primary-pool-size: 10
    max-replica-lag: 1s
    max-accounts: 500
  cold:
    enabled: true
    max-accounts: 50
    max-concurrent-queries: 4            # per pod
    queue-timeout: 2s
    time-budget: 15s                     # per request, all cold queries together
    max-files-per-query: 400
    filter-safety-factor: 5
    text-search-max-months: 3
    manifest-cache-ttl: 5m
    tier-boundary-cache-ttl: 60s
    duckdb:
      threads: 4
      memory-limit: 4GB
      temp-directory: /tmp/duckdb
      extension-directory: /opt/duckdb/extensions
  storage:
    provider: ${STORAGE_PROVIDER}        # gcs | s3
    root: ${STORAGE_ROOT}                # gs://bookings-archive-global  or  s3://bookings-archive-in
    endpoint: ${S3_ENDPOINT:}            # s3 only, e.g. ecs.bank.local:9021
    url-style: ${S3_URL_STYLE:path}      # path for Dell ECS
    use-ssl: true
    region: ${S3_REGION:us-east-1}
    key-id: ${STORAGE_KEY_ID}            # GCS HMAC access ID, or ECS object-user access key
    secret: ${STORAGE_SECRET}
    ca-cert-file: ${STORAGE_CA_FILE:}    # internal CA bundle for ECS, if any
  search:
    max-range-days: 400
    default-page-size: 50
    max-page-size: 200
  export:
    max-range-years: 7
    max-concurrent: 2                    # per pod
    fetch-size: 5000
  cursor:
    hmac-key: ${CURSOR_HMAC_KEY}         # 32+ random bytes, base64
  entitlement:
    base-url: ${ENTITLEMENT_URL}
    timeout: 500ms
```

### Endpoints

| Method and path | Input | Output |
| --- | --- | --- |
| `POST /api/v1/bookings/search` | Search request (below) | `{items, page, meta}` |
| `GET /api/v1/accounts/{accountId}/bookings/{bookingDate}/{bookingId}` | Path only | One booking, or 404 |
| `GET /api/v1/accounts/{accountId}/bookings/by-reference` | `reference` or `endToEndId`, `fromDate`, `toDate` (range ≤ 400 days) | `{items}`, at most 100 |
| `GET /api/v1/accounts/{accountId}/daily-summary` | `fromDate`, `toDate` (≤ 7 years), optional `currency` | `{items: [{bookingDate, currency, dcIndicator, status, count, amount}]}` |
| `GET /api/v1/bookings/export.csv` | `accountIds` (comma list, ≤ 50), `fromDate`, `toDate` (≤ 7 years), same filters as search | Streamed `text/csv`, header row, newest first |
| `GET /actuator/health/liveness`, `/readiness`, `/actuator/prometheus` | — | Probes and metrics |

Search request fields:

| Field | Type | Required | Rule |
| --- | --- | --- | --- |
| `accountIds` | array of int64 | Yes | 1–500; at most 50 when the range reaches archived months; every ID must be entitled |
| `fromDate`, `toDate` | ISO date | Yes | `fromDate ≤ toDate ≤ today`; at most 400 days apart; `fromDate ≥ today − 7 years` |
| `filters.status` | array of string | No | Exact match |
| `filters.dcIndicator` | `D` or `C` | No |  |
| `filters.currency` | array of ISO 4217 codes | No |  |
| `filters.bookingType` | array: `INTRADAY`, `FINAL` | No |  |
| `filters.amountMin`, `filters.amountMax` | decimal string | No | Inclusive |
| `filters.text` | string, 3–100 characters | No | Older than 150 days: one account and at most 3 months |
| `includeSuperseded` | boolean | No | Default `false` (rows with status `SUPERSEDED` hidden) |
| `pageSize` | integer | No | Default 50, maximum 200 |
| `cursor` | string | No | From the previous response of the same request |

Each item has: `bookingDate`, `bookingId`, `accountId`, `valueDate`, `bookingType`, `status`, `dcIndicator`, `amount`, `currency`, `counterpartyName`, `reference`, `endToEndId`, `narrative`, `attrs` (JSON object) and `tier` (`HOT` or `COLD`). `page` has `pageSize`, `hasMore`, `partial`, `nextCursor`; `meta` has `tiers` and `elapsedMs`. See the example in the developer summary.

Errors use RFC 7807 problem details with a `code` field:

| HTTP | `code` | When |
| --- | --- | --- |
| 400 | `VALIDATION_FAILED` | Missing or malformed field |
| 400 | `RANGE_TOO_LARGE` | Range over 400 days (search) or 7 years (export, summary) |
| 400 | `TOO_MANY_ACCOUNTS` | Over 500 accounts, or over 50 when archived months are in range |
| 400 | `TEXT_SEARCH_NOT_ALLOWED` | Text filter breaks the older-than-150-days rule |
| 400 | `INVALID_CURSOR` | Bad signature, or cursor from a different request |
| 403 | `NOT_ENTITLED` | Any account not entitled to the caller |
| 404 | `NOT_FOUND` | Detail lookup found nothing |
| 429 | `COLD_BUSY` | No cold-query slot within `queue-timeout`; sends `Retry-After: 2` |
| 429 | `EXPORT_BUSY` | Export slots full |
| 503 | `ENTITLEMENT_UNAVAILABLE` | Entitlement service failed |
| 504 | `QUERY_TIMEOUT` | Hot statement timeout, or cold budget used before any row was found |

### Search algorithm

1. Validate the request and check entitlements. A cursor is decoded and verified; its `(d, id)` becomes the upper bound, otherwise the bound is `(toDate, +∞)`.
2. Read the tier boundary `hotFrom`: the day after the newest month whose status is `SWITCHED` or `PURGED` (cached 60 s). With no such month, everything is hot.
3. Load pending overrides for the accounts and `[fromDate, bound date]` once. Let `D` be the number of `DELETE` overrides. Set `limit = pageSize + 1 + D`.
4. **Hot part** (bound date ≥ `hotFrom`): query `[max(fromDate, hotFrom), bound]` with `limit`. One account uses the single-account query; several accounts use the LATERAL query from the Booking Query API section.
5. **Cold part** (fewer than `limit` rows so far and `fromDate < hotFrom`): run the window planner on `[fromDate, min(bound, hotFrom − 1 day)]`, select files, run DuckDB for the missing rows. Repeat with the next older window until enough rows, the range is exhausted, or `time-budget` is used up (then `partial = true`).
6. Merge overrides: drop every collected row that has an override; add `UPSERT` override rows that match the filters and lie inside the range and below the bound; sort `booking_date DESC, booking_id DESC`.
7. Return the first `pageSize` rows. `hasMore` is true when more rows than `pageSize` were collected or `partial` is true. `nextCursor` is built from the last returned row, or null.

Detail, by-reference and export use the same tier split and override merge, without paging (export streams tier by tier, newest first).

### Filter mapping (both tiers)

```sql
-- default (unless includeSuperseded)   status <> 'SUPERSEDED'
-- status                                status IN (:status...)
-- dcIndicator                           dr_cr = :dc
-- currency                              currency IN (:ccy...)
-- bookingType                           booking_type IN (:bt...)
-- amountMin / amountMax                 amount >= :min / amount <= :max
-- cursor                                (booking_date < :cd OR (booking_date = :cd AND booking_id < :ci))
-- text, hot tier (escape % and _ in the input):
--   search_text LIKE '%' || lower(:t) || '%'
-- text, cold tier:
--   lower(coalesce(reference,'') || ' ' || coalesce(counterparty_name,'') || ' ' ||
--         coalesce(narrative,'')) LIKE '%' || lower(?) || '%'
```

Postgres uses `= ANY(:array)` for the list filters; DuckDB uses `IN (?, ?, ...)` with one placeholder per value.

### Hot tier details

- Two Hikari pools: replica (`booking_api_ro`) and primary (same role, used only as fallback). `ReplicaLagMonitor` runs `SELECT extract(epoch FROM now() - pg_last_xact_replay_timestamp())` on the replica every 5 s; above `max-replica-lag` the hot repository switches to the primary until lag recovers.
- Export uses a third small pool (2 connections) with role `booking_api_export`, `fetchSize = 5000` and auto-commit off, so rows stream instead of loading into memory.

### Cold tier details

**DuckDB engine.** One in-memory DuckDB per pod, opened at startup. Each query runs on a duplicated connection and is cancelled at the remaining time budget.

```java
// startup
Connection base = DriverManager.getConnection("jdbc:duckdb:");
DuckDbBootstrap.apply(base, props);            // from booking-archive-common

// per query
if (!coldSlots.tryAcquire(props.queueTimeout())) throw new ColdBusyException();
try (Connection c = ((DuckDBConnection) base).duplicate();
     PreparedStatement ps = c.prepareStatement(sql)) {
  ps.setQueryTimeout(remainingSeconds);
  bind(ps, params);
  try (ResultSet rs = ps.executeQuery()) { /* map rows */ }
} finally {
  coldSlots.release();
}
```

**Bootstrap statements** (`DuckDbBootstrap`, used by the API and archiver):

```sql
SET extension_directory = '/opt/duckdb/extensions';
SET autoinstall_known_extensions = false;
SET autoload_known_extensions = false;
LOAD httpfs;
SET threads = 4;
SET memory_limit = '4GB';
SET temp_directory = '/tmp/duckdb';
SET preserve_insertion_order = true;
SET enable_http_metadata_cache = true;
SET parquet_metadata_cache = true;
-- provider = gcs (S3 interoperability, HMAC key):
CREATE SECRET cold_store (TYPE gcs, KEY_ID '<key-id>', SECRET '<secret>');
-- provider = s3 (Dell ECS or any S3-compatible store):
CREATE SECRET cold_store (TYPE s3, KEY_ID '<key-id>', SECRET '<secret>',
                          ENDPOINT 'ecs.bank.local:9021', URL_STYLE 'path', USE_SSL true, REGION 'us-east-1');
SET ca_cert_file = '/etc/ssl/bank/ca.pem';     -- only when STORAGE_CA_FILE is set
SET lock_configuration = true;                 -- queries cannot change settings afterwards
```

Check every setting name against the pinned DuckDB version at build time; a failing `SET` must stop the pod from becoming ready.

**Window planner.** Decides which days to read so one page is filled with as little I/O as possible.

1. Query `account_daily_summary` for the accounts and the cold range, grouped by day, newest first. Apply the summary's own dimensions when filtered (`currency`, `dr_cr`, `status`) and exclude `SUPERSEDED` by default.
2. No rows at all: the cold part is empty; skip DuckDB.
3. `target = need × filter-safety-factor` when `amount`, `text` or `bookingType` filters are present, otherwise `target = need`.
4. Walk days from the newest and add the counts; the day where the sum reaches `target` starts the window. If it is never reached, the window starts at the cold range start.
5. If DuckDB returns fewer rows than needed, the next window starts the day before the previous one, with `target` doubled.

**File selector.** For each month in the window and each account: `bucket = Buckets.of(accountId, month.bucketCount)`. Read active files for `(month, bucket)` from `archive.archive_file` (cached 5 min), keep those with `min_account_id ≤ accountId ≤ max_account_id` and an overlapping date range, then de-duplicate. Over `max-files-per-query` files: split the accounts into groups and query the groups one after another.

**Cold SQL.** File paths come only from the manifest and are inlined as a DuckDB list literal (single quotes doubled). Every value is a bound parameter.

```sql
SELECT booking_date, booking_id, account_id, value_date, booking_type, status, dr_cr,
       amount, currency, counterparty_name, reference, end_to_end_id, narrative, attrs
  FROM read_parquet(['<path 1>', '<path 2>'])
 WHERE account_id IN (?, ?)
   AND booking_date BETWEEN ? AND ?
   AND <filters from the mapping above>
   AND (booking_date < ? OR (booking_date = ? AND booking_id < ?))
 ORDER BY booking_date DESC, booking_id DESC
 LIMIT ?;
```

### Cursor

```text
payload = {"v":1,"d":"2026-03-14","id":88123,"rh":"<first 16 hex chars of SHA-256 of the canonical request without cursor and pageSize>"}
cursor  = base64url(payload) + "." + base64url(HMAC-SHA256(CURSOR_HMAC_KEY, payload))
```

A wrong signature or a different `rh` returns `400 INVALID_CURSOR`. The cursor carries no tier; the router decides by date on every call.

### Security, audit and observability

- JWT validated against `OIDC_ISSUER_URI`; the user is the `sub` claim.
- `EntitlementClient` calls `GET {ENTITLEMENT_URL}/users/{sub}/accounts` and expects `{"accountIds":[...]}`; results are cached 60 s per user.
- One JSON audit line per request: time, `sub`, endpoint, account count, date range, tiers used, rows returned, `elapsedMs`, `partial`.
- Metrics: `booking_request_seconds{endpoint,tier}` histogram, `booking_cold_files_per_query`, `booking_cold_busy_total`, `booking_replica_lag_seconds`, `booking_overrides_merged_total`.
- Readiness requires: replica reachable, DuckDB bootstrap done, tier boundary loaded once.

### Resources per pod

| Item | Value |
| --- | --- |
| CPU | Request 2, limit 4 |
| Memory | Request 6 GiB, limit 8 GiB (JVM `-XX:MaxRAMPercentage=25`; DuckDB `memory_limit` 4 GB is native memory outside the heap) |
| `/tmp` | `emptyDir`, 20 GiB (DuckDB native library and spill) |
| Replicas | 3 minimum, HPA up to 12 at 60% CPU, PodDisruptionBudget `minAvailable: 2`, spread across zones |
| Filesystem | Read-only root, non-root user |

## Implementation specification: Booking Archiver

The Booking Archiver is one container image with several modes. It moves months from Postgres to Parquet in GCS or any S3-compatible store (Dell ECS), and maintains partitions and summaries. A month of several terabytes is split into day and bucket tasks that any number of pods can work on in parallel and resume after a failure.

### Modes

| Mode | Kubernetes object | Schedule | What it does |
| --- | --- | --- | --- |
| `partitions` | CronJob | Daily 00:30 | Creates daily partitions 7 days ahead; drops hot-only indexes at 150 days; creates next year's summary partition |
| `summary-worker` | Deployment, 1 replica | Loop every 30 s | Recomputes `account_daily_summary` for dirty account-days |
| `archive` | CronJob, `parallelism: 4` | Daily 01:30 | Plans the next eligible month, runs its tasks, validates and switches |
| `purge` | CronJob | Daily 03:30 | 7 days after a switch: drops the month's partitions and deletes staging and orphan objects |
| `recompact` | CronJob | Weekly, Sunday 02:00 | Folds pending overrides into new Parquet versions and refreshes affected summaries |
| `expire` | CronJob | Monthly, 1st, 04:00 | Deletes months older than 7 years |
| `rollback` | Manual Job, `--month=YYYY-MM` | On demand | Reverts a `SWITCHED` month to hot before it is purged |

All CronJobs use `concurrencyPolicy: Forbid`. Every mode is idempotent: re-running it continues where it stopped.

### Configuration

```yaml
archiver:
  mode: ${ARCHIVER_MODE}
  region: ${REGION}
  hot-days: 150
  retention-years: 7
  partitions-ahead-days: 7
  bucket-count: auto                     # or a fixed power of two, 64..4096
  target-bucket-parquet-bytes: 4GB       # used when bucket-count = auto
  workers-per-pod: 2
  task-lease: 60m
  max-attempts: 3
  freeze-settle: 60s
  purge-grace: 7d
  pg:
    primary-url: ${PG_PRIMARY_URL}       # JDBC: manifest, tasks, DDL (role booking_archiver)
    replica-host: ${PG_REPLICA_HOST}     # DuckDB ATTACH for bulk reads
    port: 5432
    database: booking
    username: ${PG_USER}
    password: ${PG_PASSWORD}
    sslmode: verify-full
  storage:                               # same block and meaning as in the Query API
    provider: ${STORAGE_PROVIDER}
    root: ${STORAGE_ROOT}
    endpoint: ${S3_ENDPOINT:}
    url-style: ${S3_URL_STYLE:path}
    region: ${S3_REGION:us-east-1}
    key-id: ${STORAGE_KEY_ID}
    secret: ${STORAGE_SECRET}
    ca-cert-file: ${STORAGE_CA_FILE:}
  duckdb:
    threads: 8
    memory-limit: 24GB
    temp-directory: /scratch/duckdb      # ephemeral SSD volume, 300 GiB
    extension-directory: /opt/duckdb/extensions
```

The archiver loads both `httpfs` and `postgres_scanner` in DuckDB. For listing and deleting objects it uses the AWS SDK v2 `S3Client` with endpoint override (`https://storage.googleapis.com` for GCS interoperability, the ECS endpoint on-prem), path-style access, the same HMAC or ECS keys, single-object deletes, and `requestChecksumCalculation(WHEN_REQUIRED)` and `responseChecksumValidation(WHEN_REQUIRED)` because newer default checksums break some S3-compatible stores.

### `archive` mode, step by step

**1. Plan (one pod).** Take `pg_advisory_lock(hashtext('booking-archiver-plan'))` on the primary. Find the oldest month with data whose last day is earlier than `current_date - hot-days`, and with every older month already `SWITCHED` or later. If it has no `archive_month` row, insert one with status `PLANNED`, a new `run_id` and its `bucket_count`:

```text
bucket_count = clamp(nextPowerOfTwo(ceil(month_heap_bytes / 5 / target-bucket-parquet-bytes)), 64, 4096)
month_heap_bytes = sum of pg_table_size() over the month's daily partitions
```

**2. Freeze.** `UPDATE archive.archive_month SET status = 'EXPORTING', frozen_at = now()`. This waits for in-flight `booking_correct()` calls holding `FOR SHARE` on the row; new calls now write overrides. Wait `freeze-settle` (60 s), read `pg_current_wal_lsn()` on the primary, and poll the replica until `pg_last_wal_replay_lsn()` has passed it.

**3. Expected totals** (replica):

```sql
SELECT (account_id % :bucketCount)::int AS bucket, count(*) AS n, sum(amount) AS amt
  FROM booking
 WHERE booking_date >= :monthStart AND booking_date < :nextMonthStart
 GROUP BY 1;
```

Store the totals on `archive_month` and create tasks: one `EXPORT_DAY` per day of the month, and one `COMPACT_BUCKET` per bucket returned above, carrying its expected `n` and `amt`. Release the lock.

**4. Claim tasks (every pod, `workers-per-pod` threads).**

```sql
UPDATE archive.archive_task
   SET status = 'RUNNING', attempts = attempts + 1,
       lease_until = now() + interval '60 minutes', worker = :pod
 WHERE task_id = (
       SELECT task_id FROM archive.archive_task
        WHERE run_id = :runId AND kind = :kind AND attempts < 3
          AND (status = 'PENDING' OR (status = 'RUNNING' AND lease_until < now()))
        ORDER BY task_id
        FOR UPDATE SKIP LOCKED
        LIMIT 1)
RETURNING *;
```

`EXPORT_DAY` tasks run first. The worker that finds every `EXPORT_DAY` task `DONE` sets the month to `COMPACTING` (`WHERE status = 'EXPORTING'`, so only one succeeds); `COMPACT_BUCKET` tasks are claimable only then. A task failing 3 times sets the month to `FAILED` with the error.

**5. `EXPORT_DAY`** (DuckDB, reads one daily partition from the replica in parallel, writes one staging file set per bucket):

```sql
ATTACH 'host=<replica> port=5432 dbname=booking user=<user> password=<pw> sslmode=verify-full'
    AS pg (TYPE postgres, READ_ONLY);
COPY (
  SELECT booking_date, booking_id, business_key, account_id, value_date, booking_type, status, dr_cr,
         amount, currency, counterparty_name, reference, end_to_end_id, narrative,
         CAST(attrs AS VARCHAR) AS attrs, source_version, created_at, updated_at,
         CAST(account_id % {bucketCount} AS INTEGER) AS bucket
    FROM pg.public.booking_p20260501
) TO '{root}/_staging/run={runId}/month=2026-05/day=2026-05-01/attempt=1'
  (FORMAT parquet, PARTITION_BY (bucket), COMPRESSION zstd);
```

The attempt number in the path means a retried day never mixes with a failed attempt. On success set `done_attempt`.

**6. `COMPACT_BUCKET`** (sorts one bucket of the whole month, about 4 GB, spilling to `/scratch` if needed):

```sql
-- staging files for bucket 17; Java keeps only each day's done_attempt
SELECT file FROM glob('{root}/_staging/run={runId}/month=2026-05/day=*/attempt=*/bucket=17/*.parquet');

COPY (
  SELECT booking_date, booking_id, business_key, account_id, value_date, booking_type, status, dr_cr,
         amount, currency, counterparty_name, reference, end_to_end_id, narrative, attrs,
         source_version, created_at, updated_at
    FROM read_parquet([<kept staging files>])
   ORDER BY account_id, booking_date, booking_id
) TO '{root}/booking/region=IN/month=2026-05/v={runId}/bucket=17'
  (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 122880, FILE_SIZE_BYTES '512MB',
   FILENAME_PATTERN 'a{attempt}_data_{i}', RETURN_STATS true);
```

`RETURN_STATS` gives each written file's name and size. Then validate the bucket in one scan:

```sql
SELECT filename, count(*) AS n, sum(amount) AS amt,
       min(account_id), max(account_id), min(booking_date), max(booking_date)
  FROM read_parquet([<written files>], filename = true)
 GROUP BY filename;
```

The totals must equal the task's expected `n` and `amt`. On a match, insert one `archive.archive_file` row per file (`active = false`) and mark the task `DONE`. On a mismatch, fail the task; the retry writes with the next attempt prefix and the failed files are never in the manifest.

**7. Switch.** When every `COMPACT_BUCKET` task is `DONE`, one transaction on the primary checks that the sums of `archive_file.row_count` and `amount_sum` for the run equal the month's expected totals, sets those files `active = true`, and sets the month `SWITCHED` with `switched_at` and the actual totals. The API starts reading the month from Parquet within its 60 s boundary cache.

Pods exit with status 0 when the run has no claimable task left and the month is `SWITCHED` or `FAILED`.

### `purge` mode

For each month `SWITCHED` more than `purge-grace` ago:

1. For each day, in auto-commit: `ALTER TABLE booking DETACH PARTITION booking_pYYYYMMDD CONCURRENTLY;` then `DROP TABLE booking_pYYYYMMDD;` (skip when already gone).
2. Delete objects under `_staging/run={runId}/`.
3. Delete objects under the month's `v=` prefixes that are not active in the manifest (failed attempts and versions replaced by `recompact`). Skip objects still under a retention lock; they are retried on the next run.
4. Delete `summary_dirty` rows for the month and set the month `PURGED`.

### `partitions` mode

```sql
CREATE TABLE IF NOT EXISTS booking_p20261126 PARTITION OF booking
  FOR VALUES FROM ('2026-11-26') TO ('2026-11-27') WITH (fillfactor = 85);
CREATE INDEX IF NOT EXISTS booking_p20261126_trgm ON booking_p20261126 USING gin (search_text gin_trgm_ops);
CREATE INDEX IF NOT EXISTS booking_p20261126_amt  ON booking_p20261126 (account_id, amount);
ALTER TABLE booking_p20261126 SET (autovacuum_vacuum_scale_factor = 0.01,
                                    autovacuum_vacuum_insert_scale_factor = 0.05);
```

- For each day from today to today + 7: create as above.
- Partitions older than 3 days: `ALTER TABLE ... RESET (autovacuum_vacuum_scale_factor, autovacuum_vacuum_insert_scale_factor)`.
- Partitions 150 days old or older that still have them: `DROP INDEX CONCURRENTLY IF EXISTS booking_pYYYYMMDD_trgm` and `..._amt`.
- From 1 November: create `account_daily_summary_y<next year>` for the next year.

### `summary-worker` mode

Every 30 s, repeat until fewer than 5,000 keys come back, each loop in one transaction:

```sql
DELETE FROM summary_dirty
 WHERE (account_id, booking_date) IN (
       SELECT account_id, booking_date FROM summary_dirty
        ORDER BY booking_date DESC LIMIT 5000 FOR UPDATE SKIP LOCKED)
RETURNING account_id, booking_date;                  -- keys K, held in Java

-- drop keys whose month is EXPORTING or later (recompact maintains those), then:
DELETE FROM account_daily_summary s
 USING unnest(:accts::bigint[], :dates::date[]) AS k(account_id, booking_date)
 WHERE s.account_id = k.account_id AND s.booking_date = k.booking_date;

INSERT INTO account_daily_summary
       (account_id, booking_date, currency, dr_cr, status, txn_count, amount_sum, refreshed_at)
SELECT b.account_id, b.booking_date, b.currency, b.dr_cr, b.status, count(*), sum(b.amount), now()
  FROM booking b
  JOIN unnest(:accts::bigint[], :dates::date[]) AS k(account_id, booking_date)
    ON b.account_id = k.account_id AND b.booking_date = k.booking_date
 GROUP BY 1, 2, 3, 4, 5;
```

### `recompact` mode

For each `(month, bucket)` of a `SWITCHED` or `PURGED` month that has unmerged overrides, with a new run ID `r2`:

1. Note the highest pending `override_id` (`M`) for that month and bucket.
2. In DuckDB, load the latest override per booking up to `M` from the primary as typed columns: `postgres_query('pg', 'SELECT o.op, r.* FROM archive.archive_override o, jsonb_populate_record(NULL::booking, o.row_image) r WHERE ...')`. Delete overrides have no row image; for them only the key columns are used.
3. Write the merged bucket:

```sql
COPY (
  WITH ov AS (SELECT * FROM latest_overrides),
  base AS (
    SELECT f.* FROM read_parquet([<active files of the bucket>]) f
    ANTI JOIN ov ON f.booking_date = ov.booking_date AND f.booking_id = ov.booking_id)
  SELECT <parquet columns> FROM base
  UNION ALL
  SELECT <parquet columns> FROM ov WHERE op = 'UPSERT'
  ORDER BY account_id, booking_date, booking_id
) TO '{root}/booking/region=IN/month=2026-05/v={r2}/bucket=17'
  (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 122880, FILE_SIZE_BYTES '512MB',
   FILENAME_PATTERN 'a1_data_{i}', RETURN_STATS true);
```

4. Validate: written rows must equal old rows minus base rows matched by an override plus `UPSERT` overrides (all counted in DuckDB before writing).
5. One transaction: old files `active = false`, new file rows inserted `active = true`, overrides up to `M` for that bucket get `merged_run_id = r2`.
6. Recompute `account_daily_summary` for the touched account-days from the new files (DuckDB aggregate, JDBC batch delete and insert).

### `expire` mode

For months earlier than `date_trunc('month', current_date - interval '7 years')` with status `PURGED`: delete every object under `booking/region={REGION}/month={YYYY-MM}/` (skipping objects still under a retention lock), delete the month's `archive_file` rows and overrides, and set it `EXPIRED`. Drop `account_daily_summary_yYYYY` once the whole year is older than 7 years.

### `rollback` mode

Allowed only for a `SWITCHED` month (partitions still exist). In one transaction: set the month's files `active = false` and the month `ROLLED_BACK`; apply its pending overrides to `booking` (UPSERT updates or inserts the row image, DELETE deletes) and mark them merged. A later archive run for that month needs the flag `--rearchive`.

### Kubernetes job definition (archive mode)

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: booking-archiver-archive
spec:
  schedule: "30 1 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      parallelism: 4
      completions: 4
      backoffLimit: 6
      activeDeadlineSeconds: 172800
      template:
        spec:
          restartPolicy: OnFailure
          serviceAccountName: booking-archiver
          containers:
            - name: archiver
              image: <registry>/booking-archiver:<version>
              args: ["--archiver.mode=archive"]
              env:
                - { name: JAVA_TOOL_OPTIONS, value: "-XX:MaxRAMPercentage=15" }
              envFrom:
                - secretRef: { name: booking-archiver-secrets }
                - configMapRef: { name: booking-archiver-config }
              resources:
                requests: { cpu: "8", memory: 32Gi }
                limits:   { cpu: "8", memory: 32Gi }
              volumeMounts:
                - { name: scratch, mountPath: /scratch }
                - { name: tmp, mountPath: /tmp }
          volumes:
            - name: scratch
              ephemeral:
                volumeClaimTemplate:
                  spec:
                    accessModes: [ReadWriteOnce]
                    storageClassName: <ssd-storage-class>
                    resources: { requests: { storage: 300Gi } }
            - name: tmp
              emptyDir: { sizeLimit: 5Gi }
```

The other CronJobs use the same template with `parallelism: 1`, 2 CPU, 4 GiB memory, no scratch volume, and their own `--archiver.mode`.

## Implementation specification: deployment, tests, acceptance

### Container images

```dockerfile
# same pattern for booking-query-api and booking-archiver
FROM <approved-jre-21-base-image>
ARG DUCKDB_VERSION
# CI downloads the extensions for this exact DuckDB version from the internal artifact mirror:
#   v${DUCKDB_VERSION}/linux_amd64/httpfs.duckdb_extension
#   v${DUCKDB_VERSION}/linux_amd64/postgres_scanner.duckdb_extension   (archiver only)
COPY build/duckdb-extensions/ /opt/duckdb/extensions/
COPY target/app.jar /app/app.jar
USER 10001
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
```

- Images are pushed to the internal registry for each region, including China; nothing is pulled from the internet at runtime.
- `booking-db` ships as a Flyway image run as a Kubernetes Job before each release.

### Kubernetes objects

| Service | Objects |
| --- | --- |
| booking-query-api | Deployment, Service, HPA (3–12 pods, 60% CPU), PodDisruptionBudget, NetworkPolicy, ServiceAccount, ConfigMap, Secret |
| booking-archiver | 6 CronJobs (`partitions`, `archive`, `purge`, `recompact`, `expire` and a suspended `rollback` template), Deployment for `summary-worker`, ServiceAccount, ConfigMap, Secret |
| booking-db | Job (Flyway migrate) |

NetworkPolicy egress for both services: Postgres (primary and replicas), the object-storage endpoint, the OIDC issuer and the entitlement service (Query API only). Secrets come from the platform's secret manager: Postgres passwords, storage keys, `CURSOR_HMAC_KEY`.

### Values per environment

| Setting | GCP | On-prem India | On-prem China |
| --- | --- | --- | --- |
| `REGION` | `GLOBAL` | `IN` | `CN` |
| Postgres | Cloud SQL private IP (primary and replicas) | CloudNativePG services `-rw` and `-ro` | CloudNativePG services `-rw` and `-ro` |
| `STORAGE_PROVIDER` | `gcs` | `s3` | `s3` |
| `STORAGE_ROOT` | `gs://bookings-archive-global` | `s3://bookings-archive-in` | `s3://bookings-archive-cn` |
| `S3_ENDPOINT` | Not used | Dell ECS endpoint (India) | Dell ECS endpoint (China) |
| `STORAGE_CA_FILE` | Not used | Bank internal CA bundle | Bank internal CA bundle |
| Bucket names | Illustrative; set by the platform team | Same | Same |

### Tests

| Level | What | Tools |
| --- | --- | --- |
| Unit | Bucket function, path builders, cursor codec (tampering rejected), request validator, window planner, file selector, override merger, both SQL builders (snapshot test per filter combination) | JUnit 5 |
| Integration | Seed 8 months for 50 accounts; run `partitions`, then `archive` for the 2 oldest months; manifest totals equal Postgres totals | Testcontainers `postgres:17` + MinIO (stand-in for GCS interoperability and Dell ECS) |
| Parity | 50 random searches return identical items before and after the switch, across page boundaries | Same |
| Corrections | `booking_correct()` on a frozen month and an archived month shows on both tiers; after `recompact` the results are unchanged and overrides are merged | Same |
| Lifecycle | `purge` drops partitions; `rollback` restores hot reads; `expire` removes a month | Same |
| Failure | Kill an archiver pod mid-task; the lease expires and another pod finishes; totals still match | Same |
| Performance | Production-like volumes in a staging project (below) | k6, synthetic data generator in `perf/` |

### Performance acceptance targets

| Test | Data | Target |
| --- | --- | --- |
| Hot, one account, one page | 150 days at production volume | p95 ≤ 50 ms at peak request rate |
| Hot, 50 accounts, one page | Same | p95 ≤ 100 ms |
| Cold, one account, one page | One month of at least 1 TB Parquet | p95 ≤ 3 s |
| Cold, 50 accounts, one page | Same | p95 ≤ 8 s |
| Archive one 1 TB month | 4 archiver pods | Completes within 24 h with matching totals |
| Summary freshness | Peak ingestion | Dirty to refreshed ≤ 60 s |

The hot and cold figures are design targets; the PoC confirms or resets them.

### Acceptance criteria

- [ ] Every endpoint in the contract works for hot-only, cold-only and mixed ranges.
- [ ] Search results across tiers are identical to a Postgres-only baseline (parity test).
- [ ] Validation, entitlement and error codes behave as specified.
- [ ] One archived month of at least 1 TB meets the cold targets.
- [ ] An archive run survives pod failures and resumes without duplicates.
- [ ] Corrections to archived months are visible immediately and survive recompaction.
- [ ] The same images and charts run in GCP (GCS) and on-prem (Dell ECS) with only values changed.
- [ ] No DuckDB extension or package is downloaded at runtime.

### Work breakdown, in build order

- [ ] 1\. `booking-db`: migrations V1–V5, migration Job image.
- [ ] 2\. `booking-archive-common`: bucket function, paths, Parquet column list, DuckDB bootstrap, storage properties.
- [ ] 3\. Archiver `partitions` and `summary-worker` modes.
- [ ] 4\. Query API hot path: validation, entitlements, hot SQL builder, replica routing, cursor, detail, by-reference, summary.
- [ ] 5\. Archiver `archive` mode: planning, freeze, tasks, export, compaction, validation, switch.
- [ ] 6\. Query API cold path: tier boundary, window planner, file selector, DuckDB engine, cold SQL builder.
- [ ] 7\. Corrections: `booking_correct()` and `booking_delete()`, processor change, override merge in the API, `recompact` mode.
- [ ] 8\. Archiver `purge`, `expire` and `rollback` modes.
- [ ] 9\. Streaming CSV export.
- [ ] 10\. Helm charts and the three environment value files.
- [ ] 11\. Integration, parity, failure and performance tests; acceptance sign-off.

## Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| A UI query shape has no matching index and is slow | Query-log analysis before build; allow-listed filters only; `statement_timeout` 2 s; index advisor and slow-query log |
| Intraday updates cause bloat and write amplification | `fillfactor` 85, no indexes on changing columns, aggressive per-partition autovacuum, batched upserts |
| Replica lag during end-of-day bursts | `wal_compression`, replica lag alert, API falls back to the primary above 1 s |
| Very large corporates (many accounts, millions of rows) | LATERAL per-account query, mandatory date range, no exact counts, summary tables |
| Free-text search across many accounts and 150 days is slower than Elasticsearch | Trigram index limited to hot partitions; require at least one account and a date range; if the business needs relevance-ranked search across all clients, add a slim search index fed by CDC (Debezium), never by dual write |
| Cloud SQL and on-prem behaviour drift apart | One Terraform/manifest source for settings, vanilla extensions only, same performance test suite run in both environments |
| Ingestion competes with UI reads on the primary | UI reads go to replicas; primary sized for end-of-day peak |

## Assumptions and defaults

The specification uses these defaults so building can start without waiting for answers. Each one is a configuration value or a single code location.

| Topic | Default used | Where to change it |
| --- | --- | --- |
| Volumes | Hot 150 days fits a 16 vCPU / 128 GB Enterprise Plus instance; a cold month can reach several TB | Instance size and `bucket-count` after the PoC |
| Peak load | Query API starts at 3 pods, scales to 12 | HPA values |
| `booking_date` | Never changes between intraday and final versions | Add a key-mapping table if it does |
| Identifiers | `account_id` positive bigint; `(booking_date, booking_id)` unique | Data model |
| UI contract | The v1 contract in this document; an adapter maps any existing contract | `api/` package |
| Free-text fields | Reference, counterparty name, narrative | `search_text` expression and cold text predicate |
| UI aggregations | Daily counts and sums by currency, debit/credit and status | `account_daily_summary` dimensions |
| Retention | 7 years from `booking_date` | `retention-years` |
| Java | 21 runtime; 25 when approved | Parent pom and base image |
| Regions | One database and one bucket per region: `GLOBAL`, `IN`, `CN` | Helm values |
| Entitlements | An HTTP service returns a user's account IDs | `EntitlementClient` |
| GCS access | HMAC keys allowed for two service accounts | Security approval (see Tiered storage) |

The only decision needed before build is the GCS HMAC-key exception; everything else can be adjusted after the PoC.

## Sources

- [Cloud SQL for PostgreSQL: configure database flags](https://docs.cloud.google.com/sql/docs/postgres/flags)
- [Cloud SQL editions](https://docs.cloud.google.com/sql/docs/postgres/editions-intro)
- [Cloud SQL data cache overview](https://docs.cloud.google.com/sql/docs/postgres/data-cache)
- [Cloud SQL Managed Connection Pooling](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)
- [Cloud SQL: manage read replicas](https://docs.cloud.google.com/sql/docs/postgres/replication/manage-replicas)
- [Managed Postgres examined: Google Cloud SQL for PostgreSQL](https://thebuild.com/blog/managed-postgres-examined-google-cloud-sql-for-postgresql/)
- [PostgreSQL 17 release notes](https://www.postgresql.org/docs/17/release-17.html)
- [PostgreSQL 17 press kit](https://www.postgresql.org/about/press/presskit17/)
- [Cloud Storage storage classes](https://docs.cloud.google.com/storage/docs/storage-classes)
- [DuckDB S3 API support (httpfs)](https://duckdb.org/docs/1.5/core_extensions/httpfs/s3api)
- [DuckDB COPY statement](https://www.duckdb.org/docs/current/sql/statements/copy.html)
- [DuckDB Parquet tips](https://duckdb.org/docs/current/data/parquet/tips.html)
- [Spring Boot 4.0.0 available now](https://spring.io/blog/2025/11/20/spring-boot-4-0-0-available-now/)
