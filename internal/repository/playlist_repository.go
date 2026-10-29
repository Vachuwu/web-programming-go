package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"playlist/internal/domain"
)

var (
	ErrUserNotFound    = errors.New("user not found")
	ErrItemNotFound    = errors.New("playlist item not found")
	ErrItemNotQueued   = errors.New("playlist item is not queued")
	ErrItemAccessDenied = errors.New("playlist item access denied")
	ErrVersionConflict = errors.New("playback version conflict")
	ErrAlreadyPlaying  = errors.New("playlist already has a playing item")
	ErrPlaylistEmpty   = errors.New("playlist queue is empty")
	ErrNothingPlaying  = errors.New("playlist has no playing item")
)

type PlaylistRepository struct {
	pool *pgxpool.Pool
}

// PlaylistTransaction represents a write operation that holds the shared
// playlist_state row lock. It is intentionally created only by
// WithPlaylistLock so every playlist mutation follows the same rule.
type PlaylistTransaction struct {
	tx              pgx.Tx
	playbackVersion int64
}

func NewPlaylistRepository(pool *pgxpool.Pool) *PlaylistRepository {
	return &PlaylistRepository{pool: pool}
}

// WithPlaylistLock serializes a playlist-changing operation with all other
// changes. The lock is acquired before the callback performs its reads or
// writes and is released only after a successful commit (or rollback).
func (r *PlaylistRepository) WithPlaylistLock(ctx context.Context, operation func(*PlaylistTransaction) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin playlist transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var version int64
	if err := tx.QueryRow(ctx, `SELECT playback_version FROM playlist_state WHERE id = 1 FOR UPDATE`).Scan(&version); err != nil {
		return fmt.Errorf("lock playlist state: %w", err)
	}

	if err := operation(&PlaylistTransaction{tx: tx, playbackVersion: version}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit playlist transaction: %w", err)
	}
	return nil
}

// PlaybackVersion returns the version read while acquiring playlist_state's
// row lock. It is safe to compare it with an expected client version inside
// the locked transaction.
func (tx *PlaylistTransaction) PlaybackVersion() int64 {
	return tx.playbackVersion
}

func (tx *PlaylistTransaction) Add(ctx context.Context, title, artist string, url *string, addedBy int64) (domain.PlaylistItem, error) {
	const query = `
		INSERT INTO playlist_items (title, artist, url, added_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id, title, artist, url, added_by, status, created_at, started_at, played_at, 0::bigint`

	item, err := scanPlaylistItem(tx.tx.QueryRow(ctx, query, title, artist, url, addedBy))
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.Code == "23503" {
			return domain.PlaylistItem{}, ErrUserNotFound
		}
		return domain.PlaylistItem{}, fmt.Errorf("add playlist item: %w", err)
	}
	return item, nil
}

func (r *PlaylistRepository) Playlist(ctx context.Context) (domain.PlaylistSnapshot, error) {
	version, err := r.playbackVersion(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}

	playing, err := r.playing(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	queue, err := r.itemsByStatus(ctx, domain.StatusQueued, "score DESC, p.created_at ASC, p.id ASC")
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}

	return domain.PlaylistSnapshot{
		Playing:         playing,
		Queue:           queue,
		PlaybackVersion: version,
	}, nil
}

func (r *PlaylistRepository) History(ctx context.Context) ([]domain.PlaylistItem, error) {
	return r.itemsByStatus(ctx, domain.StatusPlayed, "p.played_at DESC, p.id DESC")
}

