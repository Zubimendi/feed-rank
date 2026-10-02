# Title: Two Answers Aren't Enough — Why Real Feeds Use Both Fan-Out Strategies at Once

## Short version (LinkedIn)
Project 20: FeedRank, the canonical "how does the Twitter feed work"
interview question, actually built. Most answers pick one of two
textbook strategies — fan-out-on-write or fan-out-on-read — as if
choosing one is the whole answer. Real systems use both, routed by
account size: normal accounts fan out on write (cheap, bounded), accounts
with huge follower counts fan out on read instead, because pushing one
post to 50 million timelines synchronously is its own outage. I load-
tested both versions against a simulated celebrity account: the
pure-write version's post latency scales directly with follower count;
the hybrid's doesn't. Measuring that difference, not just describing it,
is the actual point. Repo: <your-fork-url>

## Conclusion
This is a project whose value is almost entirely in what it refuses to
simplify: a single fan-out strategy is a correct answer to a simpler
problem than the one real platforms actually have, and the hybrid
approach — plus cursor-based pagination, plus bounded timeline caching —
is what happens when you take "at scale" seriously instead of treating
it as a buzzword. Having load-tested the difference, not just read about
it, is what separates an answer from a demonstrated understanding.

## Interview talking points
1. Name both strategies and immediately explain why neither alone is
   the real answer.
2. Describe the hybrid's threshold-based routing and what it actually
   optimizes (bounded write cost vs. slightly more expensive reads for
   celebrity-followers).
3. Bring up the load test unprompted — it's the strongest evidence in
   this entire project.
4. Explain why offset pagination breaks under a live-writing sorted set,
   and what cursor-based pagination fixes.