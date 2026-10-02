// Package cache provides Redis typed wrappers for FeedRank's timeline data.
//
// Redis key conventions used throughout this package:
//
//   timeline:{userId}        — Sorted set. Fan-out write target for normal accounts.
//                              Members: postId (string). Score: UNIX milliseconds of post.
//                              Trimmed to TIMELINE_CAP entries on every write.
//
//   celebrity_posts:{userId} — Sorted set. All posts by a celebrity account.
//                              Members: postId (string). Score: UNIX milliseconds of post.
//                              NOT written to by the fan-out worker; written directly
//                              when a celebrity creates a post.
//
// Both key types use the same member format (string post ID) and the same
// scoring (ms timestamp) so that the feed-read merge-sort is a single
// unified comparator regardless of which set an entry came from.
package cache

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// ─────────────────────────────────────────────────────────────────────────────
// Key helpers
// ─────────────────────────────────────────────────────────────────────────────

func timelineKey(userID int64) string {
	return fmt.Sprintf("timeline:%d", userID)
}

func celebrityPostsKey(userID int64) string {
	return fmt.Sprintf("celebrity_posts:%d", userID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Timeline ops — used by the fan-out worker for normal-account posts
// ─────────────────────────────────────────────────────────────────────────────

// AddToTimeline adds postID with the given score (UNIX ms timestamp) to the
// target user's timeline sorted set, then trims the set to cap entries.
//
// ZADD + ZREMRANGEBYRANK are issued as a pipeline (not a transaction) for
// throughput — if the trim fails, the sorted set grows by one entry, which
// is tolerable; the trim will succeed on the next write.
//
// score is float64 because Redis sorted-set scores are IEEE 754 doubles.
// UNIX milliseconds fit in a double without precision loss up to year 2255.
func AddToTimeline(ctx context.Context, rdb *redis.Client, userID, postID int64, score float64, cap int) error {
	key := timelineKey(userID)
	pipe := rdb.Pipeline()

	pipe.ZAdd(ctx, key, redis.Z{Score: score, Member: strconv.FormatInt(postID, 10)})
	// Keep only the most recent `cap` entries.
	// ZREMRANGEBYRANK key 0 -(cap+1) removes everything older than the top `cap`.
	pipe.ZRemRangeByRank(ctx, key, 0, int64(-cap-1))

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cache.AddToTimeline user=%d post=%d: %w", userID, postID, err)
	}
	return nil
}

// GetTimelinePage returns a page of post IDs from a user's timeline sorted
// set, ordered by score descending (newest first).
//
// Pagination is cursor-based: cursor is the score of the last item seen on the
// previous page (exclusive upper bound). Pass +∞ (math.MaxFloat64 or redis.MaxFloat)
// for the first page.
//
// This maps to: ZREVRANGEBYSCORE key (<cursor) -inf LIMIT 0 limit
//
// Why cursor, not offset? Offset pagination shifts under a live-writing sorted
// set: if 5 new posts arrive between two page requests, "items 20–40" refers
// to different posts. A score cursor is anchored to a stable point in score
// space; new writes above the cursor never affect pages being fetched below it.
func GetTimelinePage(ctx context.Context, rdb *redis.Client, userID int64, cursor float64, limit int) ([]ScoredPost, error) {
	key := timelineKey(userID)

	// Exclusive upper bound: "(" prefix on the score string means strictly less than.
	upperBound := fmt.Sprintf("(%v", cursor)

	results, err := rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    upperBound,
		Offset: 0,
		Count:  int64(limit),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("cache.GetTimelinePage user=%d: %w", userID, err)
	}

	return parseZResults(results)
}

// ─────────────────────────────────────────────────────────────────────────────
// Celebrity post ops — used for celebrity accounts (no fan-out)
// ─────────────────────────────────────────────────────────────────────────────

// AddCelebrityPost stores a celebrity post in their own sorted set.
// Unlike fan-out, this is a single ZADD targeting one key, regardless of how
// many followers the celebrity has — that's the whole point of the hybrid.
func AddCelebrityPost(ctx context.Context, rdb *redis.Client, authorID, postID int64, score float64) error {
	key := celebrityPostsKey(authorID)
	err := rdb.ZAdd(ctx, key, redis.Z{
		Score:  score,
		Member: strconv.FormatInt(postID, 10),
	}).Err()
	if err != nil {
		return fmt.Errorf("cache.AddCelebrityPost author=%d post=%d: %w", authorID, postID, err)
	}
	return nil
}

// GetCelebrityPosts returns the most recent posts by a celebrity account,
// bounded by limit. Called at feed-read time for every celebrity the user
// follows (typically a very small number, even on large platforms).
//
// Unlike GetTimelinePage, this does not use a cursor — the caller passes a
// global cursor (the last score seen) to bound the merge window.
func GetCelebrityPosts(ctx context.Context, rdb *redis.Client, celebID int64, cursor float64, limit int) ([]ScoredPost, error) {
	key := celebrityPostsKey(celebID)

	upperBound := fmt.Sprintf("(%v", cursor)

	results, err := rdb.ZRevRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:    "-inf",
		Max:    upperBound,
		Offset: 0,
		Count:  int64(limit),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("cache.GetCelebrityPosts celeb=%d: %w", celebID, err)
	}

	return parseZResults(results)
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared types & helpers
// ─────────────────────────────────────────────────────────────────────────────

// ScoredPost is a post ID paired with its Redis sorted-set score.
// Score is UNIX milliseconds — used as the merge-sort key and as the
// next-page cursor value.
type ScoredPost struct {
	PostID int64
	Score  float64
}

// parseZResults converts Redis ZSliceCmds to our typed ScoredPost slice.
func parseZResults(zs []redis.Z) ([]ScoredPost, error) {
	out := make([]ScoredPost, 0, len(zs))
	for _, z := range zs {
		member, ok := z.Member.(string)
		if !ok {
			return nil, fmt.Errorf("cache: unexpected member type %T", z.Member)
		}
		id, err := strconv.ParseInt(member, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cache: non-integer member %q: %w", member, err)
		}
		out = append(out, ScoredPost{PostID: id, Score: z.Score})
	}
	return out, nil
}