// Play starts the highest-priority queued item. The caller must hold the
// shared playlist lock through WithPlaylistLock.
func (tx *PlaylistTransaction) Play(ctx context.Context, expectedVersion int64) (domain.PlaylistSnapshot, error) {
	if expectedVersion != tx.playbackVersion {
		return domain.PlaylistSnapshot{}, ErrVersionConflict
	}

	playing, err := tx.playing(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	if playing != nil {
		return domain.PlaylistSnapshot{}, ErrAlreadyPlaying
	}

	next, err := tx.bestQueued(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	if next == nil {
		return domain.PlaylistSnapshot{}, ErrPlaylistEmpty
	}
	if err := tx.start(ctx, next.ID); err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	return tx.incrementVersionAndSnapshot(ctx)
}

// Next completes the current item and starts the best queued replacement, if
// one exists. The caller must hold the shared playlist lock.
func (tx *PlaylistTransaction) Next(ctx context.Context, expectedVersion int64) (domain.PlaylistSnapshot, error) {
	if expectedVersion != tx.playbackVersion {
		return domain.PlaylistSnapshot{}, ErrVersionConflict
	}

	playing, err := tx.playing(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	if playing == nil {
		return domain.PlaylistSnapshot{}, ErrNothingPlaying
	}

	const finishQuery = `
		UPDATE playlist_items
		SET status = 'played', played_at = NOW()
		WHERE id = $1`
	if _, err := tx.tx.Exec(ctx, finishQuery, playing.ID); err != nil {
		return domain.PlaylistSnapshot{}, fmt.Errorf("finish playing item: %w", err)
	}

	next, err := tx.bestQueued(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	if next != nil {
		if err := tx.start(ctx, next.ID); err != nil {
			return domain.PlaylistSnapshot{}, err
		}
	}
	return tx.incrementVersionAndSnapshot(ctx)
}

func (tx *PlaylistTransaction) start(ctx context.Context, itemID int64) error {
	const query = `
		UPDATE playlist_items
		SET status = 'playing', started_at = NOW()
		WHERE id = $1`
	if _, err := tx.tx.Exec(ctx, query, itemID); err != nil {
		return fmt.Errorf("start playlist item: %w", err)
	}
	return nil
}

func (tx *PlaylistTransaction) incrementVersionAndSnapshot(ctx context.Context) (domain.PlaylistSnapshot, error) {
	const query = `
		UPDATE playlist_state
		SET playback_version = playback_version + 1
		WHERE id = 1
		RETURNING playback_version`
	if err := tx.tx.QueryRow(ctx, query).Scan(&tx.playbackVersion); err != nil {
		return domain.PlaylistSnapshot{}, fmt.Errorf("increment playback version: %w", err)
	}
	return tx.snapshot(ctx)
}

func (tx *PlaylistTransaction) snapshot(ctx context.Context) (domain.PlaylistSnapshot, error) {
	playing, err := tx.playing(ctx)
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	queue, err := tx.itemsByStatus(ctx, domain.StatusQueued, "score DESC, p.created_at ASC, p.id ASC")
	if err != nil {
		return domain.PlaylistSnapshot{}, err
	}
	return domain.PlaylistSnapshot{
		Playing:         playing,
		Queue:           queue,
		PlaybackVersion: tx.playbackVersion,
	}, nil
}

func (tx *PlaylistTransaction) playing(ctx context.Context) (*domain.PlaylistItem, error) {
	return playing(ctx, tx.tx)
}

func (tx *PlaylistTransaction) bestQueued(ctx context.Context) (*domain.PlaylistItem, error) {
	const query = `
		SELECT p.id, p.title, p.artist, p.url, p.added_by, p.status,
			p.created_at, p.started_at, p.played_at,
			COALESCE(SUM(v.value), 0)::bigint
		FROM playlist_items AS p
		LEFT JOIN votes AS v ON v.item_id = p.id
		WHERE p.status = 'queued'
		GROUP BY p.id
		ORDER BY COALESCE(SUM(v.value), 0) DESC, p.created_at ASC, p.id ASC
		LIMIT 1`

	item, err := scanPlaylistItem(tx.tx.QueryRow(ctx, query))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select next queued item: %w", err)
	}
	return &item, nil
}

func (tx *PlaylistTransaction) itemsByStatus(ctx context.Context, status domain.ItemStatus, orderBy string) ([]domain.PlaylistItem, error) {
	return itemsByStatus(ctx, tx.tx, status, orderBy)
}

// Remove logically removes a queued item. Both the author and status checks
// happen after the shared playlist lock has been acquired, so the item cannot
// be selected by Next between those checks and the update.
func (tx *PlaylistTransaction) Remove(ctx context.Context, itemID, userID int64) (domain.PlaylistItem, error) {
	const itemQuery = `SELECT added_by, status FROM playlist_items WHERE id = $1`

	var (
		addedBy int64
		status  domain.ItemStatus
	)
	if err := tx.tx.QueryRow(ctx, itemQuery, itemID).Scan(&addedBy, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.PlaylistItem{}, ErrItemNotFound
		}
		return domain.PlaylistItem{}, fmt.Errorf("read playlist item for removal: %w", err)
	}
	if status != domain.StatusQueued {
		return domain.PlaylistItem{}, ErrItemNotQueued
	}

	const userQuery = `SELECT 1 FROM users WHERE id = $1`
	var found int
	if err := tx.tx.QueryRow(ctx, userQuery, userID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.PlaylistItem{}, ErrUserNotFound
		}
		return domain.PlaylistItem{}, fmt.Errorf("read removal user: %w", err)
	}
	if addedBy != userID {
		return domain.PlaylistItem{}, ErrItemAccessDenied
	}

	const removeQuery = `UPDATE playlist_items SET status = 'removed' WHERE id = $1`
	if _, err := tx.tx.Exec(ctx, removeQuery, itemID); err != nil {
		return domain.PlaylistItem{}, fmt.Errorf("remove playlist item: %w", err)
	}
	return tx.itemByID(ctx, itemID)
}

func (tx *PlaylistTransaction) itemByID(ctx context.Context, itemID int64) (domain.PlaylistItem, error) {
	const query = `
		SELECT p.id, p.title, p.artist, p.url, p.added_by, p.status,
			p.created_at, p.started_at, p.played_at,
			COALESCE(SUM(v.value), 0)::bigint
		FROM playlist_items AS p
		LEFT JOIN votes AS v ON v.item_id = p.id
		WHERE p.id = $1
		GROUP BY p.id`

	item, err := scanPlaylistItem(tx.tx.QueryRow(ctx, query, itemID))
	if err != nil {
		return domain.PlaylistItem{}, fmt.Errorf("read removed playlist item: %w", err)
	}
	return item, nil
}

func (tx *PlaylistTransaction) Vote(ctx context.Context, itemID, userID int64, value int16) (domain.VoteResult, error) {
	if err := tx.ensureVotable(ctx, itemID, userID); err != nil {
		return domain.VoteResult{}, err
	}

	const query = `
		INSERT INTO votes (item_id, user_id, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (item_id, user_id) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
		WHERE votes.value IS DISTINCT FROM EXCLUDED.value`
	commandTag, err := tx.tx.Exec(ctx, query, itemID, userID, value)
	if err != nil {
		return domain.VoteResult{}, fmt.Errorf("save vote: %w", err)
	}

	score, err := tx.score(ctx, itemID)
	if err != nil {
		return domain.VoteResult{}, err
	}
	return domain.VoteResult{ItemID: itemID, UserID: userID, Value: &value, Score: score, Changed: commandTag.RowsAffected() > 0}, nil
}

func (tx *PlaylistTransaction) DeleteVote(ctx context.Context, itemID, userID int64) (domain.VoteResult, error) {
	if err := tx.ensureVotable(ctx, itemID, userID); err != nil {
		return domain.VoteResult{}, err
	}

	const query = `DELETE FROM votes WHERE item_id = $1 AND user_id = $2`
	commandTag, err := tx.tx.Exec(ctx, query, itemID, userID)
	if err != nil {
		return domain.VoteResult{}, fmt.Errorf("delete vote: %w", err)
	}

	score, err := tx.score(ctx, itemID)
	if err != nil {
		return domain.VoteResult{}, err
	}
	return domain.VoteResult{ItemID: itemID, UserID: userID, Score: score, Changed: commandTag.RowsAffected() > 0}, nil
}

func (tx *PlaylistTransaction) ensureVotable(ctx context.Context, itemID, userID int64) error {
	const itemQuery = `SELECT status FROM playlist_items WHERE id = $1`
	var status domain.ItemStatus
	if err := tx.tx.QueryRow(ctx, itemQuery, itemID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		return fmt.Errorf("read playlist item status: %w", err)
	}
	if status != domain.StatusQueued {
		return ErrItemNotQueued
	}

	const userQuery = `SELECT 1 FROM users WHERE id = $1`
	var found int
	if err := tx.tx.QueryRow(ctx, userQuery, userID).Scan(&found); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return fmt.Errorf("read vote user: %w", err)
	}
	return nil
}

