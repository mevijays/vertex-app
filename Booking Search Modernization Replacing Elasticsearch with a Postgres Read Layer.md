# Booking Search Modernization: Replacing Elasticsearch with a Postgres Read Layer

Nov 19, 2026 · @vijay vishwakarma

## Executive summary

**Decision proposed:** retire the Elasticsearch read store. The booking UI is served directly from PostgreSQL 17 (Cloud SQL in GCP, PostgreSQL on Anthos on-prem) through a new stateless **Booking Query API**. One database holds both the system of record and the 150-day UI read model.

- **Why it is fast enough:** almost every UI query is scoped to entitled accounts and a date range. Postgres answers those with composite B-tree index range scans in single-digit milliseconds. The Elasticsearch strengths we pay for (relevance-ranked full text over unscoped data) are not what the UI uses.
- **Why it is safer:** it removes the non-transactional dual write (Oracle + Elasticsearch) done by the processor and by every service that updates bookings, along with the drift and reconciliation work it causes.
- **Expected outcome:** p95 server-side latency at or below today's (target ≤ 50 ms), bookings visible on commit instead of after Elasticsearch's 1 s refresh, no Elasticsearch cluster to run or license, and one design that runs unchanged on Cloud SQL + GKE and on-prem Postgres + Anthos (China and India residency).
- **Before commitment:** a PoC that replays 2–4 weeks of real UI queries against both stacks. All latency figures in this document are engineering estimates until that PoC replaces them with measurements.

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

The processor and the other services write once, to the Postgres primary. Replicas serve the Booking Query API, and Elasticsearch, with every sync path into it, is gone.

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
- Heavy indexes exist only on partitions younger than 150 days. Postgres allows per-partition indexes, which reproduces Elasticsearch's "150 days searchable" at a fraction of the cost. The API refuses free-text search beyond 150 days.
- `account_id = ANY(:accounts)` benefits from the PostgreSQL 17 B-tree improvement for multi-value searches, relevant for corporate users entitled to many accounts.

### Partition lifecycle

| When | Action |
| --- | --- |
| Daily | Create partitions 7 days ahead with their indexes; set `fillfactor = 85` on new partitions so intraday updates can stay HOT |
| Partition turns 150 days old | `DROP INDEX CONCURRENTLY` on its trigram and amount indexes |
| Past the retention period | `DETACH PARTITION ... CONCURRENTLY`, export to Parquet in object storage, drop |

A small scheduled job (Kubernetes CronJob or a Spring `@Scheduled` task) runs this the same way in GCP and on-prem. `pg_partman` with `pg_cron` is an optional alternative where both are enabled.

### Summary table for dashboards

```sql
CREATE TABLE account_daily_summary (
  account_id   bigint, booking_date date, currency char(3),
  dr_cr        char(1), status text,
  txn_count    bigint, amount_sum numeric(24,4), refreshed_at timestamptz,
  PRIMARY KEY (account_id, booking_date, currency, dr_cr, status)
);
```

- The upsert transaction also inserts `(account_id, booking_date)` into a small `summary_dirty` table.
- An aggregator job recomputes only the dirty account-days every 30 s. This avoids lock contention on busy accounts that same-transaction counters would cause.
- Dashboard tiles read about 150 rows per account instead of aggregating millions, and show the `refreshed_at` time.

## Booking Query API

The Booking Query API is a stateless Spring Boot service that replaces Elasticsearch as the UI's backend. It keeps the UI's current search contract, so the UI does not change.

### Stack

| Layer | Choice | Reason |
| --- | --- | --- |
| Runtime | Java 21, or Java 25 LTS if approved | Same skills as the processor team; Java 25 removes virtual-thread pinning on `synchronized` |
| Framework | Spring Boot 3.x, Spring MVC, `spring.threads.virtual.enabled=true` | Many concurrent blocking JDBC calls without a large thread pool |
| SQL | jOOQ (or `NamedParameterJdbcTemplate`) | Builds only the SQL for filters actually present; no ORM overhead |
| Driver and pool | pgJDBC 42.6 or later, HikariCP | pgJDBC 42.6+ avoids virtual-thread pinning |
| Cache | Caffeine, reference data only (currencies, account metadata) | Bookings are never cached; Postgres is fast enough and always current |

### Endpoints (names illustrative; the existing contract is kept)

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/bookings/search` | List view: filters, sort, keyset cursor, 50 rows |
| `GET /v1/bookings/{bookingDate}/{bookingId}` | Booking detail |
| `GET /v1/bookings/by-reference/{ref}` | Exact lookup by reference or end-to-end ID |
| `GET /v1/accounts/summary` | Dashboard tiles from `account_daily_summary` |
| `POST /v1/exports` | Asynchronous CSV/PDF export, streamed with a server-side cursor |

### Query rules that keep it fast

- **Allow-listed filters only.** The UI sends filter JSON; the API maps each allowed field to a parameterized predicate. No raw SQL, and no filter without an index behind it.
- **Mandatory scope.** Every query carries the user's entitled accounts and a date range. Interactive range is at most 150 days; longer goes to async export.
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
| Archive of old partitions | Parquet in GCS | Parquet in on-prem object storage (MinIO or Ceph) |
| Monitoring | Query Insights, Cloud Monitoring | `pg_stat_statements`, Prometheus exporter, Grafana |

**Portability rule:** use only vanilla PostgreSQL 17 plus `pg_trgm`, `btree_gin` and `pg_stat_statements` (optionally `pg_partman` and `pg_cron`). Do not depend on extensions or engines missing on one side, such as TimescaleDB, Citus, ParadeDB or AlloyDB's columnar engine. Cloud SQL's data cache is fine to use because it is transparent to the application; on-prem, give the database nodes local NVMe and enough RAM instead.

## Implementation plan

Elasticsearch is switched off only after measured results pass four gates; each gate is a go or no-go decision for the review board.

&#91;embedded content: implementation plan · 5 phases, 4 gates · durations to be set after query analysis\]

The Oracle-to-Cloud SQL migration and this work share phase 3: the processor is re-pointed to Postgres once, with a single write. Rollback until phase 5 completes is a feature-flag switch back to the Elasticsearch-backed API.

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

## Open questions

- [ ] Bookings per day at peak, intraday and end-of-day, per region?
- [ ] Peak concurrent UI users and requests per second?
- [ ] Does the UI use free-text search today, and on which fields?
- [ ] Which aggregations and facet counts does the UI show?
- [ ] Can a booking's `booking_date` change between its intraday and final versions?
- [ ] Retention period for bookings in the system of record (beyond the 150-day UI window)?
- [ ] Is Java 25 LTS approved, or must the service stay on Java 21?

## Sources

- [Cloud SQL for PostgreSQL: configure database flags](https://docs.cloud.google.com/sql/docs/postgres/flags)
- [Cloud SQL editions](https://docs.cloud.google.com/sql/docs/postgres/editions-intro)
- [Cloud SQL data cache overview](https://docs.cloud.google.com/sql/docs/postgres/data-cache)
- [Cloud SQL Managed Connection Pooling](https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling)
- [Cloud SQL: manage read replicas](https://docs.cloud.google.com/sql/docs/postgres/replication/manage-replicas)
- [Managed Postgres examined: Google Cloud SQL for PostgreSQL](https://thebuild.com/blog/managed-postgres-examined-google-cloud-sql-for-postgresql/)
- [PostgreSQL 17 release notes](https://www.postgresql.org/docs/17/release-17.html)
- [PostgreSQL 17 press kit](https://www.postgresql.org/about/press/presskit17/)
