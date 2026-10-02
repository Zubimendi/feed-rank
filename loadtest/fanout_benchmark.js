/**
 * FeedRank Fan-out Benchmark — k6 load test
 *
 * What this measures:
 *   - p99 POST /posts latency when a celebrity posts (write path)
 *   - p99 GET  /feed  latency for a normal user (read path)
 *
 * Run in hybrid mode (default):
 *   k6 run --out json=loadtest/results/hybrid.json loadtest/fanout_benchmark.js
 *
 * Run in pure mode (for comparison):
 *   API + worker must be running with FANOUT_MODE=pure, then:
 *   k6 run --out json=loadtest/results/pure.json loadtest/fanout_benchmark.js
 *
 * Expected difference:
 *   Hybrid:  celebrity post p99 ≈ flat regardless of follower count
 *   Pure:    celebrity post p99 ≈ scales linearly with follower count
 *
 * Prerequisites: run `make seed` first to populate test data.
 */

import http from "k6/http";
import { check, sleep } from "k6";
import { Trend, Rate } from "k6/metrics";

// ── Config ────────────────────────────────────────────────────────────────────
const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";

// After running `make seed`, set this to the celebrity's user ID.
// The seed script logs it on completion.
const CELEBRITY_ID = parseInt(__ENV.CELEBRITY_ID || "501");

// A normal user ID to read the feed as.
const READER_USER_ID = parseInt(__ENV.READER_USER_ID || "1");

// ── Custom metrics ────────────────────────────────────────────────────────────
const celebPostLatency = new Trend("celeb_post_latency_ms", true);
const feedReadLatency  = new Trend("feed_read_latency_ms",  true);
const postErrorRate    = new Rate("post_error_rate");
const feedErrorRate    = new Rate("feed_error_rate");

// ── Test shape ────────────────────────────────────────────────────────────────
// Two scenarios run concurrently:
//   celebrity_posting: ramps to 20 VUs, each posting as the celebrity.
//   feed_reading:      ramps to 50 VUs, each reading a normal user's feed.
//
// This simulates the real-world hotspot: a celebrity posts frequently while
// many users are simultaneously reading their feeds.
export const options = {
  scenarios: {
    celebrity_posting: {
      executor: "ramping-vus",
      startVUs: 1,
      stages: [
        { duration: "30s", target: 10 },   // warm up
        { duration: "60s", target: 20 },   // sustained load
        { duration: "15s", target: 0 },    // ramp down
      ],
      exec: "celebrityPost",
      tags: { scenario: "celebrity_post" },
    },
    feed_reading: {
      executor: "ramping-vus",
      startVUs: 5,
      stages: [
        { duration: "30s", target: 20 },
        { duration: "60s", target: 50 },
        { duration: "15s", target: 0 },
      ],
      exec: "feedRead",
      tags: { scenario: "feed_read" },
    },
  },
  thresholds: {
    // In hybrid mode these should pass comfortably.
    // In pure mode, celeb_post_latency_ms p99 will likely breach the threshold
    // — that's the whole point of the comparison.
    "celeb_post_latency_ms": ["p(99)<500"],
    "feed_read_latency_ms":  ["p(99)<300"],
    "post_error_rate":       ["rate<0.01"],
    "feed_error_rate":       ["rate<0.01"],
  },
};

// ── Scenario: celebrity posts ─────────────────────────────────────────────────
export function celebrityPost() {
  const payload = JSON.stringify({
    author_id: CELEBRITY_ID,
    content:   `Load test post at ${Date.now()}`,
  });

  const res = http.post(`${BASE_URL}/posts`, payload, {
    headers: { "Content-Type": "application/json" },
    tags:    { name: "celebrity_post" },
  });

  const ok = check(res, {
    "post status 201": (r) => r.status === 201,
  });

  celebPostLatency.add(res.timings.duration);
  postErrorRate.add(!ok);

  sleep(0.1); // 100ms think time between posts
}

// ── Scenario: feed read ───────────────────────────────────────────────────────
export function feedRead() {
  const res = http.get(`${BASE_URL}/feed?user_id=${READER_USER_ID}&limit=20`, {
    tags: { name: "feed_read" },
  });

  const ok = check(res, {
    "feed status 200": (r) => r.status === 200,
    "feed has items":  (r) => {
      try {
        return JSON.parse(r.body).items !== undefined;
      } catch {
        return false;
      }
    },
  });

  feedReadLatency.add(res.timings.duration);
  feedErrorRate.add(!ok);

  sleep(0.2);
}