func (tx *PlaylistTransaction) score(ctx context.Context, itemID int64) (int64, error) {
	const query = `SELECT COALESCE(SUM(value), 0)::bigint FROM votes WHERE item_id = $1`

	var score int64
	if err := tx.tx.QueryRow(ctx, query, itemID).Scan(&score); err != nil {
		return 0, fmt.Errorf("calculate vote score: %w", err)
	}
	return score, nil
}

func (r *PlaylistRepository) playbackVersion(ctx context.Context) (int64, error) {
	const query = `SELECT playback_version FROM playlist_state WHERE id = 1`

	var version int64
	if err := r.pool.QueryRow(ctx, query).Scan(&version); err != nil {
		return 0, fmt.Errorf("read playback version: %w", err)
	}
	return version, nil
}

func (r *PlaylistRepository) playing(ctx context.Context) (*domain.PlaylistItem, error) {
	return playing(ctx, r.pool)
}

func playing(ctx context.Context, queryer playlistQueryer) (*domain.PlaylistItem, error) {
	const query = `
		SELECT p.id, p.title, p.artist, p.url, p.added_by, p.status,
			p.created_at, p.started_at, p.played_at,
			COALESCE(SUM(v.value), 0)::bigint
		FROM playlist_items AS p
		LEFT JOIN votes AS v ON v.item_id = p.id
		WHERE p.status = 'playing'
		GROUP BY p.id`

	item, err := scanPlaylistItem(queryer.QueryRow(ctx, query))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read playing item: %w", err)
	}
	return &item, nil
}

