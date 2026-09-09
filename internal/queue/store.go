package queue

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, no cgo, so no gcc needed on windows
)

//go:embed schema.sql
var schemaSQL string

var ErrNoWork = errors.New("no work available")

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the database and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cannot create database directory %s: %w", dir, err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open database %s: %w", path, err)
	}
	// One writer. The load here is hundreds of writes per hour, and a single
	// connection removes a whole class of races and SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot apply schema to %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) unix() int64 { return s.now().Unix() }

// AddSource registers a source; calling it again with the same URL returns
// the existing id instead of creating a duplicate.
func (s *Store) AddSource(ctx context.Context, kind SourceKind, url, title string) (int64, error) {
	_, err := s.db.ExecContext(ctx,
		"insert into sources(kind, url, title, added_at) values(?,?,?,?) "+
			"on conflict(url) do update set title = coalesce(nullif(excluded.title,''), sources.title)",
		string(kind), url, title, s.unix())
	if err != nil {
		return 0, fmt.Errorf("cannot store source %s: %w", url, err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, "select id from sources where url = ?", url).Scan(&id); err != nil {
		return 0, fmt.Errorf("cannot read back source %s: %w", url, err)
	}
	return id, nil
}

func (s *Store) MarkSourceSynced(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "update sources set synced_at = ? where id = ?", s.unix(), id)
	return err
}

