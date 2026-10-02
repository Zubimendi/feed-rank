// feed/read.go — the hybrid feed-read path.
//
// Feed construction for any user:
//
//   1. Fetch the user's precomputed Redis timeline (timeline:{userId}).
//      This contains postIds from every non-celebrity they follow, pushed
//      there asynchronously by the fan-out worker.
//
//   2. Determine which followed accounts are celebrities (Postgres query,
//      indexed on follows.follower_id + users.is_celebrity).
//
//   3. For each celebrity, fetch their recent posts from celebrity_posts:{id}.
//      The number of celebrities a user follows is small per user even on a
//      huge platform — so this is a bounded set of Redis reads, not O(followers).
//
//   4. Merge-sort all result sets by score descending. Slice to page size.
//      The merge is O(N log N) where N = total results across all sources,
//      which is bounded by limit × (1 + celebrity_count).
//
//   5. Hydrate post content: batch-fetch from Postgres using the collected IDs.
//
//   6. Return the page with next_cursor = last item's score (exclusive).
//
// Cursor pagination:
//   The client sends ?cursor=<score> on subsequent requests.
//   We pass this score as the exclusive upper bound to ZREVRANGEBYSCORE.
//   New posts written above the cursor never shift the page below it.
package feed

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/francisco4/feed-rank/internal/cache"
	"github.com/francisco4/feed-rank/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// FeedItem is a single post in the feed response, with post content hydrated.
type FeedItem struct {
	PostID    int64   `json:"post_id"`
	AuthorID  int64   `json:"author_id"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"` // UNIX ms — also used as cursor
}

// FeedPage is the paginated response from ReadFeed.
type FeedPage struct {
	Items      []FeedItem `json:"items"`
	NextCursor float64    `json:"next_cursor"` // 0 if no more pages
}

// Reader holds dependencies for the feed-read path.
type Reader struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

// NewReader creates a Reader.
func NewReader(pool *pgxpool.Pool, rdb *redis.Client) *Reader {
	return &Reader{pool: pool, rdb: rdb}
}

// ReadFeed constructs a paginated feed for the given user.
//
// cursor: pass 0 for the first page (interpreted as math.MaxFloat64 — no upper
//         bound). For subsequent pages, pass the next_cursor from the previous
//         response.
// limit:  maximum number of items to return.
func (r *Reader) ReadFeed(ctx context.Context, userID int64, cursor float64, limit int) (*FeedPage, error) {
	if cursor == 0 {
		// First page — start from the top of the sorted set.
		cursor = math.MaxFloat64
	}

	// ── Step 1: fetch user's precomputed timeline (normal-account follows) ──
	timelinePosts, err := cache.GetTimelinePage(ctx, r.rdb, userID, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("read: timeline fetch: %w", err)
	}

	// ── Step 2: find followed celebrities ──────────────────────────────────
	celebIDs, err := db.GetCelebrityFollowees(ctx, r.pool, userID)
	if err != nil {
		return nil, fmt.Errorf("read: celebrity followees: %w", err)
	}

	// ── Step 3: fetch recent posts from each celebrity ─────────────────────
	// We fetch up to `limit` posts per celebrity. The merge step will reduce
	// this to `limit` total items, so over-fetching slightly is fine.
	var allPosts []cache.ScoredPost
	allPosts = append(allPosts, timelinePosts...)

	for _, cid := range celebIDs {
		celebPosts, err := cache.GetCelebrityPosts(ctx, r.rdb, cid, cursor, limit)
		if err != nil {
			// Don't fail the whole feed for one celebrity's cache miss.
			// The data will appear on the next request.
			continue
		}
		allPosts = append(allPosts, celebPosts...)
	}

	// ── Step 4: merge-sort by score descending ─────────────────────────────
	sort.Slice(allPosts, func(i, j int) bool {
		return allPosts[i].Score > allPosts[j].Score // descending
	})

	// Deduplicate by post ID (a post could appear in timeline and celebrity_posts
	// if reclassification happened mid-fan-out) and slice to limit.
	seen := make(map[int64]struct{}, len(allPosts))
	merged := make([]cache.ScoredPost, 0, limit)
	for _, sp := range allPosts {
		if _, ok := seen[sp.PostID]; ok {
			continue
		}
		seen[sp.PostID] = struct{}{}
		merged = append(merged, sp)
		if len(merged) == limit {
			break
		}
	}

	if len(merged) == 0 {
		return &FeedPage{Items: []FeedItem{}, NextCursor: 0}, nil
	}

	// ── Step 5: hydrate post content from Postgres ─────────────────────────
	ids := make([]int64, len(merged))
	for i, sp := range merged {
		ids[i] = sp.PostID
	}

	postMap, err := db.GetPostsByIDs(ctx, r.pool, ids)
	if err != nil {
		return nil, fmt.Errorf("read: hydrate posts: %w", err)
	}

	// ── Step 6: build response, preserving Redis score order ──────────────
	items := make([]FeedItem, 0, len(merged))
	for _, sp := range merged {
		post, ok := postMap[sp.PostID]
		if !ok {
			// Post in Redis but deleted from Postgres — skip silently.
			// In a production system you'd also evict from Redis here.
			continue
		}
		items = append(items, FeedItem{
			PostID:   post.ID,
			AuthorID: post.AuthorID,
			Content:  post.Content,
			Score:    sp.Score,
		})
	}

	// next_cursor is the score of the last item returned (exclusive lower bound
	// for the next page). If we returned fewer items than limit, there are no
	// more pages — signal this with cursor=0.
	var nextCursor float64
	if len(items) == limit {
		nextCursor = items[len(items)-1].Score
	}

	return &FeedPage{Items: items, NextCursor: nextCursor}, nil
}