func (r *PlaylistRepository) itemsByStatus(ctx context.Context, status domain.ItemStatus, orderBy string) ([]domain.PlaylistItem, error) {
	return itemsByStatus(ctx, r.pool, status, orderBy)
}

type playlistQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func itemsByStatus(ctx context.Context, queryer playlistQueryer, status domain.ItemStatus, orderBy string) ([]domain.PlaylistItem, error) {
	query := `
		SELECT p.id, p.title, p.artist, p.url, p.added_by, p.status,
			p.created_at, p.started_at, p.played_at,
			COALESCE(SUM(v.value), 0)::bigint
		FROM playlist_items AS p
		LEFT JOIN votes AS v ON v.item_id = p.id
		WHERE p.status = $1
		GROUP BY p.id
		ORDER BY ` + orderBy

	rows, err := queryer.Query(ctx, query, status)
	if err != nil {
		return nil, fmt.Errorf("read playlist items: %w", err)
	}
	defer rows.Close()

	items := make([]domain.PlaylistItem, 0)
	for rows.Next() {
		item, err := scanPlaylistItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan playlist item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate playlist items: %w", err)
	}
	return items, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPlaylistItem(row rowScanner) (domain.PlaylistItem, error) {
	var item domain.PlaylistItem
	var url pgtype.Text
	var startedAt pgtype.Timestamptz
	var playedAt pgtype.Timestamptz

	err := row.Scan(
		&item.ID,
		&item.Title,
		&item.Artist,
		&url,
		&item.AddedBy,
		&item.Status,
		&item.CreatedAt,
		&startedAt,
		&playedAt,
		&item.Score,
	)
	if err != nil {
		return domain.PlaylistItem{}, err
	}
	if url.Valid {
		item.URL = &url.String
	}
	item.StartedAt = optionalTime(startedAt)
	item.PlayedAt = optionalTime(playedAt)
	return item, nil
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
