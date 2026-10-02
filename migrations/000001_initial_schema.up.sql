-- migrate: up

-- ─────────────────────────────────────────────────────────────────────────────
-- users
-- ─────────────────────────────────────────────────────────────────────────────
-- The primary account table. follower_count is denormalised here (incremented
-- on every INSERT into follows) so the fan-out worker can check celebrity
-- status with a single indexed column read rather than a COUNT(*) join.
--
-- is_celebrity is a derived flag set by the celebrity reclassification logic
-- (internal/celebrity/reclassify.go) whenever follower_count crosses
-- CELEBRITY_THRESHOLD. Storing it as a boolean rather than re-checking
-- follower_count in the hot fan-out path keeps the worker code simple and
-- avoids a second Postgres round-trip per fan-out job.
CREATE TABLE IF NOT EXISTS users (
    id             BIGSERIAL    PRIMARY KEY,
    username       TEXT         NOT NULL UNIQUE,
    follower_count INT          NOT NULL DEFAULT 0,
    is_celebrity   BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- follows
-- ─────────────────────────────────────────────────────────────────────────────
-- Directed edge: follower_id follows followee_id.
-- Composite PK prevents duplicate follows.
--
-- idx_follows_followee speeds up the fan-out worker's query:
--   "give me every follower of this author so I can ZADD their timelines"
-- Without it, that query is a full table scan — unacceptable at scale.
CREATE TABLE IF NOT EXISTS follows (
    follower_id  BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id  BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (follower_id, followee_id)
);

CREATE INDEX IF NOT EXISTS idx_follows_followee ON follows(followee_id);

-- idx_follows_follower speeds up the feed-read path's celebrity lookup:
--   "give me every celebrity that this user follows"
CREATE INDEX IF NOT EXISTS idx_follows_follower ON follows(follower_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- posts
-- ─────────────────────────────────────────────────────────────────────────────
-- Postgres is the durable record of every post. Redis sorted sets are bounded
-- caches (timeline:{userId}, celebrity_posts:{userId}); Postgres is the
-- fallback for deep scrollback and the source used to hydrate post content
-- after a Redis range read returns post IDs.
CREATE TABLE IF NOT EXISTS posts (
    id          BIGSERIAL    PRIMARY KEY,
    author_id   BIGINT       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content     TEXT         NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- idx_posts_author speeds up the "get all posts by this author" query used
-- when building celebrity_posts on a cache miss.
CREATE INDEX IF NOT EXISTS idx_posts_author ON posts(author_id);