func (s *Store) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		"select id, kind, url, coalesce(title,''), added_at, synced_at from sources order by id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		var src Source
		var added int64
		var synced sql.NullInt64
		if err := rows.Scan(&src.ID, &src.Kind, &src.URL, &src.Title, &added, &synced); err != nil {
			return nil, err
		}
		src.AddedAt = time.Unix(added, 0)
		if synced.Valid {
			t := time.Unix(synced.Int64, 0)
			src.SyncedAt = &t
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// NewItem is a video stub produced by expanding a source.
type NewItem struct {
	VideoID    string
	URL        string
	Title      string
	Uploader   string
	UploadDate string
	Duration   int64
}

// AddItems inserts videos in bulk, skipping ones already known.
func (s *Store) AddItems(ctx context.Context, sourceID int64, items []NewItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		"insert into items(video_id, source_id, url, title, uploader, upload_date, duration, "+
			"state, created_at, updated_at) values(?,?,?,?,?,?,?,?,?,?) "+
			"on conflict(video_id) do nothing")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := s.unix()
	var src any
	if sourceID > 0 {
		src = sourceID
	}
	added := 0
	for _, it := range items {
		if it.VideoID == "" {
			continue
		}
		res, err := stmt.ExecContext(ctx, it.VideoID, src, it.URL, it.Title, it.Uploader,
			it.UploadDate, it.Duration, string(StatePending), now, now)
		if err != nil {
			return added, fmt.Errorf("cannot add video %s: %w", it.VideoID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// ReclaimExpired returns tasks whose lease has run out. Called on startup:
// this is what repairs rows left behind by a crash.
func (s *Store) ReclaimExpired(ctx context.Context) (int, error) {
	now := s.unix()
	var total int64

	res, err := s.db.ExecContext(ctx,
		"update items set state = 'pending', lease_owner = null, lease_until = null, updated_at = ? "+
			"where state = 'downloading' and (lease_until is null or lease_until < ?)", now, now)
	if err != nil {
		return 0, fmt.Errorf("cannot reclaim expired downloads: %w", err)
	}
	n, _ := res.RowsAffected()
	total += n

	res, err = s.db.ExecContext(ctx,
		"update item_targets set state = 'pending', lease_owner = null, lease_until = null, updated_at = ? "+
			"where state = 'uploading' and (lease_until is null or lease_until < ?)", now, now)
	if err != nil {
		return 0, fmt.Errorf("cannot reclaim expired uploads: %w", err)
	}
	n, _ = res.RowsAffected()
	total += n

	return int(total), nil
}

// ReclaimAll returns every claimed task regardless of its lease.
//
// Only call this once the caller has proven it is the sole live process over
// this database. Waiting out a lease after a crash would otherwise make the
// tool pretend there is no work for minutes.
func (s *Store) ReclaimAll(ctx context.Context) (int, error) {
	now := s.unix()
	var total int64

	res, err := s.db.ExecContext(ctx,
		"update items set state = 'pending', lease_owner = null, lease_until = null, updated_at = ? "+
			"where state = 'downloading'", now)
	if err != nil {
		return 0, fmt.Errorf("cannot reclaim abandoned downloads: %w", err)
	}
	n, _ := res.RowsAffected()
	total += n

	res, err = s.db.ExecContext(ctx,
		"update item_targets set state = 'pending', lease_owner = null, lease_until = null, updated_at = ? "+
			"where state = 'uploading'", now)
	if err != nil {
		return 0, fmt.Errorf("cannot reclaim abandoned uploads: %w", err)
	}
	n, _ = res.RowsAffected()
	total += n

	return int(total), nil
}

const itemColumns = `id, video_id, source_id, url, coalesce(title,''), coalesce(uploader,''),
	coalesce(upload_date,''), coalesce(duration,0), state, attempts, coalesce(last_error,''),
	coalesce(format_id,''), expected_bytes, bytes_done, coalesce(local_path,''), local_bytes,
	created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanItem(row scanner) (*Item, error) {
	var it Item
	var srcID sql.NullInt64
	var created, updated int64
	err := row.Scan(&it.ID, &it.VideoID, &srcID, &it.URL, &it.Title, &it.Uploader,
		&it.UploadDate, &it.Duration, &it.State, &it.Attempts, &it.LastError,
		&it.FormatID, &it.ExpectedBytes, &it.BytesDone, &it.LocalPath, &it.LocalBytes,
		&created, &updated)
	if err != nil {
		return nil, err
	}
	if srcID.Valid {
		it.SourceID = &srcID.Int64
	}
	it.CreatedAt = time.Unix(created, 0)
	it.UpdatedAt = time.Unix(updated, 0)
	return &it, nil
}

// ClaimDownload takes one video under a lease, returning ErrNoWork if none
// is available.
func (s *Store) ClaimDownload(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*Item, error) {
	row := s.db.QueryRowContext(ctx,
		"update items set state = 'downloading', lease_owner = ?, lease_until = ?, "+
			"attempts = attempts + 1, updated_at = ? "+
			"where id = (select id from items where state = 'pending' and attempts < ? order by id limit 1) "+
			"returning "+itemColumns,
		owner, s.now().Add(lease).Unix(), s.unix(), maxAttempts)

	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoWork
	}
	if err != nil {
		return nil, fmt.Errorf("cannot claim a video for download: %w", err)
	}
	return it, nil
}

// RenewLease extends a lease. Long downloads must call it or another worker
// will treat the task as abandoned.
func (s *Store) RenewLease(ctx context.Context, itemID int64, owner string, lease time.Duration) error {
	res, err := s.db.ExecContext(ctx,
		"update items set lease_until = ?, updated_at = ? where id = ? and lease_owner = ?",
		s.now().Add(lease).Unix(), s.unix(), itemID, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("lost the lease on item %d: another worker took it", itemID)
	}
	return nil
}

func (s *Store) UpdateProgress(ctx context.Context, itemID, bytesDone, expected int64) error {
	_, err := s.db.ExecContext(ctx,
		"update items set bytes_done = ?, expected_bytes = max(expected_bytes, ?), updated_at = ? where id = ?",
		bytesDone, expected, s.unix(), itemID)
	return err
}

// ReleaseDownload puts a task back without penalty. Used on graceful
// shutdown: stopping should not burn an attempt.
func (s *Store) ReleaseDownload(ctx context.Context, itemID int64) error {
	_, err := s.db.ExecContext(ctx,
		"update items set state = 'pending', lease_owner = null, lease_until = null, "+
			"attempts = max(attempts - 1, 0), updated_at = ? where id = ? and state = 'downloading'",
		s.unix(), itemID)
	return err
}

func (s *Store) MarkDownloaded(ctx context.Context, itemID int64, path string, bytes int64, formatID string) error {
	_, err := s.db.ExecContext(ctx,
		"update items set state = 'downloaded', local_path = ?, local_bytes = ?, bytes_done = ?, "+
			"format_id = ?, last_error = null, lease_owner = null, lease_until = null, updated_at = ? "+
			"where id = ?",
		path, bytes, bytes, formatID, s.unix(), itemID)
	return err
}

// MarkFailed records a failed attempt. Unless the error is permanent or the
// retries are spent, the task goes back to pending.
func (s *Store) MarkFailed(ctx context.Context, itemID int64, cause error, permanent bool, maxAttempts int) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.db.ExecContext(ctx,
		"update items set state = case when ? or attempts >= ? then 'failed' else 'pending' end, "+
			"last_error = ?, lease_owner = null, lease_until = null, updated_at = ? where id = ?",
		permanent, maxAttempts, msg, s.unix(), itemID)
	return err
}

func (s *Store) MarkSkipped(ctx context.Context, itemID int64, reason string) error {
	_, err := s.db.ExecContext(ctx,
		"update items set state = 'skipped', last_error = ?, lease_owner = null, "+
			"lease_until = null, updated_at = ? where id = ?", reason, s.unix(), itemID)
	return err
}

// Retry puts permanently failed tasks back in the queue with a clean counter.
func (s *Store) Retry(ctx context.Context, videoID string) (int, error) {
	query := "update items set state = 'pending', attempts = 0, last_error = null, updated_at = ? where state = 'failed'"
	args := []any{s.unix()}
	if videoID != "" {
		query += " and video_id = ?"
		args = append(args, videoID)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	res, err = s.db.ExecContext(ctx,
		"update item_targets set state = 'pending', attempts = 0, last_error = null, updated_at = ? "+
			"where state = 'failed'", s.unix())
	if err != nil {
		return int(n), err
	}
	m, _ := res.RowsAffected()
	return int(n + m), nil
}

// ClearLocal forgets the local file, called once it is removed from disk.
func (s *Store) ClearLocal(ctx context.Context, itemID int64) error {
	_, err := s.db.ExecContext(ctx,
		"update items set local_path = null, local_bytes = 0, updated_at = ? where id = ?",
		s.unix(), itemID)
	return err
}

func (s *Store) Item(ctx context.Context, itemID int64) (*Item, error) {
	row := s.db.QueryRowContext(ctx, "select "+itemColumns+" from items where id = ?", itemID)
	return scanItem(row)
}

// UpdateMetadata fills in details that only became known after the download.
// A video added from a link list has no title or duration up front, which
// would otherwise send the layout to "unknown" and silently disable the
// duration check. Existing non-empty values are kept.
func (s *Store) UpdateMetadata(ctx context.Context, itemID int64, title, uploader, uploadDate string, duration int64) error {
	_, err := s.db.ExecContext(ctx,
		"update items set "+
			"title = coalesce(nullif(title,''), nullif(?,'')), "+
			"uploader = coalesce(nullif(uploader,''), nullif(?,'')), "+
			"upload_date = coalesce(nullif(upload_date,''), nullif(?,'')), "+
			"duration = case when coalesce(duration,0) > 0 then duration else ? end, "+
			"updated_at = ? where id = ?",
		title, uploader, uploadDate, duration, s.unix(), itemID)
	return err
}

// ImportDownloaded registers a video that is already on disk as downloaded.
//
// It is an upsert: a video the queue has never seen is added, and one that is
// already queued is moved straight to downloaded rather than fetched again.
// Anything already done or uploading is left alone, since its file may have
// been deleted on purpose.
// @return true when the row was created or moved, false when it was skipped
func (s *Store) ImportDownloaded(ctx context.Context, it NewItem, path string, bytes int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := s.unix()
	url := it.URL
	if url == "" {
		url = "https://www.youtube.com/watch?v=" + it.VideoID
	}

	var state string
	err = tx.QueryRowContext(ctx, "select state from items where video_id = ?", it.VideoID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			"insert into items(video_id, url, title, uploader, upload_date, duration, "+
				"state, local_path, local_bytes, bytes_done, created_at, updated_at) "+
				"values(?,?,?,?,?,?,'downloaded',?,?,?,?,?)",
			it.VideoID, url, it.Title, it.Uploader, it.UploadDate, it.Duration,
			path, bytes, bytes, now, now); err != nil {
			return false, fmt.Errorf("cannot import %s: %w", it.VideoID, err)
		}
		return true, tx.Commit()

	case err != nil:
		return false, err
	}

	if state == string(StateDone) || state == string(StateUploading) {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		"update items set state = 'downloaded', local_path = ?, local_bytes = ?, bytes_done = ?, "+
			"title = coalesce(nullif(title,''), nullif(?,'')), "+
			"uploader = coalesce(nullif(uploader,''), nullif(?,'')), "+
			"upload_date = coalesce(nullif(upload_date,''), nullif(?,'')), "+
			"duration = case when coalesce(duration,0) > 0 then duration else ? end, "+
			"attempts = 0, last_error = null, lease_owner = null, lease_until = null, updated_at = ? "+
			"where video_id = ?",
		path, bytes, bytes, it.Title, it.Uploader, it.UploadDate, it.Duration,
		now, it.VideoID); err != nil {
		return false, fmt.Errorf("cannot import %s: %w", it.VideoID, err)
	}
	return true, tx.Commit()
}
