// Package db provides typed Postgres query wrappers for FeedRank.
//
// All functions receive a *pgxpool.Pool (connection pool) rather than a raw
// *sql.DB because pgx's native pool gives us prepared-statement caching,
// pipeline support, and proper pgx type handling without the driver overhead
// of database/sql.
//
// Convention: every function that modifies state accepts a context.Context as
// its first argument so callers can propagate deadlines from HTTP handlers.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// User mirrors the users table row.
type User struct {
	ID             int64     `json:"id"`
	Username       string    `json:"username"`
	FollowerCount  int       `json:"follower_count"`
	IsCelebrity    bool      `json:"is_celebrity"`
	CreatedAt      time.Time `json:"created_at"`
}

// Post mirrors the posts table row.
type Post struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Users
// ─────────────────────────────────────────────────────────────────────────────

// CreateUser inserts a new user and returns the persisted row.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, username string) (*User, error) {
	const q = `
		INSERT INTO users (username)
		VALUES ($1)
		RETURNING id, username, follower_count, is_celebrity, created_at
	`
	u := &User{}
	err := pool.QueryRow(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.FollowerCount, &u.IsCelebrity, &u.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("db.CreateUser: %w", err)
	}
	return u, nil
}

// GetUser fetches a single user by ID.
func GetUser(ctx context.Context, pool *pgxpool.Pool, id int64) (*User, error) {
	const q = `
		SELECT id, username, follower_count, is_celebrity, created_at
		FROM users WHERE id = $1
	`
	u := &User{}
	err := pool.QueryRow(ctx, q, id).Scan(
		&u.ID, &u.Username, &u.FollowerCount, &u.IsCelebrity, &u.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("db.GetUser: %w", err)
	}
	return u, nil
}

// SetCelebrity updates is_celebrity for a user. Called by the reclassification
// logic when follower_count crosses the configured threshold.
func SetCelebrity(ctx context.Context, pool *pgxpool.Pool, userID int64, isCelebrity bool) error {
	const q = `UPDATE users SET is_celebrity = $1 WHERE id = $2`
	_, err := pool.Exec(ctx, q, isCelebrity, userID)
	if err != nil {
		return fmt.Errorf("db.SetCelebrity: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Follows
// ─────────────────────────────────────────────────────────────────────────────

// CreateFollow inserts a follow edge and increments the followee's
// follower_count in a single transaction, returning the updated count.
//
// Doing both in one transaction is critical: if we did them separately and
// crashed between the two, follower_count would be stale — and the
// celebrity-threshold check that fires after this returns would be wrong.
func CreateFollow(ctx context.Context, pool *pgxpool.Pool, followerID, followeeID int64) (newFollowerCount int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("db.CreateFollow begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	_, err = tx.Exec(ctx,
		`INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		followerID, followeeID,
	)
	if err != nil {
		return 0, fmt.Errorf("db.CreateFollow insert: %w", err)
	}

	err = tx.QueryRow(ctx,
		`UPDATE users SET follower_count = follower_count + 1 WHERE id = $1 RETURNING follower_count`,
		followeeID,
	).Scan(&newFollowerCount)
	if err != nil {
		return 0, fmt.Errorf("db.CreateFollow update count: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("db.CreateFollow commit: %w", err)
	}
	return newFollowerCount, nil
}

// GetFollowerIDs returns all follower IDs for a given user.
// Used by the fan-out worker to build the ZADD target list.
//
// For very high follower counts (celebrity accounts) this query is intentionally
// never called — the worker checks is_celebrity before calling this.
func GetFollowerIDs(ctx context.Context, pool *pgxpool.Pool, userID int64) ([]int64, error) {
	const q = `SELECT follower_id FROM follows WHERE followee_id = $1`
	rows, err := pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("db.GetFollowerIDs: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db.GetFollowerIDs scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetCelebrityFollowees returns the IDs of all celebrity accounts that
// userID follows. Used by the feed-read path to determine which
// celebrity_posts:{id} sorted sets to merge into the timeline.
func GetCelebrityFollowees(ctx context.Context, pool *pgxpool.Pool, userID int64) ([]int64, error) {
	const q = `
		SELECT f.followee_id
		FROM follows f
		JOIN users u ON u.id = f.followee_id
		WHERE f.follower_id = $1
		  AND u.is_celebrity = TRUE
	`
	rows, err := pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("db.GetCelebrityFollowees: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db.GetCelebrityFollowees scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Posts
// ─────────────────────────────────────────────────────────────────────────────

// CreatePost inserts a new post and returns the persisted row including its
// generated ID and timestamp — the caller needs both to enqueue the fan-out
// job (post ID) and to score the Redis sorted-set entry (timestamp).
func CreatePost(ctx context.Context, pool *pgxpool.Pool, authorID int64, content string) (*Post, error) {
	const q = `
		INSERT INTO posts (author_id, content)
		VALUES ($1, $2)
		RETURNING id, author_id, content, created_at
	`
	p := &Post{}
	err := pool.QueryRow(ctx, q, authorID, content).Scan(
		&p.ID, &p.AuthorID, &p.Content, &p.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("db.CreatePost: %w", err)
	}
	return p, nil
}

// GetPostsByIDs fetches posts in bulk given a slice of IDs.
// Used to hydrate post content after a Redis range read returns raw post IDs.
//
// The result map[id]Post lets callers preserve ordering from the Redis range
// (which is the score-sorted order we want to present to the user) without
// relying on Postgres's ORDER BY matching our Redis order.
func GetPostsByIDs(ctx context.Context, pool *pgxpool.Pool, ids []int64) (map[int64]*Post, error) {
	if len(ids) == 0 {
		return map[int64]*Post{}, nil
	}

	// pgx supports $1 = []int64 natively via pgx.CollectRows + anyarray
	const q = `
		SELECT id, author_id, content, created_at
		FROM posts
		WHERE id = ANY($1)
	`
	rows, err := pool.Query(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("db.GetPostsByIDs: %w", err)
	}
	defer rows.Close()

	posts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Post, error) {
		p := &Post{}
		if err := row.Scan(&p.ID, &p.AuthorID, &p.Content, &p.CreatedAt); err != nil {
			return nil, err
		}
		return p, nil
	})
	if err != nil {
		return nil, fmt.Errorf("db.GetPostsByIDs collect: %w", err)
	}

	result := make(map[int64]*Post, len(posts))
	for _, p := range posts {
		result[p.ID] = p
	}
	return result, nil
}
