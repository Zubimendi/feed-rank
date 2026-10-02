# Agent context: building FeedRank from scratch

Read `ARCHITECTURE.md` first.

## Step 0 — infra (set up yourself)
Standard Go layout, Postgres + Redis via Docker Compose, matching this
portfolio's established shape (see Dispatcher/LedgerLine for the
convention). QueueLine must be reachable for async fan-out.

## Step 1 — data model
- Postgres: `users` (with `follower_count`, used for celebrity
  classification), `follows` (follower_id, followee_id), `posts` (id,
  author_id, content, created_at) — the durable source of truth.
- Redis: `timeline:{userId}` sorted sets (fan-out-on-write targets),
  `celebrity_posts:{userId}` sorted sets (celebrity accounts' own posts,
  read directly rather than fanned out).

## Step 2 — build in this order
1. **Post creation** — write to Postgres, return immediately, enqueue a
   "fan out this post" job to QueueLine (don't fan out inline).
2. **Fan-out worker** — consumes the job; if the author is below the
   celebrity threshold, `ZADD` + `ZREMRANGEBYRANK` (trim) onto every
   follower's `timeline:{followerId}`. If at/above threshold, skip
   fan-out entirely — the post lives only in `celebrity_posts:{authorId}`.
3. **Feed read** — fetch the requesting user's own `timeline:{userId}`,
   fetch each followed celebrity's `celebrity_posts` (bounded by however
   many celebrities they follow, typically small), merge-sort by score,
   paginate via cursor (last-seen score), return.
4. **Celebrity threshold reclassification** — a scheduled or triggered
   check that flips `users.is_celebrity` when `follower_count` crosses
   the configured threshold; no backfill needed (see ARCHITECTURE.md's
   "what's out of scope").

## Step 3 — the load test that proves the claim (don't skip this)
Build two code paths (or two build flags): pure fan-out-on-write, and
the hybrid. Simulate a normal follower-count distribution plus one
celebrity account with a very large follower count. Script (k6 or
similar) that has the celebrity post repeatedly while measuring p99
write latency and p99 feed-read latency for both versions. Expect: pure
version's write latency scales with follower count (likely degrading
badly at the simulated celebrity's scale); hybrid's write latency stays
flat. This comparison is the actual deliverable of this project — more
important than any single endpoint's correctness.

## Design decisions already made
1. Threshold-based hybrid routing, not one strategy — see ARCHITECTURE.md
   §1–2.
2. Fan-out writes are async via QueueLine, never inline in the
   post-creation request path.
3. Cursor-based pagination only — never offset-based, for the reason in
   ARCHITECTURE.md §4.
4. Timelines are bounded/trimmed caches, not infinite history — Postgres
   is the fallback for deep scrollback.

make sure to explain the code in-depth, create postman json collection for easy testing, and update the readme.md when all builiding is done. add tags for the linkedin and medium writeups. generate a thumbnail image for the medium article. 