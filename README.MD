# FeedRank

> **Project 20 of a 15-week backend roadmap** — the canonical "how does the Twitter feed work" system design question, actually built and load-tested.

A social feed/timeline service in Go that implements the **hybrid fan-out architecture** real platforms (Twitter, Instagram, Bluesky) use — and includes a load test that *measures*, not just describes, why the hybrid exists.

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![Postgres](https://img.shields.io/badge/Postgres-16-336791?logo=postgresql)](https://postgresql.org)
[![Redis](https://img.shields.io/badge/Redis-7-DC382D?logo=redis)](https://redis.io)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker)](https://docker.com)

---

## The problem most answers miss

Most responses to the "design a social feed" interview question stop at either:
- **Fan-out-on-write** — push post IDs to every follower's timeline on write.
- **Fan-out-on-read** — compute the feed at read time by merging all followed accounts.

Real systems use **both**, routed by account size:

| Account type | Write path | Read path |
|---|---|---|
| Normal (< threshold followers) | Push to every follower's Redis sorted set (cheap, bounded) | O(1) — precomputed |
| Celebrity (≥ threshold) | Write only to their own post list (O(1)) | Merge live on read (bounded by # celebrities followed) |

FeedRank implements this hybrid and **load-tests the difference**. The celebrity post p99 latency chart is the actual deliverable — not any single endpoint.

---

## Architecture

```
POST /posts (author: normal user)
    │
    ▼  (synchronous, O(1))
 Postgres INSERT posts
    │
    ▼  (async, via Asynq/Redis queue)
 Fan-out worker
    ├── author.is_celebrity = false → ZADD timeline:{followerId} for each follower
    └── author.is_celebrity = true  → ZADD celebrity_posts:{authorId} only

GET /feed?user_id=X
    ├── ZREVRANGEBYSCORE timeline:{X}          (precomputed, normal follows)
    ├── ZREVRANGEBYSCORE celebrity_posts:{C1}  (live fetch, celebrity 1)
    ├── ZREVRANGEBYSCORE celebrity_posts:{C2}  (live fetch, celebrity 2)
    └── merge-sort by score → paginate by cursor → hydrate from Postgres
```

See [`docs/ARCHITECTURE.MD`](docs/ARCHITECTURE.MD) for the full design rationale including:
- Why fan-out writes are async (§1)
- Why celebrity accounts never fan out (§2)
- Why Redis sorted sets (§3)
- Why cursor-based pagination (§4)
- The load-test methodology (§5)

---

## Stack

| Component | Technology | Role |
|---|---|---|
| API server | Go + chi | HTTP endpoints |
| Fan-out worker | Go + Asynq | Async job consumer |
| Queue | Asynq (Redis-backed) | Decouples post creation from fan-out |
| Timeline cache | Redis sorted sets | `timeline:{userId}`, `celebrity_posts:{userId}` |
| Durable store | PostgreSQL 16 | Users, follows, posts |
| Load test | k6 | Benchmarks hybrid vs pure |

---

## Quick start

**Prerequisites:** Docker, Docker Compose, `golang-migrate` CLI, Go 1.25+

```bash
# 1. Clone
git clone https://github.com/youruser/feed-rank && cd feed-rank

# 2. Bring up Postgres + Redis
make infra

# 3. Run migrations
make migrate

# 4. Start API + worker (native, hot-reload friendly)
DATABASE_URL=postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable \
REDIS_URL=localhost:6379 \
FANOUT_MODE=hybrid \
go run ./cmd/api &

DATABASE_URL=postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable \
REDIS_URL=localhost:6379 \
FANOUT_MODE=hybrid \
go run ./cmd/worker

# OR use Docker Compose for everything
make up
```

---

## API reference

| Method | Endpoint | Body / Query | Description |
|---|---|---|---|
| `GET` | `/health` | — | Service health check |
| `POST` | `/users` | `{"username": "alice"}` | Create a user |
| `GET` | `/users/:id` | — | Get user (includes `is_celebrity`, `follower_count`) |
| `POST` | `/follows` | `{"follower_id":1,"followee_id":2}` | Follow a user; triggers celebrity reclassification |
| `POST` | `/posts` | `{"author_id":1,"content":"..."}` | Create post; returns immediately, fan-out is async |
| `GET` | `/feed` | `?user_id=1&cursor=0&limit=20` | Paginated hybrid feed |

### Cursor pagination

Pass `cursor=0` for the first page. Each response includes `next_cursor` — pass that value as `cursor` on the next request. A `next_cursor` of `0` means no more pages.

Cursor = the UNIX millisecond score of the last returned item (exclusive upper bound on the next `ZREVRANGEBYSCORE` call). New writes above the cursor never affect pages already fetched below it — unlike offset pagination.

---

## Postman collection

Import [`postman/feedrank.postman_collection.json`](postman/feedrank.postman_collection.json) into Postman.

The collection:
1. Creates two users (Alice + Celebrity Bob)
2. Alice follows Celebrity Bob (triggers reclassification check)
3. Both create posts — see the different Redis paths in worker logs
4. Reads Alice's feed (hybrid merge) with cursor pagination
5. Each request has test assertions and explanatory comments

---

## Load test

### 1. Seed data

```bash
DATABASE_URL=postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable \
go run ./loadtest/seed/main.go
# Outputs: celebrity_id=N — set this below
```

### 2. Run benchmark (hybrid mode)

```bash
CELEBRITY_ID=N READER_USER_ID=1 \
k6 run --out json=loadtest/results/hybrid.json loadtest/fanout_benchmark.js
```

### 3. Switch to pure mode and re-run

```bash
# Restart worker with FANOUT_MODE=pure, then:
CELEBRITY_ID=N READER_USER_ID=1 \
k6 run --out json=loadtest/results/pure.json loadtest/fanout_benchmark.js
```

### Expected results

| Metric | Pure mode | Hybrid mode |
|---|---|---|
| `celeb_post_latency_ms` p99 | Scales with follower count (degrades badly) | Flat, O(1) |
| `feed_read_latency_ms` p99 | Fast (precomputed) | Slightly higher (celebrity merge step) |

The pure-mode write latency scaling directly with follower count is the point. The hybrid eliminates it at the cost of a bounded, small read-time merge — which is the correct tradeoff for platforms with even a handful of high-follower accounts.

---

## Key design decisions

| Decision | Why |
|---|---|
| Fan-out is async (Asynq) | Post-creation response is O(1); fan-out latency doesn't affect UX |
| `is_celebrity` boolean flag on users | Avoids re-reading `follower_count` on every fan-out job; updated atomically on follow |
| Redis sorted sets, not lists | O(log N) insert + O(log N + M) range; supports score-anchored cursor pagination |
| Cursor = last-seen score (UNIX ms) | Stable under concurrent writes; offset pagination shifts under a live feed |
| Timeline cap (`ZREMRANGEBYRANK`) | Bounded memory; Postgres fallback for deep scrollback |
| No reclassification backfill | Explicitly out of scope; old entries age out via normal trim |

---

## Project context

- Part of a 15-week backend portfolio demonstrating production-grade patterns in Go.
- Related projects: Dispatcher (webhook delivery), LedgerLine (ledger/accounting), FlagForge (feature flags).

## Tags

`#SystemDesign` `#GoLang` `#BackendEngineering` `#DistributedSystems` `#SoftwareArchitecture` `#Redis` `#PostgreSQL` `#LoadTesting` `#CareerDevelopment` `#TechPortfolio`