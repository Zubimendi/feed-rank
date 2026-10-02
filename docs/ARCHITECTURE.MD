markdown
# Architecture: principles → design

## System shape

normal user posts ──▶ write accepted immediately, returns fast
│
▼ (async, via QueueLine — Project 1, not reimplemented here)
fan-out-on-write: push postId onto EVERY follower's
Redis sorted set (ZADD timeline:{followerId} <timestamp> <postId>)

celebrity posts ──▶ written ONLY to their own post list.
Never pushed to followers' timelines.

feed read (any user) ──▶ merge:
(a) their own precomputed Redis sorted set (normal-account follows)
(b) a live fetch of each followed celebrity's recent posts
sorted by score, paginated by cursor


## 1. Fan-out-on-write for normal accounts

**The idea:** when a normal user posts, the system immediately pushes
that post's ID onto the Redis sorted set (`ZADD timeline:{followerId}
<timestamp> <postId>`) of every one of their followers. A follower's
feed read then becomes trivial and fast — `ZREVRANGE` on their own
precomputed set, no fan-in computation at read time.

**The cost this doesn't pay:** for a normal account with a bounded
follower count (hundreds, low thousands), fanning out on write is cheap
and keeps reads O(1). This is the right tradeoff when write amplification
is small relative to read volume — which is true for the overwhelming
majority of accounts on any real social platform.

**Where it's actually done:** fan-out writes happen **asynchronously**,
via QueueLine (Project 1) — the post-creation request itself returns the
instant the post row is persisted in Postgres; a background worker
consumes a "fan out this post" job and performs the (bounded) number of
`ZADD` calls. This is the same "don't make the request-response cycle
wait on non-urgent work" principle used throughout this portfolio
(Dispatcher's webhook delivery, FlagForge's reminder scheduling),
applied here to feed fan-out specifically.

## 2. Fan-out-on-read for celebrity accounts — the part almost nobody implements

**The problem a pure fan-out-on-write system has:** if a celebrity
account with 50 million followers posts, fan-out-on-write means pushing
that post to 50 million Redis sorted sets. Even done asynchronously and
batched, this is a genuinely enormous, slow, resource-intensive
operation for a single post — and it happens every time that account
posts, which for a very popular account can be frequent.

**The hybrid fix, which is what real systems actually do (not just the
textbook fan-out-on-write answer most interview responses stop at):**
accounts whose follower count crosses a configured threshold (e.g.
10,000) are classified as "celebrity" accounts. Their posts are **never**
fanned out to follower timelines at write time — they're simply appended
to that account's own post list (a Redis sorted set or a Postgres query,
scoped to just that one account, trivially cheap regardless of follower
count). At **read** time, when any user requests their feed, the read
path merges two sources: their own precomputed timeline (built from
fan-out-on-write, covering everyone they follow who isn't a celebrity)
and a live, on-demand fetch of recent posts from every celebrity they
follow (typically a small number per user, even if each celebrity has
millions of followers) — merged and sorted by score before returning.

**The tradeoff being made explicitly:** this moves cost from write time
(bounded by follower count, unbounded badly) to read time (bounded by
*how many celebrities a given user follows*, which is small per user
even at huge platform scale). This is the actual interview-differentiating
insight: not knowing fan-out-on-write exists, and not knowing fan-out-
on-read exists, but knowing **when each one is the wrong default** and
why real systems use both at once, routed by account size.

## 3. Redis sorted sets as the timeline data structure

**Where:** `ZADD`/`ZREVRANGE`/`ZREVRANGEBYSCORE` over a per-user sorted
set keyed by `timeline:{userId}`, scored by post timestamp. Chosen
specifically because a sorted set gives O(log N) insertion and O(log N +
M) range retrieval for M items — the two operations a feed actually
needs — without requiring a full re-sort on every read or every write.

**Bounded size, not infinite history:** each timeline is trimmed
(`ZREMRANGEBYRANK`) to a capped recent window (several hundred entries)
on every write — a feed's Redis representation is a bounded, fast cache
of *recent* activity, not the permanent historical record (Postgres
holds that). A user scrolling back far enough to exceed the cached
window falls back to a direct Postgres query — slower, correctly so,
since "infinite scrollback at feed speed" isn't a requirement most
products actually have, and pretending it is would mean paying
unbounded memory cost for a rarely-exercised case.

## 4. Cursor-based pagination, never offset-based

**The bug offset pagination has here specifically:** a sorted set that's
being actively written to (new posts arriving) shifts under an
offset-based pager — "give me items 20–40" means something different
between two requests if 5 new items were inserted at the top in between,
causing a real user to see duplicated or skipped posts while scrolling,
non-deterministically, depending on how much write activity happened
during their scroll session.

**The fix:** paginate using the **score of the last item seen** as an
exclusive cursor — `ZREVRANGEBYSCORE timeline:{userId} (<lastScore> -inf
LIMIT 0 pageSize`. Each page's request is anchored to a specific,
stable point in the sorted set's score space, not a shifting numeric
offset — new writes above the cursor never affect a page already being
fetched below it.

## 5. Load-testing the actual claim, not just asserting it

This project's point isn't complete without measuring the difference
the hybrid approach makes, not just describing it. The load test to
build: simulate a normal-distribution-of-follower-counts population,
then introduce one simulated "celebrity" account with a very large
follower count, and compare (a) p99 post-write latency and (b) p99
feed-read latency, between a pure fan-out-on-write implementation and
the hybrid one, as that celebrity account posts repeatedly. The pure
fan-out-on-write version should show write latency that scales directly
with follower count (and likely becomes unacceptable at the simulated
celebrity's scale); the hybrid version's write latency should stay flat
regardless of follower count, with only a small, bounded read-time cost
for users following celebrities. This comparison, actually run and
measured, is the single best artifact this project produces — "I built
both and measured the difference" is a dramatically stronger interview
answer than "I know fan-out-on-write and fan-out-on-read exist."

## What's out of scope, and why
- **Engagement-based ranking** (ML-scored "relevance" feeds rather than
  reverse-chronological). v1's score is purely recency — a real ranking
  model is a separate, much larger problem (feature engineering, a
  scoring service, A/B infrastructure) that would dilute this project's
  actual focus on the fan-out architecture.
- **Celebrity-threshold reclassification backfill.** When an account
  crosses the threshold, existing fanned-out entries in followers'
  timelines simply age out via the normal trim/cap; there's no
  retroactive cleanup job, which is a deliberate, low-cost simplification
  rather than a gap.