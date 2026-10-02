# FeedRank

A social feed/timeline generation service built in Go — the canonical
"how does the Twitter feed work" system design question, actually built
and load-tested, not just described. Project 20 of a 15-week backend
roadmap.

Most answers to this interview question stop at "fan-out-on-write" or
"fan-out-on-read" as if picking one is the whole answer. FeedRank
implements the **hybrid** real systems actually use — fan-out-on-write
for normal accounts, fan-out-on-read for high-follower-count accounts —
and includes a load test that measures, not just asserts, why the hybrid
exists: a pure fan-out-on-write system's write latency scales with
follower count; the hybrid's doesn't.

See `docs/ARCHITECTURE.md` for the full design, including Redis
sorted-set timeline mechanics and why pagination has to be cursor-based,
not offset-based, under a live-writing feed.

Stack: Go, PostgreSQL, Redis, Docker. Depends on QueueLine (Project 1)
for asynchronous fan-out writes.